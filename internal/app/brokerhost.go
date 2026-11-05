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

	kick    chan model.DeviceID
	pending map[model.DeviceID]brokerWant
	next    map[model.DeviceID]time.Time // when the periodic read of a device is due again; absent: due
	// probeNext: when a device may be asked for an HTTP probe again; absent: now.
	probeNext map[model.DeviceID]time.Time
}

// brokerWant is a read somebody asked for.
type brokerWant struct{ due, until time.Time }

func newBrokerHostReader(list func(context.Context) ([]model.Device, error), // httpUp says whether the HTTP health poll reached the device lately: the periodic read is HTTP
	// and must not hold the lock of a device it cannot reach.
	httpUp func(model.DeviceID) bool,
	read func(context.Context, model.DeviceID) error, enter func(context.Context, model.DeviceID, string) (func(), error), log *slog.Logger) *brokerHostReader {
	return &brokerHostReader{list: list, httpUp: httpUp, read: read, enter: enter, now: time.Now, log: log,
		kick: make(chan model.DeviceID, brokerHostQueue), pending: map[model.DeviceID]brokerWant{}, next: map[model.DeviceID]time.Time{}, probeNext: map[model.DeviceID]time.Time{}}
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

func (r *brokerHostReader) run(ctx context.Context) {
	t := time.NewTicker(brokerHostTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.kick:
			r.want(id)
		case <-t.C:
			r.pass(ctx)
		}
	}
}

// want records that id is to be read once it has settled.
func (r *brokerHostReader) want(id model.DeviceID) {
	now := r.now()
	r.pending[id] = brokerWant{due: now.Add(brokerHostSettle), until: now.Add(brokerHostPendingFor)}
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
			switch err := r.readOne(ctx, id); {
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
		if due, ok := r.next[d.ID]; ok && now.Before(due) {
			continue
		}
		if !r.readable(d) {
			ask(d)
			continue
		}
		if err := r.readOne(ctx, d.ID); !errors.Is(err, errBrokerBusy) {
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

// readOne reads id under the device lock. errBrokerBusy: a job holds the device, nothing was sent.
// errBrokerRetry: the read failed. nil: stored (or nothing to store).
func (r *brokerHostReader) readOne(ctx context.Context, id model.DeviceID) error {
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
	rctx, cancel := context.WithTimeout(ctx, brokerHostTimeout)
	defer cancel()
	if err := r.read(rctx, id); err != nil {
		if ctx.Err() == nil {
			r.next[id] = r.now().Add(brokerHostRetry)
			r.log.Debug("broker host: read failed", "device", id, "err", err)
		}
		return errBrokerRetry
	}
	r.next[id] = r.now().Add(brokerHostEvery)
	return nil
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
	a.goBG("broker-host", func() error { a.brokerHost.run(ctx); return nil })
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
	if !h.APIOff() || a.facts == nil {
		return ""
	}
	f, err := a.facts.Facts(ctx, id)
	if err != nil {
		return ""
	}
	if transport.Choose(f).Backup != transport.None {
		return ""
	}
	return skipAPIOff
}
