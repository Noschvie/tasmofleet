package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// jobNotifier turns persisted job transitions into SSE events and metrics.
type jobNotifier struct{ rt *runtime }

func (n jobNotifier) JobChanged(j model.Job, ev model.JobEvent) {
	n.rt.svc.Hub.Publish(events.Event{Topic: events.TopicJob, Payload: j})
	n.rt.learnFWReachFromJob(j)
	n.rt.learnFWReachNoFromJob(j, ev)
	if j.State.Terminal() {
		n.rt.svc.Std.OTAJobsTotal.With(string(j.State)).Inc()
		if n.rt.notifier != nil {
			n.rt.bgWG.Go(func() { n.rt.notifyBatchDone(j) }) // a database read: not on the engine's thread; shutdown waits for it
		}
	}
}

// learnFWReachFromJob: a download update that succeeded proves the device reaches the firmware
// server (D11). Push jobs, policy jobs and the MQTT upload never download from it; a skipped job (already on
// the target, no plan) ends succeeded without any download.
func (a *runtime) learnFWReachFromJob(j model.Job) {
	if j.State != model.JobSucceeded || j.Kind != "" || j.Plan == "" || j.Delivery != model.DeliveryPull || j.MQTTUpload {
		return
	}
	id := j.DeviceID
	a.learnFWReach(func(ctx context.Context, l *FWReachLearner) error {
		return l.NoteYes(ctx, id, FWReachSourceOTADownload)
	})
}

// learnFWReachNoFromJob: the device told us over MQTT that it could not connect to the firmware server
// and the job ended on that report (D9, D11). The machine fails such a job without a second Upgrade;
// the detail is the device's own text.
func (a *runtime) learnFWReachNoFromJob(j model.Job, ev model.JobEvent) {
	if j.State != model.JobFailed || j.Kind != "" || ev.Event != string(ota.EvUpgradeReport) || !ota.FWUnreachableReport(ev.Detail) {
		return
	}
	id, detail := j.DeviceID, ev.Detail
	a.learnFWReach(func(ctx context.Context, l *FWReachLearner) error {
		return l.NoteFWReachNo(ctx, id, detail)
	})
}

// updatePaths implements ota.UpdatePaths: transport.Choose over the facts of the device and the plan.
type updatePaths struct{ facts *TransportFacts }

func (u updatePaths) UpdatePath(ctx context.Context, d model.Device, q ota.PathQuery) (transport.Paths, error) {
	f, err := u.facts.ForUpdate(ctx, d.ID, UpdateRequest{
		Delivery: q.Delivery, ImageSize: q.ImageSize, UpLayout: q.Layout, HTTPPush: q.HTTPPush, HTTPPushNote: q.HTTPPushNote,
	})
	if err != nil {
		return transport.Paths{}, err
	}
	return transport.Choose(f), nil
}

// setupOTA builds the OTA engine. It needs the firmware cache, which exists
// only with FIRMWARE_BASE_URL; without it OTA is disabled and the UI hides it.
func (a *runtime) setupOTA() {
	if a.cache == nil {
		return
	}
	cfgM := ota.DefaultConfig()
	cfgM.RestoreOtaURL = a.cfg.OTARestoreURL
	cfgM.PowerRestoreMaxAge = a.cfg.OTAPowerMaxAge
	a.engine = ota.NewEngine(
		ota.Options{Machine: cfgM, MaxParallel: a.cfg.OTAParallel, Power: a.deviceCmd, Audit: a.svc.Audit, Policy: a.policyApplier(), Templates: a.templateApplier(),
			Commands: a.deviceCmd, Principals: principalResolver{local: a.svc.Store, dir: a.svc.Store}, Paths: a.updatePaths(),
			HoldFlashing: a.holdFlashing},
		a.svc.Store, a.svc.Store, ota.NewDeviceAPI(a.svc.Client), a.cache, a.backups, clock.Real{}, jobNotifier{rt: a},
	)
	if a.templates != nil {
		a.templates.SetEngine(a.engine)
	}
	a.goBG("ota", func() error { return a.engine.Run(a.bgContext) })
	if a.bus != nil {
		h := a.startOTAHints(a.svc.Store.ListDevices, a.engine)
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindUpgrade}, h.onUpgradeReport)
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindPower}, h.onPowerReport)
	}
}

// otaHintQueue bounds the UPGRADE hints that wait for the worker.
const otaHintQueue = 256

// otaTopicRefresh is how often the topics of the inventory are read again.
const otaTopicRefresh = 15 * time.Second

// The reasons a report is not passed on (label of tasmofleet_mqtt_dropped_total).
const (
	hintDropQueueFull    = "ota_hint_queue_full"    // an UPGRADE hint found the queue full
	hintDropUnknownTopic = "ota_hint_unknown_topic" // the topic is no device of the inventory
	hintDropInvalidRelay = "ota_hint_invalid_relay" // POWER0, POWER33, POWERx: no relay the engine knows
)

// hintTarget is what the MQTT reports are handed to: the OTA engine. at is when the broker
// delivered the report.
type hintTarget interface {
	Hint(id model.DeviceID, report string)
	PowerReport(id model.DeviceID, relay int, on bool, at time.Time)
}

// otaHint is one MQTT report of a known device waiting for the worker: relay 0 is an UPGRADE
// report with text, relay >= 1 is that relay switched OFF, received at at.
type otaHint struct {
	id    model.DeviceID
	text  string
	relay int
	at    time.Time
}

// offKey identifies a relay whose OFF report is waiting.
type offKey struct {
	id    model.DeviceID
	relay int
}

// otaHints carries the UPGRADE and POWER OFF reports from the bus goroutine to the engine
// (#507). The bus handler must not block, so the reports wait for one worker that hands them
// over in arrival order.
//
// Only reports of devices in the inventory are taken in (#534): the topic of a report is
// whatever a client of the broker publishes, so anything that waits for the worker must be
// bounded by what the inventory holds, not by what the broker carries, and junk must not be
// able to push out a real device's report. The topics of the inventory, with their device IDs,
// are read at start and again every otaTopicRefresh, and a report is matched against that map
// when it arrives: no report depends on a database read that could fail or time out. A report of
// any other topic is dropped and counted.
//
// The two reports are not equally expendable. An UPGRADE report only ends a wait early (the
// job still stops at its deadline), so the UPGRADE reports wait in a bounded queue and one
// that does not fit is dropped and counted. A POWER OFF report is how the engine learns that
// somebody switched a relay off during an update; without it the power restore switches that
// relay back ON. It is therefore never dropped: a relay's OFF report that is already waiting
// stands for every later one (the state is the same), so the OFF reports are coalesced per
// device and relay instead of queued. They are bounded by the devices times their relays
// (model.MaxRelays), not by the number of reports. Everything here lives in memory: a restart
// loses the reports that are waiting.
type otaHints struct {
	list    func(ctx context.Context) ([]model.Device, error) // the inventory; nil: devices are set by hand
	target  hintTarget
	dropped func(reason string)
	wake    chan struct{} // capacity 1: the worker has something to take
	every   time.Duration // how often run reads the inventory again

	mu       sync.Mutex
	ids      map[string]model.DeviceID // the topics of the inventory
	pending  []otaHint                 // arrival order
	offs     map[offKey]struct{}       // the OFF reports in pending
	upgrades int                       // the UPGRADE reports in pending
}

func newOTAHints(target hintTarget, dropped func(reason string)) *otaHints {
	return &otaHints{target: target, dropped: dropped, wake: make(chan struct{}, 1), every: otaTopicRefresh, ids: map[string]model.DeviceID{}, offs: map[offKey]struct{}{}}
}

// startOTAHints reads the inventory and starts the worker as a background task, so that shutdown
// waits for it. It returns with the topics known, so that the first report the bus delivers is
// matched against them.
func (a *runtime) startOTAHints(list func(context.Context) ([]model.Device, error), target hintTarget) *otaHints {
	h := newOTAHints(target, func(reason string) { a.svc.Std.MQTTDropped.With(reason).Inc() })
	h.list = list
	h.refreshTopics(a.bgContext)
	a.goBG("ota-hints", func() error { h.run(a.bgContext); return nil })
	return h
}

// setDevices replaces the devices whose reports are taken in. A device without a topic never matches.
func (h *otaHints) setDevices(devs ...model.Device) {
	ids := make(map[string]model.DeviceID, len(devs))
	for _, d := range devs {
		if d.Topic != "" {
			ids[d.Topic] = d.ID
		}
	}
	h.mu.Lock()
	h.ids = ids
	h.mu.Unlock()
}

// refreshTopics reads the inventory again. When that fails the devices stay as they were.
func (h *otaHints) refreshTopics(ctx context.Context) {
	if h.list == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	devs, err := h.list(ctx)
	if err != nil {
		slog.Warn("ota hints: could not read the device topics, keeping the known ones", "err", err)
		return
	}
	h.setDevices(devs...)
}

// offer queues the report of the device that publishes under topic, or coalesces (OFF) or drops
// (UPGRADE, unknown topic) it as described on otaHints.
func (h *otaHints) offer(topic string, m otaHint) {
	h.mu.Lock()
	id, known := h.ids[topic]
	if !known {
		h.mu.Unlock()
		h.dropped(hintDropUnknownTopic)
		return
	}
	m.id = id
	if m.relay > 0 {
		k := offKey{id, m.relay}
		if _, waiting := h.offs[k]; waiting {
			h.mu.Unlock()
			return
		}
		h.offs[k] = struct{}{}
	} else {
		if h.upgrades >= otaHintQueue {
			h.mu.Unlock()
			h.dropped(hintDropQueueFull)
			return
		}
		h.upgrades++
	}
	h.pending = append(h.pending, m)
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default: // the worker is already due
	}
}

// next takes the oldest waiting report.
func (h *otaHints) next() (otaHint, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) == 0 {
		return otaHint{}, false
	}
	m := h.pending[0]
	h.pending[0] = otaHint{}
	h.pending = h.pending[1:]
	if m.relay > 0 {
		delete(h.offs, offKey{m.id, m.relay}) // a report that comes now is a new one
	} else {
		h.upgrades--
	}
	return m, true
}

// run hands the queued reports to the engine one at a time until ctx ends. A report in flight
// is finished before run returns.
func (h *otaHints) run(ctx context.Context) {
	refresh := time.NewTicker(h.every)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			h.refreshTopics(ctx)
		case <-h.wake:
		}
		for ctx.Err() == nil {
			m, ok := h.next()
			if !ok {
				break
			}
			h.deliver(m)
		}
	}
}

func (h *otaHints) deliver(m otaHint) {
	if m.relay > 0 {
		h.target.PowerReport(m.id, m.relay, false, m.at)
		return
	}
	h.target.Hint(m.id, m.text)
}

// onUpgradeReport forwards stat/<topic>/UPGRADE to the engine as a hint
// ("Failed ..." ends the wait early). It runs on the bus goroutine and only queues.
func (h *otaHints) onUpgradeReport(m mqttbus.Message) {
	text, ok := upgradeReportText(m.Payload)
	if !ok {
		return
	}
	h.offer(m.Topic, otaHint{text: text})
}

// upgradeReportText extracts the text of {"Upgrade":"..."}.
func upgradeReportText(payload []byte) (string, bool) {
	var v struct {
		Upgrade *string `json:"Upgrade"`
	}
	if err := json.Unmarshal(payload, &v); err != nil || v.Upgrade == nil {
		return "", false
	}
	return *v.Upgrade, true
}

// onPowerReport forwards non-retained stat/<topic>/POWER[n] to the engine so
// that a relay somebody switches off during an update is not switched back on
// by the power restore. The engine ignores ON reports and devices without a job.
func (h *otaHints) onPowerReport(m mqttbus.Message) {
	if m.Retained {
		return
	}
	if strings.ToUpper(strings.TrimSpace(string(m.Payload))) != "OFF" {
		return
	}
	relay, ok := powerRelay(m.Suffix)
	if !ok {
		h.dropped(hintDropInvalidRelay)
		return
	}
	h.offer(m.Topic, otaHint{relay: relay, at: m.At})
}

// powerRelay maps the topic suffix POWER (relay 1) or POWERn to n.
func powerRelay(suffix string) (int, bool) {
	rest, ok := strings.CutPrefix(suffix, "POWER")
	if !ok {
		return 0, false
	}
	if rest == "" {
		return 1, true
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 1 && n <= model.MaxRelays // the engine ignores any other relay
}

// updatePaths is the chooser of the engine; nil (HTTP for every update) while the transport facts do not exist.
func (a *runtime) updatePaths() ota.UpdatePaths {
	if a.facts == nil {
		return nil
	}
	return updatePaths{facts: a.facts}
}
