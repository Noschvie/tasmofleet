package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// DefaultFailFor is how long a failed HTTP call keeps the device's HTTP state "failing" (D4).
const DefaultFailFor = 10 * time.Minute

// ErrCutOff is returned for a write over MQTT that can cut the device off (a new topic, broker or login)
// while HTTP is not usable (failing, or not proven to answer), when no person confirmed it (WithCutOffConsent).
// Nothing was published.
var ErrCutOff = errors.New("not sent")

// DeviceGetter reads one inventory device.
type DeviceGetter interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// DeviceFacts gathers Facts from a device snapshot the caller holds, so the facts and the send
// look at the same device. HTTPState is the cheap part (memory and health, no store).
type DeviceFacts interface {
	FactsFor(ctx context.Context, d model.Device) (Facts, error)
	HTTPState(id model.DeviceID) HTTPState
}

// MQTTDoer is the command channel (mqttcmd.Channel) as the router uses it.
type MQTTDoer interface {
	Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
	DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
	Connected() bool
}

// BackupBlockedError is a backup that MQTT could not carry (no usable password, the password refused,
// a path that is not usable): Reason is a sentence for the person, written by this package and free of
// any secret or address. Err is what the caller would have seen without it (the HTTP failure), so
// errors.Is still finds tasmota.ErrUnreachable and the like.
type BackupBlockedError struct {
	Err    error
	Reason string
	// Attempted: something was published for this backup before it was refused (the opening abort, a
	// start that may have been sent, an Error 1 answer). Only a refusal decided before the first publish
	// has it false; the web layer then lets the person retry after a short floor instead of the cooldown.
	Attempted bool
}

func (e *BackupBlockedError) Error() string { return e.Err.Error() + " (" + e.Reason + ")" }
func (e *BackupBlockedError) Unwrap() error { return e.Err }

// UserReason is the sentence for the browser and the backup history, starting with a capital.
func (e *BackupBlockedError) UserReason() string {
	r := []rune(e.Reason)
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// MQTTDownloader is what the command channel offers for FileDownload (mqttcmd.Channel.Download).
// The channel is asserted to it, so a fake MQTTDoer without it simply has no backup over MQTT.
type MQTTDownloader interface {
	Download(ctx context.Context, d model.Device, password string) ([]byte, error)
}

// PasswordMarker records a refused MQTT password.
type PasswordMarker interface {
	MarkMQTTPasswordBad(ctx context.Context, id model.DeviceID, at time.Time) error
}

// Options configure the Router.
type Options struct {
	HTTP    tasmota.Client // required: the raw HTTP client
	Devices DeviceGetter   // required
	Facts   DeviceFacts    // set later with SetFacts: the facts need the health service
	Memory  *Memory        // optional; NewMemory otherwise
	// BrokerPassword is TasmoFleet's live MQTT_PASSWORD, sent by devices in broker mode (D6).
	BrokerPassword func() string
	// Marker records that a device refused its MQTT password (store.MarkMQTTPasswordBad).
	Marker PasswordMarker
	Log    *slog.Logger
}

// Router implements tasmota.Client over two transports (D1). Reads (#456) and writes (#605) of a device
// usable on our broker go over MQTT first; everything else, and those when MQTT cannot carry them, over
// HTTP, with MQTT carrying a call when HTTP is known to fail (Choose) or when the HTTP request provably
// never left (D4). Calls without a device (Target ID 0: scans, MAC verifiers) and passes of Uploader and
// BootSwitcher go to HTTP untouched.
type Router struct {
	o    Options
	http tasmota.Client // the raw client, recording what HTTP tells about the device
	mqtt atomic.Pointer[mqttHolder]
	// unanswered remembers (10 minutes) the devices that left a read or a write published over MQTT unanswered.
	unanswered *Memory
	// connChanged remembers (10 minutes, or until the device answers over MQTT) the devices whose MQTT connection
	// the router changed while HTTP answered, over either transport: HTTP carries their next commands
	// (MQTTFacts.ConnChangedSince).
	connChanged *Memory
}

type mqttHolder struct{ d MQTTDoer }

var (
	_ tasmota.Client       = (*Router)(nil)
	_ tasmota.Uploader     = (*Router)(nil)
	_ tasmota.BootSwitcher = (*Router)(nil)
)

// NewRouter returns a router that uses HTTP only until SetMQTT is called.
func NewRouter(o Options) *Router {
	if o.Memory == nil {
		o.Memory = NewMemory(DefaultFailFor, time.Now)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Router{o: o, http: &observer{c: o.HTTP, mem: o.Memory}, unanswered: NewMemory(DefaultFailFor, time.Now),
		connChanged: NewMemory(DefaultFailFor, time.Now)}
}

// SetMQTT attaches the command channel, once the broker connection exists. nil detaches it.
func (r *Router) SetMQTT(d MQTTDoer) {
	if d == nil {
		r.mqtt.Store(nil)
		return
	}
	r.mqtt.Store(&mqttHolder{d})
}

// SetFacts attaches the source of the facts. Until it is set (and MQTT attached) the router is the HTTP
// client it wraps. Call it before SetMQTT.
func (r *Router) SetFacts(f DeviceFacts) { r.o.Facts = f }

// Memory is the router's record of failing HTTP calls (app.HTTPMemory).
func (r *Router) Memory() *Memory { return r.o.Memory }

// UnansweredReads is the router's memory of devices that left a read over MQTT unanswered: the facts
// read it (MQTTFacts.ReadsUnansweredSince).
func (r *Router) UnansweredReads() *Memory { return r.unanswered }

// MQTTConnChanged is the router's memory of devices whose MQTT connection it just changed over MQTT while HTTP
// answered: the facts read it (MQTTFacts.ConnChangedSince). The failure kind it records means nothing.
func (r *Router) MQTTConnChanged() *Memory { return r.connChanged }

// HTTPOnly is the raw HTTP client with the router's memory attached: what it learns (a refused
// connection, an answer) is remembered, but it never routes to MQTT. The health poll, the prober
// and the discovery scan use it, because they measure or verify HTTP.
func (r *Router) HTTPOnly() tasmota.Client { return r.http }

func (r *Router) mqttDoer() MQTTDoer {
	if h := r.mqtt.Load(); h != nil && h.d.Connected() {
		return h.d
	}
	return nil
}

// ---- context

type ctxKey int

const (
	keyPath ctxKey = iota
	keyTrace
	keyLoose
	keyConsent
	keyRoute
)

// WithPath pins the transport of every call made with ctx (D5): a job that chose its path (OTA,
// restore) is never rerouted and never fails over. Only HTTP and MQTT can be pinned.
func WithPath(ctx context.Context, p Path) context.Context {
	if p != HTTP && p != MQTT {
		return ctx
	}
	return context.WithValue(ctx, keyPath, p)
}

// PathFrom returns the pinned path of ctx.
func PathFrom(ctx context.Context) (Path, bool) {
	p, ok := ctx.Value(keyPath).(Path)
	return p, ok
}

// WithConsole marks calls typed by a person (devicecmd.Run: the console and bulk commands): over MQTT a reply
// with other keys than the command's name (Template, Gpios) is accepted as the answer (mqttcmd DoLoose).
func WithConsole(ctx context.Context) context.Context { return context.WithValue(ctx, keyLoose, true) }

func typedByAPerson(ctx context.Context) bool { loose, _ := ctx.Value(keyLoose).(bool); return loose }

// WithCutOffConsent marks calls a person confirmed after being told that the device is reached over MQTT
// only and that the change can cut TasmoFleet off from it (the console's confirmation). Without it the
// router publishes no such write over MQTT unless HTTP answers the router's own Status 11 right before (an answer
// on record does not count): then a device that MQTT loses after the change is still reached over HTTP, so nothing
// asks (#605). Make compliant and bulk commands never carry it.
func WithCutOffConsent(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyConsent, true)
}

// HasCutOffConsent reports whether ctx carries WithCutOffConsent.
func HasCutOffConsent(ctx context.Context) bool { c, _ := ctx.Value(keyConsent).(bool); return c }

// ErrRouteChanged is returned, before anything is sent, for a call whose context names the transport its confirmation
// announced (WithRoute) when the router would take the other one (#826): the facts changed since the confirmation,
// or HTTP proved it sent nothing and the router would fail over to MQTT, or MQTT published nothing and the router
// would go on over HTTP. The caller asks the person again with what holds now.
var ErrRouteChanged = errors.New("not sent")

// WithRoute marks the calls made with ctx as announced to go over p (HTTP or MQTT): the console's confirmation or the
// bulk preview named that transport for a command whose arguments the audit hides (ConfirmsRoute, #826). The router
// then sends such a call only over p, never as a failover over the other transport; anything else is ErrRouteChanged
// with nothing sent. Any other p (None: TypedRoute said it goes nowhere; empty) lets nothing through: fail closed.
// A command the channel never publishes is refused for MQTT as the channel refuses it (publish), whatever p says.
func WithRoute(ctx context.Context, p Path) context.Context {
	return context.WithValue(ctx, keyRoute, p)
}

// RouteFrom returns the transport announced for the calls made with ctx (WithRoute).
func RouteFrom(ctx context.Context) (Path, bool) {
	p, ok := ctx.Value(keyRoute).(Path)
	return p, ok
}

// ConfirmsRoute reports a command whose confirmation names the transport it takes when a person types it (the
// console, a bulk command; #826): a change of the device whose arguments the audit log does not show in full
// (cmdpolicy.ShownInFull), because it may carry a secret. That secret crosses the network in plain text either way:
// over HTTP, or over the MQTT broker, which also delivers it to every client subscribed to the device's commands.
// A command that is never sent (malformed, a file transfer: cmdpolicy.ClassForbidden) takes no transport to name;
// it is refused at once.
func ConfirmsRoute(cmnd string) bool {
	if v, err := cmdpolicy.Classify(cmnd); err != nil || v.Class == cmdpolicy.ClassForbidden {
		return false
	}
	return classifyCommand(cmnd).write && !cmdpolicy.ShownInFull(cmnd)
}

// TypedRoute is the transport Command sends the change cmnd over now when a person typed it for device id: MQTT or
// HTTP, or None when it is sent nowhere. It takes the steps of Command with the same inputs (the cheap HTTP state, the
// device snapshot, its facts and Choose; writeOverMQTT and decide are the router's own), so the console's confirmation
// and the bulk preview name the route the send takes. A command that starts a job is HTTP. One the MQTT channel never
// publishes (mqttcmd.Refuses: it runs other commands, a file transfer) never crosses the broker: HTTP, or None while
// HTTP is known to fail and Command would try MQTT, which refuses it. With the route announced (WithRoute) the send
// keeps to it or sends nothing.
func (r *Router) TypedRoute(ctx context.Context, id model.DeviceID, cmnd string) Path {
	cls := classifyCommand(cmnd)
	if id == 0 || r.mqttDoer() == nil || r.o.Facts == nil || cls.job || !cls.write {
		return HTTP
	}
	refused := mqttcmd.Refuses(cmnd)
	if r.o.Facts.HTTPState(id).State != HTTPFailing {
		if _, _, mqtt := r.writeOverMQTT(ctx, id); mqtt && !refused {
			return MQTT
		}
		return HTTP // write goes on over HTTP after the channel refused
	}
	if _, _, p, err := r.decide(ctx, id); err == nil && p.Writes == MQTT {
		if refused {
			return None
		}
		return MQTT
	}
	return HTTP
}

// offRoute refuses a send over p when ctx announced the other transport (WithRoute, #826). Nothing was sent.
func offRoute(ctx context.Context, p Path) error {
	a, ok := RouteFrom(ctx)
	if !ok || a == p {
		return nil
	}
	noteTrace(ctx, None)
	return fmt.Errorf("%w: the way to the device changed since the confirmation (it would go over %s now, the confirmation said %s); confirm again",
		ErrRouteChanged, pathName(p), pathName(a))
}

func pathName(p Path) string {
	if p == MQTT || p == HTTP {
		return strings.ToUpper(string(p))
	}
	return "no transport"
}

// Trace records which transports carried the calls made under its context, for the audit "via", and which of
// them returned a reply (Answered, for the identity check that vouches for those replies, #810).
type Trace struct {
	parent     *Trace // a trace opened inside another one reports to it too
	mu         sync.Mutex
	http, mqtt bool
	refused    bool // a call was stopped before anything was sent
	// answered: a command over that transport got a reply from a device (a value, or an answer it could not
	// use); a call that timed out or was given up got none, although it counts for Via.
	answeredHTTP, answeredMQTT bool
}

// WithTrace returns a context whose calls are recorded in the returned Trace.
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	outer, _ := ctx.Value(keyTrace).(*Trace)
	t := &Trace{parent: outer}
	return context.WithValue(ctx, keyTrace, t), t
}

func (t *Trace) note(p Path) {
	if t == nil {
		return
	}
	t.parent.note(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	switch p {
	case MQTT:
		t.mqtt = true
	case None:
		t.refused = true
	case HTTP, DownloadHTTP, DownloadMQTT, PushHTTP, PushSafeboot, PushMQTT:
		t.http = true
	}
}

// onlyHTTP reports whether HTTP, and nothing else, carried the calls recorded so far.
func (t *Trace) onlyHTTP() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.http && !t.mqtt
}

// Via is the audit value of "via": "http", "mqtt", or "http+mqtt" when a job used both; "none" when
// every call was stopped before anything was sent. Nothing recorded at all (no router involved) reads
// as "http", the default path.
func (t *Trace) Via() string {
	if t == nil {
		return "http"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.http && t.mqtt:
		return "http+mqtt"
	case t.mqtt:
		return "mqtt"
	case t.http:
		return "http"
	case t.refused:
		return "none"
	}
	return "http"
}

func noteTrace(ctx context.Context, p Path) {
	if t, _ := ctx.Value(keyTrace).(*Trace); t != nil {
		t.note(p)
	}
}

// Answered lists the transports over which a command made under the trace got a reply, HTTP before MQTT: the
// transports the values of a read can have come from. Unlike Via it leaves out a call that was sent and got no
// answer (a timeout, a caller that gave up): an MQTT read nobody answered brought no value, and the read went on
// over HTTP. Empty: nothing was answered, or nothing was recorded (no router carried the calls). File transfers
// are not counted.
func (t *Trace) Answered() []Path {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Path
	if t.answeredHTTP {
		out = append(out, HTTP)
	}
	if t.answeredMQTT {
		out = append(out, MQTT)
	}
	return out
}

func (t *Trace) noteAnswer(mqtt bool) {
	if t == nil {
		return
	}
	t.parent.noteAnswer(mqtt)
	t.mu.Lock()
	defer t.mu.Unlock()
	if mqtt {
		t.answeredMQTT = true
	} else {
		t.answeredHTTP = true
	}
}

// noteAnswer records that a command sent over p (HTTP or MQTT) got a reply: anything but a timeout or a caller
// that gave up (an answer that could not be used is still an answer from whoever is there).
func noteAnswer(ctx context.Context, p Path, err error) {
	if err != nil && (errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return
	}
	if t, _ := ctx.Value(keyTrace).(*Trace); t != nil {
		t.noteAnswer(p == MQTT)
	}
}

// ---- classification

type cmdClass struct {
	write  bool // changes the device (everything but a read): follows Paths.Writes
	job    bool // starts a firmware upgrade or a restore
	redact bool // the reply may echo a credential
}

func classifyCommand(cmnd string) cmdClass {
	v, err := cmdpolicy.Classify(cmnd)
	// Routing takes "changes the device" from ReportsOnly, not from the role class: a bare MqttUser is admin-only
	// but only reports, and sent as a write it went over HTTP first and failed the Settings read (#702).
	c := cmdClass{write: err != nil || !cmdpolicy.ReportsOnly(cmnd), redact: v.Redact || err != nil}
	c.job = startsJob(cmnd)
	return c
}

// CommandAction is the action a free-form command needs, by the router's own classification (classifyCommand,
// which Command uses): ActJob for a command that starts a job, ActWrites for anything that changes the device
// (not cmdpolicy.ReportsOnly; a command the policy does not classify counts as a write), else ActCommands. The console and bulk gates use
// it, so they refuse what the router would not send over MQTT.
func CommandAction(cmnd string) Action {
	switch cls := classifyCommand(cmnd); {
	case cls.job:
		return ActJob
	case cls.write:
		return ActWrites
	}
	return ActCommands
}

// startsJob reports an Upgrade or WebGetConfig with an argument, alone or in a Backlog. The bare
// Upgrade is the probe the engine sends and changes nothing. OtaUrl is a setting and only
// prepares an upgrade.
func startsJob(cmnd string) bool {
	for _, part := range strings.Split(cmnd, ";") {
		part = strings.TrimSpace(part)
		for len(part) >= len("backlog") && strings.EqualFold(part[:len("backlog")], "backlog") {
			part = strings.TrimLeft(strings.TrimSpace(part[len("backlog"):]), "0123456789 ")
		}
		name, args, _ := strings.Cut(part, " ")
		base := strings.ToLower(strings.TrimRight(name, "0123456789"))
		if (base == "upgrade" || base == "webgetconfig") && strings.TrimSpace(args) != "" {
			return true
		}
	}
	return false
}

// ---- Command

// Command sends one command, over the transport the facts allow.
func (r *Router) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	id := t.DeviceID()
	if id == 0 {
		return r.http.Command(ctx, t, cmnd) // scans and address checks verify HTTP itself
	}
	cls := classifyCommand(cmnd)
	if pin, ok := PathFrom(ctx); ok {
		if pin == HTTP {
			return r.httpCommand(ctx, t, cmnd)
		}
		return r.pinnedMQTT(ctx, id, cmnd, cls)
	}
	if r.mqttDoer() == nil || cls.job {
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	if !cls.write {
		return r.read(ctx, t, cmnd, cls)
	}
	if r.o.Facts.HTTPState(id).State != HTTPFailing {
		return r.write(ctx, t, cmnd, cls)
	}
	// HTTP is known to fail: ask the facts which way works.
	d, f, p, err := r.decide(ctx, id)
	if err != nil {
		return r.httpFirst(ctx, t, cmnd, cls) // no judgement possible: the established path
	}
	if r.pathFor(p, cls) == MQTT {
		return r.sendMQTT(ctx, d, cmnd, cls)
	}
	raw, err := r.httpCommand(ctx, t, cmnd)
	return raw, r.explain(err, f, p, cls)
}

// read sends a read (Status N, settings reads, read-only console commands) MQTT first (#456) when the
// device is on our broker and live there (Paths.Commands), else over HTTP as before.
//
// A read changes nothing on the device, so HTTP may be asked once after MQTT in two cases: MQTT
// provably published nothing (notPublished: the channel refused the command, the bus was down, the
// device was offline by its LWT, the snapshot changed), or MQTT published and the device never
// answered (tasmota.ErrTimeout) while the caller still waits and HTTP is not known to fail. The second
// case marks the device (its replies are not where TasmoFleet listens), so its
// next reads go over HTTP for a while; an answer over MQTT clears the mark. Never back to MQTT within
// one call. An answer we cannot use (ErrBadResponse, a device error) is an answer and is returned.
func (r *Router) read(ctx context.Context, t tasmota.Target, cmnd string, cls cmdClass) (json.RawMessage, error) {
	id := t.DeviceID()
	if MQTTCannotMatch(cmnd) {
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	if d.MQTT != model.MQTTOurs || d.Topic == "" {
		return r.httpFirst(ctx, t, cmnd, cls) // not on our broker: no need to gather the inventory's facts
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	p := Choose(f)
	if p.Commands == None { // HTTP fails and MQTT is not usable: HTTP once with the reason, as before #456
		raw, err := r.httpCommand(ctx, t, cmnd)
		return raw, r.explain(err, f, p, cls)
	}
	if p.Commands != MQTT { // httpFirst explains its own failover; a second explain would repeat the reason
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	raw, err := r.sendMQTT(ctx, d, cmnd, cls)
	switch {
	case err == nil:
		r.unanswered.Clear(id)
		return raw, nil
	case notPublished(err):
		r.o.Log.Info("transport: MQTT did not publish the read, using HTTP", "device", int64(id))
		return r.httpCommand(ctx, t, cmnd)
	case errors.Is(err, tasmota.ErrTimeout) && ctx.Err() == nil && r.o.Facts.HTTPState(id).State != HTTPFailing:
		r.unanswered.Fail(id, HTTPFailUnreachable)
		r.o.Log.Info("transport: MQTT read got no answer, asking over HTTP once", "device", int64(id))
		return r.httpCommand(ctx, t, cmnd)
	}
	return raw, err
}

// write sends a write while HTTP is not known to fail: MQTT first (#605) when Choose says so (the device is
// usable on our broker under the strict rule and its MQTT reads are not going unanswered), else HTTP first
// (httpFirst, with its failover when HTTP provably sent nothing). HTTP is used only when MQTT cannot carry a write
// (the product owner's rule of 2026-11-14): what a write carries is no reason. A secret crosses the broker as it
// crosses the network to the device's HTTP API, in plain text on a usual Tasmota (docs/security/mqtt-transport.md);
// its reply is masked as before (sendMQTT). A change of the device's MQTT connection (cmdpolicy.CutOffName) is
// published without a question only while HTTP is usable, so that a device MQTT loses after the change is reached
// over HTTP for the next command: proven by one harmless HTTP question asked right before (httpAnswers), never by
// an answer on record (that is only the console's hint, app.reachedOnlyOverMQTT). Without that answer it needs the
// person's confirmation (WithCutOffConsent), else ErrCutOff and nothing is sent. Once such a change was published
// after that answer, or HTTP carried it (overHTTP), the device is marked (connChanged): Tasmota restarts after it with
// a clean disconnect, so its last will stays Online even when it cannot rejoin the broker, and HTTP carries its next
// commands while HTTP works (Choose), until it answers over MQTT or the mark expires. Make compliant follows the same
// rule (#606).
//
// A write over MQTT is sent over HTTP once only when MQTT provably published nothing (notPublished: the channel
// refused the command because it cannot carry it, such as one that runs other commands or a Backlog2; the bus was
// down; the device was offline by its LWT; the snapshot changed), never back to MQTT within the call. A write MQTT
// published and nobody confirmed (tasmota.ErrTimeout) may have run: it is an unknown outcome, returned as such and
// never sent again; while the caller still waits and HTTP is not known to fail, it marks the device like an
// unanswered read (unanswered), so its next commands go over HTTP for a while.
func (r *Router) write(ctx context.Context, t tasmota.Target, cmnd string, cls cmdClass) (json.RawMessage, error) {
	id := t.DeviceID()
	d, onBroker, mqtt := r.writeOverMQTT(ctx, id)
	if !mqtt {
		return r.overHTTP(ctx, id, onBroker, cmnd, func(ctx context.Context) (json.RawMessage, error) { return r.httpFirst(ctx, t, cmnd, cls) })
	}
	// Only an answer right now proves HTTP for a change of the MQTT connection: a fresh answer on record may predate
	// the person's own WebServer 0 or SetOption128 0 sent over MQTT a minute ago.
	usable := false
	if !HasCutOffConsent(ctx) && cmdpolicy.CutsMQTTOff(cmnd) {
		usable = r.httpAnswers(ctx, t)
	}
	raw, err := r.publish(ctx, d, cmnd, cls, usable)
	if usable && (err == nil || errors.Is(err, tasmota.ErrTimeout)) {
		r.markConnChanged(id) // published, answered or not: HTTP carries the next commands while it works
	}
	switch {
	case err != nil && notPublished(err):
		r.o.Log.Info("transport: MQTT did not publish the write, using HTTP", "device", int64(id))
		return r.overHTTP(ctx, id, true, cmnd, func(ctx context.Context) (json.RawMessage, error) { return r.httpCommand(ctx, t, cmnd) })
	case errors.Is(err, tasmota.ErrTimeout) && ctx.Err() == nil:
		// Never sent again (it may have run), but the device's replies are not where TasmoFleet listens, as after an
		// unanswered read: its next commands go over HTTP for a while, not into the void. HTTP is not failing here
		// (the facts said so above).
		r.unanswered.Fail(id, HTTPFailUnreachable)
		r.o.Log.Info("transport: MQTT write got no answer, the device's next commands go over HTTP for a while", "device", int64(id))
	}
	return raw, err
}

// writeOverMQTT is write's decision for a device whose HTTP is not known to fail: the device snapshot, whether it is
// on our broker, and whether the write goes MQTT first. Not on our broker needs no facts. Without facts no judgement
// is possible and the established path (HTTP first) is taken; the facts may also know of an HTTP failure the cheap
// check did not see, and httpFirst then decides as it always did. TypedRoute asks the same (#826).
func (r *Router) writeOverMQTT(ctx context.Context, id model.DeviceID) (d model.Device, onBroker, mqtt bool) {
	d, err := r.o.Devices.GetDevice(ctx, id)
	onBroker = err == nil && d.MQTT == model.MQTTOurs && d.Topic != ""
	if !onBroker {
		return d, false, false
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	return d, true, err == nil && Choose(f).Writes == MQTT && !httpFailing(f)
}

// overHTTP sends a write over HTTP (send) and marks the device (connChanged) when the write changes its MQTT
// connection, the device is on our broker and HTTP carried it (an answer, or a timeout: it may have run). The mark
// is set whichever transport carried such a change, because the device may not rejoin the broker after it either way;
// a change that reached the device only over MQTT (httpFirst's failover) or not at all sets none. Fail extends a mark
// that is already set, so a second corrective change keeps HTTP for another 10 minutes.
func (r *Router) overHTTP(ctx context.Context, id model.DeviceID, onBroker bool, cmnd string, send func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	if !onBroker || !cmdpolicy.CutsMQTTOff(cmnd) {
		return send(ctx)
	}
	tctx, tr := WithTrace(ctx)
	raw, err := send(tctx)
	if tr.onlyHTTP() && (err == nil || errors.Is(err, tasmota.ErrTimeout)) {
		r.markConnChanged(id)
	}
	return raw, err
}

// markConnChanged marks a device whose MQTT connection a write just changed: HTTP carries its next commands while
// HTTP works (the product owner's rule), until it answers over MQTT or 10 minutes pass.
func (r *Router) markConnChanged(id model.DeviceID) {
	r.connChanged.Fail(id, HTTPFailUnreachable)
	r.o.Log.Info("transport: the device's MQTT connection was changed, its next commands go over HTTP for a while", "device", int64(id))
}

// httpAnswers asks the device one harmless question over HTTP (Status 11, the health poll's) and reports whether it
// answered: the proof that HTTP is usable right now, which an answer on record is not. It goes through the observer,
// so the memory learns what HTTP showed, but not through httpCommand: it carries nothing of the write, and the trace
// (the audit's "via") names only the transport that carried the change.
func (r *Router) httpAnswers(ctx context.Context, t tasmota.Target) bool {
	_, err := r.http.Command(ctx, t, "Status 11")
	return err == nil
}

// MQTTCannotMatch reports reads whose replies the command channel does not match (State answers with
// its own keys, a bare Gpio only when GPIO1 is set, Gpios and Modules in several parts): they would
// time out over MQTT and wrongly mark the device, so they stay HTTP first. A caller that pins MQTT
// leaves them out (the data refresh, #676).
func MQTTCannotMatch(cmnd string) bool {
	name, args, _ := strings.Cut(strings.TrimSpace(cmnd), " ")
	switch strings.ToLower(name) {
	case "state", "gpios", "modules":
		return true
	case "gpio":
		return strings.TrimSpace(args) == ""
	}
	return false
}

// httpFirst sends over HTTP and fails over to MQTT only when the error proves that nothing was
// sent (D4): a refused connection, an address outside the device networks, a 401 or a device that
// refuses the HTTP API. A timeout, an unparsable answer or a connection dropped after the request
// went out prove nothing; the command may have run and is never sent a second time.
func (r *Router) httpFirst(ctx context.Context, t tasmota.Target, cmnd string, cls cmdClass) (json.RawMessage, error) {
	kind, _, had := r.o.Memory.HTTPFailure(t.DeviceID())
	raw, err := r.httpCommand(ctx, t, cmnd)
	if err == nil || !tasmota.NotSent(err) || cls.job || r.mqttDoer() == nil {
		return raw, err
	}
	// SetOption128 0 makes the handler send nothing, but a write that got that answer on its first try is
	// not proven unsent (the same silence can be a command that ran): only reads fail over, or a device
	// the memory already knew to refuse the API.
	if errors.Is(err, tasmota.ErrHTTPAPIRefused) && cls.write && (!had || kind != HTTPFailAPIRefused) {
		return nil, err
	}
	d, f, p, derr := r.decide(ctx, t.DeviceID())
	if derr != nil {
		return nil, err
	}
	if r.pathFor(p, cls) == MQTT {
		r.o.Log.Info("transport: HTTP did not send, using MQTT", "device", int64(d.ID))
		return r.sendMQTT(ctx, d, cmnd, cls)
	}
	return nil, r.explain(err, f, p, cls)
}

// pathFor picks reads or writes.
func (r *Router) pathFor(p Paths, cls cmdClass) Path {
	if cls.write {
		return p.Writes
	}
	return p.Commands
}

// explain adds the reason MQTT was not used to an HTTP error of a device that is on our broker.
func (r *Router) explain(err error, f Facts, p Paths, cls cmdClass) error {
	if err == nil || !f.MQTT.Ours || f.MQTT.Topic == "" {
		return err
	}
	act := ActCommands
	if cls.write {
		act = ActWrites
	}
	if why := p.Reason(act); why != "" && r.pathFor(p, cls) == None {
		r.o.Log.Debug("transport: not using MQTT", "topic", f.MQTT.Topic, "action", string(act), "why", why)
		return fmt.Errorf("%w (%s)", err, why)
	}
	return err
}

// decide reads the device once and derives facts and paths from that one snapshot.
func (r *Router) decide(ctx context.Context, id model.DeviceID) (model.Device, Facts, Paths, error) {
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return d, Facts{}, Paths{}, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return d, f, Paths{}, err
	}
	return d, f, Choose(f), nil
}

func (r *Router) httpCommand(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	if err := offRoute(ctx, HTTP); err != nil {
		return nil, err // announced over MQTT (#826): also MQTT's "published nothing" never moves it to HTTP
	}
	raw, err := r.http.Command(ctx, t, cmnd)
	if tasmota.NotSent(err) {
		noteTrace(ctx, None)
	} else {
		noteTrace(ctx, HTTP)
		noteAnswer(ctx, HTTP, err)
	}
	return raw, err
}

// pinnedMQTT is a call of a job that chose MQTT: no failover, no HTTP, and the same checks as a
// routed call. Writes need the strict conditions.
func (r *Router) pinnedMQTT(ctx context.Context, id model.DeviceID, cmnd string, cls cmdClass) (json.RawMessage, error) {
	if r.mqttDoer() == nil {
		return nil, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return nil, err
	}
	if why := MQTTBlocker(f, cls.write); why != "" {
		return nil, fmt.Errorf("%w: not usable over MQTT: %s", tasmota.ErrUnreachable, why)
	}
	return r.sendMQTT(ctx, d, cmnd, cls)
}

// sendMQTT publishes the command for the device snapshot d, for a caller that does not know HTTP to work: a
// change that can cut the device off from MQTT needs the person's consent (WithCutOffConsent).
func (r *Router) sendMQTT(ctx context.Context, d model.Device, cmnd string, cls cmdClass) (json.RawMessage, error) {
	return r.publish(ctx, d, cmnd, cls, false)
}

// publish publishes the command for the device snapshot d. httpWorks: HTTP answered the router's probe right before,
// so a change of the device's MQTT connection cannot lock it out and is published without the consent. The device is
// read once more right before the send: a topic or broker change since the snapshot (an edit, a refresh) must not
// send to the old topic. An answer to anything but such a change proves the device is on the broker and clears
// the mark of a recent one.
func (r *Router) publish(ctx context.Context, d model.Device, cmnd string, cls cmdClass, httpWorks bool) (json.RawMessage, error) {
	m := r.mqttDoer()
	if m == nil {
		return nil, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	// Announced over HTTP (#826): checked here, where every path publishes (routed, the failover after HTTP sent
	// nothing, pinned). A command the channel never publishes is refused as the channel would refuse it, so write
	// goes on over HTTP as it does for every such command; anything else would cross the broker unannounced.
	if a, ok := RouteFrom(ctx); ok && a != MQTT && mqttcmd.Refuses(cmnd) {
		noteTrace(ctx, None) // nothing sent; an HTTP send that follows (route HTTP) notes HTTP, which outranks this
		return nil, fmt.Errorf("%w: MQTT does not carry this command", mqttcmd.ErrRefused)
	}
	if err := offRoute(ctx, MQTT); err != nil {
		return nil, err
	}
	if cls.write && !httpWorks {
		if name := cmdpolicy.CutOffName(cmnd); name != "" {
			if !HasCutOffConsent(ctx) {
				noteTrace(ctx, None)
				return nil, fmt.Errorf("%w: this device is reached over MQTT only; changing %s can cut TasmoFleet off from it. Use the console, which asks to confirm", ErrCutOff, name)
			}
		}
	}
	now, err := r.o.Devices.GetDevice(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if mqttSettingsChanged(now, d) {
		return nil, fmt.Errorf("%w: the device's MQTT settings changed while the command was prepared; nothing was sent", tasmota.ErrUnreachable)
	}
	var raw json.RawMessage
	if typedByAPerson(ctx) {
		raw, err = m.DoLoose(ctx, d, cmnd)
	} else {
		raw, err = m.Do(ctx, d, cmnd)
	}
	if err == nil || !notPublished(err) {
		noteTrace(ctx, MQTT) // also a call nobody answered: for the audit an unanswered write may have run
		noteAnswer(ctx, MQTT, err)
	}
	if err != nil {
		return nil, err
	}
	if !cmdpolicy.CutsMQTTOff(cmnd) {
		r.connChanged.Clear(d.ID)
	}
	if cls.redact {
		raw = cmdpolicy.ScrubReply(raw) // the reply of a secret-setting command echoes the secret
	}
	return raw, nil
}

// notPublished: the channel refused the command or could not publish it.
func notPublished(err error) bool {
	return !errors.Is(err, tasmota.ErrTimeout) && (errors.Is(err, tasmota.ErrUnreachable) || errors.Is(err, mqttcmd.ErrRefused))
}

// NotPublished reports an error of the command channel that proves nothing was published (a refusal, a bus
// that was down, a device offline by its LWT): the same command may then be sent over HTTP once. A timeout is
// an unknown outcome and never counts. The power toggle, which calls the channel itself, uses it.
func NotPublished(err error) bool { return err != nil && notPublished(err) }

// ---- the rest of tasmota.Client

func mqttSettingsChanged(now, d model.Device) bool {
	return now.Topic != d.Topic || now.GroupTopic != d.GroupTopic || now.FullTopic != d.FullTopic || now.MQTT != d.MQTT
}

// DownloadConfig reads the settings dump over HTTP (/dl), or over MQTT (FileDownload, #257) when
// HTTP is known to fail, the device is usable over MQTT and its MqttPassword is set and not refused
// (Choose: Paths.Backup). A job that pinned its path is never rerouted. A transfer that started over
// MQTT never falls back to HTTP: the caller sees its error.
func (r *Router) DownloadConfig(ctx context.Context, t tasmota.Target) ([]byte, error) {
	id := t.DeviceID()
	if id == 0 {
		return r.http.DownloadConfig(ctx, t)
	}
	if pin, ok := PathFrom(ctx); ok {
		if pin == HTTP {
			return r.httpDownload(ctx, t)
		}
		return r.pinnedDownload(ctx, id)
	}
	if r.mqttDoer() == nil || r.o.Facts.HTTPState(id).State != HTTPFailing {
		raw, err := r.httpDownload(ctx, t)
		if err == nil || !tasmota.NotSent(err) || r.mqttDoer() == nil {
			return raw, err
		}
		// HTTP proved that nothing was sent: the same chance for MQTT as a command gets (D4).
		d, f, p, derr := r.decide(ctx, id)
		if derr != nil {
			return nil, err
		}
		if p.Backup == MQTT {
			r.o.Log.Info("transport: HTTP did not send, backing up over MQTT", "device", int64(d.ID))
			return r.mqttDownload(ctx, d)
		}
		return nil, r.explainBackup(err, f, p)
	}
	d, f, p, err := r.decide(ctx, id)
	if err != nil {
		return r.httpDownload(ctx, t)
	}
	if p.Backup == MQTT {
		return r.mqttDownload(ctx, d)
	}
	raw, herr := r.httpDownload(ctx, t)
	return raw, r.explainBackup(herr, f, p)
}

func (r *Router) httpDownload(ctx context.Context, t tasmota.Target) ([]byte, error) {
	b, err := r.http.DownloadConfig(ctx, t)
	if tasmota.NotSent(err) {
		noteTrace(ctx, None)
	} else {
		noteTrace(ctx, HTTP)
	}
	return b, err
}

// explainBackup adds why a backup over MQTT was not possible to the HTTP error of a device on our broker.
func (r *Router) explainBackup(err error, f Facts, p Paths) error {
	if err == nil || !f.MQTT.Ours || f.MQTT.Topic == "" {
		return err
	}
	if why := p.Reason(ActBackup); why != "" && p.Backup == None {
		r.o.Log.Debug("transport: not using MQTT", "topic", f.MQTT.Topic, "action", string(ActBackup), "why", why)
		return &BackupBlockedError{Err: err, Reason: why}
	}
	return err
}

// pinnedDownload is the backup of a job that chose MQTT: same checks as the chosen path, no HTTP.
func (r *Router) pinnedDownload(ctx context.Context, id model.DeviceID) ([]byte, error) {
	if r.mqttDoer() == nil {
		return nil, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return nil, err
	}
	if why := transferBlocker(f, "back up"); why != "" {
		return nil, &BackupBlockedError{Err: fmt.Errorf("%w: a backup over MQTT is not possible", tasmota.ErrUnreachable), Reason: why}
	}
	return r.mqttDownload(ctx, d)
}

// mqttDownload runs FileDownload for the device snapshot d. The device is read once more right before
// the send: a topic, broker or password change since the snapshot must not send to the old topic or
// with the old password, and the password comes from that fresh read. Nothing here is logged or
// returned that could contain the password.
func (r *Router) mqttDownload(ctx context.Context, d model.Device) ([]byte, error) {
	m := r.mqttDoer()
	dl, ok := m.(MQTTDownloader)
	if m == nil || !ok {
		return nil, fmt.Errorf("%w: a backup over MQTT is not available", tasmota.ErrNotImplemented)
	}
	now, err := r.o.Devices.GetDevice(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if mqttSettingsChanged(now, d) {
		return nil, fmt.Errorf("%w: the device's MQTT settings changed while the backup was prepared; nothing was sent", tasmota.ErrUnreachable)
	}
	pw, why := r.downloadPassword(now)
	if why != "" {
		noteTrace(ctx, None)
		return nil, &BackupBlockedError{Err: fmt.Errorf("%w: a backup over MQTT is not possible", tasmota.ErrUnreachable), Reason: "a backup over MQTT is not possible: " + why}
	}
	data, err := dl.Download(ctx, now, pw)
	if err == nil || !notPublished(err) {
		noteTrace(ctx, MQTT)
	}
	if errors.Is(err, mqttcmd.ErrRefused) { // nothing was sent: say why, in our own words, never the error text
		return nil, &BackupBlockedError{Err: fmt.Errorf("%w: %w", tasmota.ErrUnreachable, mqttcmd.ErrRefused), Reason: "a backup over MQTT was refused: " + mqttcmd.Reason(err), Attempted: errors.Is(err, mqttcmd.ErrPublished)}
	}
	if errors.Is(err, mqttcmd.ErrPasswordRefused) {
		if r.o.Marker != nil {
			// The caller may be cancelled by now; the mark must still be written.
			if merr := r.o.Marker.MarkMQTTPasswordBad(context.WithoutCancel(ctx), now.ID, time.Now()); merr != nil {
				r.o.Log.Warn("transport: could not record the refused MQTT password", "device", int64(now.ID), "err", merr)
			}
		}
		return nil, &BackupBlockedError{Err: err, Reason: "the device refused its MQTT password; enter the right one on the device's Edit page to back up over MQTT", Attempted: true}
	}
	return data, err
}

// downloadPassword resolves the device's MqttPassword per mode (D6): its own sealed value, or TasmoFleet's
// live broker password. why is set when there is none to send or the device refused it before.
func (r *Router) downloadPassword(d model.Device) (pw, why string) {
	switch {
	case !d.MQTTPasswordBadAt.IsZero():
		return "", "the device refused its MQTT password; enter the right one"
	case d.MQTTPasswordMode == model.MQTTPasswordOwn && d.MQTTPassword != "":
		return d.MQTTPassword, ""
	case d.MQTTPasswordMode == model.MQTTPasswordBroker:
		if r.o.BrokerPassword != nil {
			if bp := r.o.BrokerPassword(); bp != "" {
				return bp, ""
			}
		}
	}
	return "", "the device has no usable MQTT password"
}

// UploadFirmware passes straight to HTTP: an upload never fails over (D4).
func (r *Router) UploadFirmware(ctx context.Context, t tasmota.Target, filename string, size int64, body io.Reader) error {
	u, ok := r.o.HTTP.(tasmota.Uploader)
	if !ok {
		return fmt.Errorf("%w: the HTTP client cannot upload", tasmota.ErrUploadUnsupported)
	}
	return u.UploadFirmware(ctx, t, filename, size, body)
}

// ReadUpLayout passes straight to HTTP.
func (r *Router) ReadUpLayout(ctx context.Context, t tasmota.Target) (tasmota.UpLayout, error) {
	s, ok := r.o.HTTP.(tasmota.BootSwitcher)
	if !ok {
		return "", fmt.Errorf("%w: the HTTP client cannot read the layout", tasmota.ErrNotImplemented)
	}
	return s.ReadUpLayout(ctx, t)
}

// SwitchBoot passes straight to HTTP.
func (r *Router) SwitchBoot(ctx context.Context, t tasmota.Target, to tasmota.BootTarget) (tasmota.BootReply, error) {
	s, ok := r.o.HTTP.(tasmota.BootSwitcher)
	if !ok {
		return "", fmt.Errorf("%w: the HTTP client cannot switch the boot partition", tasmota.ErrNotImplemented)
	}
	return s.SwitchBoot(ctx, t, to)
}
