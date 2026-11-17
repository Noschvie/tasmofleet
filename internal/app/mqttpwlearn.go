package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/hellwand/tasmofleet/internal/audit"
	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

// mqttPWStore is what the learner needs of the store (*store.SQLite).
type mqttPWStore interface {
	ListDevices(ctx context.Context) ([]model.Device, error)
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	ApplyMQTTPasswordFromBackup(ctx context.Context, id model.DeviceID, mode model.MQTTPasswordMode, plain string, takenAt time.Time) (bool, error)
}

// mqttPWBackups is what the learner reads of the stored backups (*backup.Manager): files on disk, no device I/O.
type mqttPWBackups interface {
	List(ctx context.Context, id model.DeviceID) ([]model.Backup, error)
	Open(ctx context.Context, id model.BackupID) ([]byte, model.Backup, error)
}

// mqttPasswordLearner takes each device's MqttPassword from its newest decoded backup (#609): the decoded JSON holds
// it in clear under mqtt_pwd. A value equal to TasmoFleet's broker password becomes broker mode (no copy is stored),
// an empty one the empty mode, anything else the device's own, sealed by the store. The newer fact wins: the store
// writes only when the backup is newer than the stored password (a typed save or an earlier backup). It talks to
// no device. It keeps no state of its own: a pass over every device at every start (the pass after the upgrade
// included; it repeats harmlessly, since an equal password is not written), after a lost event (Resync) and when
// the switch is turned on, and one device after each backup event. The value never reaches a log, an error or the
// audit (safety rule 4): they name the device, the mode and the backup's date.
type mqttPasswordLearner struct {
	st      mqttPWStore
	backups mqttPWBackups
	broker  func() string // TasmoFleet's live broker password
	enabled func() bool   // MQTT_PASSWORD_FROM_BACKUPS
	audit   audit.Recorder
	clk     clock.Clock
	log     *slog.Logger
}

// startMQTTPasswordLearner wires the learner to the store, the backups, the hub and the switch.
func (a *runtime) startMQTTPasswordLearner(ctx context.Context) {
	l := &mqttPasswordLearner{st: a.svc.Store, backups: a.backups, audit: a.svc.Audit, clk: clock.Real{}, log: a.log,
		broker:  func() string { return a.svc.Sys.Snapshot().MQTTPassword },
		enabled: func() bool { return a.svc.Sys.Snapshot().MQTTPasswordFromBackups }}
	kick := make(chan struct{}, 1)
	a.svc.Sys.Subscribe(func(e sysconfig.Event) {
		if slices.Contains(e.Keys, "MQTT_PASSWORD_FROM_BACKUPS") && e.New.MQTTPasswordFromBackups {
			select {
			case kick <- struct{}{}:
			default: // a pass is already due
			}
		}
	})
	ch, unsub := a.svc.Hub.Subscribe(events.TopicBackup) // before the first pass: no backup between them is missed
	a.goBG("mqtt-password-learner", func() error { defer unsub(); l.run(ctx, ch, kick); return nil })
}

// run is the learner's loop until ctx ends or the hub closes ch.
func (l *mqttPasswordLearner) run(ctx context.Context, ch <-chan events.Event, kick <-chan struct{}) {
	l.all(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
			l.all(ctx)
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Resync { // events were dropped: one of them may have been a backup
				l.all(ctx)
				continue
			}
			b, isBackup := e.Payload.(model.Backup)
			if !isBackup || b.DeviceID == 0 { // the status bar's re-read ping, a backup of a deleted device
				continue
			}
			d, err := l.st.GetDevice(ctx, b.DeviceID)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					l.log.Warn("mqtt password learner: could not read the device", "device", int64(b.DeviceID))
				}
				continue
			}
			l.device(ctx, d)
		}
	}
}

// all runs the learner over every device.
func (l *mqttPasswordLearner) all(ctx context.Context) {
	if !l.enabled() {
		return
	}
	devs, err := l.st.ListDevices(ctx)
	if err != nil {
		l.log.Warn("mqtt password learner: could not list the devices")
		return
	}
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		l.device(ctx, d)
	}
}

// device takes d's MqttPassword from its newest own run, when that run has a decoded file that holds mqtt_pwd as a
// string and is newer than the stored password. A run of replacement hardware ("Replace device") is not the
// device's own, nor is a run whose snapshot does not show one of the device's MACs (runIsDevice); a newest run
// without a decoded file (.dmp only) teaches nothing, and no older run is read instead, since its password may be
// older than one the device has had since. A backup taken after a refusal that shows
// the stored password again is written too: the device holds that password, so the newer fact ends the refusal
// mark (it may have been about a password replaced while it was sent). Errors are logged with the device only: the
// texts of JSON errors can quote the input.
func (l *mqttPasswordLearner) device(ctx context.Context, d model.Device) {
	if !l.enabled() {
		return
	}
	list, err := l.backups.List(ctx, d.ID)
	if err != nil {
		l.log.Warn("mqtt password learner: could not list the device's backups", "device", int64(d.ID))
		return
	}
	takenAt, ok := backup.NewestOwnRun(list)
	if !ok || (!d.MQTTPasswordSetAt.IsZero() && !takenAt.After(d.MQTTPasswordSetAt)) {
		return // no backup, or the stored password is as new as the newest backup
	}
	i := slices.IndexFunc(list, func(b model.Backup) bool { return b.Kind == model.BackupDecoded && b.TakenAt.Equal(takenAt) })
	if i < 0 {
		return
	}
	if !l.runIsDevice(ctx, list, takenAt, d) {
		l.log.Info("mqtt password learner: the backup is not proven to be this device", "device", int64(d.ID))
		return
	}
	data, _, err := l.backups.Open(ctx, list[i].ID)
	if err != nil {
		l.log.Warn("mqtt password learner: could not read the decoded backup", "device", int64(d.ID))
		return
	}
	v, ok := mqttPwdOf(data)
	if !ok {
		return
	}
	mode := model.MQTTPasswordModeFor(v, l.broker())
	plain := ""
	if mode == model.MQTTPasswordOwn {
		plain = v
	}
	if mode == d.MQTTPasswordMode && (mode != model.MQTTPasswordOwn || plain == d.MQTTPassword) &&
		(d.MQTTPasswordBadAt.IsZero() || !takenAt.After(d.MQTTPasswordBadAt)) {
		return // the same password and no refusal older than this backup: nothing to write or audit
	}
	applied, err := l.st.ApplyMQTTPasswordFromBackup(ctx, d.ID, mode, plain, takenAt)
	if err != nil {
		l.log.Warn("mqtt password learner: could not store the password", "device", int64(d.ID))
		return
	}
	if !applied {
		return // a save landed in between, or the device is gone
	}
	at := takenAt.UTC().Format(time.RFC3339)
	l.log.Info("device MQTT password taken from its backup", "device", int64(d.ID), "mode", string(mode), "backup_taken_at", at)
	if l.audit != nil {
		b, jerr := json.Marshal(map[string]any{"mode": string(mode), "was": string(d.MQTTPasswordMode), "source": string(model.MQTTPasswordFromBackup),
			"backup_taken_at": at, "cleared_refused_mark": !d.MQTTPasswordBadAt.IsZero()})
		if jerr != nil {
			b = []byte("{}")
		}
		l.audit.Record(ctx, model.AuditEvent{At: l.clk.Now(), Actor: "system", Action: "device.mqtt_password",
			Target: "device:" + strconv.FormatInt(int64(d.ID), 10), Detail: string(b), Result: "ok"})
	}
}

// runIsDevice reports whether the run of d taken at takenAt shows d's MAC in its snapshot (backup.SnapshotShowsDevice).
// A dump over HTTP comes from whatever host answers at d's stored address: without this, another device's password
// would be stored as d's and sent on d's topic. No snapshot, a failed Status 5 or no MAC in the inventory prove nothing.
func (l *mqttPasswordLearner) runIsDevice(ctx context.Context, list []model.Backup, takenAt time.Time, d model.Device) bool {
	i := slices.IndexFunc(list, func(b model.Backup) bool { return b.Kind == model.BackupSnapshot && b.TakenAt.Equal(takenAt) })
	if i < 0 {
		return false
	}
	snap, _, err := l.backups.Open(ctx, list[i].ID)
	return err == nil && backup.SnapshotShowsDevice(snap, d)
}

// mqttPwdOf reads mqtt_pwd of a decoded backup. ok is false when the file is not JSON or the key is missing, null
// or not a string. No error is kept: its text could quote the file.
func mqttPwdOf(data []byte) (string, bool) {
	var doc struct {
		Pwd *string `json:"mqtt_pwd"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.Pwd == nil {
		return "", false
	}
	return *doc.Pwd, true
}
