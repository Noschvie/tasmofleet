package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
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
//   - identity (Prober.RefreshFacts: Status 5, 2, 4 and the facts, MQTT first after the check that hears every reply on
//     the topic, #682, else at the address; never Status 0; it renames nothing) after a restart and every
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
// restart is one refresh however many messages announce it. A restart reads the identity and the data only
// if no restart read them in the last hour (restartRefreshEvery): a device in a boot loop gets the broker
// read after every boot, as before #600, and the rest once when the hour is over (a failed read is not
// retried before), so about once an hour. A restart read that read nothing for the device does not count (#1003): its
// check found minimal firmware (no MAC to show, the first step of an update on an ESP8266), another device, or a
// topic it could not vouch for; the boot into the new firmware a minute later is then read in full. The policy values
// are left out of the data read while the policy live check is paused or its interval is 0, as for the policy poller.
//
// Three more things ask for the identity alone, at once and inside the hour (#1003): a live INFO1 or a backup that names
// another firmware version than the stored identity (identityChanged), and a console or bulk write the device accepted
// for a value the identity holds (identityWritten: OtaUrl, a group topic, the topic, the names, the hostname). They
// only ask: nothing of the INFO1, the backup or the answer is stored, the identity read stores what it reads with its
// own checks (T20). A version asks once (versionAsked) until an identity read of the device succeeds, so a boot loop on
// that version stays braked; the requests of a device share its one pending entry, and the reader reads one device at a
// time, so there is never more than one identity read of a device in flight. A read one of them asked for that fails is
// retried after brokerHostRetry like a scheduled one (#817), and a restart in between leaves that retry alone.
//
// A device whose HTTP API is off (or HTTP blocked) is never "readable" (the HTTP health poll does not reach it), yet MQTT
// serves it: when its identity or its data is due, after a restart too, and the router reads it over MQTT, it is
// visited for those (overMQTT: the data since #676, the identity since #682, whose probe goes MQTT first), without the
// broker read, which goes MQTT first like every read (#839) but looks for what only HTTP can show: a device that moved
// to another broker does not answer on TasmoFleet's, and on it the read could only confirm what its messages show.
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
	// brokerHostClaimsPerPass: topic claim checks (#876) one pass runs. A claim is untrusted input: one login can flag
	// every stored device at once, and each check holds the reader for the 1.5 s identity window, up to 8 s
	// (mqttcmd.DefaultReplyWait) for a device that stays silent, while nothing else on it (kicks, restart reads, the
	// periodic reads) is served. The claims left over wait for the next pass, up to their hard end.
	brokerHostClaimsPerPass = 2

	// dataEvery: how often an online device's Settings values, policy values and template reading are read
	// without a reason (#600). They change only when somebody changes them, and a restart (the usual
	// moment) already triggers a read. The pages mark values older than an hour as stale and read them again
	// when they open, and the policy poller reads the policy values every hour, so a daily read is enough to
	// keep the last known values from getting old: about 25 small requests per device a day.
	dataEvery = 24 * time.Hour
	// identityEvery: how often the identity (firmware, hardware, network facts) is probed again without a
	// reason (#600). A firmware update outside TasmoFleet restarts the device, which triggers a read.
	identityEvery = 24 * time.Hour
	// restartRefreshEvery: a restart reads the identity and the data again only this long after a restart
	// last read them. A device in a boot loop (stability: three restarts in an hour) would otherwise get the
	// whole read after every boot; its further restarts read only the broker and leave the rest to the end
	// of the hour.
	restartRefreshEvery = time.Hour
	// dataTimeout bounds the whole data read of one device (its parts have their own limits).
	dataTimeout     = 3 * time.Minute
	identityTimeout = 30 * time.Second
)

type brokerHostReader struct {
	list func(context.Context) ([]model.Device, error)
	// httpUp says whether the HTTP health poll reached the device lately: the broker and identity visit waits for it
	// (readable) and must not hold the lock of a device it cannot reach.
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
	// readIdentity re-probes the identity, MQTT first (Prober.RefreshFacts); nil: not done.
	readIdentity func(context.Context, model.DeviceID) error
	// readData fills the caches of the Settings tab, the policy values and the GPIO templates from the
	// device; nil: not done. It runs without the device lock (see above).
	readData func(context.Context, model.Device) error
	// readsOverMQTT says whether the reads of d go over MQTT (the router's choice, transport.Choose: Commands is MQTT);
	// nil: never. Only then is a device whose HTTP API is off visited for its identity and data (overMQTT).
	readsOverMQTT func(context.Context, model.Device) bool
	// topicMarked says whether the router holds d off its MQTT topic because another device answers there (#852,
	// Prober.topicMarked); recheckTopic asks that topic again and hears every reply (Prober.recheckMarked):
	// anotherOnTopic while another device still answers, nil otherwise. Either nil: not done.
	topicMarked  func(model.Device) bool
	recheckTopic func(context.Context, model.Device) error
	// checkClaim asks the topic of an unmarked device a discovery config named with another MAC (#876): the same check
	// as recheckTopic (Prober.checkClaimed), for the topic claimed only; nil: claims are not followed.
	checkClaim func(context.Context, model.Device, string) error

	kick        chan model.DeviceID
	kickRestart chan model.DeviceID
	// kickIdentity: the identity reads a new version or a write asked for (#1003).
	kickIdentity chan identityKick
	claims       chan topicClaim
	pending      map[model.DeviceID]brokerWant
	next         map[model.DeviceID]time.Time // when the periodic read of a device is due again; absent: due
	// dataNext and identityNext: the same for the data and the identity; absent: due.
	dataNext, identityNext map[model.DeviceID]time.Time
	// probeNext: when a device may be asked for an HTTP probe again; absent: now.
	probeNext map[model.DeviceID]time.Time
	// recheckNext: when the topic of a marked device is asked again (recheckTopics); absent: due.
	recheckNext map[model.DeviceID]time.Time
	// claimed: the topic claims waiting for the next pass, at most one per device (#876); claimNext: until when a
	// claim of a device is not taken up again, so a device is asked at most once per brokerHostRetry however often
	// its claim is published. Both belong to the reader's goroutine, like every map here.
	claimed   map[model.DeviceID]topicClaim
	claimNext map[model.DeviceID]time.Time
	// restartRead: when a restart last read the identity and the data of a device, or the read it left to the
	// end of the hour ran (restartRefreshEvery); restartLeft: a restart left that read to the end of the hour.
	restartRead map[model.DeviceID]time.Time
	restartLeft map[model.DeviceID]bool
	// retryAt: when a regular-interval read of the identity or the data that failed is tried again (#817);
	// requested leaves such a time alone, once. inRestartRead: the visit running is a restart's read (also the one
	// left to the end of the hour), whose failures are not recorded there. retrying: the last read of that kind
	// failed, so the next one is a retry, whose failure is not recorded either. A boot loop is therefore read in
	// full once an hour, not every ten minutes.
	retryAt       map[retryKey]time.Time
	retrying      map[retryKey]bool
	inRestartRead bool
	// versionAsked: the firmware version that last asked for an identity read of a device (#1003). The same version
	// asks no more until an identity read of the device succeeds (readLocked) or the device is gone (pass): a boot loop
	// announces its version at every boot.
	versionAsked map[model.DeviceID]string
	// outcomes: why the last background data refresh of a device failed (#818), read by the device page.
	outcomes *refreshOutcomes
}

// topicClaim is a discovery config that named a stored device's topic with a MAC the device does not have (#876). until
// is its hard end: a claim a busy device could not be asked about is dropped then.
type topicClaim struct {
	id    model.DeviceID
	topic string
	until time.Time
}

// retryKey names the identity or the data schedule of one device.
type retryKey struct {
	id       model.DeviceID
	identity bool
}

// brokerWant is a read somebody asked for. broker: the broker is read (a restart, a broker command). restart: the
// device restarted, so its identity and data are read as well. identity: the identity is read even inside the hour a
// restart read brakes (a new version or an identity value was written, #1003).
type brokerWant struct {
	due, until                time.Time
	broker, restart, identity bool
}

// identityKick asks for an identity read of id. version: the firmware version an INFO1 or a backup named; "" for a
// write, which asks every time.
type identityKick struct {
	id      model.DeviceID
	version string
}

func newBrokerHostReader(list func(context.Context) ([]model.Device, error), // httpUp says whether the HTTP health poll reached the device lately: the periodic read is HTTP
	// and must not hold the lock of a device it cannot reach.
	httpUp func(model.DeviceID) bool,
	read func(context.Context, model.DeviceID) error, enter func(context.Context, model.DeviceID, string) (func(), error), log *slog.Logger) *brokerHostReader {
	return &brokerHostReader{list: list, httpUp: httpUp, read: read, enter: enter, now: time.Now, log: log,
		kick: make(chan model.DeviceID, brokerHostQueue), kickRestart: make(chan model.DeviceID, brokerHostQueue), kickIdentity: make(chan identityKick, brokerHostQueue),
		versionAsked: map[model.DeviceID]string{},
		claims:       make(chan topicClaim, brokerHostQueue), claimed: map[model.DeviceID]topicClaim{}, claimNext: map[model.DeviceID]time.Time{},
		dataNext: map[model.DeviceID]time.Time{}, identityNext: map[model.DeviceID]time.Time{}, pending: map[model.DeviceID]brokerWant{}, next: map[model.DeviceID]time.Time{}, probeNext: map[model.DeviceID]time.Time{},
		recheckNext: map[model.DeviceID]time.Time{}, restartRead: map[model.DeviceID]time.Time{}, restartLeft: map[model.DeviceID]bool{}, retryAt: map[retryKey]time.Time{}, retrying: map[retryKey]bool{}, outcomes: newRefreshOutcomes()}
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

// identityChanged asks for an identity read of id, whose live INFO1 or newest backup names version, another firmware
// version than the stored identity (#1003), and returns at once. It is called from the MQTT identity reader and the
// backup watcher, which must not wait for the reader.
func (r *brokerHostReader) identityChanged(id model.DeviceID, version string) {
	r.kickIdentityRead(identityKick{id: id, version: version})
}

// identityWritten asks for an identity read of id after a write the device accepted changed a value the identity holds
// (holdingCmd, #1003), and returns at once.
func (r *brokerHostReader) identityWritten(id model.DeviceID) {
	r.kickIdentityRead(identityKick{id: id})
}

func (r *brokerHostReader) kickIdentityRead(k identityKick) {
	if r == nil {
		return
	}
	select {
	case r.kickIdentity <- k:
	default: // queue full: the periodic identity read catches up
	}
}

// topicClaimed tells the reader that a discovery config named id's topic with another MAC (#876) and returns at once:
// it runs on the discovery worker, which must not wait for a check that hears the topic for seconds. The claim only
// flags the device for the next pass (recheckTopics); the check, not the claim, decides whether the device is marked.
// A full queue drops the claim: discovery's per-MAC rate limit and the next publish of the config bring it back, and
// the identity probe (Prober.hearTopic, every 24 hours and after a restart) is the backstop.
func (r *brokerHostReader) topicClaimed(id model.DeviceID, topic string) {
	if r == nil {
		return
	}
	select {
	case r.claims <- topicClaim{id: id, topic: topic}:
	default: // queue full: see above
		r.log.Debug("topic claim: queue full, dropped", "device", id, "topic", topic)
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
		case k := <-r.kickIdentity:
			r.wantIdentity(k)
		case c := <-r.claims:
			r.noteClaim(c)
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

// noteClaim keeps a topic claim for the next pass. A device that was asked less than brokerHostRetry ago (claimNext)
// is not flagged again, and one claim waits at a time: its hard end is brokerHostPendingFor after the first.
func (r *brokerHostReader) noteClaim(c topicClaim) {
	now := r.now()
	if at, ok := r.claimNext[c.id]; ok && now.Before(at) {
		return
	}
	if old, ok := r.claimed[c.id]; ok {
		c.until = old.until
	} else {
		c.until = now.Add(brokerHostPendingFor)
	}
	r.claimed[c.id] = c
}

func (r *brokerHostReader) request(id model.DeviceID, restart bool) {
	now := r.now()
	old := r.pending[id]
	r.pending[id] = brokerWant{due: now.Add(brokerHostSettle), until: now.Add(brokerHostPendingFor), broker: true, restart: restart || old.restart, identity: old.identity}
}

// wantIdentity records that id's identity is to be read once it has settled, inside the hour a restart read brakes
// (#1003). A version that already asked is not asked about again until an identity read of the device succeeds.
func (r *brokerHostReader) wantIdentity(k identityKick) {
	if k.version != "" {
		if r.versionAsked[k.id] == k.version {
			return
		}
		r.versionAsked[k.id] = k.version
	}
	now := r.now()
	w := r.pending[k.id]
	w.due, w.until, w.identity = now.Add(brokerHostSettle), now.Add(brokerHostPendingFor), true
	r.pending[k.id] = w
}

// pass asks the topics of the marked devices that are due again (recheckTopics), then reads the devices somebody
// asked for whose time has come, then at most one online device whose periodic read is due. Both reads only for a
// device the read can reach (readable): a requested read waits for that, without sending anything, until it is given
// up.
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
	r.outcomes.keep(listed)
	for id := range r.versionAsked {
		if _, ok := listed[id]; !ok {
			delete(r.versionAsked, id) // deleted: nothing more to ask
		}
	}
	r.recheckTopics(ctx, devs, now)
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
		case !r.readable(d) && (!w.restart && !w.identity || !r.overMQTT(ctx, d, visitWhat{identity: true, data: true}).any()):
			ask(d) // nothing the read could reach now: HTTP is checked, once the read is due
		default:
			what := r.requested(id, w, now)
			full := what.data // requested reads the data only as the restart's own read
			if !r.readable(d) {
				// A restart of a device HTTP does not reach: its identity and data go over MQTT (#682, #676), the broker
				// read waits for HTTP. A restart within the hour leaves them to the end of the hour (requested): nothing
				// to read now, so the read keeps waiting like any other.
				ask(d)
				if what = r.overMQTT(ctx, d, what); !what.any() {
					break
				}
			}
			// A read a new version or a write asked for is no restart's read: its failure is retried soon (#1003).
			r.inRestartRead = !w.identity
			err := r.visit(ctx, d, what)
			r.inRestartRead = false
			switch {
			case err == nil:
				delete(r.pending, id)
				if full {
					r.restartRead[id] = now
					delete(r.restartLeft, id)
				}
			case errors.Is(err, errBrokerRefused): // read nothing for the device: no restart's read, the hour is not used up (#1003)
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
			if what = r.overMQTT(ctx, d, what); !what.any() {
				continue // the broker waits for HTTP (readable)
			}
		}
		r.inRestartRead = r.restartLeft[d.ID] // the read a restart left to the end of the hour is the restart's read
		err := r.visit(ctx, d, what)
		r.inRestartRead = false
		if err == nil && r.restartLeft[d.ID] && (what.identity || what.data) {
			r.restartRead[d.ID] = now // the read a restart left to the end of the hour: the next hour starts here
			delete(r.restartLeft, d.ID)
		}
		if !errors.Is(err, errBrokerBusy) {
			return // one periodic read per pass; a busy device does not use up the turn
		}
	}
}

// recheckTopics asks the topic of every listed device the router holds off it (#852) again once its own schedule is
// due, every brokerHostRetry, and warns while another device still answers there. Why a schedule of its own, apart from
// readable, overMQTT and the read schedules: the mark sends the device's reads over HTTP, so the visits that ask
// the topic on their way may never come. The broker read waits for HTTP (readable), and the data refresh of a device
// HTTP does not reach waits for its reads to go over MQTT (overMQTT), which the mark itself prevents: a device
// reached over MQTT whose HTTP times out (HTTP state unknown, never failing) stayed held off its topic until TasmoFleet
// restarted. The check is the one that alone may end a mark (Prober.recheckTopic: Status 5, every reply on the stored
// topic within the identity window); nothing else is sent. The device lock is only tried, as for the data read: a
// device a job holds is asked at the next pass.
func (r *brokerHostReader) recheckTopics(ctx context.Context, devs []model.Device, now time.Time) {
	if r.topicMarked == nil || r.recheckTopic == nil {
		return
	}
	r.pruneClaims(devs, now)
	claimChecks := 0 // brokerHostClaimsPerPass: the claims over the cap wait for a later pass
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		if !r.topicMarked(d) {
			delete(r.recheckNext, d.ID) // a new mark is asked about at the next pass
			if claimChecks < brokerHostClaimsPerPass && r.checkClaimed(ctx, d, now) {
				claimChecks++
			}
			continue
		}
		delete(r.claimed, d.ID) // the mark holds already: its own schedule asks the topic
		if !r.isDue(r.recheckNext, d.ID, now) {
			continue
		}
		release, err := r.enter(ctx, d.ID, backgroundData)
		if err != nil {
			continue // held by a job or a typed firmware update: asked at the next pass, nothing was sent
		}
		release()
		r.recheckNext[d.ID] = now.Add(brokerHostRetry)
		cctx, cancel := context.WithTimeout(ctx, brokerHostTimeout)
		err = r.recheckTopic(cctx, d)
		cancel()
		r.logTopicCheck(ctx, d, err)
	}
}

// logTopicCheck warns of a check that found the topic taken and notes one that learnt nothing.
func (r *brokerHostReader) logTopicCheck(ctx context.Context, d model.Device, err error) {
	var on anotherOnTopic
	switch {
	case errors.As(err, &on):
		r.log.Warn("topic check: "+topicTakenLog, "device", d.ID, "topic", on.topic)
	case err != nil && ctx.Err() == nil:
		r.log.Debug("topic check: failed; the mark stays", "device", d.ID, "err", err)
	}
}

// checkClaimed asks the topic of an unmarked device a discovery config named with another MAC (#876), once, through the
// router's own check: it hears every reply on the topic and marks the device only for a second reply or a foreign MAC,
// so a forged claim marks nothing. At most one check per device per brokerHostRetry (claimNext, also across the
// mark: recheckNext is seeded, so the mark's own schedule does not ask again 30 seconds later). A claim ends with its
// check whatever it learnt: no reply means nobody else listens, a window cut short comes from the 15 s budget or a
// shutdown, and while MQTT is not usable a mark would change nothing (writes go to no topic then); the identity
// probe's check (every 24 hours, after a restart) is the backstop. A device a job holds keeps its claim until a pass
// where the lock is free, or its hard end (topicClaim.until). A claim ends too when the device is marked by now, no
// longer has the claimed topic, or is gone (pruneClaims). The device lock is only tried, as for the marked check.
// It reports whether it called the check, which the caller counts against brokerHostClaimsPerPass.
func (r *brokerHostReader) checkClaimed(ctx context.Context, d model.Device, now time.Time) bool {
	c, ok := r.claimed[d.ID]
	if !ok {
		return false
	}
	if r.checkClaim == nil || now.After(c.until) || d.Topic != c.topic {
		delete(r.claimed, d.ID)
		return false
	}
	release, err := r.enter(ctx, d.ID, backgroundData)
	if err != nil {
		return false // held by a job or a typed firmware update: asked at the next pass, nothing was sent
	}
	release()
	delete(r.claimed, d.ID)
	r.claimNext[d.ID] = now.Add(brokerHostRetry)
	r.recheckNext[d.ID] = now.Add(brokerHostRetry)
	cctx, cancel := context.WithTimeout(ctx, brokerHostTimeout)
	err = r.checkClaim(cctx, d, c.topic)
	cancel()
	r.logTopicCheck(ctx, d, err)
	return true
}

// pruneClaims ends the claims and claim windows of devices that are no longer listed, and the windows that are over.
func (r *brokerHostReader) pruneClaims(devs []model.Device, now time.Time) {
	listed := make(map[model.DeviceID]bool, len(devs))
	for _, d := range devs {
		listed[d.ID] = true
	}
	for id := range r.claimed {
		if !listed[id] {
			delete(r.claimed, id)
		}
	}
	for id, at := range r.claimNext {
		if !listed[id] || !now.Before(at) {
			delete(r.claimNext, id)
		}
	}
}

// requested says what a visit somebody asked for reads: the broker, and after a restart the identity and the
// data too, unless a restart read them less than restartRefreshEvery ago. full: this is the restart's read, which
// starts that hour once it read the device (it is the only visit somebody asked for that includes the data). The identity a new version or
// a write asked for is read in any case (#1003): a boot loop keeps its version, so that does not undo the brake. Then they are read once when that
// hour is over, and not before (a failed restart read is not retried every ten minutes either): a device that keeps
// restarting is read in full about once an hour, not after every boot. The exception is the retry of a failed
// scheduled refresh (#817): it was not caused by a restart, often by this very one (the device restarted while it
// was read), and moving it to the end of the hour would leave the values stale for most of that hour.
func (r *brokerHostReader) requested(id model.DeviceID, w brokerWant, now time.Time) visitWhat {
	what := visitWhat{broker: w.broker, identity: w.identity}
	if !w.restart {
		return what
	}
	if last, ok := r.restartRead[id]; ok && now.Sub(last) < restartRefreshEvery {
		at := last.Add(restartRefreshEvery)
		for _, m := range []struct {
			next     map[model.DeviceID]time.Time
			identity bool
		}{{r.identityNext, true}, {r.dataNext, false}} {
			if retry, ok := r.retryAt[retryKey{id, m.identity}]; ok && retry.Equal(m.next[id]) && retry.After(now) && !retry.After(at) {
				continue // a failed scheduled read waits for its retry, which comes before the end of the hour
			}
			m.next[id] = at
			if !m.identity {
				r.outcomes.moveNext(id, at)
			}
		}
		r.restartLeft[id] = true
		return what
	}
	what.identity, what.data = true, true
	return what
}

// readable says whether the whole visit can reach d. The broker read, MQTT first since #839, looks for a move off
// TasmoFleet's broker, which only HTTP can show (a device that left does not answer here), so only a device the HTTP
// health poll reached lately qualifies: one online over MQTT alone would hold its lock for the whole timeout at every
// try and refuse the person's writes as busy. For such a device the identity and the data go over MQTT (overMQTT). And
// only on regular firmware: minimal and safeboot answer Unknown (#558; the list gives the effective variant).
func (r *brokerHostReader) readable(d model.Device) bool {
	return !policy.LimitedFirmware(d.Identity.Variant) && r.httpUp(d.ID)
}

// overMQTT narrows what is due for d, which readable refuses for the HTTP reason only (regular firmware, the HTTP
// health poll has not reached it: its API is off, or HTTP is blocked), to the reads that go over MQTT now: the identity
// (the probe goes MQTT first, #682) and the data (#676), when the router reads d over MQTT. Such a device never got
// them refreshed otherwise, because an API refusal never moves the HTTP fact readable asks for. The broker read keeps
// its gate. A device stored without a MAC is not visited: the probe and the data refresh have no check over MQTT to
// pin their reads to (probeOverMQTT, refreshData), so they would go over HTTP. The data read holds no lock
// (readDataOf); the identity read holds the device lock for at most identityTimeout, like over HTTP.
func (r *brokerHostReader) overMQTT(ctx context.Context, d model.Device, what visitWhat) visitWhat {
	w := visitWhat{identity: what.identity && r.readIdentity != nil, data: what.data && r.readData != nil}
	if !w.any() || r.readsOverMQTT == nil || d.MAC == "" || policy.LimitedFirmware(d.Identity.Variant) || !r.readsOverMQTT(ctx, d) {
		return visitWhat{}
	}
	return w
}

// topicTakenLog is the warning of a check that found another device on the device's MQTT topic (#852).
const topicTakenLog = "another device answers on the device's MQTT topic; until only this device answers there, its writes never go to the topic and its reads go over HTTP while HTTP works"

var (
	errBrokerBusy  = errors.New("device busy")
	errBrokerRetry = errors.New("read failed, try again")
	// errBrokerRefused: the device could not be told to be the stored one (another device answers on the topic or at
	// the address, or minimal firmware shows no MAC), so nothing was read for it (#1003). An answer, not a failure.
	errBrokerRefused = errors.New("not the stored device")
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
// answer). errBrokerRefused: whoever answered could not be told to be the stored device, so nothing more was read.
// nil: done, or the identity or data read failed, which then waits for its own retry.
func (r *brokerHostReader) visit(ctx context.Context, d model.Device, what visitWhat) error {
	id := d.ID
	if what.broker || what.identity {
		refused, err := r.readLocked(ctx, id, what)
		if err != nil {
			return err
		}
		if refused {
			return errBrokerRefused // nothing is read for this device from whoever answered
		}
	}
	if what.data && r.readData != nil && !r.readDataOf(ctx, d) && !what.broker && !what.identity {
		return errBrokerBusy // only the data was due and the device is held: nothing was sent
	}
	return nil
}

// readLocked reads the broker and the identity of id under the device lock. refused: another device answers where
// it was asked (the broker read's check, MQTT first since #839, found a MAC outside the stored set, or none, on the
// topic or at the address; or the identity refresh found one at the address).
func (r *brokerHostReader) readLocked(ctx context.Context, id model.DeviceID, what visitWhat) (refused bool, err error) {
	release, err := r.enter(ctx, id, backgroundRead)
	if err != nil {
		var busy devlock.BusyError
		if errors.As(err, &busy) {
			return false, errBrokerBusy
		}
		r.log.Debug("broker host: device not available", "device", id, "err", err)
		return false, errBrokerRetry
	}
	defer release()
	if what.broker {
		rctx, cancel := context.WithTimeout(ctx, brokerHostTimeout)
		err := r.read(rctx, id)
		cancel()
		if err != nil {
			switch {
			case ctx.Err() != nil:
			case notStoredDevice(err): // not this device: nothing was stored. An answer, not a failure (like a refused
				// identity): nothing more is read in this visit, and it is asked again at the regular interval, also when a
				// restart or a command asked for the read
				r.next[id] = r.now().Add(brokerHostEvery)
				var on anotherOnTopic
				if errors.As(err, &on) {
					// the check went over MQTT first (#839): another device listens on this device's topic, where its
					// commands over MQTT went. Not a lease handed on, which the address case usually is: louder. The
					// router holds the device off the topic now (#852); the topic is asked again soon, so the mark ends
					// soon after the topic is fixed
					r.next[id] = r.now().Add(brokerHostRetry)
					r.log.Warn("broker host: "+topicTakenLog, "device", id, "topic", on.topic)
				} else {
					r.log.Debug("broker host: another device answers at the address", "device", id)
				}
				return true, nil
			default:
				r.next[id] = r.now().Add(brokerHostRetry)
				r.log.Debug("broker host: read failed", "device", id, "err", err)
			}
			return false, errBrokerRetry
		}
		r.next[id] = r.now().Add(brokerHostEvery)
	}
	if what.identity && r.readIdentity != nil {
		ictx, cancel := context.WithTimeout(ctx, identityTimeout)
		err := r.readIdentity(ictx, id)
		cancel()
		var ref *RefreshRefusedError
		if errors.As(err, &ref) {
			refused = true
			err = nil                                   // an answer, not a failure: another device answers at the address (logged and audited by the prober); asked again at the regular interval
			r.dataNext[id] = r.now().Add(identityEvery) // and nothing is read for this device from that address until the identity matches again
			why := settings.FailOtherDevice
			if ref.Reason == refusedUnverified {
				why = settings.FailNoMAC // minimal firmware, the first step of an update (#1003): not another device
			}
			r.outcomes.fail(id, r.now(), r.dataNext[id], why)
		}
		if err == nil && !refused {
			delete(r.versionAsked, id) // the stored identity is the device's now: a new version may ask again
		}
		r.finished(ctx, r.identityNext, retryKey{id, true}, identityEvery, err, "identity")
	}
	return refused, nil
}

// readDataOf reads the data of d if nothing holds the device and reports whether it did. The lock is only
// taken and given back (the check), not kept during the read.
func (r *brokerHostReader) readDataOf(ctx context.Context, d model.Device) bool {
	release, err := r.enter(ctx, d.ID, backgroundData)
	if err != nil {
		delete(r.dataNext, d.ID) // held by a job (or not available): due again at the next visit
		r.outcomes.moveNext(d.ID, time.Time{})
		return false
	}
	release()
	dctx, cancel := context.WithTimeout(ctx, dataTimeout)
	err = r.readData(dctx, d)
	cancel()
	if notStoredDevice(err) { // the caches stay as they are; asked again at the broker's interval
		r.dataNext[d.ID] = r.now().Add(brokerHostEvery)
		var on anotherOnTopic
		if errors.As(err, &on) {
			// the data refresh is the only background visit of a device whose HTTP API is off: the one place its
			// taken topic shows (#852). It comes back soon like the broker read; the mark itself is asked about on its
			// own schedule (recheckTopics), which the mark cannot hold off
			r.dataNext[d.ID] = r.now().Add(brokerHostRetry)
			r.log.Warn("device refresh: "+topicTakenLog, "device", d.ID, "topic", on.topic)
		} else {
			r.log.Debug("device refresh: another device answers at the address", "device", d.ID)
		}
		r.outcomes.fail(d.ID, r.now(), r.dataNext[d.ID], refreshReason(err))
		return true
	}
	r.finished(ctx, r.dataNext, retryKey{d.ID, false}, dataEvery, err, "data")
	r.noteData(ctx, d.ID, err)
	return true
}

// noteData records how a data read ended for the device page (#818): a read that worked, or one that failed after the
// Settings values were read, ends the record; a failure starts it, with the time the schedule just set. A shutdown
// concludes nothing.
func (r *brokerHostReader) noteData(ctx context.Context, id model.DeviceID, err error) {
	var later laterStepError
	switch {
	case err == nil, errors.As(err, &later):
		r.outcomes.clear(id)
	case ctx.Err() == nil:
		r.outcomes.fail(id, r.now(), r.dataNext[id], refreshReason(err))
	}
}

// finished schedules the next read of kind after one that ended with err: the regular interval, or the
// retry delay when it failed. A failure caused by shutdown is not counted. The retry time of a failed
// regular-interval read is remembered (retryAt) so that a restart read within the hour leaves it alone (#817). The
// failure of a restart's read and of a retry is not: otherwise the retries would chain through a boot loop.
func (r *brokerHostReader) finished(ctx context.Context, m map[model.DeviceID]time.Time, k retryKey, every time.Duration, err error, kind string) {
	id := k.id
	again := r.retrying[k] // this read was a retry
	delete(r.retrying, k)
	delete(r.retryAt, k)
	switch {
	case err == nil:
		m[id] = r.now().Add(every)
	case ctx.Err() == nil:
		m[id] = r.now().Add(brokerHostRetry)
		r.retrying[k] = true
		if !r.inRestartRead && !again {
			r.retryAt[k] = m[id]
		}
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
	if a.svc.Hub != nil { // a backup that names another firmware version reads the identity at once (#1003)
		ch, unsub := a.svc.Hub.Subscribe(events.TopicBackup)
		a.goBG("backup-versions", func() error {
			defer unsub()
			watchBackupVersions(ctx, ch, a.svc.Store.GetDevice, a.brokerHost.identityChanged, a.log)
			return nil
		})
	}
	a.goBG("broker-host", func() error { a.brokerHost.run(ctx); return nil })
}

// wireDeviceRefresh gives the reader the identity probe and the data read (#600), and the check of a taken topic (#852).
func (a *runtime) wireDeviceRefresh(r *brokerHostReader) {
	r.readIdentity = a.svc.Prober.RefreshFacts
	r.readData = a.refreshData
	r.readsOverMQTT = a.readsOverMQTT
	r.topicMarked = a.svc.Prober.topicMarked
	r.recheckTopic = a.svc.Prober.recheckMarked
	r.checkClaim = a.svc.Prober.checkClaimed
}

// readsOverMQTT says whether the router reads d over MQTT: transport.Choose over the device's facts, the same
// decision the router acts on. False when the facts cannot be read.
func (a *runtime) readsOverMQTT(ctx context.Context, d model.Device) bool {
	if a.facts == nil { // MQTT is off
		return false
	}
	f, err := a.facts.Facts(ctx, d.ID)
	return err == nil && transport.Choose(f).Commands == transport.MQTT
}

// refreshData fills the caches that pages and the policy report show from: the Settings values (the
// Settings tab's last-known values, #599), the values the policy rules check (not while the policy live
// check is off, policyReadsOff), and the GPIO template reading. The first part that fails ends the read:
// the device does not answer, and the retry reads all.
func (a *runtime) refreshData(ctx context.Context, d model.Device) error {
	// Through the router: over HTTP the check would refuse every device whose HTTP API is off (#676). The rest of
	// the refresh goes over the transport this check went over, so the check vouches for every read (verifyAndPin).
	ctx, err := a.svc.Prober.verifyAndPin(ctx, d)
	if err != nil { // fail closed: only the stored device fills these caches
		return err
	}
	if a.settings != nil {
		if _, err := a.settings.Read(ctx, d.ID); err != nil {
			return err
		}
	}
	if a.policyLive != nil && !a.policyReadsOff() && !a.policyLive.readDevice(ctx, d) && !policy.LimitedFirmware(d.Identity.Variant) {
		return laterStepError{errors.New("policy values not read")}
	}
	if a.templates != nil {
		if err := a.templates.Refresh(ctx, d); err != nil {
			return laterStepError{err}
		}
	}
	return nil
}

// policyReadsOff says whether the background reads of the policy values are switched off: paused on the
// policy page or the interval (PolicyLiveInterval) set to 0, the same test as the policy poller's
// (startPolicyLive).
func (a *runtime) policyReadsOff() bool {
	if a.livePause != nil && a.livePause.Paused() {
		return true
	}
	return a.svc != nil && a.svc.Sys != nil && a.svc.Sys.Snapshot().PolicyLiveInterval <= 0
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
	release, err := a.brokerReadGuard()(a.bgContext, id, backgroundProbe)
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
	case transport.ActJob: // never sent over MQTT: no path while the HTTP API is off
		return true
	}
	return false
}
