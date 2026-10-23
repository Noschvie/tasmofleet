package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
)

var ctx = context.Background()

// Compile-time proof that *SQLite satisfies every repository interface.
var _ Store = (*SQLite)(nil)

func newStore(t *testing.T) (*SQLite, string, secretbox.Box) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tasmofleet.db")
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // cleanup of a temp DB, nothing to report to
	return s, path, box
}

func mustDevice(t *testing.T, s *SQLite, name, addr string) model.DeviceID {
	t.Helper()
	id, err := s.CreateDevice(ctx, model.Device{Name: name, Address: addr})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func job(dev model.DeviceID) model.Job {
	return model.Job{DeviceID: dev, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota"}
}

func TestPragmas(t *testing.T) {
	s, _, _ := newStore(t)
	var mode string
	var fk, busy int
	for q, dst := range map[string]any{"PRAGMA journal_mode": &mode, "PRAGMA foreign_keys": &fk, "PRAGMA busy_timeout": &busy} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	// WAL + FK are what the spec promises; FK also makes job rows follow device deletion.
	if mode != "wal" || fk != 1 || busy != 5000 {
		t.Fatalf("journal_mode=%s foreign_keys=%d busy_timeout=%d", mode, fk, busy)
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max open connections %d, want 1 (single writer removes SQLITE_BUSY handling)", got)
	}
}

func TestOpenAppliesMigrationsAndReopenIsNoop(t *testing.T) {
	s, path, box := newStore(t)
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("user_version %d after open", v)
	}
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close() //nolint:errcheck // temp DB
	if _, err := s2.GetDevice(ctx, id); err != nil {
		t.Fatalf("data lost across reopen: %v", err)
	}
	// Nothing was pending, so no pre-migration copy may be written on reopen.
	if m, _ := filepath.Glob(path + ".pre-*"); len(m) != 0 {
		t.Fatalf("unexpected pre-migration copies %v", m)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	s, path, box := newStore(t)
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	s.Close() //nolint:errcheck // temp DB
	// Downgrades are unsupported: running old code on a newer schema would corrupt data.
	_, err := Open(path, box)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("error %v does not wrap ErrSchemaTooNew", err)
	}
}

func TestDevicePasswordSealedAtRest(t *testing.T) {
	s, _, box := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1", HTTPUser: "admin", HTTPPassword: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT http_password FROM devices WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	// A leaked database file alone must not leak device passwords.
	if raw == "" || raw == "hunter2" {
		t.Fatalf("raw column %q is not sealed", raw)
	}
	if pt, err := box.Open(raw); err != nil || pt != "hunter2" {
		t.Fatalf("raw column does not open with the box: %q %v", pt, err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil || d.HTTPPassword != "hunter2" {
		t.Fatalf("read back %q %v", d.HTTPPassword, err)
	}
	// Update re-seals; clearing stores the empty string, not a sealed empty value.
	d.HTTPPassword = "new"
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRowContext(ctx, "SELECT http_password FROM devices WHERE id=?", id).Scan(&raw) //nolint:errcheck // checked via the value below
	if pt, _ := box.Open(raw); pt != "new" {
		t.Fatalf("update did not re-seal: %q", pt)
	}
	d.HTTPPassword = ""
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRowContext(ctx, "SELECT http_password FROM devices WHERE id=?", id).Scan(&raw) //nolint:errcheck // checked via the value below
	if raw != "" {
		t.Fatalf("empty password stored as %q", raw)
	}
}

// A wrong key must be an error, never an empty password that then makes every
// request fail as "auth failed" on the device.
func TestDevicePasswordUnreadableWithOtherKeyFails(t *testing.T) {
	s, path, _ := newStore(t)
	id, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1", HTTPPassword: "pw"})
	s.Close() //nolint:errcheck // temp DB
	other, _ := secretbox.LoadOrCreate(t.TempDir())
	s2, err := Open(path, other)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close() //nolint:errcheck // temp DB
	if _, err := s2.GetDevice(ctx, id); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeviceRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	in := model.Device{
		Name: "kitchen", Address: "10.0.0.5", Port: 8080, MAC: "AA:BB:CC:00:11:22", Topic: "kitchen", FullTopic: "%prefix%/%topic%/",
		Tags: []string{"kitchen", "plug"}, Notes: "n", HTTPUser: "admin", HTTPPassword: "pw", OTAExcluded: true, MQTT: model.MQTTOurs,
		Identity: model.Identity{Family: model.ChipESP32C3, Hardware: "ESP32-C3", SingleCore: true, Version: "16.0.0", Image: "tasmota32c3", Variant: "tasmota", Language: "DE", FlashSizeKB: 4096, ProgramFlashSizeKB: 1856, FreeKB: 900, DetectedAt: at},
	}
	id, err := s.CreateDevice(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := in
	want.ID = id
	want.MAC = "aa:bb:cc:00:11:22"            // model contract: lower case
	want.MACs = []string{"aa:bb:cc:00:11:22"} // the set always contains the primary MAC
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set")
	}
	want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\n got %+v\nwant %+v", got, want)
	}
	// Lookups by the stable identities.
	if d, err := s.GetDeviceByMAC(ctx, "AA:BB:CC:00:11:22"); err != nil || d.ID != id {
		t.Fatalf("by mac: %v", err)
	}
	if d, err := s.GetDeviceByTopic(ctx, "kitchen"); err != nil || d.ID != id {
		t.Fatalf("by topic: %v", err)
	}
	// Empty identity must never match "the first device without a MAC".
	other := mustDevice(t, s, "other", "10.0.0.6")
	_ = other
	if _, err := s.GetDeviceByMAC(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty mac lookup: %v", err)
	}
	if _, err := s.GetDeviceByTopic(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty topic lookup: %v", err)
	}
}

func TestDeviceEmptyTagsAreNil(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	d, _ := s.GetDevice(ctx, id)
	if d.Tags != nil {
		t.Fatalf("tags %#v, want nil", d.Tags)
	}
}

func TestDeviceUniqueness(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1", MAC: "aa:aa:aa:aa:aa:aa", Topic: "t"}); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]model.Device{
		"mac":     {Name: "b", Address: "10.0.0.2", MAC: "AA:AA:AA:AA:AA:AA"},
		"topic":   {Name: "b", Address: "10.0.0.2", Topic: "t"},
		"address": {Name: "b", Address: "10.0.0.1"},
	} {
		if _, err := s.CreateDevice(ctx, d); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: err %v, want ErrConflict", name, err)
		}
	}
	// Devices without MAC/topic must not collide with each other (partial indexes).
	if _, err := s.CreateDevice(ctx, model.Device{Name: "c", Address: "10.0.0.3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDevice(ctx, model.Device{Name: "d", Address: "10.0.0.4"}); err != nil {
		t.Fatal(err)
	}
	// Update into a duplicate is also a conflict.
	d, _ := s.GetDeviceByMAC(ctx, "aa:aa:aa:aa:aa:aa")
	d.Address = "10.0.0.3"
	if err := s.UpdateDevice(ctx, d); !errors.Is(err, ErrConflict) {
		t.Fatalf("update err %v", err)
	}
}

func TestDeviceNotFound(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.GetDevice(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Error("get")
	}
	if err := s.UpdateDevice(ctx, model.Device{ID: 99, Name: "x", Address: "x"}); !errors.Is(err, ErrNotFound) {
		t.Error("update")
	}
	if err := s.UpdateIdentity(ctx, 99, model.Identity{}); !errors.Is(err, ErrNotFound) {
		t.Error("identity")
	}
	if err := s.DeleteDevice(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Error("delete")
	}
	if err := s.SetLastSeen(ctx, 99, model.HealthOnline, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Error("lastseen")
	}
}

// UpdateDevice edits inventory fields only. The identity is written by
// UpdateIdentity after detection; an inventory edit with a stale copy of the
// device must not roll it back.
func TestUpdateDeviceKeepsIdentity(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	ident := model.Identity{Family: model.ChipESP8266, Version: "15.0.0", Variant: "tasmota", DetectedAt: ms(1_700_000_000_000)}
	if err := s.UpdateIdentity(ctx, id, ident); err != nil {
		t.Fatal(err)
	}
	stale := model.Device{ID: id, Name: "renamed", Address: "10.0.0.1"}
	if err := s.UpdateDevice(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDevice(ctx, id)
	if got.Name != "renamed" || got.Identity.Version != "15.0.0" || got.Identity.Family != model.ChipESP8266 {
		t.Fatalf("got %+v", got)
	}
	if !got.Identity.DetectedAt.Equal(ident.DetectedAt) {
		t.Fatalf("detected at %v", got.Identity.DetectedAt)
	}
}

func TestSetLastSeenPersists(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	at := ms(1_700_000_123_000)
	if err := s.SetLastSeen(ctx, id, model.HealthOffline, at); err != nil {
		t.Fatal(err)
	}
	var st string
	var ms int64
	if err := s.db.QueryRowContext(ctx, "SELECT health_state,last_seen_at FROM devices WHERE id=?", id).Scan(&st, &ms); err != nil {
		t.Fatal(err)
	}
	if st != "offline" || ms != at.UnixMilli() {
		t.Fatalf("%s %d", st, ms)
	}
}

func TestListDevicesOrderedByName(t *testing.T) {
	s, _, _ := newStore(t)
	mustDevice(t, s, "b", "10.0.0.2")
	mustDevice(t, s, "a", "10.0.0.1")
	l, err := s.ListDevices(ctx)
	if err != nil || len(l) != 2 || l[0].Name != "a" {
		t.Fatalf("%v %+v", err, l)
	}
}

func TestDeleteDeviceCascades(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{job(id)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBackup(ctx, model.Backup{DeviceID: id, Kind: model.BackupSnapshot, Path: "p", SHA256: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendHealthEvent(ctx, HealthEvent{DeviceID: id, At: time.Now(), From: model.HealthUnknown, To: model.HealthOnline, Source: model.SourceHTTP}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Orphaned jobs would show up as active forever and be resumed against a vanished device.
	if _, err := s.GetJob(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job survived: %v", err)
	}
	// Backups do not cascade (N2): whether they go is the user's choice, made
	// by the caller; the store keeps them as orphans (TestDeleteDeviceKeepsBackupsAsOrphans).
	if l, _ := s.ListBackups(ctx, id); len(l) != 0 {
		t.Fatal("backup still listed under the deleted device")
	}
	if l, _ := s.ListHealthEvents(ctx, id, 0); len(l) != 0 {
		t.Fatal("health event survived")
	}
}

func TestOneActiveJobPerDevice(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{job(dev)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateBatch(ctx, "u", []model.Job{job(dev)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active job: %v", err)
	}
	// Terminal job does not block a new one.
	j, _ := s.GetJob(ctx, ids[0])
	prev := j.State
	j.State = model.JobFailed
	if err := s.SaveTransition(ctx, j, model.JobEvent{JobID: j.ID, From: prev, To: model.JobFailed, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateBatch(ctx, "u", []model.Job{job(dev)}); err != nil {
		t.Fatalf("after terminal: %v", err)
	}
}

// A batch is one user action. If one device is busy, none of the batch may be
// created, otherwise the user sees a half-started update.
func TestCreateBatchIsAtomic(t *testing.T) {
	s, _, _ := newStore(t)
	a := mustDevice(t, s, "a", "10.0.0.1")
	b := mustDevice(t, s, "b", "10.0.0.2")
	if _, _, err := s.CreateBatch(ctx, "u", []model.Job{job(b)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateBatch(ctx, "u", []model.Job{job(a), job(b)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v", err)
	}
	l, _ := s.ListJobs(ctx, JobFilter{DeviceID: a})
	if len(l) != 0 {
		t.Fatalf("job for a created despite conflict: %+v", l)
	}
	var n int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ota_batches").Scan(&n) //nolint:errcheck // asserted below
	if n != 1 {
		t.Fatalf("batches %d, want 1", n)
	}
}

func TestCreateBatchUnknownDevice(t *testing.T) {
	s, _, _ := newStore(t)
	if _, _, err := s.CreateBatch(ctx, "u", []model.Job{job(77)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if _, _, err := s.CreateBatch(ctx, "u", nil); err == nil {
		t.Fatal("empty batch accepted")
	}
}

func TestJobRoundTripAndSaveTransition(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	in := job(dev)
	in.IdentityBefore = model.Identity{Family: model.ChipESP8266, Version: "15.0.0", Variant: "tasmota", Hardware: "ESP8285N08", FlashSizeKB: 1024}
	bid, ids, err := s.CreateBatch(ctx, "alice", []model.Job{in})
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.GetJob(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if j.BatchID != bid || j.CreatedBy != "alice" || j.State != model.JobQueued || j.IdentityBefore.Hardware != "ESP8285N08" || j.CreatedAt.IsZero() {
		t.Fatalf("job %+v", j)
	}

	j.State = model.JobPreflight
	j.Plan = model.PlanStaged
	j.Stage = model.StageMinimal
	j.Attempt = 1
	j.TargetArtifact, j.MinimalArtifact = 5, 6
	j.OriginalOtaURL = "http://ota.example/x.bin"
	j.UptimeBefore = 1234
	j.SeenOffline, j.CancelRequested, j.SkipBackup = true, true, true
	// Both must survive a restart: a forgotten auto-continue sighting or a
	// forgotten relay state would re-wait or leave the device switched off.
	j.AutoContinueSeen, j.SkipPowerRestore = true, true
	j.PowerOn = model.RelaySet(0).Add(1).Add(3)
	j.PowerReadAt, j.PowerNote = ms(1_700_000_123_000), "relay 2: PulseTime"
	j.RestoreRelays, j.PowerCutoff = true, ms(1_700_000_456_000)
	j.DeadlineAt = ms(1_700_000_999_000)
	j.LastError = "boom"
	ev := model.JobEvent{JobID: j.ID, At: ms(1_700_000_000_000), From: model.JobQueued, To: model.JobPreflight, Event: "start", Detail: "d"}
	if err := s.SaveTransition(ctx, j, ev); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	j.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, j) {
		t.Fatalf("persisted job differs\n got %+v\nwant %+v", got, j)
	}
	evs, err := s.JobEvents(ctx, j.ID)
	if err != nil || len(evs) != 1 || !reflect.DeepEqual(evs[0], ev) {
		t.Fatalf("events %v %+v", err, evs)
	}
}

// Why (design v2, N15): "One device at a time" and "Wait up to 5 s" are part
// of the transaction. A restart resumes queued jobs from the store, so the
// options must come back from it, also after a transition; a job without them
// reads back as 0 (fleet limit, default timeout), never as "no limit".
func TestJobBulkOptionsSurviveCreateAndTransition(t *testing.T) {
	s, _, _ := newStore(t)
	in := job(mustDevice(t, s, "a", "10.0.0.1"))
	in.BatchParallel, in.ReplyTimeout = 1, 5*time.Second
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{in, job(mustDevice(t, s, "b", "10.0.0.2"))})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := s.GetJob(ctx, ids[0])
	if j.BatchParallel != 1 || j.ReplyTimeout != 5*time.Second {
		t.Fatalf("after create: parallel %d, timeout %v", j.BatchParallel, j.ReplyTimeout)
	}
	j.State = model.JobPreflight
	if err := s.SaveTransition(ctx, j, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobPreflight, Event: "start"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetJob(ctx, j.ID); got.BatchParallel != 1 || got.ReplyTimeout != 5*time.Second {
		t.Fatalf("after a transition: parallel %d, timeout %v", got.BatchParallel, got.ReplyTimeout)
	}
	if got, _ := s.GetJob(ctx, ids[1]); got.BatchParallel != 0 || got.ReplyTimeout != 0 {
		t.Fatalf("defaults: parallel %d, timeout %v", got.BatchParallel, got.ReplyTimeout)
	}
}

// Why: a push job must still be a push job after a restart; losing the delivery
// would silently turn an upload into an OtaUrl change on resume.
func TestJobDeliverySurvivesCreateAndTransition(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	in := job(dev)
	in.Delivery = model.DeliveryPush
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{in})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := s.GetJob(ctx, ids[0])
	if j.Delivery != model.DeliveryPush {
		t.Fatalf("delivery %q after create", j.Delivery)
	}
	j.State = model.JobPreflight
	if err := s.SaveTransition(ctx, j, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobPreflight, Event: "start"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetJob(ctx, j.ID); got.Delivery != model.DeliveryPush {
		t.Fatalf("delivery %q after a transition", got.Delivery)
	}
	_, ids, _ = s.CreateBatch(ctx, "u", []model.Job{job(mustDevice(t, s, "b", "10.0.0.2"))})
	if got, _ := s.GetJob(ctx, ids[0]); got.Delivery != model.DeliveryPull {
		t.Fatalf("default delivery %q, want pull", got.Delivery)
	}
}

// Optimistic concurrency: two writers that both read state X must not both win,
// and the loser must leave neither a job change nor an event behind.
func TestSaveTransitionStaleFromConflicts(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, _ := s.CreateBatch(ctx, "u", []model.Job{job(dev)})
	j, _ := s.GetJob(ctx, ids[0])

	j.State = model.JobPreflight
	if err := s.SaveTransition(ctx, j, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobPreflight, Event: "a"}); err != nil {
		t.Fatal(err)
	}
	stale := j
	stale.State = model.JobCancelled
	stale.LastError = "should not be stored"
	err := s.SaveTransition(ctx, stale, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobCancelled, Event: "b"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v", err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.State != model.JobPreflight || got.LastError != "" {
		t.Fatalf("stale write changed the job: %+v", got)
	}
	evs, _ := s.JobEvents(ctx, j.ID)
	if len(evs) != 1 || evs[0].Event != "a" {
		t.Fatalf("events after conflict: %+v", evs)
	}
}

func TestSaveTransitionUnknownJob(t *testing.T) {
	s, _, _ := newStore(t)
	err := s.SaveTransition(ctx, model.Job{ID: 5, State: model.JobFailed}, model.JobEvent{JobID: 5, From: model.JobQueued, To: model.JobFailed})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestSaveTransitionRejectsForeignEvent(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, _ := s.CreateBatch(ctx, "u", []model.Job{job(dev)})
	j, _ := s.GetJob(ctx, ids[0])
	if err := s.SaveTransition(ctx, j, model.JobEvent{JobID: j.ID + 1, From: model.JobQueued, To: model.JobPreflight}); err == nil {
		t.Fatal("event for another job accepted")
	}
}

func TestSaveTransitionRaceHasOneWinner(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, _ := s.CreateBatch(ctx, "u", []model.Job{job(dev)})
	j, _ := s.GetJob(ctx, ids[0])
	const n = 8
	var wg sync.WaitGroup
	res := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := j
			c.State = model.JobPreflight
			res[i] = s.SaveTransition(ctx, c, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobPreflight, Event: "race"})
		}()
	}
	wg.Wait()
	wins := 0
	for _, e := range res {
		switch {
		case e == nil:
			wins++
		case !errors.Is(e, ErrConflict):
			t.Fatalf("unexpected error %v", e)
		}
	}
	if evs, _ := s.JobEvents(ctx, j.ID); wins != 1 || len(evs) != 1 {
		t.Fatalf("wins=%d events=%d", wins, len(evs))
	}
}

func TestActiveJobsAndFilters(t *testing.T) {
	s, path, box := newStore(t)
	var devs []model.DeviceID
	for _, a := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		devs = append(devs, mustDevice(t, s, a, a))
	}
	b1, ids1, _ := s.CreateBatch(ctx, "u", []model.Job{job(devs[0]), job(devs[1])})
	_, ids2, _ := s.CreateBatch(ctx, "u", []model.Job{job(devs[2])})
	j, _ := s.GetJob(ctx, ids1[1])
	j.State = model.JobSucceeded
	if err := s.SaveTransition(ctx, j, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobSucceeded}); err != nil {
		t.Fatal(err)
	}

	act, err := s.ActiveJobs(ctx)
	if err != nil || len(act) != 2 || act[0].ID != ids1[0] || act[1].ID != ids2[0] {
		t.Fatalf("active %v %+v", err, act)
	}
	if l, _ := s.ListJobs(ctx, JobFilter{BatchID: b1}); len(l) != 2 {
		t.Errorf("batch filter %d", len(l))
	}
	if l, _ := s.ListJobs(ctx, JobFilter{DeviceID: devs[2]}); len(l) != 1 {
		t.Errorf("device filter %d", len(l))
	}
	if l, _ := s.ListJobs(ctx, JobFilter{States: []model.JobState{model.JobSucceeded, model.JobFailed}}); len(l) != 1 || l[0].ID != ids1[1] {
		t.Errorf("state filter %+v", l)
	}
	if l, _ := s.ListJobs(ctx, JobFilter{Limit: 2}); len(l) != 2 {
		t.Errorf("limit %d", len(l))
	}
	if l, _ := s.ListJobs(ctx, JobFilter{}); len(l) != 3 {
		t.Errorf("no filter %d", len(l))
	}

	// Write-ahead state must survive a restart: that is how the engine resumes.
	s.Close() //nolint:errcheck // temp DB
	s2, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close() //nolint:errcheck // temp DB
	if act, _ := s2.ActiveJobs(ctx); len(act) != 2 {
		t.Fatalf("active after reopen %d", len(act))
	}
}

func TestGetJobNotFound(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.GetJob(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestHealthEvents(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		e := HealthEvent{DeviceID: id, At: base.Add(time.Duration(i) * time.Hour), From: model.HealthUnknown, To: model.HealthOnline, Source: model.SourceMQTT, Detail: "d"}
		if err := s.AppendHealthEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.ListHealthEvents(ctx, id, 2)
	if err != nil || len(l) != 2 || !l[0].At.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("newest first expected: %v %+v", err, l)
	}
	if l[0].Source != model.SourceMQTT || l[0].To != model.HealthOnline || l[0].Detail != "d" {
		t.Fatalf("%+v", l[0])
	}
	n, err := s.PruneHealthEvents(ctx, base.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if err := s.AppendHealthEvent(ctx, HealthEvent{DeviceID: 99, At: base}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
}

func TestArtifacts(t *testing.T) {
	s, _, _ := newStore(t)
	a := model.Artifact{Release: "16.0.0", Filename: "tasmota.bin.gz", Family: model.ChipESP8266, Variant: "tasmota", Safeboot: true, Size: 10, SHA256: "ab", Source: "github", SourceURL: "https://x/y", FetchedAt: ms(1_700_000_000_000)}
	id, err := s.PutArtifact(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	a.ID = id
	for name, get := range map[string]func() (model.Artifact, error){
		"id":   func() (model.Artifact, error) { return s.GetArtifact(ctx, id) },
		"find": func() (model.Artifact, error) { return s.FindArtifact(ctx, "16.0.0", "tasmota.bin.gz") },
	} {
		got, err := get()
		if err != nil || !reflect.DeepEqual(got, a) {
			t.Errorf("%s: %v\n got %+v\nwant %+v", name, err, got, a)
		}
	}
	// Artifacts are immutable: a re-fetch must not silently replace a file devices may be flashing.
	if _, err := s.PutArtifact(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := s.FindArtifact(ctx, "16.0.0", "nope"); !errors.Is(err, ErrNotFound) {
		t.Error("find missing")
	}
	if l, _ := s.ListArtifacts(ctx); len(l) != 1 {
		t.Error("list")
	}
	if err := s.DeleteArtifact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteArtifact(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Error("delete twice")
	}
}

func TestBackups(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	base := ms(1_700_000_000_000)
	old := model.Backup{DeviceID: dev, Kind: model.BackupConfigDump, TakenAt: base, Version: "15.0.0", Path: "backups/1/a.dmp", Size: 4096, SHA256: "aa"}
	newer := model.Backup{DeviceID: dev, Kind: model.BackupDecoded, TakenAt: base.Add(time.Hour), Path: "backups/1/b.json", Size: 10, SHA256: "bb", DecoderVersion: "15.1", Note: "decoder outdated"}
	oldID, err := s.PutBackup(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	newID, _ := s.PutBackup(ctx, newer)
	newer.ID, old.ID = newID, oldID
	l, err := s.ListBackups(ctx, dev)
	if err != nil || len(l) != 2 || !reflect.DeepEqual(l[0], newer) || !reflect.DeepEqual(l[1], old) {
		t.Fatalf("list newest first: %v %+v", err, l)
	}
	if got, err := s.GetBackup(ctx, oldID); err != nil || !reflect.DeepEqual(got, old) {
		t.Fatalf("get %v %+v", err, got)
	}
	if _, err := s.PutBackup(ctx, model.Backup{DeviceID: 99, Path: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown device: %v", err)
	}
	if err := s.DeleteBackup(ctx, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBackup(ctx, oldID); !errors.Is(err, ErrNotFound) {
		t.Error("get deleted")
	}
	if err := s.DeleteBackup(ctx, oldID); !errors.Is(err, ErrNotFound) {
		t.Error("delete twice")
	}
}

func TestAudit(t *testing.T) {
	s, _, _ := newStore(t)
	base := ms(1_700_000_000_000)
	evs := []model.AuditEvent{
		{At: base, Actor: "alice", Action: "device.create", Target: "device:1", Detail: `{"a":1}`, Result: "ok", RemoteAddr: "10.0.0.9"},
		{At: base.Add(time.Hour), Actor: "bob", Action: "ota.start", Target: "batch:1", Result: "ok"},
		{At: base.Add(2 * time.Hour), Actor: "alice", Action: "ota.start", Target: "batch:1", Result: "denied"},
	}
	for _, e := range evs {
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListAudit(ctx, AuditFilter{})
	if err != nil || len(all) != 3 || all[0].Result != "denied" {
		t.Fatalf("newest first expected: %v %+v", err, all)
	}
	if all[2].ID == 0 || all[2].Detail != `{"a":1}` || all[2].RemoteAddr != "10.0.0.9" || !all[2].At.Equal(base) {
		t.Fatalf("%+v", all[2])
	}
	if all[1].Detail != "{}" {
		t.Fatalf("empty detail must read back as an empty JSON object, got %q", all[1].Detail)
	}
	if l, _ := s.ListAudit(ctx, AuditFilter{Actor: "alice"}); len(l) != 2 {
		t.Error("actor")
	}
	if l, _ := s.ListAudit(ctx, AuditFilter{Target: "batch:1", Since: base.Add(90 * time.Minute)}); len(l) != 1 {
		t.Error("target+since")
	}
	// Rows are never rewritten after the batch -> transaction rename, so a filter
	// on either spelling has to find rows of both.
	if err := s.AppendAudit(ctx, model.AuditEvent{At: base.Add(3 * time.Hour), Actor: "bob", Action: "transaction.retry", Target: "transaction:1", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	for _, tg := range []string{"batch:1", "transaction:1"} {
		if l, _ := s.ListAudit(ctx, AuditFilter{Target: tg}); len(l) != 3 {
			t.Errorf("filter %s must find old batch:1 and new transaction:1 rows, got %d", tg, len(l))
		}
	}
	if l, _ := s.ListAudit(ctx, AuditFilter{Target: "transaction:2"}); len(l) != 0 {
		t.Error("alias must not widen to other ids")
	}
	if l, _ := s.ListAudit(ctx, AuditFilter{Limit: 1}); len(l) != 1 {
		t.Error("limit")
	}
	if n, err := s.PruneAudit(ctx, base.Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("prune %d %v", n, err)
	}
}

func TestSessions(t *testing.T) {
	s, _, _ := newStore(t)
	base := ms(1_700_000_000_000)
	sess := Session{IDHash: "h1", Principal: model.Principal{Subject: "sub", Name: "N", Email: "e@x", Groups: []string{"a", "b"}, Role: model.RoleOperator, Method: "oidc"}, CSRFToken: "c", IDToken: "tok", CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, "h1")
	if err != nil || !reflect.DeepEqual(got, sess) {
		t.Fatalf("%v\n got %+v\nwant %+v", err, got, sess)
	}
	if err := s.CreateSession(ctx, sess); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate hash: %v", err)
	}
	// The raw cookie value must never be a valid lookup key by accident.
	if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Error("unknown")
	}
	if err := s.TouchSession(ctx, "h1", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, "h1"); !got.LastSeen.Equal(base.Add(time.Minute)) {
		t.Errorf("touch: %v", got.LastSeen)
	}
	if err := s.TouchSession(ctx, "nope", base); !errors.Is(err, ErrNotFound) {
		t.Error("touch unknown")
	}

	for i, exp := range []time.Duration{-time.Hour, 0, time.Hour} {
		x := sess
		x.IDHash = "e" + string(rune('0'+i))
		x.ExpiresAt = base.Add(exp)
		if err := s.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	// Expired means expires_at <= now: a session ending exactly now is dead.
	n, err := s.DeleteExpiredSessions(ctx, base)
	if err != nil || n != 2 {
		t.Fatalf("deleted %d %v", n, err)
	}
	if err := s.DeleteSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	// Logout is idempotent (double click, expiry race).
	if err := s.DeleteSession(ctx, "h1"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	var left int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&left) //nolint:errcheck // asserted below
	if left != 1 {
		t.Fatalf("left %d", left)
	}
}

func TestLocalUsers(t *testing.T) {
	s, _, _ := newStore(t)
	u := LocalUser{Username: "admin", PasswordHash: "pbkdf2$x", Role: model.RoleOperator, UpdatedAt: ms(1_700_000_000_000)}
	// /login/local is disabled while no break-glass account exists.
	if has, err := s.HasLocalUsers(ctx); err != nil || has {
		t.Fatalf("empty store: HasLocalUsers = %v, %v", has, err)
	}
	if err := s.SetLocalUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.PasswordHash = "pbkdf2$y"
	if err := s.SetLocalUser(ctx, u); err != nil { // upsert: set-password on an existing user
		t.Fatal(err)
	}
	if has, err := s.HasLocalUsers(ctx); err != nil || !has {
		t.Fatalf("after SetLocalUser: HasLocalUsers = %v, %v", has, err)
	}
	got, err := s.GetLocalUser(ctx, "admin")
	u.PasswordGen = 2 // the second SetLocalUser replaced the password
	if err != nil || !reflect.DeepEqual(got, u) {
		t.Fatalf("%v %+v", err, got)
	}
	if err := s.DeleteLocalUser(ctx, "admin", Actor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLocalUser(ctx, "admin"); !errors.Is(err, ErrNotFound) {
		t.Error("get deleted")
	}
	if err := s.DeleteLocalUser(ctx, "admin", Actor{}); !errors.Is(err, ErrNotFound) {
		t.Error("delete twice")
	}
}

func TestPingAndClosedStore(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close() //nolint:errcheck // temp DB
	if err := s.Ping(ctx); err == nil {
		t.Fatal("ping after close succeeded")
	}
}

func TestCanceledContext(t *testing.T) {
	s, _, _ := newStore(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListDevices(c); err == nil {
		t.Fatal("canceled context ignored")
	}
}

// ms builds a UTC time like the store returns it, so tests can compare with DeepEqual.
func ms(n int64) time.Time { return time.UnixMilli(n).UTC() }

func deviceFor(name, addr string) model.Device { return model.Device{Name: name, Address: addr} }

// Why: the broker a device is configured for must survive a restart, or the
// UI would fall back to guessing after every restart.
func TestDeviceMQTTBrokerRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateDevice(ctx, model.Device{Name: "a", Address: "172.31.13.9", MQTT: model.MQTTOtherBroker, MQTTHost: "mqtt.example.internal", MQTTPort: 1883})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil || d.MQTTHost != "mqtt.example.internal" || d.MQTTPort != 1883 {
		t.Fatalf("after create: %+v %v", d, err)
	}
	d.MQTTHost, d.MQTTPort = "10.0.0.5", 1884
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if d.MQTTHost != "10.0.0.5" || d.MQTTPort != 1884 {
		t.Fatalf("after update: %+v", d)
	}
}

// Why: a retry is a new transaction that points at the one it retries; one
// batch can be retried more than once, and the link must survive a reload.
func TestRetryBatchLinksToTheOriginal(t *testing.T) {
	s, _, _ := newStore(t)
	d1 := mustDevice(t, s, "a", "10.0.0.1")
	orig, _, err := s.CreateBatch(ctx, "alice", []model.Job{job(d1)})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.GetBatch(ctx, orig); err != nil || b.RetryOf != 0 || b.CreatedBy != "alice" || b.CreatedAt.IsZero() {
		t.Fatalf("original %+v %v", b, err)
	}
	j := job(d1)
	jobs, _ := s.ListJobs(ctx, JobFilter{BatchID: orig})
	jobs[0].State = model.JobFailed
	if err := s.SaveTransition(ctx, jobs[0], model.JobEvent{JobID: jobs[0].ID, From: model.JobQueued, To: model.JobFailed, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	r1, ids, err := s.CreateRetryBatch(ctx, "bob", orig, []model.Job{j})
	if err != nil || len(ids) != 1 {
		t.Fatalf("retry: %v %v", ids, err)
	}
	if b, _ := s.GetBatch(ctx, r1); b.RetryOf != orig || b.CreatedBy != "bob" {
		t.Fatalf("retry batch %+v", b)
	}
	got, err := s.BatchRetries(ctx, orig)
	if err != nil || len(got) != 1 || got[0].ID != r1 {
		t.Fatalf("retries %+v %v", got, err)
	}
	if _, _, err := s.CreateRetryBatch(ctx, "bob", 999, []model.Job{job(d1)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry of an unknown batch: %v", err)
	}
	if _, _, err := s.CreateRetryBatch(ctx, "bob", 0, []model.Job{job(d1)}); err == nil {
		t.Fatal("a retry without an original must be refused")
	}
	if _, err := s.GetBatch(ctx, 12345); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown batch: %v", err)
	}
}

// Why: a "Make compliant" job is replayed by Retry from what is stored, so the
// kind and the exact command lines must survive the database unchanged, and a
// plain update must still read back as an update (zero kind).
func TestPolicyJobKindAndCommandsRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	a := mustDevice(t, s, "a", "10.0.0.1")
	b := mustDevice(t, s, "b", "10.0.0.2")
	pj := job(a)
	pj.Kind, pj.Commands = model.KindPolicy, "PowerOnState 0\nSetOption0 0"
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{pj, job(b)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, ids[0])
	if err != nil || !got.IsPolicy() || got.Commands != pj.Commands {
		t.Fatalf("policy job %+v %v", got, err)
	}
	upd, err := s.GetJob(ctx, ids[1])
	if err != nil || upd.Kind != "" || upd.Commands != "" {
		t.Fatalf("update job %+v %v", upd, err)
	}
	// a transition must not drop the kind or the commands
	next := got
	next.State = model.JobApplying
	if err := s.SaveTransition(ctx, next, model.JobEvent{JobID: got.ID, From: got.State, To: next.State, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetJob(ctx, got.ID); !again.IsPolicy() || again.Commands != pj.Commands || again.State != model.JobApplying {
		t.Fatalf("after transition %+v", again)
	}
}

// Why: a command job is sent as its creator, and subjects are not unique across
// sign-in methods, so the method must survive the database and a transition;
// an update job keeps it empty.
func TestCommandJobKeepsTheCreatorsSignInMethod(t *testing.T) {
	s, _, _ := newStore(t)
	a := mustDevice(t, s, "a", "10.0.0.1")
	cj := job(a)
	cj.Kind, cj.Commands, cj.CreatedKind = model.KindCommand, "Power1 ON", "header"
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{cj})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, ids[0])
	if err != nil || !got.IsCommand() || got.CreatedKind != "header" {
		t.Fatalf("%+v %v", got, err)
	}
	next := got
	next.State = model.JobApplying
	if err := s.SaveTransition(ctx, next, model.JobEvent{JobID: got.ID, From: got.State, To: next.State, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetJob(ctx, got.ID); again.CreatedKind != "header" {
		t.Fatalf("after transition %+v", again)
	}
}

// Why: the CLI import runs in another process while the server is up; the
// health tracker only notices it through DataVersion, so it must change on a
// foreign commit and stay put when nothing was committed.
func TestDataVersionChangesOnlyOnForeignCommits(t *testing.T) {
	s, path, box := newStore(t)
	v1, err := s.DataVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v2, _ := s.DataVersion(ctx); v2 != v1 {
		t.Fatalf("version moved without a commit: %d -> %d", v1, v2)
	}
	other, err := Open(path, box) // a second process stands in as a second connection
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close() //nolint:errcheck // temp DB
	mustDevice(t, other, "imported", "10.0.0.9")
	if v3, _ := s.DataVersion(ctx); v3 == v1 {
		t.Fatal("version did not change after a commit from another connection")
	}
}

// Why: PRAGMA data_version is only comparable on one connection. The pool is
// pinned to a single connection, so this process's own writes (which reach the
// tracker via the hub) must not move the version, and exactly one external
// commit must move it, however many calls are made in between.
func TestDataVersionIgnoresOwnWritesAndSeesOneExternalCommit(t *testing.T) {
	s, path, box := newStore(t)
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("pool allows %d connections; data_version needs exactly one", got)
	}
	v0, _ := s.DataVersion(ctx)
	for i := 0; i < 5; i++ {
		mustDevice(t, s, fmt.Sprintf("own-%d", i), fmt.Sprintf("10.0.1.%d", 10+i))
		if v, _ := s.DataVersion(ctx); v != v0 {
			t.Fatalf("own write %d moved the version %d -> %d", i, v0, v)
		}
	}
	other, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close() //nolint:errcheck // temp DB
	mustDevice(t, other, "external", "10.0.1.2")
	v1, _ := s.DataVersion(ctx)
	if v1 == v0 {
		t.Fatal("external commit not seen")
	}
	if v2, _ := s.DataVersion(ctx); v2 != v1 {
		t.Fatalf("version unstable without a commit: %d -> %d", v1, v2)
	}
}

var _ AuditFacetStore = (*SQLite)(nil)

// Why (design v2, N14): the audit page filters by action family ("device.*"),
// a search box and a period in the store, so a filter never has to read the
// whole log into memory. The family must be a literal prefix ("device." must
// not match "devices.x" via a LIKE wildcard), the search must find a device by
// its name (resolved to targets by the caller) as well as by actor or action.
func TestAuditFilterFamilySearchAndFacets(t *testing.T) {
	s, _, _ := newStore(t)
	base := ms(1_700_000_000_000)
	evs := []model.AuditEvent{
		{At: base, Actor: "alice", Action: "device.create", Target: "device:1", Result: "ok"},
		{At: base.Add(time.Hour), Actor: "bob", Action: "device_x.odd", Target: "device:2", Result: "ok"},
		{At: base.Add(2 * time.Hour), Actor: "Carol", Action: "firmware.update", Target: "device:3", Result: "ok"},
		{At: base.Add(3 * time.Hour), Actor: "system", Action: "devicexcreate", Target: "transaction:9", Result: "ok"},
	}
	for _, e := range evs {
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	actions := func(f AuditFilter) []string {
		t.Helper()
		l, err := s.ListAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range l {
			out = append(out, e.Action)
		}
		return out
	}
	if got := actions(AuditFilter{ActionPrefix: "device."}); !slices.Equal(got, []string{"device.create"}) {
		t.Errorf("family device.* = %v (the dot is literal, _ is no wildcard)", got)
	}
	if got := actions(AuditFilter{ActionPrefix: "device_"}); !slices.Equal(got, []string{"device_x.odd"}) {
		t.Errorf("prefix with an underscore = %v", got)
	}
	if got := actions(AuditFilter{Search: "carol"}); !slices.Equal(got, []string{"firmware.update"}) {
		t.Errorf("search by actor, any case = %v", got)
	}
	if got := actions(AuditFilter{Search: "UPDATE"}); !slices.Equal(got, []string{"firmware.update"}) {
		t.Errorf("search by action = %v", got)
	}
	if got := actions(AuditFilter{Search: "%"}); len(got) != 0 {
		t.Errorf("a %% in the search is literal, got %v", got)
	}
	// "Kitchen" matched device 2 by name: the row is found although no column says "Kitchen".
	if got := actions(AuditFilter{Search: "Kitchen", SearchTargets: []string{"device:2"}}); !slices.Equal(got, []string{"device_x.odd"}) {
		t.Errorf("search by device name = %v", got)
	}
	// "dana" is the display name of the subject "sub-1": found by the resolved actor.
	if got := actions(AuditFilter{Search: "dana", SearchActors: []string{"bob"}}); !slices.Equal(got, []string{"device_x.odd"}) {
		t.Errorf("search by actor display name = %v", got)
	}
	if got := actions(AuditFilter{ActionPrefix: "device", Since: base.Add(30 * time.Minute)}); !slices.Equal(got, []string{"devicexcreate", "device_x.odd"}) {
		t.Errorf("prefix and period combine = %v", got)
	}
	actors, acts, err := s.AuditFacets(ctx, base.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actors, []string{"Carol", "system"}) || !slices.Equal(acts, []string{"devicexcreate", "firmware.update"}) {
		t.Errorf("facets since = %v %v", actors, acts)
	}
	if actors, _, _ := s.AuditFacets(ctx, time.Time{}); len(actors) != 4 {
		t.Errorf("facets of all rows = %v", actors)
	}
}
