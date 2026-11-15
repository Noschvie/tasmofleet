package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// step stores a state change; a failure to store is logged, the run goes on.
func (m *Manager) step(ctx context.Context, j *Job, state, detail string) {
	j.State, j.Detail = state, detail
	// A restore cancelled by a shutdown must still record how it ended: the store write outlives the run's context.
	if err := m.o.Store.UpdateRestore(context.WithoutCancel(ctx), *j); err != nil {
		m.o.Log.Error("restore: store state", "component", "restore", "job", j.ID, "err", err)
	}
}

func (m *Manager) fail(ctx context.Context, j *Job, why string) {
	m.step(ctx, j, store.RestoreFailed, why)
	m.record(ctx, j.Actor, "restore.finish", j.ID, j.DeviceID, "error: "+why, "")
}

// runOptions is what the start decided for the run of one job.
type runOptions struct {
	confirmedFiles int // the files the confirmation page promised (StartOptions.ConfirmedFiles)
	// connRisk: a restore over MQTT whose backup changes the device's MQTT connection, or cannot be compared (the
	// sentence of mqttLockout); "" otherwise and over HTTP.
	connRisk  string
	lockoutOK bool // the admin accepted that risk (StartOptions.AllowMQTTLockout)
}

// run is the whole restore of one job. Order matters: the safety backup comes
// first and a failure there stops everything, because without it a bad
// restore cannot be undone. source is the TakenAt of the chosen backup: the
// retention that follows the safety backup (and the verification backup) must
// not delete the restore point, or a failed restore could not be retried.
func (m *Manager) run(ctx context.Context, j Job, dev model.Device, dmp []byte, source time.Time, ro runOptions) {
	target := tasmota.TargetFor(dev)
	ctx = backup.WithRun(ctx, backup.Run{Trigger: model.TriggerRestore, Protect: []time.Time{source}})
	if j.Transport == model.TransportMQTT {
		ctx = transport.WithPath(ctx, transport.MQTT) // the safety backup, the checks and the verification too
	} else {
		ctx = transport.WithPath(ctx, transport.HTTP) // a restore that began over HTTP never moves to MQTT
	}

	m.step(ctx, &j, store.RestoreBackup, "taking a backup of the current configuration")
	pre, err := m.o.Backups.Backup(ctx, j.Actor, dev.ID)
	if err != nil {
		m.fail(ctx, &j, failPreBackup+err.Error())
		return
	}
	for _, b := range pre {
		if b.Kind == model.BackupConfigDump {
			j.PreBackupID = b.ID
		}
	}

	via := m.filesRoute(j, dev)
	// The files go over HTTP, to the device's address: the identity is checked there, whatever carries the settings.
	identity := func() error { return m.checkIdentity(ctx, dev) }
	if via.settingsOverMQTT {
		identity = func() error { return m.checkIdentity(transport.WithPath(ctx, transport.HTTP), dev) }
	}
	fo, err := m.filesStep(ctx, &j, target, func(c context.Context) (string, error) { return m.liveVersion(c, dev) }, via,
		identity, ro.confirmedFiles)
	if err != nil {
		m.failFiles(ctx, &j, err)
		return
	}
	m.send(ctx, &j, dev, target, dmp, fo, ro)
}

// filesRoute is how the files of job j reach dev: over HTTP always; for a restore whose settings go over MQTT an HTTP
// that cannot be used leaves the files out instead of failing the run.
func (m *Manager) filesRoute(j Job, dev model.Device) filesVia {
	if j.Transport != model.TransportMQTT {
		return filesVia{}
	}
	v := filesVia{settingsOverMQTT: true}
	if dev.Address == "" {
		v.noHTTP = whyFilesNoAddress
		return v
	}
	v.sameHost = func(ctx context.Context) error { return m.checkIdentity(transport.WithPath(ctx, transport.HTTP), dev) }
	return v
}

// httpReaches reports whether the device's HTTP API answers right now as this device (Status 5 with one of its MACs):
// the proof that a restore over MQTT which changes the device's MQTT connection cannot cut TasmoFleet off from it
// (#607; an answer on record is no proof, docs/pitfalls.md). dev.Address is the one IP (httpAddress); without one HTTP
// cannot be asked.
func (m *Manager) httpReaches(ctx context.Context, dev model.Device) bool {
	if dev.Address == "" {
		return false
	}
	return m.checkIdentity(transport.WithPath(ctx, transport.HTTP), dev) == nil
}

// send is the upload half of a restore: identity, the boot count before, the upload (the HTTP restore upload, or FileUpload over MQTT), wait for the restart. Over both
// paths a refused or aborted upload leaves the device unchanged: over HTTP the device answers its
// "Failed" page and keeps running, over MQTT it applies the dump only after the MD5 matched.
//
// A restore over MQTT that changes the device's MQTT connection (ro.connRisk) went ahead without the admin's
// acceptance only because HTTP answered at the start; HTTP must still answer right before the upload, or nothing is
// uploaded. HTTP is asked right before the upload also when the admin accepted the risk. After such an upload with HTTP
// answering right before it the device is marked (MarkMQTTConnChanged), and the wait for the restart asks HTTP
// whenever the MQTT topic is silent: the device comes back under the restored connection, which may not be the topic
// TasmoFleet still knows. Without that proof (an accepted lockout while HTTP is silent, or the host at the address is
// another device) the wait never asks HTTP: Status 1 and 11 carry the stored web credentials, which must not reach a
// host that was not shown to be this device.
func (m *Manager) send(ctx context.Context, j *Job, dev model.Device, target tasmota.Target, dmp []byte, fo *filesOutcome, ro runOptions) {
	viaMQTT := j.Transport == model.TransportMQTT
	if viaMQTT {
		m.step(ctx, j, store.RestoreSending, "uploading the configuration over MQTT")
	} else {
		m.step(ctx, j, store.RestoreSending, "uploading the configuration")
	}
	// The address may have changed hands since Start (DHCP): the dump of this device must never
	// go to another one.
	if err := m.checkIdentity(ctx, dev); err != nil {
		m.fail(ctx, j, fo.afterFiles(failUnchanged+err.Error()))
		return
	}
	changesConn := viaMQTT && ro.connRisk != ""
	httpProved := false
	if changesConn {
		// Asked also when the admin accepted the risk: HTTP that answers again by now still marks the device below.
		if httpProved = m.httpReaches(ctx, dev); !httpProved && !ro.lockoutOK {
			m.fail(ctx, j, fo.afterFiles(failUnchanged+"the device's HTTP API stopped answering before the upload, and "+ro.connRisk+
				", so TasmoFleet could lose the device; start the restore again to confirm that risk"))
			return
		}
	}
	before, err := m.bootCount(ctx, target)
	if err != nil {
		m.fail(ctx, j, fo.afterFiles(failUnreachable+err.Error()))
		return
	}
	began := m.o.Clock.Now()
	if viaMQTT {
		err := m.o.Uploader.UploadConfig(ctx, target, dmp, transport.BindingOf(dev))
		if httpProved && (err == nil || errors.Is(err, mqttcmd.ErrOutcomeUnknown)) && m.o.MarkMQTTConnChanged != nil {
			m.o.MarkMQTTConnChanged(dev.ID) // the device may not be back on the broker: HTTP carries its next commands
		}
		if err != nil {
			// A lost "Done" is an unknown outcome: the device may have applied the dump and be
			// restarting. Look at the device instead of failing; the upload is never repeated.
			if !errors.Is(err, mqttcmd.ErrOutcomeUnknown) {
				m.fail(ctx, j, fo.afterFiles(failUpload+err.Error()))
				return
			}
			m.o.Log.Warn("restore: FileUpload outcome unknown, checking the device", "component", "restore", "job", j.ID)
		}
	} else if err := m.o.Client.(tasmota.ConfigRestorer).RestoreConfig(ctx, target, dmp); err != nil {
		// The "Failed" page and an upload that never left are certain; anything else (a timeout,
		// a lost or unreadable reply) may come after the device applied the dump: look at the
		// device, never send it again.
		if errors.Is(err, tasmota.ErrUploadRefused) || errors.Is(err, tasmota.ErrNotSent) {
			m.fail(ctx, j, fo.afterFiles(failRestoreUpload+err.Error()))
			return
		}
		m.o.Log.Warn("restore: upload outcome unknown, checking the device", "component", "restore", "job", j.ID, "err", err)
	}
	m.step(ctx, j, store.RestoreWaiting, "waiting for the restart")
	var alt context.Context // the other transport the restart may show on
	if httpProved {
		alt = transport.WithPath(ctx, transport.HTTP)
	}
	proved, err := m.waitBoot(ctx, alt, target, before, began)
	if err != nil {
		m.fail(ctx, j, err.Error())
		return
	}
	m.finish(proved, *j, dev, dmp, fo)
}

// bootCount reads Status 1 (a fresh answer from the device, not the INFO3 of an earlier boot).
func (m *Manager) bootCount(ctx context.Context, t tasmota.Target) (int, error) {
	raw, err := m.o.Client.Command(ctx, t, "Status 1")
	if err != nil {
		return 0, err
	}
	var s struct {
		StatusPRM struct{ BootCount *int }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusPRM.BootCount == nil {
		return 0, tasmota.ErrBadResponse
	}
	return *s.StatusPRM.BootCount, nil
}

// waitBoot returns when Status 1 answers with a larger BootCount than before the upload and
// Status 11 shows an uptime shorter than the time since the upload began (the device really
// restarted since: a count that rose for another reason, or a stale answer, is not a restart). A
// restore does not bring the backup's BootCount back: it sits in the first 16 bytes of the
// settings, which SettingsConfigRestore does not copy (v16 settings.ino 557, tasmota_types.h 490).
// The firmware counts the boot at 10 s of uptime, so the count rises a few polls after the device
// answers again. While the device restarts the poll fails (LWT Offline, refused or timed out);
// that just means "not yet" until the deadline.
//
// alt, when set, is the other transport a poll tries when ctx's gets no answer (#607: after a restore over MQTT that
// changed the device's MQTT connection, the device comes back under the restored one, so the old topic stays silent
// although it restarted). The result is the context of the transport that proved the restart: the verification runs
// on it, without the polls' transport.WithRestarting: an HTTP poll the restarting device leaves unanswered says nothing
// about its HTTP API, but the check backup's HTTP calls do (#854).
func (m *Manager) waitBoot(ctx, alt context.Context, t tasmota.Target, before int, began time.Time) (context.Context, error) {
	deadline := m.o.Clock.Now().Add(m.o.RebootTimeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.o.Clock.After(m.o.PollInterval):
		}
		for _, c := range []context.Context{ctx, alt} {
			if c == nil {
				continue
			}
			// The device restarts: a poll it leaves unanswered must not mark its HTTP failing (#854).
			poll := transport.WithRestarting(c)
			n, err := m.bootCount(poll, t)
			if err != nil {
				continue // no answer on this transport: the next one may have it
			}
			if n > before {
				if up, uerr := m.uptime(poll, t); uerr == nil && time.Duration(up)*time.Second <= m.o.Clock.Now().Sub(began)+2*time.Second {
					return c, nil
				}
			}
			break // an answer: the device is not back yet, whatever the other transport would say
		}
		if m.o.Clock.Now().After(deadline) {
			return nil, errors.New("the device did not restart in time (its boot count did not change); the configuration may or may not have been applied")
		}
	}
}

// finish takes the fresh backup and compares it with the restored one.
func (m *Manager) finish(ctx context.Context, j Job, dev model.Device, dmp []byte, fo *filesOutcome) {
	m.step(ctx, &j, store.RestoreVerifying, "taking a fresh backup to verify")
	if err := m.checkIdentity(ctx, dev); err != nil {
		m.fail(ctx, &j, failAfterRestore+err.Error())
		return
	}
	detail, verified, changes, err := m.verify(ctx, j, dmp, func() ([]model.Backup, error) {
		return m.o.Backups.Backup(ctx, j.Actor, j.DeviceID)
	}, fo.check)
	if err != nil {
		m.fail(ctx, &j, failAfterRestore+"verification failed: "+err.Error())
		return
	}
	j.Verified, j.Changes = verified, changes
	detail = fo.withSummary(detail)
	m.step(ctx, &j, store.RestoreDone, detail)
	m.record(ctx, j.Actor, "restore.finish", j.ID, j.DeviceID, "ok", detail)
}

// failFiles ends the job with the failure text of the files step.
func (m *Manager) failFiles(ctx context.Context, j *Job, err error) {
	var ff *failedFiles
	if errors.As(err, &ff) {
		m.fail(ctx, j, ff.text)
		return
	}
	m.fail(ctx, j, failFilesNone+err.Error())
}

func (m *Manager) uptime(ctx context.Context, t tasmota.Target) (int64, error) {
	raw, err := m.o.Client.Command(ctx, t, "Status 11")
	if err != nil {
		return 0, err
	}
	st, err := tasmota.ParseStatus11(raw)
	if err != nil {
		return 0, err
	}
	return st.UptimeSec, nil
}

// verify takes a fresh backup (take) and compares it with the restored one.
// The detail names paths only, never values.
func (m *Manager) verify(ctx context.Context, j Job, dmp []byte, take func() ([]model.Backup, error), fc *filesCheck) (detail string, verified bool, changes int, err error) {
	fresh, err := take()
	if err != nil {
		return "", false, 0, err
	}
	detail, verified, changes, err = m.verifySettings(ctx, j, dmp, fresh)
	if err != nil || fc == nil {
		return detail, verified, changes, err
	}
	if fd, ok := m.verifyFiles(ctx, fresh, fc); fd != "" {
		detail, verified = detail+"; "+fd, verified && ok
	}
	return detail, verified, changes, nil
}

// verifySettings compares the fresh dump with the restored one.
func (m *Manager) verifySettings(ctx context.Context, j Job, dmp []byte, fresh []model.Backup) (detail string, verified bool, changes int, err error) {
	var freshDmp, freshDec []byte
	for _, b := range fresh {
		switch b.Kind {
		case model.BackupConfigDump:
			freshDmp, _, err = m.o.Backups.Open(ctx, b.ID)
		case model.BackupDecoded:
			freshDec, _, err = m.o.Backups.Open(ctx, b.ID)
		case model.BackupSnapshot, model.BackupFiles:
			// not needed for the comparison
		}
		if err != nil {
			return "", false, 0, err
		}
	}
	if bytes.Equal(freshDmp, dmp) {
		return "restored; the device's configuration dump is identical to the backup", true, 0, nil
	}
	want, err := m.decodedOf(ctx, j.BackupID)
	if err != nil {
		// The copy may exist: saying it is missing would send the person to look for the wrong cause.
		m.o.Log.Warn("restore: reading the decoded copy of the restored backup", "component", "restore", "job", j.ID, "err", err)
		return "restored; the dumps differ in bytes and the decoded copy of the backup being restored could not be read", false, 0, nil //nolint:nilerr // logged; an unreadable copy is a result of the check, not a failed restore
	}
	if want == nil || freshDec == nil {
		return noDecodedCopyText(want == nil, freshDec == nil), false, 0, nil
	}
	diff, err := backup.Diff(want, freshDec)
	if err != nil {
		return "", false, 0, err
	}
	diff = backup.FilterVolatile(diff)
	if len(diff) == 0 {
		return "restored and verified: no differences except counters and checksums", true, 0, nil
	}
	paths := make([]string, 0, 5)
	for i, c := range diff {
		if i == 5 {
			paths = append(paths, "...")
			break
		}
		paths = append(paths, c.Path)
	}
	return fmt.Sprintf("restored, but %d setting(s) differ from the backup: %s", len(diff), strings.Join(paths, ", ")), false, len(diff), nil
}

// noDecodedCopyText says which of the two backups it compared lacks the decoded copy the comparison needs, so the person
// looks at the right one: the backup that was restored (an old one, taken without the decoder) or the check backup taken
// after the restore (the decoder did not run now), or both (#855).
func noDecodedCopyText(restoredLacks, checkLacks bool) string {
	const lead = "restored; the dumps differ in bytes and cannot be compared: "
	switch {
	case restoredLacks && checkLacks:
		return lead + "neither the backup being restored nor the check backup taken after the restore has a decoded copy"
	case restoredLacks:
		return lead + "the backup being restored has no decoded copy"
	}
	return lead + "the check backup taken after the restore has no decoded copy"
}

// decodedOf returns the decoded twin (same device and run) of a dump backup.
func (m *Manager) decodedOf(ctx context.Context, dump model.BackupID) ([]byte, error) {
	_, b, err := m.o.Backups.Open(ctx, dump)
	if err != nil {
		return nil, err
	}
	all, err := m.o.Backups.List(ctx, b.DeviceID)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.Kind == model.BackupDecoded && c.TakenAt.Equal(b.TakenAt) {
			data, _, err := m.o.Backups.Open(ctx, c.ID)
			return data, err
		}
	}
	return nil, nil
}

// colonMAC writes a normalised MAC (12 hex digits) the way the inventory stores
// it, lower case with colons; anything else is returned as it is.
func colonMAC(n string) string {
	if len(n) != 12 {
		return n
	}
	return n[0:2] + ":" + n[2:4] + ":" + n[4:6] + ":" + n[6:8] + ":" + n[8:10] + ":" + n[10:12]
}

// liveMAC reads the MAC the device reports (transport.AskMAC: over MQTT by its topic; over HTTP without the stored
// credentials first, so a host that is not the device never sees the stored password unless it asks for one).
func (m *Manager) liveMAC(ctx context.Context, dev model.Device) (string, error) {
	return transport.AskMAC(ctx, m.o.Client, dev)
}

// checkIdentity refuses unless the host at the address reports the MAC the
// inventory holds. A device without a stored MAC cannot be checked and is
// refused (probe it first).
func (m *Manager) checkIdentity(ctx context.Context, dev model.Device) error {
	want := transport.NormMAC(dev.MAC)
	if want == "" {
		return fmt.Errorf("%w: %s", ErrIdentity, backup.NoMACWhy)
	}
	got, err := m.liveMAC(ctx, dev)
	if err != nil {
		return fmt.Errorf("device not reachable: %w", err)
	}
	if got != want && !slices.ContainsFunc(dev.MACs, func(m string) bool { return transport.NormMAC(m) == got }) {
		return ErrIdentity // not the MAC of any interface of the stored device
	}
	return nil
}

// liveVersion reads the firmware version from Status 2.
func (m *Manager) liveVersion(ctx context.Context, dev model.Device) (string, error) {
	raw, err := m.o.Client.Command(ctx, tasmota.TargetFor(dev), "Status 2")
	if err != nil {
		return "", err
	}
	fw, err := tasmota.ParseStatus2(raw)
	if err != nil {
		return "", err
	}
	return fw.Version, nil
}

var majorRe = regexp.MustCompile(`^\s*v?(\d+)\.`)

func major(v string) (int, bool) {
	m := majorRe.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// preflight checks, against the live device, everything that decides whether
// this dump may be applied. It runs before any state is changed. With adopt,
// a device without a stored MAC gets the live one stored (the start of a
// restore); without it (the read-only assessment) that step is skipped.
//
// connRisk, when set, receives the restore's MQTT connection risk (mqttLockout's sentence, "" when none or over
// HTTP), which the run needs whether or not it was asked about.
func (m *Manager) preflight(ctx context.Context, actor string, dev *model.Device, b model.Backup, opt StartOptions, adopt bool, connRisk *string) error {
	if transport.NormMAC(dev.MAC) == "" {
		if err := m.adoptMAC(ctx, actor, dev, adopt, opt.ExpectMAC); err != nil {
			return err
		}
	} else if err := m.checkIdentity(ctx, *dev); err != nil {
		return err
	}
	live, err := m.liveVersion(ctx, *dev)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVersionUnknown, err)
	}
	mj, okD := major(live)
	bm, okB := major(b.Version)
	if !okD || !okB {
		return ErrVersionUnknown
	}
	if backup.CompareVersions(b.Version, live) > 0 {
		return fmt.Errorf("%w (backup %s, device %s)", ErrNewerBackup, b.Version, live)
	}
	fam, err := m.backupFamily(ctx, b)
	if err != nil {
		return err
	}
	var need OverrideError
	var why []string
	if mj != bm && !opt.AllowMajorChange {
		need.Major = true
		why = append(why, fmt.Sprintf("the backup is from firmware %s, the device runs %s", b.Version, live))
	}
	switch {
	case fam == familyUnknown || dev.Identity.Family == model.ChipUnknown:
		if !opt.AllowUnknownFamily {
			need.Family = true
			why = append(why, "the chip family of the backup could not be checked (no decoded backup)")
		}
	case (fam == familyESP8266) != (dev.Identity.Family == model.ChipESP8266):
		return ErrFamilyMismatch
	}
	if pin, ok := transport.PathFrom(ctx); ok && pin == transport.MQTT {
		// Over MQTT a backup that changes the MQTT connection can cut TasmoFleet off, unless HTTP answers right now
		// (#607): then HTTP stays the way back, and nothing asks.
		risk := m.mqttLockout(ctx, *dev, b)
		if connRisk != nil {
			*connRisk = risk
		}
		if risk != "" && !opt.AllowMQTTLockout && !m.httpReaches(ctx, *dev) {
			need.Lockout = true
			why = append(why, risk+lockoutTail)
		}
	}
	if need.Major || need.Family || need.Lockout {
		need.Detail = strings.Join(why, "; ")
		return &need
	}
	return nil
}

// adoptMAC reads the live MAC of a device that has none and, with record,
// stores it. A MAC that belongs to another entry means the host at this
// address is not this device.
func (m *Manager) adoptMAC(ctx context.Context, actor string, dev *model.Device, record bool, expect string) error {
	if m.o.Hardware == nil {
		return fmt.Errorf("%w: %s", ErrIdentity, backup.NoMACWhy)
	}
	mac, err := m.liveMAC(ctx, *dev)
	if err != nil {
		return fmt.Errorf("device not reachable: %w", err)
	}
	mac = colonMAC(mac)
	dev.MAC = mac // the assessment reports it; a start stores it below
	if !record {
		return nil
	}
	// Trust on first use: the identity that gets recorded is exactly the one
	// the admin was shown and confirmed, not whatever answers now.
	if transport.NormMAC(expect) == "" {
		return &OverrideError{MAC: mac, Detail: fmt.Sprintf("the device at %s reports MAC %s; confirm it as the identity of %s", dev.Address, mac, dev.Name)}
	}
	if transport.NormMAC(expect) != transport.NormMAC(mac) {
		return fmt.Errorf("%w: the device at the address no longer reports the MAC you confirmed", ErrIdentity)
	}
	if err := m.o.Hardware.AdoptMAC(ctx, dev.ID, mac); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: the MAC of the device at this address belongs to another inventory entry", ErrIdentity)
		}
		return err
	}
	if m.o.Audit != nil {
		m.o.Audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "device.mac_adopted", Target: fmt.Sprintf("device:%d", dev.ID),
			Result: "ok", Detail: fmt.Sprintf(`{"mac":%q,"via":"restore","info":"identity recorded on first restore"}`, mac)})
	}
	return nil
}

// Assess implements Service.
func (m *Manager) Assess(ctx context.Context, bid model.BackupID) (Assessment, error) {
	_, b, err := m.o.Backups.Open(ctx, bid)
	if err != nil {
		return Assessment{}, err
	}
	if b.Kind != model.BackupConfigDump {
		return Assessment{}, ErrNotDump
	}
	dev, err := m.o.Devices.GetDevice(ctx, b.DeviceID)
	if err != nil {
		return Assessment{}, err
	}
	path, note, err := m.route(ctx, dev)
	if err != nil {
		return Assessment{}, err
	}
	ctx = transport.WithPath(ctx, path)
	if path != transport.MQTT {
		ips, err := m.o.Resolve(ctx, dev.Address)
		if err != nil || len(distinct(ips)) != 1 {
			return Assessment{}, ErrNoAddress
		}
		dev.Address = distinct(ips)[0].String()
	} else {
		dev.Address = m.httpAddress(ctx, dev.Address)
	}
	a := Assessment{MACUnknown: transport.NormMAC(dev.MAC) == "", ViaMQTT: path == transport.MQTT, PathNote: note}
	err = m.preflight(ctx, "", &dev, b, StartOptions{}, false, nil)
	if err == nil || errors.As(err, new(*OverrideError)) {
		via := filesVia{}
		if path == transport.MQTT {
			via = m.filesRoute(Job{Transport: model.TransportMQTT}, dev)
		}
		a.Files = m.assessFiles(ctx, b, dev, via)
	}
	if a.MACUnknown {
		a.LiveMAC, a.Address, a.Chip = dev.MAC, dev.Address, string(dev.Identity.Family)
	}
	var need *OverrideError
	switch {
	case errors.As(err, &need):
		a.Major, a.Family, a.Lockout, a.Detail = need.Major, need.Family, need.Lockout, need.Detail
		return a, nil
	case err != nil:
		return a, err
	}
	return a, nil
}

type family int

const (
	familyUnknown family = iota
	familyESP8266
	familyESP32
)

// backupFamily reads the chip family decode-config recorded in the header of
// the decoded twin of a dump ("ESP82xx" or "ESP32..."). No twin, or no header:
// unknown.
func (m *Manager) backupFamily(ctx context.Context, b model.Backup) (family, error) {
	dec, err := m.decodedOf(ctx, b.ID)
	if err != nil || dec == nil {
		return familyUnknown, nil //nolint:nilerr // a missing twin means "unknown", which needs the admin's confirmation
	}
	var d struct {
		Header struct {
			Data struct {
				Hardware string `json:"hardware"`
			} `json:"data"`
		} `json:"header"`
	}
	if uerr := json.Unmarshal(dec, &d); uerr != nil {
		return familyUnknown, nil //nolint:nilerr // unreadable header means "unknown", which needs the admin's confirmation
	}
	switch hw := strings.ToUpper(d.Header.Data.Hardware); {
	case strings.HasPrefix(hw, "ESP82"):
		return familyESP8266, nil
	case strings.HasPrefix(hw, "ESP32"):
		return familyESP32, nil
	}
	return familyUnknown, nil
}

// assessFiles is the files part of the assessment. A device that cannot be asked now is not a
// refusal: the page says the files could not be checked, the run asks again and fails loudly.
func (m *Manager) assessFiles(ctx context.Context, b model.Backup, dev model.Device, via filesVia) FilesPlan {
	plan, err := m.planFiles(ctx, b.ID, tasmota.TargetFor(dev), func(c context.Context) (string, error) { return m.liveVersion(c, dev) }, via)
	if err != nil {
		m.o.Log.Warn("restore: assess files", "component", "restore", "err", err)
		return FilesPlan{Files: plan.Files, Why: "the device's file system could not be read right now, so the files could not be checked"}
	}
	return plan
}
