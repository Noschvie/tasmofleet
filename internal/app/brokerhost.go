package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Keeping the stored broker host current (#598). TasmoFleet learns where a device reports to only
// when it probes the device, so a device moved to another broker outside TasmoFleet (its own web
// page, its console, a restored config that points to an old broker) kept the old host and the
// "Other broker" hint (#580) stayed away. The reader re-reads it (Prober.ReadBroker: Status 6,
// never Status 0) in three cases:
//
//   - the device restarted: Tasmota restarts after MqttHost and MqttPort (restart_flag = 2,
//     xdrv_02_9_mqtt.ino CmndMqttHost / CmndMqttPort) and after a config restore (the upload sets
//     restart_flag = 2, xdrv_01_9_webserver.ino HandleUploadDone), so a changed host always comes
//     with a restart that the health tracker records (tracker.AppendBoot);
//   - TasmoFleet sent MqttHost or MqttPort (console, bulk command, settings, policy fix): the
//     result of that command is observed where it is sent;
//   - now and then for every online device (brokerHostEvery), which also catches a restart nobody
//     saw (the tracker was down, the device was offline for a while).
//
// The same reader keeps the rest of a device's data current (#600), in the same visit, so there is one
// scheduler and one device at a time, not several that happen to meet on a device:
//
//   - identity (Prober.RefreshFacts, HTTP: Status 5, 2, 4; never Status 0; it renames nothing) after a restart and every
//     identityEvery, under the device lock like the broker read;
//   - the Settings values (settings.Manager.Read, which fills the Settings tab's last-known values),
//     the values the policy rules check (policyReporter.readDevice) and the GPIO template reading
//     (templates.Manager.Refresh) after a restart and every dataEvery. These are read without the device
//     lock: a person's save must not be refused as busy for the half minute a full read takes, and a save
//     during the read keeps the outdated values out of the Settings cache by itself (settings save
//     counter, #599). The lock is only tried first, so a device a job holds is skipped.
//
// A restart is recorded by the health tracker from INFO3 (once per boot) and from the uptime drop in
// STATE, whichever it sees first (health/stability.go); either leads here through bootWatch, so a
// restart is one refresh however many messages announce it.
//
// One device at a time on the whole process, and only while nobody else works on the device: the
// read takes the device lock like any writer and is skipped while a job holds the device or a firmware
// update typed in the console is still flashing it (backupGuard, as for a scheduled backup).
const (
	// brokerHostEvery: how often an online device's broker is read without a reason. A move outside
	// TasmoFleet is rare and a restart already triggers a read, so this only has to catch the rest;
	// six hours keep the load at four small Status 6 reads per device and day.
	brokerHostEvery = 6 * time.Hour
	// brokerHostTick: how often the reader looks for work. At most one device is read per tick for
	// the periodic reads, so a fleet is spread out over time instead of read in one burst.
	brokerHostTick = 30 * time.Second
	// brokerHostSettle: a read asked for by a restart or a command waits this long, so the device
	// has finished restarting and the session that caused it has let go of the device.
	brokerHostSettle = 20 * time.Second
	// brokerHostRetry: a periodic read that failed (offline, timeout) is tried again after this.
	brokerHostRetry = 10 * time.Minute
	// brokerHostPendingFor: a requested read that cannot run (device busy or not answering) is
	// given up after this; the periodic read catches up later.
	brokerHostPendingFor = 30 * time.Minute
	// brokerHostAskAgain: a requested read that failed is asked again after this.
	brokerHostAskAgain = 2 * time.Minute
	brokerHostTimeout  = 15 * time.Second
	// brokerHostHTTPFresh: the periodic read goes only to a device the HTTP health poll reached this
	// lately. A device that is online over MQTT alone (HTTP blocked or off) would otherwise hold its
	// lock for the whole timeout at every retry and refuse the person's writes as busy.
	brokerHostHTTPFresh = 15 * time.Minute
	// brokerHostProbeEvery: a device the read is due for but HTTP does not reach is asked for an HTTP probe
	// at most this often (#456); a device fine on MQTT is not HTTP-polled, so this is how it gets a fresh
	// HTTP fact, one device per pass.
	brokerHostProbeEvery = 30 * time.Minute
	brokerHostQueue      = 64

	// dataEvery: how often an online device's Settings values, policy values and template reading are read
	// without a reason (#600). They change only when somebody changes them, and a restart (the usual
	// moment) already triggers a read; six hours are four reads of about 40 small requests a day.
	dataEvery = 6 * time.Hour
	// identityEvery: how often the identity (firmware, hardware, network facts) is probed again without a
	// reason (#600). A firmware update outside TasmoFleet restarts the device, which triggers a read.
	identityEvery = 24 * time.Hour
	// dataTimeout bounds the whole data read of one device (its parts have their own limits).
	dataTimeout     = 3 * time.Minute
	identityTimeout = 30 * time.Second
)

type brokerHostReader struct {
	list func(context.Context) ([]model.Device, error)
	// httpUp says whether the HTTP health poll reached the device lately: the periodic read is HTTP
	// and must not hold the lock of a device it cannot reach.
	httpUp func(model.DeviceID) bool
	// wantHTTP asks for one HTTP probe of a device whose read is due but whose HTTP fact is not fresh
	// (health.Service.ProbeHTTP); nil: never.
	wantHTTP func(model.DeviceID)
	read     func(context.Context, model.DeviceID) error
	// enter takes the device lock for the read and refuses while a job or a typed firmware update holds
	// the device (runtime.brokerReadGuard); the caller calls the returned release.
	enter func(context.Context, model.DeviceID, string) (func(), error)
	now   func() time.Time
	log   *slog.Logger
	// readIdentity re-probes the identity over HTTP (Prober.RefreshFacts); nil: not done.
	readIdentity func(context.Context, model.DeviceID) error
	// readData fills the caches of the Settings tab, the policy values and the GPIO templates from the
	// device; nil: not done. It runs without the device lock (see above).
	readData func(context.Context, model.Device) error

	kick        chan model.DeviceID
	kickRestart chan model.DeviceID
	pending     map[model.DeviceID]brokerWant
	next        map[model.DeviceID]time.Time // when the periodic read of a device is due again; absent: due
	// dataNext and identityNext: the same for the data and the identity; absent: due.
	dataNext, identityNext map[model.DeviceID]time.Time
	// probeNext: when a device may be asked for an HTTP probe again; absent: now.
	probeNext map[model.DeviceID]time.Time
}

// brokerWant is a read somebody asked for. restart: the device restarted, so its identity and data are
// read as well.
type brokerWant struct {
	due, until time.Time
	restart    bool
}

func newBrokerHostReader(list func(context.Context) ([]model.Device, error), // httpUp says whether the HTTP health poll reached the device lately: the periodic read is HTTP
	// and must not hold the lock of a device it cannot reach.
	httpUp func(model.DeviceID) bool,
	read func(context.Context, model.DeviceID) error, enter func(context.Context, model.DeviceID, string) (func(), error), log *slog.Logger) *brokerHostReader {
	return &brokerHostReader{list: list, httpUp: httpUp, read: read, enter: enter, now: time.Now, log: log,
		kick: make(chan model.DeviceID, brokerHostQueue), kickRestart: make(chan model.DeviceID, brokerHostQueue),
		dataNext: map[model.DeviceID]time.Time{}, identityNext: map[model.DeviceID]time.Time{}, pending: map[model.DeviceID]brokerWant{}, next: map[model.DeviceID]time.Time{}, probeNext: map[model.DeviceID]time.Time{}}
}

// later asks for a read of id soon and returns at once.
func (r *brokerHostReader) later(id model.DeviceID) {
	if r == nil {
		return
	}
	select {
	case r.kick <- id:
	default: // queue full: the periodic read catches up
	}
}

// restarted asks for a read of id, its identity and its data soon and returns at once.
func (r *brokerHostReader) restarted(id model.DeviceID) {
	if r == nil {
		return
	}
	select {
	case r.kickRestart <- id:
	default: // queue full: the periodic reads catch up
	}
}

func (r *brokerHostReader) run(ctx context.Context) {
	t := time.NewTicker(brokerHostTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.kick:
			r.want(id)
		case id := <-r.kickRestart:
			r.wantRestart(id)
		case <-t.C:
			r.pass(ctx)
		}
	}
}

// want records that id's broker is to be read once it has settled.
func (r *brokerHostReader) want(id model.DeviceID) { r.request(id, false) }

// wantRestart records that id restarted: its broker, identity and data are read once it has settled. A
// device that announces its restart several times (INFO3, the uptime drop, a second boot) keeps one entry
// whose time moves back, so it is one visit after the last announcement.
func (r *brokerHostReader) wantRestart(id model.DeviceID) { r.request(id, true) }

func (r *brokerHostReader) request(id model.DeviceID, restart bool) {
	now := r.now()
	r.pending[id] = brokerWant{due: now.Add(brokerHostSettle), until: now.Add(brokerHostPendingFor), restart: restart || r.pending[id].restart}
}

// pass reads the devices somebody asked for whose time has come, then at most one online device
// whose periodic read is due. Both only for a device the read can reach (readable): a requested read
// waits for that, without sending anything, until it is given up.
func (r *brokerHostReader) pass(ctx context.Context) {
	now := r.now()
	devs, err := r.list(ctx)
	if err != nil {
		r.log.Warn("broker host: list devices", "err", err)
		return
	}
	listed := make(map[model.DeviceID]model.Device, len(devs))
	for _, d := range devs {
		listed[d.ID] = d
	}
	probed := false // one probe request per pass: not the whole fleet at once
	ask := func(d model.Device) {
		if probed || r.wantHTTP == nil || policy.LimitedFirmware(d.Identity.Variant) {
			return
		}
		if at, ok := r.probeNext[d.ID]; ok && now.Before(at) {
			return
		}
		r.probeNext[d.ID] = now.Add(brokerHostProbeEvery)
		probed = true
		r.wantHTTP(d.ID)
	}
	for id, w := range r.pending {
		if ctx.Err() != nil {
			return
		}
		d, ok := listed[id]
		switch {
		case !ok || now.After(w.until): // deleted, or waited long enough: the periodic read catches up
			delete(r.pending, id)
		case now.Before(w.due): // not yet
		case !r.readable(d): // nothing the read could reach now: HTTP is checked, once the read is due
			ask(d)
		default:
			switch err := r.visit(ctx, d, visitWhat{broker: true, identity: w.restart, data: w.restart}); {
			case err == nil:
				delete(r.pending, id)
			case errors.Is(err, errBrokerRetry): // not answering: not asked again at every tick
				w.due = now.Add(brokerHostAskAgain)
				r.pending[id] = w
			} // busy: asked again at the next tick, nothing was sent
		}
	}
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		if _, asked := r.pending[d.ID]; asked {
			continue
		}
		what := visitWhat{
			broker:   r.isDue(r.next, d.ID, now),
			identity: r.readIdentity != nil && r.isDue(r.identityNext, d.ID, now),
			data:     r.readData != nil && r.isDue(r.dataNext, d.ID, now),
		}
		if !what.any() {
			continue
		}
		if !r.readable(d) {
			ask(d)
			continue
		}
		if err := r.visit(ctx, d, what); !errors.Is(err, errBrokerBusy) {
			return // one periodic read per pass; a busy device does not use up the turn
		}
	}
}

// readable says whether the read can reach d. It is HTTP (Prober.ReadBroker), so only a device the HTTP
// health poll reached lately qualifies: one online over MQTT alone would hold its lock for the whole
// timeout at every try and refuse the person's writes as busy. And only on regular firmware: minimal and
// safeboot answer Unknown (#558; the list gives the effective variant).
func (r *brokerHostReader) readable(d model.Device) bool {
	return !policy.LimitedFirmware(d.Identity.Variant) && r.httpUp(d.ID)
}

var (
	errBrokerBusy  = errors.New("device busy")
	errBrokerRetry = errors.New("read failed, try again")
)

// visitWhat says what a visit to one device reads.
type visitWhat struct{ broker, identity, data bool }

func (w visitWhat) any() bool { return w.broker || w.identity || w.data }

// isDue says whether the schedule m has a read of id due at now (absent: due).
func (r *brokerHostReader) isDue(m map[model.DeviceID]time.Time, id model.DeviceID, now time.Time) bool {
	at, ok := m[id]
	return !ok || !now.Before(at)
}

// visit reads what the schedule or a restart asks for from one device: the broker and the identity under
// the device lock, then the data (see the top of the file). errBrokerBusy: a job holds the device, nothing
// was sent. errBrokerRetry: the broker read failed (nothing else is tried then: the device does not
// answer). nil: done, or the identity or data read failed, which then waits for its own retry.
func (r *brokerHostReader) visit(ctx context.Context, d model.Device, what visitWhat) error {
	id := d.ID
	if what.broker || what.identity {
		if err := r.readLocked(ctx, id, what); err != nil {
			return err
		}
	}
	if what.data && r.readData != nil {
		r.readDataOf(ctx, d)
	}
	return nil
}

// readLocked reads the broker and the identity of id under the device lock.
func (r *brokerHostReader) readLocked(ctx context.Context, id model.DeviceID, what visitWhat) error {
	release, err := r.enter(ctx, id, "broker read")
	if err != nil {
		var busy devlock.BusyError
		if errors.As(err, &busy) {
			return errBrokerBusy
		}
		r.log.Debug("broker host: device not available", "device", id, "err", err)
		return errBrokerRetry
	}
	defer release()
	if what.broker {
		rctx, cancel := context.WithTimeout(ctx, brokerHostTimeout)
		err := r.read(rctx, id)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				r.next[id] = r.now().Add(brokerHostRetry)
				r.log.Debug("broker host: read failed", "device", id, "err", err)
			}
			return errBrokerRetry
		}
		r.next[id] = r.now().Add(brokerHostEvery)
	}
	if what.identity && r.readIdentity != nil {
		ictx, cancel := context.WithTimeout(ctx, identityTimeout)
		err := r.readIdentity(ictx, id)
		cancel()
		r.finished(ctx, r.identityNext, id, identityEvery, err, "identity")
	}
	return nil
}

// readDataOf reads the data of d if nothing holds the device. The lock is only taken and given back (the
// check), not kept during the read.
func (r *brokerHostReader) readDataOf(ctx context.Context, d model.Device) {
	release, err := r.enter(ctx, d.ID, "data read")
	if err != nil {
		delete(r.dataNext, d.ID) // held by a job (or not available): due again at the next visit
		return
	}
	release()
	dctx, cancel := context.WithTimeout(ctx, dataTimeout)
	err = r.readData(dctx, d)
	cancel()
	r.finished(ctx, r.dataNext, d.ID, dataEvery, err, "data")
}

// finished schedules the next read of kind after one that ended with err: the regular interval, or the
// retry delay when it failed. A failure caused by shutdown is not counted.
func (r *brokerHostReader) finished(ctx context.Context, m map[model.DeviceID]time.Time, id model.DeviceID, every time.Duration, err error, kind string) {
	switch {
	case err == nil:
		m[id] = r.now().Add(every)
	case ctx.Err() == nil:
		m[id] = r.now().Add(brokerHostRetry)
		r.log.Debug("device refresh: read failed", "kind", kind, "device", id, "err", err)
	}
}

// setsBrokerTarget reports whether a command line sets MqttHost or MqttPort (also inside a Backlog).
// Only a command with a value changes anything; a bare "MqttHost" only reads.
func setsBrokerTarget(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if len(cmd) >= 7 && strings.EqualFold(cmd[:7], "backlog") {
		cmd = cmd[7:]
	}
	for _, part := range strings.FieldsFunc(cmd, func(r rune) bool { return r == ';' }) {
		f := strings.Fields(part)
		if len(f) >= 2 && (strings.EqualFold(f[0], "MqttHost") || strings.EqualFold(f[0], "MqttPort")) {
			return true
		}
	}
	return false
}

// bootWatch asks for a broker read after every restart the health tracker records. Why here: the
// tracker stores each restart it notices (INFO3 over MQTT, an uptime drop over HTTP) through this
// interface, and the broker host is the one thing a restart may have changed (a restored config, an
// MqttHost typed on the device). Nothing in internal/restore or internal/transport is involved.
type bootWatch struct {
	store.StabilityStore
	onBoot func(model.DeviceID)
}

func (w bootWatch) AppendBoot(ctx context.Context, b model.DeviceBoot) error {
	err := w.StabilityStore.AppendBoot(ctx, b)
	if err == nil {
		w.onBoot(b.DeviceID)
	}
	return err
}

// startBrokerHost wires the reader to the runtime and starts it: devices with their effective
// firmware, the HTTP health poll, and brokerReadGuard.
func (a *runtime) startBrokerHost(ctx context.Context) {
	a.brokerHost.list = a.fw.list(a.svc.Inventory.List)
	a.brokerHost.wantHTTP = func(id model.DeviceID) {
		if a.tracker.Get(id).State == model.HealthOnline { // a dead device is not probed for this
			a.probeHTTP(id)
		}
	}
	a.brokerHost.httpUp = func(id model.DeviceID) bool { return brokerHTTPUp(a.tracker.Get(id), time.Now()) }
	a.brokerHost.enter = a.brokerReadGuard()
	a.wireDeviceRefresh(a.brokerHost)
	a.goBG("broker-host", func() error { a.brokerHost.run(ctx); return nil })
}

// wireDeviceRefresh gives the reader the identity probe and the data read (#600).
func (a *runtime) wireDeviceRefresh(r *brokerHostReader) {
	r.readIdentity = a.svc.Prober.RefreshFacts
	r.readData = a.refreshData
}

// refreshData fills the caches that pages and the policy report show from: the Settings values (the
// Settings tab's last-known values, #599), the values the policy rules check, and the GPIO template
// reading. The first part that fails ends the read: the device does not answer, and the retry reads all.
func (a *runtime) refreshData(ctx context.Context, d model.Device) error {
	if a.settings != nil {
		if _, err := a.settings.Read(ctx, d.ID); err != nil {
			return err
		}
	}
	if a.policyLive != nil && !a.policyLive.readDevice(ctx, d) && !policy.LimitedFirmware(d.Identity.Variant) {
		return errors.New("policy values not read")
	}
	if a.templates != nil {
		return a.templates.Refresh(ctx, d)
	}
	return nil
}

// brokerReadGuard keeps the read off a device the way a scheduled backup is kept off (backupGuard): its
// lock, an active job, and a firmware update typed in the console that is still flashing it (#347),
// which also keeps the read away from the minimal image such an update passes through.
func (a *runtime) brokerReadGuard() func(context.Context, model.DeviceID, string) (func(), error) {
	return backupGuard(deviceGuard{locks: a.deviceLocks(), jobs: a.svc.Store, holds: a.fwHolds})
}

// brokerHTTPUp says whether h shows a device that answered the HTTP health poll lately.
func brokerHTTPUp(h model.Health, now time.Time) bool {
	return h.State == model.HealthOnline && !h.SeenHTTP.IsZero() && now.Sub(h.SeenHTTP) < brokerHostHTTPFresh
}

// probeHTTP asks the tracker for one HTTP probe of a device (#456) unless something holds it. It is what
// the device page (web.Deps.ProbeHTTP) and the broker re-read (wantHTTP) call.
func (a *runtime) probeHTTP(id model.DeviceID) {
	if a.probeAllowed(id) {
		a.askProbe(id)
	}
}

// probeAllowed says nothing holds the device: no job, no operation under its lock, no firmware update typed
// in the console that is still flashing it. Taking and releasing the lock is the check; the probe itself runs
// later and holds nothing.
func (a *runtime) probeAllowed(id model.DeviceID) bool {
	release, err := a.brokerReadGuard()(a.bgContext, id, "http probe")
	if err != nil {
		return false
	}
	release()
	return true
}

// apiOffSkip is skipAPIOff for an online device whose HTTP API is off and which no other path can back up, "" for any
// other device (#447). It decides on the router's own facts, which carry the tracker's finding (httpState), so a
// device it leaves to the run is backed up the way it says: over MQTT.
func (a *runtime) apiOffSkip(ctx context.Context, id model.DeviceID, h model.Health) string {
	if !a.apiOffNoPath(ctx, id, h, transport.ActBackup) {
		return ""
	}
	return skipAPIOff
}

// apiOffNoPath says the device's HTTP API is off and nothing else (MQTT) can carry the action: the one rule of the
// gates (model.Health.APIOff plus the router's facts, #447, #645). False when the facts cannot be read.
func (a *runtime) apiOffNoPath(ctx context.Context, id model.DeviceID, h model.Health, act transport.Action) bool {
	if !h.APIOff() || a.facts == nil {
		return false
	}
	f, err := a.facts.Facts(ctx, id)
	if err != nil {
		return false
	}
	p := transport.Choose(f)
	switch act {
	case transport.ActCommands:
		return p.Commands == transport.None
	case transport.ActWrites:
		return p.Writes == transport.None
	case transport.ActBackup:
		return p.Backup == transport.None
	case transport.ActRestore:
		return p.Restore == transport.None
	case transport.ActUpdate:
		return p.Update == transport.None
	}
	return false
}
