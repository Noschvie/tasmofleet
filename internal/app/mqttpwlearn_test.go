package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/web"
)

// pwBackups is the stored backups as the learner reads them.
type pwBackups struct {
	mu    sync.Mutex
	runs  map[model.DeviceID][]model.Backup
	files map[model.BackupID][]byte
	macs  map[model.DeviceID]string // the MAC the snapshot of each run of the device reports
	next  model.BackupID
}

func newPWBackups() *pwBackups {
	return &pwBackups{runs: map[model.DeviceID][]model.Backup{}, files: map[model.BackupID][]byte{}, macs: map[model.DeviceID]string{}}
}

// add stores a run of id taken at at: the dump, the snapshot of the device itself (its MAC in Status 5), and the
// decoded file when decoded is not nil.
func (p *pwBackups) add(id model.DeviceID, at time.Time, note string, decoded []byte) {
	p.addFrom(id, at, note, decoded, p.mac(id))
}

func (p *pwBackups) mac(id model.DeviceID) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.macs[id]
}

// addFrom stores a run of id whose snapshot reports mac in Status 5: the host that answered at the device's address.
func (p *pwBackups) addFrom(id model.DeviceID, at time.Time, note string, decoded []byte, mac string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	p.runs[id] = append(p.runs[id], model.Backup{ID: p.next, DeviceID: id, Kind: model.BackupConfigDump, TakenAt: at, Note: note, Trigger: model.TriggerManual})
	p.next++
	p.runs[id] = append(p.runs[id], model.Backup{ID: p.next, DeviceID: id, Kind: model.BackupSnapshot, TakenAt: at, Note: note, Trigger: model.TriggerManual})
	p.files[p.next] = []byte(`{"Status5":{"StatusNET":{"Hostname":"x","Mac":"` + mac + `"}}}`)
	if decoded != nil {
		p.next++
		p.runs[id] = append(p.runs[id], model.Backup{ID: p.next, DeviceID: id, Kind: model.BackupDecoded, TakenAt: at, Note: note, Trigger: model.TriggerManual})
		p.files[p.next] = decoded
	}
}

// dropRun deletes the run of id taken at at.
func (p *pwBackups) dropRun(id model.DeviceID, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var keep []model.Backup
	for _, b := range p.runs[id] {
		if !b.TakenAt.Equal(at) {
			keep = append(keep, b)
		}
	}
	p.runs[id] = keep
}

func (p *pwBackups) List(_ context.Context, id model.DeviceID) ([]model.Backup, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Backup(nil), p.runs[id]...), nil
}

func (p *pwBackups) Open(_ context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.files[id], model.Backup{ID: id}, nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type pwAudit struct {
	mu     sync.Mutex
	events []model.AuditEvent
}

func (a *pwAudit) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

func (a *pwAudit) all() []model.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]model.AuditEvent(nil), a.events...)
}

type pwRig struct {
	st      *store.SQLite
	bk      *pwBackups
	audit   *pwAudit
	logs    *syncBuf
	enabled atomic.Bool
	l       *mqttPasswordLearner
}

const pwBroker = "broker-pw-1"

func newPWRig(t *testing.T) *pwRig {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	g := &pwRig{st: st, bk: newPWBackups(), audit: &pwAudit{}, logs: &syncBuf{}}
	g.enabled.Store(true)
	g.l = &mqttPasswordLearner{st: st, backups: g.bk, audit: g.audit, clk: clock.Real{},
		log:     slog.New(slog.NewTextHandler(g.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		broker:  func() string { return pwBroker },
		enabled: g.enabled.Load}
	return g
}

// device creates a device whose MAC ends in name; its runs (pwBackups.add) come from that MAC.
func (g *pwRig) device(t *testing.T, name string) model.DeviceID {
	t.Helper()
	return g.deviceWithMAC(t, name, "aa:bb:cc:00:00:0"+name)
}

func (g *pwRig) deviceWithMAC(t *testing.T, name, mac string) model.DeviceID {
	t.Helper()
	id, err := g.st.CreateDevice(t.Context(), model.Device{Name: name, Address: "10.0.0." + name, MAC: mac})
	if err != nil {
		t.Fatal(err)
	}
	g.bk.mu.Lock()
	g.bk.macs[id] = mac
	g.bk.mu.Unlock()
	return id
}

func (g *pwRig) get(t *testing.T, id model.DeviceID) model.Device {
	t.Helper()
	d, err := g.st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var pwTaken = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)

// Why (#609): the decoded backup holds the device's MqttPassword; reading it saves the person typing it for
// every device. A value equal to the broker password is broker mode (no copy, a later broker change follows),
// an empty one the empty mode, anything else the device's own. A file without the key, or with a value that is
// no string, teaches nothing. Each change is audited with its source and the backup's date; a second pass (the
// next start) finds the same passwords and writes nothing, so restarts do not flood the audit.
func TestABackupTeachesTheDevicesMQTTPassword(t *testing.T) {
	g := newPWRig(t)
	own, broker, empty, missing, null := g.device(t, "1"), g.device(t, "2"), g.device(t, "3"), g.device(t, "4"), g.device(t, "5")
	g.bk.add(own, pwTaken, "", []byte(`{"mqtt_pwd":"device-pw-1","mqtt_user":"u"}`))
	g.bk.add(broker, pwTaken, "", []byte(`{"mqtt_pwd":"`+pwBroker+`"}`))
	g.bk.add(empty, pwTaken, "", []byte(`{"mqtt_pwd":""}`))
	g.bk.add(missing, pwTaken, "", []byte(`{"mqtt_user":"u"}`))
	g.bk.add(null, pwTaken, "", []byte(`{"mqtt_pwd":null}`))
	g.l.all(t.Context())
	for _, tt := range []struct {
		id   model.DeviceID
		mode model.MQTTPasswordMode
		pw   string
	}{{own, model.MQTTPasswordOwn, "device-pw-1"}, {broker, model.MQTTPasswordBroker, ""}, {empty, model.MQTTPasswordEmpty, ""},
		{missing, model.MQTTPasswordNone, ""}, {null, model.MQTTPasswordNone, ""}} {
		d := g.get(t, tt.id)
		if d.MQTTPasswordMode != tt.mode || d.MQTTPassword != tt.pw {
			t.Errorf("device %d: %q/%q, want %q/%q", tt.id, d.MQTTPasswordMode, d.MQTTPassword, tt.mode, tt.pw)
		}
		if tt.mode != model.MQTTPasswordNone && (d.MQTTPasswordSource != model.MQTTPasswordFromBackup || !d.MQTTPasswordSetAt.Equal(pwTaken)) {
			t.Errorf("device %d: source %q set at %v", tt.id, d.MQTTPasswordSource, d.MQTTPasswordSetAt)
		}
	}
	ev := g.audit.all()
	if len(ev) != 3 {
		t.Fatalf("want one audit entry per learned device, got %+v", ev)
	}
	for _, e := range ev {
		if e.Action != "device.mqtt_password" || e.Actor != "system" || !strings.Contains(e.Detail, `"source":"backup"`) ||
			!strings.Contains(e.Detail, `"backup_taken_at":"2026-11-17T03:00:00Z"`) {
			t.Errorf("audit %+v", e)
		}
	}
	g.l.all(t.Context())
	if n := len(g.audit.all()); n != 3 {
		t.Fatalf("a second pass wrote again: %d audit entries", n)
	}
}

// Why (#609): the safety backup of "Replace device" is the dump of other hardware filed under the dead device;
// its password is not this device's. Only the device's own newest run counts, and when that run has no decoded
// file (.dmp only), nothing is learned: an older run's password may be older than one the device has had since.
func TestReplacementHardwareTeachesNothing(t *testing.T) {
	g := newPWRig(t)
	id := g.device(t, "1")
	g.bk.add(id, pwTaken, "", []byte(`{"mqtt_pwd":"own-pw-1"}`))
	g.bk.add(id, pwTaken.Add(time.Hour), backup.NoteReplacementHardware, []byte(`{"mqtt_pwd":"spare-pw-1"}`))
	g.l.all(t.Context())
	if d := g.get(t, id); d.MQTTPassword != "own-pw-1" {
		t.Fatalf("learned %q/%q, want the device's own run", d.MQTTPasswordMode, d.MQTTPassword)
	}
	dmpOnly := g.device(t, "2")
	g.bk.add(dmpOnly, pwTaken, "", []byte(`{"mqtt_pwd":"old-pw-1"}`))
	g.bk.add(dmpOnly, pwTaken.Add(time.Hour), "", nil)
	g.l.all(t.Context())
	if d := g.get(t, dmpOnly); d.MQTTPasswordMode != model.MQTTPasswordNone {
		t.Fatalf("learned %q/%q from an older run behind a .dmp-only one", d.MQTTPasswordMode, d.MQTTPassword)
	}
}

// Why (#609): taking passwords from backups can be turned off. Off, nothing is learned; turning it on runs a
// pass at once, so a fleet backed up while it was off does not wait for the next backup.
func TestTheSwitchOffLearnsNothing(t *testing.T) {
	g := newPWRig(t)
	id := g.device(t, "1")
	g.bk.add(id, pwTaken, "", []byte(`{"mqtt_pwd":"device-pw-1"}`))
	g.enabled.Store(false)
	g.l.all(t.Context())
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); d.MQTTPasswordMode != model.MQTTPasswordNone || len(g.audit.all()) != 0 {
		t.Fatalf("learned %q while off", d.MQTTPasswordMode)
	}
	ch, kick := make(chan events.Event), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { g.l.run(ctx, ch, kick); close(done) }()
	defer func() { cancel(); <-done }()
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Payload: model.Backup{DeviceID: 999}}) // received after the start's pass
	g.enabled.Store(true)
	kick <- struct{}{}
	waitLearned(t, g, id, model.MQTTPasswordOwn)
}

// sendEvent hands e to the learner's loop, or fails when the loop does not take it (it ended).
func sendEvent(t *testing.T, ch chan<- events.Event, e events.Event) {
	t.Helper()
	select {
	case ch <- e:
	case <-time.After(5 * time.Second):
		t.Fatal("the learner's loop did not take the event")
	}
}

func waitLearned(t *testing.T, g *pwRig, id model.DeviceID, mode model.MQTTPasswordMode) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.get(t, id).MQTTPasswordMode != mode {
		if time.Now().After(deadline) {
			t.Fatalf("device %d never learned mode %q", id, mode)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Why (#609): the hub drops events for a slow subscriber and leaves a Resync marker; one of the dropped events may
// have been a backup, so a Resync must run the full pass. A backup event teaches its device. The status bar's
// re-read ping (an empty Backup, DeviceID 0) is no backup and must not start a pass of every device.
func TestAResyncRunsTheFullPass(t *testing.T) {
	g := newPWRig(t)
	a, b, c := g.device(t, "1"), g.device(t, "2"), g.device(t, "3")
	ch, kick := make(chan events.Event), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { g.l.run(ctx, ch, kick); close(done) }()
	defer func() { cancel(); <-done }()
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Payload: model.Backup{DeviceID: c}}) // received after the start's pass
	g.bk.add(a, pwTaken, "", []byte(`{"mqtt_pwd":"a-pw-1"}`))
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Payload: model.Backup{}})
	g.bk.add(b, pwTaken, "", []byte(`{"mqtt_pwd":""}`))
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Payload: model.Backup{DeviceID: b, Kind: model.BackupConfigDump}})
	waitLearned(t, g, b, model.MQTTPasswordEmpty)
	if d := g.get(t, a); d.MQTTPasswordMode != model.MQTTPasswordNone {
		t.Fatalf("the ping or another device's backup taught device %d: %q", a, d.MQTTPasswordMode)
	}
	g.bk.add(c, pwTaken, "", []byte(`{"mqtt_pwd":"`+pwBroker+`"}`))
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Resync: true})
	waitLearned(t, g, a, model.MQTTPasswordOwn)
	waitLearned(t, g, c, model.MQTTPasswordBroker)
}

// Why (#609, "the newer fact wins"): a password typed after the newest backup stays; a backup taken after the
// save replaces it. When the newest run is deleted, the one left is older than the password learned from the
// deleted run, so the delete event must not bring the older password back.
func TestTheNewerFactWinsOverTheBackup(t *testing.T) {
	g := newPWRig(t)
	id := g.device(t, "1")
	if err := g.st.SetMQTTPassword(t.Context(), id, model.MQTTPasswordOwn, "typed-pw-1"); err != nil {
		t.Fatal(err)
	}
	typedAt := g.get(t, id).MQTTPasswordSetAt
	g.bk.add(id, typedAt.Add(-time.Hour), "", []byte(`{"mqtt_pwd":"older-pw-1"}`))
	g.l.all(t.Context())
	if d := g.get(t, id); d.MQTTPassword != "typed-pw-1" {
		t.Fatalf("an older backup replaced the typed password: %q", d.MQTTPassword)
	}
	newer := typedAt.Add(time.Hour)
	g.bk.add(id, newer, "", []byte(`{"mqtt_pwd":"newer-pw-1"}`))
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); d.MQTTPassword != "newer-pw-1" {
		t.Fatalf("a newer backup did not replace the typed password: %q", d.MQTTPassword)
	}
	g.bk.dropRun(id, newer)
	ch, kick := make(chan events.Event), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { g.l.run(ctx, ch, kick); close(done) }()
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Payload: model.Backup{DeviceID: id}})
	sendEvent(t, ch, events.Event{Topic: events.TopicBackup, Payload: model.Backup{DeviceID: id}}) // processed after the first
	cancel()
	<-done
	if d := g.get(t, id); d.MQTTPassword != "newer-pw-1" {
		t.Fatalf("the delete brought the older password back: %q", d.MQTTPassword)
	}
}

// Why (#609, "the newer fact wins"): a refusal mark blocks every MQTT transfer of the device. A backup taken
// after the refusal that shows the stored password again proves the device holds it, so the mark must end there
// and say so in the audit; otherwise it stays until a person saves, since every later backup shows the same
// value. A backup taken before the refusal is the older fact and leaves the mark.
func TestANewerBackupWithTheSamePasswordEndsARefusal(t *testing.T) {
	g := newPWRig(t)
	id := g.device(t, "1")
	g.bk.add(id, pwTaken, "", []byte(`{"mqtt_pwd":"device-pw-1"}`))
	g.l.all(t.Context())
	refused := pwTaken.Add(time.Hour)
	if err := g.st.MarkMQTTPasswordBad(t.Context(), id, refused); err != nil {
		t.Fatal(err)
	}
	g.bk.add(id, refused.Add(-time.Minute), "", []byte(`{"mqtt_pwd":"device-pw-1"}`))
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); !d.MQTTPasswordBadAt.Equal(refused) || len(g.audit.all()) != 1 {
		t.Fatalf("a backup older than the refusal ended it: bad %v, audit %d", d.MQTTPasswordBadAt, len(g.audit.all()))
	}
	newer := refused.Add(time.Hour)
	g.bk.add(id, newer, "", []byte(`{"mqtt_pwd":"device-pw-1"}`))
	g.l.device(t.Context(), g.get(t, id))
	d := g.get(t, id)
	if !d.MQTTPasswordBadAt.IsZero() || d.MQTTPassword != "device-pw-1" || !d.MQTTPasswordSetAt.Equal(newer) {
		t.Fatalf("a newer backup with the same password kept the refusal: bad %v, %q set at %v", d.MQTTPasswordBadAt, d.MQTTPassword, d.MQTTPasswordSetAt)
	}
	ev := g.audit.all()
	if len(ev) != 2 || !strings.Contains(ev[1].Detail, `"cleared_refused_mark":true`) {
		t.Fatalf("audit %+v", ev)
	}
}

// Why (safety rule 4): the learner reads a secret. Neither the log nor the audit may hold it: they name the
// device, the mode and the backup's date. A decoded file that is not valid JSON is skipped without logging the
// parser's error, whose text can quote the file and with it the password.
func TestTheAuditAndLogNameTheDateNeverTheValue(t *testing.T) {
	g := newPWRig(t)
	broken, good := g.device(t, "1"), g.device(t, "2")
	g.bk.add(broken, pwTaken, "", []byte(`{"mqtt_pwd":"leak-pw-1", oops`))
	g.bk.add(good, pwTaken, "", []byte(`{"mqtt_pwd":"secret-pw-2"}`))
	g.l.all(t.Context())
	if d := g.get(t, good); d.MQTTPassword != "secret-pw-2" {
		t.Fatalf("not learned: %q", d.MQTTPasswordMode)
	}
	logs := g.logs.String()
	if !strings.Contains(logs, "backup_taken_at=2026-11-17T03:00:00Z") {
		t.Errorf("the log does not name the backup's date: %s", logs)
	}
	for _, secret := range []string{"leak-pw-1", "secret-pw-2"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q: %s", secret, logs)
		}
		for _, e := range g.audit.all() {
			if strings.Contains(e.Detail, secret) {
				t.Errorf("the audit holds %q: %s", secret, e.Detail)
			}
		}
	}
}

// Why (#609, rubric security): a dump over HTTP comes from whatever host answers at the device's stored address.
// After a DHCP reassignment that is another Tasmota, and its MqttPassword would be stored as this device's own and
// then sent in every FileDownload start on this device's topic: another device's credential on the broker, a
// refusal, and (since a newer backup showing the stored password ends a refusal) the same again every other run.
// Only a run whose snapshot shows one of the device's MACs teaches; a device without a MAC in the inventory learns
// nothing, as its files are not read over HTTP either (backup.NoMACWhy). The device's own next run still teaches.
func TestABackupFromAnotherHostTeachesNothing(t *testing.T) {
	g := newPWRig(t)
	const mine, other = "AA:BB:CC:00:00:01", "AA:BB:CC:00:00:02"
	id := g.deviceWithMAC(t, "1", mine)
	g.bk.addFrom(id, pwTaken, "", []byte(`{"mqtt_pwd":"other-pw-1"}`), other)
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); d.MQTTPasswordMode != model.MQTTPasswordNone || d.MQTTPassword != "" || !d.MQTTPasswordSetAt.IsZero() || len(g.audit.all()) != 0 {
		t.Fatalf("another host's backup taught %q/%q set at %v, audit %d", d.MQTTPasswordMode, d.MQTTPassword, d.MQTTPasswordSetAt, len(g.audit.all()))
	}
	noMAC := g.deviceWithMAC(t, "2", "")
	g.bk.addFrom(noMAC, pwTaken, "", []byte(`{"mqtt_pwd":"other-pw-1"}`), other)
	g.l.device(t.Context(), g.get(t, noMAC))
	if d := g.get(t, noMAC); d.MQTTPasswordMode != model.MQTTPasswordNone {
		t.Fatalf("a device without a MAC learned %q from a run nothing ties to it", d.MQTTPasswordMode)
	}

	// The device's own run teaches; a refusal of that password is then not ended by another host showing the same value.
	g.bk.addFrom(id, pwTaken.Add(time.Hour), "", []byte(`{"mqtt_pwd":"p"}`), "aa-bb-cc-00-00-01")
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); d.MQTTPasswordMode != model.MQTTPasswordOwn || d.MQTTPassword != "p" {
		t.Fatalf("the device's own run did not teach: %q/%q", d.MQTTPasswordMode, d.MQTTPassword)
	}
	refused := pwTaken.Add(2 * time.Hour)
	if err := g.st.MarkMQTTPasswordBad(t.Context(), id, refused); err != nil {
		t.Fatal(err)
	}
	g.bk.addFrom(id, refused.Add(time.Hour), "", []byte(`{"mqtt_pwd":"p"}`), other)
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); !d.MQTTPasswordBadAt.Equal(refused) || len(g.audit.all()) != 1 {
		t.Fatalf("another host's backup ended the refusal: bad %v, audit %d", d.MQTTPasswordBadAt, len(g.audit.all()))
	}
}

// pwClock is a clock the test moves; the backup run reads it.
type pwClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pwClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *pwClock) After(time.Duration) <-chan time.Time { return nil }
func (c *pwClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// slowDevice answers a backup run: the dump, Status 5 with its MAC, nothing else. onSnapshot runs at the
// snapshot's first command, after the dump was read.
type slowDevice struct {
	mac        string
	onSnapshot func()
	once       sync.Once
}

func (f *slowDevice) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	f.once.Do(f.onSnapshot)
	if cmnd == "Status 5" {
		return json.RawMessage(`{"StatusNET":{"Mac":"` + f.mac + `"}}`), nil
	}
	return nil, tasmota.ErrUnknownCommand
}
func (f *slowDevice) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return []byte("DMP-FAKE"), nil
}
func (f *slowDevice) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, tasmota.ErrUnknownCommand
}

type oldPwDecoder struct{}

func (oldPwDecoder) Version(context.Context) (string, error) { return "16.0.0", nil }
func (oldPwDecoder) Decode(context.Context, []byte) ([]byte, error) {
	return []byte(`{"mqtt_pwd":"old-pw-1"}`), nil
}

// Why (#609, "the newer fact wins"): a backup's time is when the device's settings were read, not when the run
// stores its files after the snapshot and the files step. A password saved after the read is the newer fact; were
// the run stamped at the end, the older dump would replace it, be audited as "taken from the backup", and the next
// MQTT transfer would send the stale password, be refused, and stay refused on a device backed up only over MQTT.
func TestAPasswordSavedWhileABackupRunsIsKept(t *testing.T) {
	g := newPWRig(t)
	id := g.device(t, "1")
	clk := &pwClock{t: time.Now().Add(-time.Minute)} // the read; the store stamps the save with the real time
	dev := &slowDevice{mac: "aa:bb:cc:00:00:01", onSnapshot: func() {
		if err := g.st.SetMQTTPassword(t.Context(), id, model.MQTTPasswordOwn, "new-pw-1"); err != nil {
			t.Error(err)
		}
		clk.set(time.Now().Add(time.Minute)) // the snapshot and the files step take their time
	}}
	bm, err := backup.New(backup.Options{DataDir: t.TempDir(), Devices: g.st, Store: g.st, Client: dev, Decoder: oldPwDecoder{},
		Clock: clk, Log: slog.New(slog.NewTextHandler(g.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bm.Backup(t.Context(), "t", id); err != nil {
		t.Fatal(err)
	}
	g.l.backups = bm
	g.l.device(t.Context(), g.get(t, id))
	if d := g.get(t, id); d.MQTTPassword != "new-pw-1" || d.MQTTPasswordSource != model.MQTTPasswordTyped {
		t.Fatalf("the dump read before the save replaced it: %q from %q", d.MQTTPassword, d.MQTTPasswordSource)
	}
	if ev := g.audit.all(); len(ev) != 0 {
		t.Fatalf("audited a password the save had replaced: %+v", ev)
	}
}

// Why (#609, the card "Not known"): a person who sees devices without a password must be told why each has none,
// or the list is a riddle. The learner is the one place that knows its rules (no backup, only a .dmp, a snapshot
// that does not show the device, the switch), so the card asks it instead of keeping a copy of them. The answer
// holds reason codes only: a password in it would reach the HTML.
func TestTheLearnerSaysWhyADeviceHasNoPassword(t *testing.T) {
	g := newPWRig(t)
	none, dmp, foreign, nofield, usable := g.device(t, "1"), g.device(t, "2"), g.device(t, "3"), g.device(t, "4"), g.device(t, "5")
	g.bk.add(dmp, pwTaken, "", nil)
	g.bk.addFrom(foreign, pwTaken, "", []byte(`{"mqtt_pwd":"other-pw-1"}`), "aa:bb:cc:99:99:99")
	g.bk.add(nofield, pwTaken, "", []byte(`{"mqtt_user":"u"}`))
	g.bk.add(usable, pwTaken, "", []byte(`{"mqtt_pwd":"secret-pw-1"}`))
	ids := []model.DeviceID{none, dmp, foreign, nofield, usable}
	want := map[model.DeviceID]web.PasswordGap{none: web.GapNoBackup, dmp: web.GapDmpOnly, foreign: web.GapNotProven, nofield: web.GapNoField, usable: web.GapPending}
	got := g.l.Gaps(t.Context(), ids)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("device %d: gap %q, want %q", id, got[id], w)
		}
	}
	if strings.Contains(fmt.Sprint(got), "secret-pw-1") || strings.Contains(fmt.Sprint(got), "other-pw-1") {
		t.Fatalf("a gap carries a password: %v", got)
	}
	g.enabled.Store(false)
	for id, gap := range g.l.Gaps(t.Context(), ids) {
		if gap != web.GapOff {
			t.Errorf("switch off: device %d gap %q, want %q", id, gap, web.GapOff)
		}
	}
	if d := g.get(t, usable); d.MQTTPasswordMode != model.MQTTPasswordNone {
		t.Fatalf("asking for the reasons wrote a password: %q", d.MQTTPasswordMode)
	}
}
