package app

import (
	"context"
	"encoding/json"
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
		h := a.startOTAHints(a.svc.Store.GetDeviceByTopic, a.engine)
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindUpgrade}, h.onUpgradeReport)
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindPower}, h.onPowerReport)
	}
}

// otaHintQueue bounds the UPGRADE hints that wait for their device lookup.
const otaHintQueue = 256

// hintTarget is what the MQTT reports are handed to: the OTA engine.
type hintTarget interface {
	Hint(id model.DeviceID, report string)
	PowerReport(id model.DeviceID, relay int, on bool)
}

// otaHint is one MQTT report waiting for its device lookup: relay 0 is an UPGRADE report
// with text, relay >= 1 is that relay switched OFF.
type otaHint struct {
	topic string
	text  string
	relay int
}

// offKey identifies a relay whose OFF report is waiting.
type offKey struct {
	topic string
	relay int
}

// otaHints carries the UPGRADE and POWER OFF reports from the bus goroutine to the engine
// (#507). The bus handler must not block, and the device is found by topic in the database,
// so the reports wait for one worker that looks them up in arrival order.
//
// The two reports are not equally expendable (#534). An UPGRADE report only ends a wait
// early (the job still stops at its deadline), so the UPGRADE reports wait in a bounded
// queue and one that does not fit is dropped and counted. A POWER OFF report is how the
// engine learns that somebody switched a relay off during an update; without it the power
// restore switches that relay back ON. It is therefore never dropped: a relay's OFF report
// that is already waiting stands for every later one (the state is the same), so the OFF
// reports are coalesced per topic and relay instead of queued. They are bounded by the
// devices times their relays, not by the number of reports.
type otaHints struct {
	byTopic func(ctx context.Context, topic string) (model.Device, error)
	target  hintTarget
	dropped func()
	wake    chan struct{} // capacity 1: the worker has something to take

	mu       sync.Mutex
	pending  []otaHint           // arrival order
	offs     map[offKey]struct{} // the OFF reports in pending
	upgrades int                 // the UPGRADE reports in pending
}

func newOTAHints(byTopic func(context.Context, string) (model.Device, error), target hintTarget, dropped func()) *otaHints {
	return &otaHints{byTopic: byTopic, target: target, dropped: dropped, wake: make(chan struct{}, 1), offs: map[offKey]struct{}{}}
}

// startOTAHints starts the worker as a background task, so that shutdown waits for it.
func (a *runtime) startOTAHints(byTopic func(context.Context, string) (model.Device, error), target hintTarget) *otaHints {
	h := newOTAHints(byTopic, target, func() { a.svc.Std.MQTTDropped.With("ota_hint_queue_full").Inc() })
	a.goBG("ota-hints", func() error { h.run(a.bgContext); return nil })
	return h
}

// offer queues a report, or coalesces (OFF) or drops (UPGRADE) it as described on otaHints.
func (h *otaHints) offer(m otaHint) {
	h.mu.Lock()
	if m.relay > 0 {
		k := offKey{m.topic, m.relay}
		if _, waiting := h.offs[k]; waiting {
			h.mu.Unlock()
			return
		}
		h.offs[k] = struct{}{}
	} else {
		if h.upgrades >= otaHintQueue {
			h.mu.Unlock()
			h.dropped()
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
		delete(h.offs, offKey{m.topic, m.relay}) // a report that comes now is a new one
	} else {
		h.upgrades--
	}
	return m, true
}

// run delivers the queued reports one at a time until ctx ends. A lookup in flight is finished
// (it is bounded by its own timeout) before run returns.
func (h *otaHints) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		}
		for ctx.Err() == nil {
			m, ok := h.next()
			if !ok {
				break
			}
			h.deliver(ctx, m)
		}
	}
}

func (h *otaHints) deliver(ctx context.Context, m otaHint) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d, err := h.byTopic(ctx, m.topic)
	if err != nil {
		return // not in the inventory: nothing to hint
	}
	if m.relay > 0 {
		h.target.PowerReport(d.ID, m.relay, false)
		return
	}
	h.target.Hint(d.ID, m.text)
}

// onUpgradeReport forwards stat/<topic>/UPGRADE to the engine as a hint
// ("Failed ..." ends the wait early). It runs on the bus goroutine and only queues.
func (h *otaHints) onUpgradeReport(m mqttbus.Message) {
	text, ok := upgradeReportText(m.Payload)
	if !ok {
		return
	}
	h.offer(otaHint{topic: m.Topic, text: text})
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
	relay, ok := powerRelay(m.Suffix)
	if !ok || strings.ToUpper(strings.TrimSpace(string(m.Payload))) != "OFF" {
		return
	}
	h.offer(otaHint{topic: m.Topic, relay: relay})
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
