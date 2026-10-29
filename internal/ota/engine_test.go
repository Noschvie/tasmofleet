package ota

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Engine tests run against fakes: an in-memory store that records every
// persisted transition, a fake device that "flashes" whatever OtaUrl points
// at, and a clock whose After returns at once while advancing virtual time.
// All of them write into one ordered log, so the tests can prove that each
// transition was persisted before the device command it allows.

// ---- shared log ----

type recorder struct {
	mu  sync.Mutex
	log []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	r.log = append(r.log, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) entries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

func (r *recorder) count(prefix string) int {
	n := 0
	for _, e := range r.entries() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// ---- clock ----

type instantClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *instantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	c.mu.Unlock()
	return ch
}

// ---- store ----

type fakeStore struct {
	rec     *recorder
	mu      sync.Mutex
	jobs    map[model.JobID]model.Job
	events  []model.JobEvent
	devices map[model.DeviceID]model.Device
	nextID  model.JobID
	batches int
	retryOf map[model.BatchID]model.BatchID
	onSave  func(model.Job)
	maxBusy int  // most jobs past queued at the same time
	reverse bool // ActiveJobs newest first, to prove the engine sorts
	started []model.JobID
}

func newFakeStore(rec *recorder) *fakeStore {
	return &fakeStore{rec: rec, jobs: map[model.JobID]model.Job{}, devices: map[model.DeviceID]model.Device{}, nextID: 1}
}

func (s *fakeStore) CreateBatch(_ context.Context, _ string, jobs []model.Job) (model.BatchID, []model.JobID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches++
	var ids []model.JobID
	for _, j := range jobs {
		j.ID = s.nextID
		j.BatchID = model.BatchID(s.batches)
		s.nextID++
		s.jobs[j.ID] = j
		ids = append(ids, j.ID)
	}
	return model.BatchID(s.batches), ids, nil
}

func (s *fakeStore) put(j model.Job) model.JobID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.ID == 0 {
		j.ID = s.nextID
	}
	if j.ID >= s.nextID {
		s.nextID = j.ID + 1
	}
	s.jobs[j.ID] = j
	return j.ID
}

func (s *fakeStore) GetJob(_ context.Context, id model.JobID) (model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return j, store.ErrNotFound
	}
	return j, nil
}

func (s *fakeStore) ListJobs(_ context.Context, f store.JobFilter) ([]model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Job
	for _, j := range s.jobs {
		if f.DeviceID != 0 && j.DeviceID != f.DeviceID {
			continue
		}
		if f.BatchID != 0 && j.BatchID != f.BatchID {
			continue
		}
		if len(f.States) > 0 && !containsState(f.States, j.State) {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func containsState(ss []model.JobState, s model.JobState) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (s *fakeStore) ActiveJobs(ctx context.Context) ([]model.Job, error) {
	out, err := s.ListJobs(ctx, store.JobFilter{States: activeStates})
	if s.reverse {
		sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	}
	return out, err
}

func (s *fakeStore) SaveTransition(_ context.Context, j model.Job, ev model.JobEvent) error {
	s.mu.Lock()
	cur, ok := s.jobs[j.ID]
	if !ok || cur.State != ev.From {
		s.mu.Unlock()
		return store.ErrConflict
	}
	s.jobs[j.ID] = j
	s.events = append(s.events, ev)
	if ev.From == model.JobQueued {
		s.started = append(s.started, j.ID)
	}
	busy := 0
	for _, x := range s.jobs {
		if x.State != model.JobQueued && !x.State.Terminal() {
			busy++
		}
	}
	if busy > s.maxBusy {
		s.maxBusy = busy
	}
	hook := s.onSave
	s.mu.Unlock()
	s.rec.add("save %d %s", j.ID, j.State)
	if hook != nil {
		hook(j)
	}
	return nil
}

func (s *fakeStore) JobEvents(_ context.Context, id model.JobID) ([]model.JobEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.JobEvent
	for _, e := range s.events {
		if e.JobID == id {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeStore) ListDevices(context.Context) ([]model.Device, error) {
	return nil, store.ErrNotImplemented
}
func (s *fakeStore) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return d, store.ErrNotFound
	}
	return d, nil
}
func (s *fakeStore) GetDeviceByMAC(context.Context, string) (model.Device, error) {
	return model.Device{}, store.ErrNotImplemented
}
func (s *fakeStore) GetDeviceByTopic(context.Context, string) (model.Device, error) {
	return model.Device{}, store.ErrNotImplemented
}
func (s *fakeStore) CreateDevice(context.Context, model.Device) (model.DeviceID, error) {
	return 0, store.ErrNotImplemented
}
func (s *fakeStore) UpdateDevice(context.Context, model.Device) error { return store.ErrNotImplemented }
func (s *fakeStore) UpdateIdentity(_ context.Context, id model.DeviceID, ident model.Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.devices[id]
	d.Identity = ident
	s.devices[id] = d
	return nil
}
func (s *fakeStore) DeleteDevice(context.Context, model.DeviceID) error {
	return store.ErrNotImplemented
}
func (s *fakeStore) SetLastSeen(context.Context, model.DeviceID, model.HealthState, time.Time) error {
	return store.ErrNotImplemented
}

// ---- device ----

// testMAC is the MAC every fake device has in the fake inventory.
const testMAC = "02:00:00:00:00:01"

type fakeDevice struct {
	ident        model.Identity
	uptime       int64
	otaURL       string
	offline      int    // probes left that fail (reboot gap)
	ignore       bool   // accept Upgrade but never flash (download failed)
	echo         string // override of the Upgrade echo
	upgradeErr   bool   // flash, but lose the reply
	upgradeTimes []time.Time
	// autoContinue: on Upgrade the device flashes minimal by itself
	// (Tasmota ota_loader), answers on minimal for this many probes, then
	// drops off again and comes back on the final image.
	autoContinue int
	then         *model.Identity
	// power is the set of relays that are ON; flashClearsPower models the
	// two-step ESP8266 path and ESP32, which lose the relay state on flashing.
	power            model.RelaySet
	flashClearsPower bool
	bootPower        model.RelaySet // relays ON after such a flash
	restricted       model.RelaySet // PulseTime/Interlock/shutter relays
	unclear          model.RelaySet // relays whose Status 11 value is neither ON nor OFF
	light            model.RelaySet // the light channel(s): restored by default; other relays only on request
	mac              string         // MAC the device reports; testMAC when empty
	macErr           error
	reads            int         // ReadPower calls
	onRead           func(n int) // called after the nth ReadPower, outside the lock
	readPowerErr     error
}

// step advances the fake device by one probe; it reports false while the
// device is unreachable.
func (fd *fakeDevice) step() bool {
	if fd.offline > 0 {
		fd.offline--
		return false
	}
	if fd.then != nil {
		if fd.autoContinue > 0 {
			fd.autoContinue--
		} else {
			fd.ident, fd.then, fd.offline, fd.uptime = *fd.then, nil, 2, 0
			return fd.step()
		}
	}
	fd.uptime += 5
	return true
}

type fakeAPI struct {
	rec      *recorder
	clk      *instantClock
	mu       sync.Mutex
	devs     map[model.DeviceID]*fakeDevice
	onUpdate func(model.DeviceID) // called inside Upgrade
	onSet    func(model.DeviceID, string)
	onIdent  func() // called first in ProbeIdentity, outside the lock
}

func (a *fakeAPI) dev(id model.DeviceID) *fakeDevice { return a.devs[id] }

func (a *fakeAPI) ProbeIdentity(_ context.Context, d model.Device) (model.Identity, string, int64, error) {
	a.rec.add("probe_identity %d", d.ID)
	if a.onIdent != nil {
		a.onIdent()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fd := a.dev(d.ID)
	if !fd.step() {
		return model.Identity{}, "", 0, errors.New("unreachable")
	}
	up := fd.uptime
	if isIntermediate(fd.ident.Variant) {
		up = 0 // minimal has no Status 11
	}
	return fd.ident, fd.otaURL, up, nil
}

func (a *fakeAPI) Probe(_ context.Context, d model.Device) (model.Identity, int64, error) {
	a.rec.add("probe %d", d.ID)
	a.mu.Lock()
	defer a.mu.Unlock()
	fd := a.dev(d.ID)
	if !fd.step() {
		return model.Identity{}, 0, errors.New("unreachable")
	}
	up := fd.uptime
	if isIntermediate(fd.ident.Variant) {
		up = 0
	}
	return fd.ident, up, nil
}

func (a *fakeAPI) ReadPower(_ context.Context, d model.Device) (PowerReading, error) {
	a.mu.Lock()
	fd := a.dev(d.ID)
	if fd.readPowerErr != nil {
		a.mu.Unlock()
		return PowerReading{}, fd.readPowerErr
	}
	fd.reads++
	n, p, hook := fd.reads, PowerReading{On: fd.power &^ fd.unclear, Unknown: fd.unclear, Light: fd.light, All: fd.power | fd.unclear | fd.restricted | fd.light}, fd.onRead
	a.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return p, nil
}

func (a *fakeAPI) ReadMAC(_ context.Context, d model.Device) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fd := a.dev(d.ID)
	if fd.macErr != nil {
		return "", fd.macErr
	}
	if fd.mac != "" {
		return fd.mac, nil
	}
	return testMAC, nil
}

func (a *fakeAPI) RestrictedRelays(_ context.Context, d model.Device, _ model.RelaySet) (model.RelaySet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dev(d.ID).restricted, nil
}

func (a *fakeAPI) SetOtaURL(_ context.Context, d model.Device, u string) error {
	a.rec.add("set_ota_url %d %s", d.ID, u)
	a.mu.Lock()
	a.dev(d.ID).otaURL = u
	hook := a.onSet
	a.mu.Unlock()
	if hook != nil {
		hook(d.ID, u)
	}
	return nil
}

func (a *fakeAPI) Upgrade(_ context.Context, d model.Device) (string, error) {
	a.rec.add("upgrade %d", d.ID)
	if a.onUpdate != nil {
		a.onUpdate(d.ID)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	fd := a.dev(d.ID)
	fd.upgradeTimes = append(fd.upgradeTimes, a.clk.Now())
	echo := fd.otaURL
	if fd.echo != "" {
		echo = fd.echo
	}
	if fd.autoContinue > 0 {
		final := finalIdent()
		fd.ident, fd.then, fd.offline, fd.uptime = minimalIdent("16.0.0"), &final, 2, 0
	} else if !fd.ignore {
		if strings.Contains(fd.otaURL, "minimal") {
			fd.ident = minimalIdent("16.0.0")
		} else {
			fd.ident = finalIdent()
		}
		fd.offline = 2
		fd.uptime = 0
		if fd.flashClearsPower {
			fd.power = fd.bootPower
		}
	}
	if fd.upgradeErr {
		return "", errors.New("timeout")
	}
	return echo, nil
}

// ---- firmware, backups, selector ----

type fakeFW struct {
	// minBudget records the shortest context budget Ensure saw: firmware
	// downloads need more than one command timeout.
	mu        *sync.Mutex
	minBudget *time.Duration
}

var artifactIDs = map[string]model.ArtifactID{"tasmota.bin.gz": 11, "tasmota-minimal.bin.gz": 12}

func (f fakeFW) Ensure(ctx context.Context, release, name string) (model.Artifact, error) {
	if f.mu != nil {
		if dl, ok := ctx.Deadline(); ok {
			f.mu.Lock()
			if b := time.Until(dl); *f.minBudget == 0 || b < *f.minBudget {
				*f.minBudget = b
			}
			f.mu.Unlock()
		}
	}
	id, ok := artifactIDs[name]
	if !ok {
		return model.Artifact{}, fmt.Errorf("no such file %q", name)
	}
	size := int64(469152)
	if id == 12 {
		size = 263670
	}
	return model.Artifact{ID: id, Release: release, Filename: name, Family: model.ChipESP8266, Size: size}, nil
}

func (fakeFW) URLFor(a model.Artifact) string {
	return "http://fw.test/fw/" + a.Release + "/" + a.Filename
}

type fakeBackups struct {
	rec  *recorder
	hook func()        // runs inside Backup
	runs *[]backup.Run // optional: what each backup was told about itself
}

func (b fakeBackups) Backup(ctx context.Context, _ string, id model.DeviceID) ([]model.Backup, error) {
	b.rec.add("backup %d", id)
	if b.runs != nil {
		*b.runs = append(*b.runs, backup.RunFrom(ctx))
	}
	if b.hook != nil {
		b.hook()
	}
	return nil, nil
}

type fakeSel struct {
	rec     *recorder
	mu      sync.Mutex
	refuse  bool
	choices []fwselect.Choice // every Decide call
	// esp32Direct plans every ESP32 direct, like fwselect does (by default
	// the fake plans by free flash for every chip).
	esp32Direct bool
	// selectErr, when set, is what Select returns (after recording nothing else).
	selectErr error
}

func (s *fakeSel) Decide(id model.Identity, c fwselect.Choice) (fwselect.Decision, error) {
	s.mu.Lock()
	s.choices = append(s.choices, c)
	s.mu.Unlock()
	if id.Version == c.Release && id.Variant != variantMinimal {
		return fwselect.DecideSkip, nil
	}
	return fwselect.DecideUpgrade, nil
}

func (s *fakeSel) Select(model.Identity, fwselect.Choice) (fwselect.Selection, error) {
	if s.selectErr != nil {
		return fwselect.Selection{}, s.selectErr
	}
	return fwselect.Selection{Family: model.ChipESP8266, Variant: "tasmota", Filename: "tasmota.bin.gz", Minimal: "tasmota-minimal.bin.gz"}, nil
}

func (s *fakeSel) Plan(id model.Identity, size int64) (model.Plan, error) {
	if s.esp32Direct && id.Family.IsESP32() {
		return model.PlanDirect, nil
	}
	if size > int64(id.FreeKB)*1024-16*1024 {
		return model.PlanStaged, nil
	}
	return model.PlanDirect, nil
}

func (s *fakeSel) Guard(_ model.Identity, a model.Artifact, u string, _ fwselect.Choice) error {
	s.rec.add("guard %s", u)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse {
		return fwselect.ErrFamilyMismatch
	}
	if !strings.HasSuffix(u, "/"+a.Filename) {
		return fwselect.ErrFilenameMismatch
	}
	return nil
}

// ---- harness ----

// fakePower is the PowerSwitch: it switches the fake device and logs the call.
type fakePower struct {
	rec  *recorder
	api  *fakeAPI
	fail map[int]bool // relays whose command fails
	// ignore: the command succeeds but the relay stays OFF
	ignore map[int]bool
	// ignoreOnce: the first command for the relay is lost, the second works
	ignoreOnce map[int]bool
}

func (p *fakePower) Power(_ context.Context, actor string, id model.DeviceID, relay int, action string) (string, error) {
	p.rec.add("power %d relay %d %s by %s", id, relay, action, actor)
	if p.fail[relay] {
		return "", errors.New("power command failed")
	}
	if p.ignoreOnce[relay] {
		delete(p.ignoreOnce, relay)
		return action, nil
	}
	if !p.ignore[relay] {
		p.api.mu.Lock()
		fd := p.api.dev(id)
		fd.power = fd.power.Add(relay)
		p.api.mu.Unlock()
	}
	return action, nil
}

type harness struct {
	power   *fakePower
	backups *fakeBackups
	rec     *recorder
	clk     *instantClock
	st      *fakeStore
	api     *fakeAPI
	sel     *fakeSel
	fw      fakeFW
	e       *Engine
}

func newHarness(t *testing.T, maxParallel int, devs ...*fakeDevice) *harness {
	t.Helper()
	rec := &recorder{}
	clk := &instantClock{now: t0}
	h := &harness{rec: rec, clk: clk, st: newFakeStore(rec), api: &fakeAPI{rec: rec, clk: clk, devs: map[model.DeviceID]*fakeDevice{}}, sel: &fakeSel{rec: rec},
		fw: fakeFW{mu: &sync.Mutex{}, minBudget: new(time.Duration)}}
	for i, d := range devs {
		id := model.DeviceID(i + 1)
		h.st.devices[id] = model.Device{ID: id, Name: fmt.Sprintf("plug-%d", id), Address: "192.0.2.1", MAC: testMAC}
		h.api.devs[id] = d
	}
	h.power = &fakePower{rec: rec, api: h.api}
	h.backups = &fakeBackups{rec: rec}
	h.e = h.newEngine(maxParallel)
	return h
}

func (h *harness) newEngine(maxParallel int) *Engine {
	e := NewEngine(Options{Machine: testCfg(), MaxParallel: maxParallel, Power: h.power}, h.st, h.st, h.api, h.fw, h.backups, h.clk, nil)
	e.sel = h.sel
	return e
}

func plugDevice() *fakeDevice { return &fakeDevice{ident: plug1M(), uptime: 5000, otaURL: origURL} }

// run starts Run and stops it once every job is terminal.
func (h *harness) run(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		act, _ := h.st.ActiveJobs(ctx)
		if len(act) == 0 {
			break
		}
		// Every scenario ends within hours of virtual time; a job still
		// running after a virtual day is stuck in a loop.
		if time.Now().After(deadline) || h.clk.Now().After(t0.Add(24*time.Hour)) {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
			t.Fatalf("jobs did not finish: %+v\nlog: %v", act, h.rec.entries())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func (h *harness) job(t *testing.T, id model.JobID) model.Job {
	t.Helper()
	j, err := h.st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func submitOne(t *testing.T, h *harness, r Request) model.JobID {
	t.Helper()
	if _, err := h.e.Submit(context.Background(), "alice", []Request{r}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{DeviceID: r.DeviceID})
	return jobs[len(jobs)-1].ID
}

// assertWriteAhead checks that every device-changing command was preceded
// by persisting the state that allows it. This is what makes a crash at
// any point resumable without repeating a non-idempotent step.
func assertWriteAhead(t *testing.T, log []string) {
	t.Helper()
	last := map[string]string{} // device/job id -> last saved state
	for i, e := range log {
		f := strings.Fields(e)
		switch f[0] {
		case "save":
			last[f[1]] = f[2]
		case "upgrade":
			if last[f[1]] != string(model.JobUpgradeIssued) {
				t.Fatalf("log[%d] %q: last persisted state %q, want upgrade_issued\n%v", i, e, last[f[1]], log)
			}
		case "set_ota_url":
			if s := last[f[1]]; s != string(model.JobSetOtaURL) && s != string(model.JobRestoreOtaURL) {
				t.Fatalf("log[%d] %q: last persisted state %q\n%v", i, e, s, log)
			}
		case "backup":
			if last[f[1]] != string(model.JobBackup) {
				t.Fatalf("log[%d] %q: last persisted state %q\n%v", i, e, last[f[1]], log)
			}
		}
	}
}

// ---- tests ----

// Why (N1): the backup list says "Automatic, before update to 16.0.0"; the
// backup before an update must say so, with the release it prepares for.
func TestPreUpdateBackupIsMarkedWithTheTargetRelease(t *testing.T) {
	h := newHarness(t, 1, plugDevice())
	var runs []backup.Run
	h.backups = &fakeBackups{rec: h.rec, runs: &runs}
	h.e = h.newEngine(1)
	submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	if len(runs) != 1 || runs[0].Trigger != model.TriggerUpdate || runs[0].Ref != "16.0.0" {
		t.Fatalf("runs %+v", runs)
	}
}

// The 1 MB ESP8266 path end to end: minimal hop, final image, original
// OtaUrl restored, every step persisted before it runs.
func TestEngineStagedUpdate(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)

	j := h.job(t, id)
	if j.State != model.JobSucceeded || j.LastError != "" {
		t.Fatalf("job %s %q\n%v", j.State, j.LastError, h.rec.entries())
	}
	log := h.rec.entries()
	assertWriteAhead(t, log)
	want := []string{
		"backup 1",
		"set_ota_url 1 http://fw.test/fw/16.0.0/tasmota-minimal.bin.gz",
		"upgrade 1",
		"set_ota_url 1 http://fw.test/fw/16.0.0/tasmota.bin.gz",
		"upgrade 1",
		"set_ota_url 1 " + origURL,
	}
	var got []string
	for _, e := range log {
		if strings.HasPrefix(e, "backup") || strings.HasPrefix(e, "set_ota_url") || strings.HasPrefix(e, "upgrade") {
			got = append(got, e)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("device commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Guard runs right before each OtaUrl and each Upgrade.
	for i, e := range log {
		if strings.HasPrefix(e, "set_ota_url 1 http://fw.test") || strings.HasPrefix(e, "upgrade") {
			if !strings.HasPrefix(log[i-1], "guard ") {
				t.Fatalf("%q not directly preceded by a guard run: %v", e, log[:i])
			}
		}
	}
	if d := h.api.dev(1); d.ident != finalIdent() || d.otaURL != origURL {
		t.Fatalf("device ends as %+v with OtaUrl %q", d.ident, d.otaURL)
	}
	evs, _ := h.st.JobEvents(context.Background(), id)
	if len(evs) == 0 || evs[len(evs)-1].To != model.JobSucceeded {
		t.Fatalf("job events not recorded: %+v", evs)
	}
}

// A crash right after upgrade_issued was persisted, before Upgrade was
// sent: the resumed engine must probe, wait out the reboot window, and only
// then send Upgrade again (the device never rebooted).
func TestEngineCrashAfterPersistingUpgradeIssued(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})

	crashed := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	h.st.onSave = func(j model.Job) {
		if j.State == model.JobUpgradeIssued {
			once.Do(func() { cancel(); close(crashed) })
		}
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()
	<-crashed
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := h.rec.count("upgrade"); n != 0 {
		t.Fatalf("Upgrade sent after the crash point: %d", n)
	}
	h.st.onSave = nil
	resumedAt := h.clk.Now()
	before := len(h.rec.entries())

	h.run(t, h.newEngine(2))
	log := h.rec.entries()[before:]
	if !strings.HasPrefix(log[0], "save 1 upgrade_issued") || !strings.HasPrefix(log[1], "probe 1") {
		t.Fatalf("resume must persist the new deadline and probe first, got %v", log[:2])
	}
	times := h.api.dev(1).upgradeTimes
	if len(times) == 0 || times[0].Before(resumedAt.Add(testCfg().RebootWindow)) {
		t.Fatalf("Upgrade re-sent at %v, before the reboot window (%v) passed", times, resumedAt.Add(testCfg().RebootWindow))
	}
	if j := h.job(t, id); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	assertWriteAhead(t, h.rec.entries())
}

// A crash after Upgrade went out and the device flashed: on resume the
// probe shows the reboot, so no second Upgrade for that stage.
func TestEngineResumeAfterUpgradeTookEffect(t *testing.T) {
	dev := &fakeDevice{ident: minimalIdent("16.0.0"), uptime: 3, otaURL: minimalURL}
	h := newHarness(t, 2, dev)
	j := job(model.JobUpgradeIssued, model.StageMinimal)
	j.DeviceID = 1
	j.ID = 1
	j.MinimalArtifact, j.TargetArtifact = 12, 11
	h.st.put(j)

	h.run(t, h.e)
	if got := h.job(t, 1); got.State != model.JobSucceeded {
		t.Fatalf("job %s %q\n%v", got.State, got.LastError, h.rec.entries())
	}
	log := h.rec.entries()
	firstUpgrade := -1
	for i, e := range log {
		if strings.HasPrefix(e, "upgrade") {
			firstUpgrade = i
			break
		}
	}
	if h.rec.count("upgrade") != 1 || firstUpgrade < 0 || !strings.Contains(strings.Join(log[:firstUpgrade], "\n"), "tasmota.bin.gz") {
		t.Fatalf("minimal stage Upgrade was resent; log %v", log)
	}
}

// A lost Upgrade reply (the download blocks the web server) is not a
// failure and never triggers a resend.
func TestEngineLostUpgradeReply(t *testing.T) {
	dev := plugDevice()
	dev.upgradeErr = true
	h := newHarness(t, 2, dev)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := h.rec.count("upgrade"); n != 2 {
		t.Fatalf("Upgrade sent %d times, want 2 (one per stage)", n)
	}
	if h.rec.count("backup") != 0 {
		t.Fatal("backup ran although SkipBackup was chosen")
	}
}

// A device that never takes the update gets MaxAttempts Upgrades per
// stage, each only after the accept window passed, then the job fails.
func TestEngineDownloadNeverStarts(t *testing.T) {
	dev := plugDevice()
	dev.ignore = true
	h := newHarness(t, 2, dev)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextNoUpgradeStart) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	evs, _ := h.st.JobEvents(context.Background(), id)
	if last := evs[len(evs)-1]; last.Detail != ErrTextNoUpgradeStart || last.Event != string(EvTimeout) {
		t.Fatalf("last event %+v: the history must say why", last)
	}
	retried := false
	for _, ev := range evs {
		if ev.From == model.JobUpgradeIssued && ev.To == model.JobSetOtaURL {
			retried = ev.Detail == ErrTextNoUpgradeStart
		}
	}
	if !retried {
		t.Fatalf("retry row without reason: %+v", evs)
	}
	times := dev.upgradeTimes
	if len(times) != 2 || times[1].Sub(times[0]) < testCfg().UpgradeAccept {
		t.Fatalf("upgrade times %v: want 2, at least UpgradeAccept apart", times)
	}
}

// Someone else changed OtaUrl between our OtaUrl and Upgrade: the device
// flashes a file we did not check, so the job fails loudly.
func TestEngineUpgradeEchoMismatch(t *testing.T) {
	dev := plugDevice()
	dev.echo = "http://other.example/tasmota.bin.gz"
	h := newHarness(t, 2, dev)
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextOtaURLChanged) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := h.rec.count("upgrade"); n != 1 {
		t.Fatalf("Upgrade sent %d times", n)
	}
}

// Guard runs again right before Upgrade; if it refuses, Upgrade is not sent.
func TestEngineGuardRefusesBeforeUpgrade(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	h.api.onSet = func(model.DeviceID, string) {
		h.sel.mu.Lock()
		h.sel.refuse = true
		h.sel.mu.Unlock()
	}
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "family mismatch") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if h.rec.count("upgrade") != 0 {
		t.Fatal("Upgrade sent despite guard refusal")
	}
}

// OTA_PARALLEL: ESP8266 web servers and the firmware server are small, so
// only MaxParallel jobs may be past queued at any time.
func TestEngineMaxParallel(t *testing.T) {
	devs := []*fakeDevice{plugDevice(), plugDevice(), plugDevice(), plugDevice(), plugDevice()}
	h := newHarness(t, 2, devs...)
	var reqs []Request
	for i := range devs {
		reqs = append(reqs, Request{DeviceID: model.DeviceID(i + 1), Release: "16.0.0"})
	}
	if _, err := h.e.Submit(context.Background(), "alice", reqs); err != nil {
		t.Fatal(err)
	}
	// Barrier: the first two jobs wait for each other in preflight, so
	// the test sees real overlap instead of jobs finishing one by one.
	var bmu sync.Mutex
	calls, both := 0, make(chan struct{})
	h.api.onIdent = func() {
		bmu.Lock()
		calls++
		n := calls
		if n == 2 {
			close(both)
		}
		bmu.Unlock()
		if n <= 2 {
			select {
			case <-both:
			case <-time.After(2 * time.Second):
				t.Error("second job never started next to the first")
			}
		}
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{})
	for _, j := range jobs {
		if j.State != model.JobSucceeded {
			t.Fatalf("job %d %s %q", j.ID, j.State, j.LastError)
		}
	}
	if h.st.maxBusy != 2 {
		t.Fatalf("max jobs past queued = %d, want exactly 2", h.st.maxBusy)
	}
}

// Submit refuses (all-or-nothing) excluded devices, devices with an active
// job and devices already at the target, and says why for each.
func TestSubmitRefuses(t *testing.T) {
	atTarget := &fakeDevice{ident: finalIdent(), uptime: 1, otaURL: origURL}
	h := newHarness(t, 2, plugDevice(), plugDevice(), atTarget, plugDevice())
	h.st.devices[1] = withExcluded(h.st.devices[1])
	h.st.put(model.Job{DeviceID: 2, State: model.JobAwaitReboot})
	_, err := h.e.Submit(context.Background(), "alice", []Request{
		{DeviceID: 1, Release: "16.0.0"}, {DeviceID: 2, Release: "16.0.0"},
		{DeviceID: 3, Release: "16.0.0"}, {DeviceID: 4, Release: "16.0.0"}, {DeviceID: 4, Release: "16.0.0"},
		{DeviceID: 99, Release: "16.0.0"},
	})
	var re *RefusedError
	if !errors.Is(err, ErrRefused) || !errors.As(err, &re) {
		t.Fatalf("err = %v, want RefusedError", err)
	}
	reasons := map[model.DeviceID]string{}
	for _, r := range re.Refused {
		reasons[r.DeviceID] = r.Reason
	}
	for id, want := range map[model.DeviceID]string{1: "excluded", 2: "active job", 3: "already at target", 4: "listed twice", 99: "unknown device"} {
		if !strings.Contains(reasons[id], want) {
			t.Errorf("device %d: reason %q, want %q", id, reasons[id], want)
		}
	}
	if h.st.batches != 0 {
		t.Fatal("a batch was created although devices were refused")
	}
	if _, err := h.e.Submit(context.Background(), "alice", nil); err == nil {
		t.Fatal("empty batch accepted")
	}
}

func withExcluded(d model.Device) model.Device { d.OTAExcluded = true; return d }

// A variant is persisted only with explicit consent; otherwise the job keeps
// the detected variant.
func TestSubmitPersistsJobFields(t *testing.T) {
	h := newHarness(t, 2, plugDevice(), plugDevice())
	if _, err := h.e.Submit(context.Background(), "alice", []Request{
		{DeviceID: 1, Release: "16.0.0", Variant: "sensors"},
		{DeviceID: 2, Release: "16.0.0", Variant: "sensors", AllowVariantChange: true, Policy: model.PolicyReinstall, SkipBackup: true},
	}); err != nil {
		t.Fatal(err)
	}
	j1, j2 := h.job(t, 1), h.job(t, 2)
	if j1.TargetVariant != "" || j1.Policy != model.PolicyOnlyNewer || j1.State != model.JobQueued || j1.CreatedBy != "alice" || j1.TargetRelease != "16.0.0" {
		t.Fatalf("job 1 %+v", j1)
	}
	if j2.TargetVariant != "sensors" || j2.Policy != model.PolicyReinstall || !j2.SkipBackup {
		t.Fatalf("job 2 %+v", j2)
	}
}

func TestPreview(t *testing.T) {
	big := plugDevice()
	big.ident.FreeKB = 2000
	h := newHarness(t, 2, plugDevice(), big, &fakeDevice{ident: finalIdent()}, plugDevice())
	h.st.devices[4] = withExcluded(h.st.devices[4])
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}, {DeviceID: 2, Release: "16.0.0"}, {DeviceID: 3, Release: "16.0.0"}, {DeviceID: 4, Release: "16.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []PreviewItem{
		{DeviceID: 1, Decision: "upgrade", Plan: model.PlanStaged, Filename: "tasmota.bin.gz"},
		{DeviceID: 2, Decision: "upgrade", Plan: model.PlanDirect, Filename: "tasmota.bin.gz"},
		{DeviceID: 3, Decision: "skip", Reason: detailSkip},
		{DeviceID: 4, Decision: "refuse", Reason: "excluded from OTA"},
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
	if n := h.rec.count("set_ota_url") + h.rec.count("upgrade") + h.rec.count("backup"); n != 0 {
		t.Fatalf("preview changed devices: %v", h.rec.entries())
	}
}

// Preview reports guard refusals per device instead of failing the call.
func TestPreviewGuardRefusal(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	h.sel.refuse = true
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}})
	if err != nil || items[0].Decision != "refuse" || !strings.Contains(items[0].Reason, "family mismatch") {
		t.Fatalf("items %+v err %v", items, err)
	}
}

func TestCancelQueuedJob(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	if err := h.e.Cancel(context.Background(), "bob", id); err != nil {
		t.Fatal(err)
	}
	if j := h.job(t, id); j.State != model.JobCancelled {
		t.Fatalf("state %s", j.State)
	}
	evs, _ := h.st.JobEvents(context.Background(), id)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "bob") {
		t.Fatalf("cancel not recorded with actor: %+v", evs)
	}
	if err := h.e.Cancel(context.Background(), "bob", id); !errors.Is(err, ErrNotActive) {
		t.Fatalf("second cancel: %v, want ErrNotActive", err)
	}
	before := len(h.rec.entries())
	h.run(t, h.e)
	if after := h.rec.entries(); len(after) != before {
		t.Fatalf("cancelled job touched the device: %v", after[before:])
	}
}

// Cancel while the device flashes: the stage finishes (the device cannot be
// stopped), then the job ends cancelled instead of starting the next stage.
func TestCancelWhileFlashing(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.api.onUpdate = func(model.DeviceID) {
		if err := h.e.Cancel(context.Background(), "bob", id); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobCancelled || !j.CancelRequested {
		t.Fatalf("job %s cancelRequested=%v", j.State, j.CancelRequested)
	}
	if n := h.rec.count("upgrade"); n != 1 {
		t.Fatalf("Upgrade sent %d times, want 1 (minimal stage only)", n)
	}
	if h.api.dev(1).ident.Variant != "minimal" {
		t.Fatal("device should have finished the minimal stage")
	}
}

// Tasmota's own fallback on a 1 MB device: one Upgrade, the device hops
// through minimal by itself and continues. The engine must follow it
// without a second Upgrade and without calling the minimal sighting a
// failure, however long minimal answers before it starts propagating.
func TestEngineFollowsTasmotaAutoContinue(t *testing.T) {
	for _, minimalProbes := range []int{1, 3, 20} {
		dev := plugDevice()
		dev.ident.FreeKB = 2000 // looks big enough: direct plan
		dev.autoContinue = minimalProbes
		h := newHarness(t, 2, dev)
		id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
		h.run(t, h.e)
		j := h.job(t, id)
		// With a single minimal answer the identity probe already misses
		// it; from three on, verify sees minimal and waits for it.
		if j.State != model.JobSucceeded || j.AutoContinueSeen != (minimalProbes >= 3) {
			t.Fatalf("minimal probes %d: job %s %q autoContinueSeen=%v\n%v", minimalProbes, j.State, j.LastError, j.AutoContinueSeen, h.rec.entries())
		}
		if n := h.rec.count("upgrade"); n != 1 {
			t.Fatalf("minimal probes %d: Upgrade sent %d times, want 1", minimalProbes, n)
		}
	}
}

// A download that fails without reboot: the device answers on the old
// image between attempts. Upgrade is resent only after the reboot window,
// never while Tasmota may still be retrying.
func TestEngineFailedDownloadWaitsForWindow(t *testing.T) {
	dev := plugDevice()
	dev.ident.FreeKB = 2000
	dev.ignore = true
	dev.offline = 0
	h := newHarness(t, 2, dev)
	h.api.onUpdate = func(model.DeviceID) {
		h.api.mu.Lock()
		h.api.dev(1).offline = 1 // one blocked download attempt
		h.api.mu.Unlock()
	}
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	times := dev.upgradeTimes
	if len(times) != 2 || times[1].Sub(times[0]) < testCfg().RebootWindow {
		t.Fatalf("upgrade times %v: second Upgrade must wait for the reboot window", times)
	}
}

func TestNewEngineDefaults(t *testing.T) {
	st := newFakeStore(&recorder{})
	cases := []struct {
		in, want int
	}{{0, 2}, {-1, 2}, {1, 1}, {5, 5}}
	for _, c := range cases {
		e := NewEngine(Options{MaxParallel: c.in}, st, st, nil, fakeFW{}, nil, &instantClock{}, nil)
		if e.opts.MaxParallel != c.want {
			t.Errorf("MaxParallel %d -> %d, want %d", c.in, e.opts.MaxParallel, c.want)
		}
		if e.opts.Machine != DefaultConfig() {
			t.Errorf("zero Machine config not defaulted: %+v", e.opts.Machine)
		}
	}
	custom := testCfg()
	custom.MaxAttempts = 3
	if e := NewEngine(Options{Machine: custom}, st, st, nil, fakeFW{}, nil, &instantClock{}, nil); e.opts.Machine != custom {
		t.Errorf("custom config replaced: %+v", e.opts.Machine)
	}
}

// Variant consent: a variant change is allowed only when the user chose a
// variant (Submit persists TargetVariant only with consent), and the Guard
// re-runs allow exactly the variant the plan selected.
func TestChoices(t *testing.T) {
	j := job(model.JobPreflight, "")
	j.TargetVariant = ""
	if c := planChoice(j); c.AllowVariantChange || c.Variant != "" || c.Release != "16.0.0" || c.Policy != model.PolicyOnlyNewer {
		t.Errorf("plan choice without consent: %+v", c)
	}
	j.TargetVariant = "sensors"
	if c := planChoice(j); !c.AllowVariantChange || c.Variant != "sensors" {
		t.Errorf("plan choice with consent: %+v", c)
	}
	j.TargetVariant = "tasmota" // same as detected
	if c := guardChoice(j); c.AllowVariantChange || c.Variant != "tasmota" {
		t.Errorf("guard choice, same variant: %+v", c)
	}
	j.TargetVariant = "sensors"
	if c := guardChoice(j); !c.AllowVariantChange {
		t.Errorf("guard choice, planned variant change: %+v", c)
	}
}

// Batches start FIFO by job ID, whatever order the store returns.
func TestEngineStartsFIFO(t *testing.T) {
	h := newHarness(t, 1, plugDevice(), plugDevice(), plugDevice())
	h.st.reverse = true
	for id := model.DeviceID(1); id <= 3; id++ {
		submitOne(t, h, Request{DeviceID: id, Release: "16.0.0"})
	}
	h.run(t, h.e)
	if want := []model.JobID{1, 2, 3}; fmt.Sprint(h.st.started) != fmt.Sprint(want) {
		t.Fatalf("start order %v, want %v", h.st.started, want)
	}
	if h.st.maxBusy != 1 {
		t.Fatalf("max busy %d, want 1", h.st.maxBusy)
	}
}

// Firmware downloads during planning get more time than one command.
func TestEnginePlanBudget(t *testing.T) {
	h := newHarness(t, 1, plugDevice())
	if _, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}}); err != nil {
		t.Fatal(err)
	}
	if b := *h.fw.minBudget; b <= testCfg().CommandTimeout {
		t.Fatalf("plan budget %v, want more than CommandTimeout", b)
	}
}

// A device updated by someone else between Submit and the job start is
// skipped, and the history says why.
func TestEngineSkipAtPlanTime(t *testing.T) {
	h := newHarness(t, 1, plugDevice())
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.api.dev(1).ident = finalIdent()
	h.run(t, h.e)
	j := h.job(t, id)
	evs, _ := h.st.JobEvents(context.Background(), id)
	if j.State != model.JobSucceeded || evs[len(evs)-1].Detail != detailSkip {
		t.Fatalf("job %s events %+v", j.State, evs)
	}
	if h.rec.count("upgrade")+h.rec.count("set_ota_url")+h.rec.count("backup") != 0 {
		t.Fatal("skipped job touched the device")
	}
}

// fakeStore also implements store.BatchStore.
func (s *fakeStore) GetBatch(_ context.Context, id model.BatchID) (model.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || int(id) > s.batches {
		return model.Batch{}, store.ErrNotFound
	}
	return model.Batch{ID: id, RetryOf: s.retryOf[id]}, nil
}

func (s *fakeStore) CreateRetryBatch(ctx context.Context, by string, retryOf model.BatchID, jobs []model.Job) (model.BatchID, []model.JobID, error) {
	if _, err := s.GetBatch(ctx, retryOf); err != nil {
		return 0, nil, err
	}
	id, ids, err := s.CreateBatch(ctx, by, jobs)
	if err == nil {
		s.mu.Lock()
		if s.retryOf == nil {
			s.retryOf = map[model.BatchID]model.BatchID{}
		}
		s.retryOf[id] = retryOf
		s.mu.Unlock()
	}
	return id, ids, err
}

func (s *fakeStore) BatchRetries(_ context.Context, id model.BatchID) ([]model.Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Batch
	for b, of := range s.retryOf {
		if of == id {
			out = append(out, model.Batch{ID: b, RetryOf: of})
		}
	}
	return out, nil
}

var _ store.BatchStore = (*fakeStore)(nil)

// Why (#87): the preflight probes the device over HTTP and stores what it found. That identity
// must carry DetectedAt, because MQTT (INFO1, Status 2 over MQTT) only ever fills a device
// whose DetectedAt is unset. Without it, a forged retained INFO1 could rewrite the identity of
// a device the OTA engine just probed, and with it the variant Guard checks against.
func TestEngineStoresThePreflightIdentityAsHTTPDetected(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
	h.run(t, h.e)
	h.st.mu.Lock()
	got := h.st.devices[1].Identity
	h.st.mu.Unlock()
	if got.DetectedAt.IsZero() {
		t.Fatalf("stored identity has no DetectedAt: %+v", got)
	}
	if got.DetectedAt.Before(t0) {
		t.Errorf("DetectedAt %v is before the job started (%v)", got.DetectedAt, t0)
	}
}

// Why: an ESP32-S2 without a chosen build must come back from the preview as a refusal that
// asks for the choice, not as a plain failure, so the page can offer the builds (#88). Any
// other refusal must not offer a choice.
func TestPreviewFlagsABuildChoice(t *testing.T) {
	h := newHarness(t, 2, plugDevice(), plugDevice())
	h.sel.selectErr = fwselect.ErrBuildChoiceNeeded
	items, err := h.e.Preview(context.Background(), []Request{{DeviceID: 1, Release: "16.0.0"}})
	if err != nil || items[0].Decision != decisionRefuse || !items[0].BuildChoice {
		t.Fatalf("items %+v err %v", items, err)
	}
	h.sel.selectErr = fwselect.ErrUnknownHardware
	items, err = h.e.Preview(context.Background(), []Request{{DeviceID: 2, Release: "16.0.0"}})
	if err != nil || items[0].Decision != decisionRefuse || items[0].BuildChoice {
		t.Fatalf("other refusal offers a choice: %+v err %v", items, err)
	}
	if _, err := h.e.Submit(context.Background(), "u", []Request{{DeviceID: 1, Release: "16.0.0"}}); !errors.Is(err, ErrRefused) {
		t.Fatalf("Submit without a build: %v, want ErrRefused", err)
	}
}

// Why: the build is chosen once, at submit; the job must carry it through planning, the guard
// re-runs (a restart resumes from the stored job alone) and a retry.
func TestBuildTravelsWithTheJob(t *testing.T) {
	h := newHarness(t, 2, plugDevice())
	if _, err := h.e.Submit(context.Background(), "u", []Request{{DeviceID: 1, Release: "16.0.0", Build: "s2cdc"}}); err != nil {
		t.Fatal(err)
	}
	j := h.job(t, 1)
	if j.TargetBuild != "s2cdc" {
		t.Fatalf("TargetBuild = %q", j.TargetBuild)
	}
	if c := planChoice(j); c.Build != "s2cdc" {
		t.Errorf("plan choice lost the build: %+v", c)
	}
	if c := guardChoice(j); c.Build != "s2cdc" {
		t.Errorf("guard choice lost the build: %+v", c)
	}
	j.TargetVariant, j.IdentityBefore.Variant = "tasmota", "unknown"
	if r := retryRequest(j); r.Build != "s2cdc" || r.Variant != "tasmota" || !r.AllowVariantChange {
		t.Errorf("retry request %+v", r)
	}
	if c := choiceFor(Request{Build: "s2"}); c.Build != "s2" {
		t.Errorf("choiceFor lost the build: %+v", c)
	}
}
