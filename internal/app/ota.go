package app

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/ota"
)

// jobNotifier turns persisted job transitions into SSE events and metrics.
type jobNotifier struct{ rt *runtime }

func (n jobNotifier) JobChanged(j model.Job, _ model.JobEvent) {
	n.rt.svc.Hub.Publish(events.Event{Topic: events.TopicJob, Payload: j})
	n.rt.learnFWReachFromJob(j)
	if j.State.Terminal() {
		n.rt.svc.Std.OTAJobsTotal.With(string(j.State)).Inc()
	}
}

// learnFWReachFromJob: a download update that succeeded proves the device reaches the firmware
// server (D11). Push jobs, policy jobs and the MQTT upload never download from it.
func (a *runtime) learnFWReachFromJob(j model.Job) {
	if j.State != model.JobSucceeded || j.Kind != "" || j.Delivery != model.DeliveryPull || j.MQTTUpload {
		return
	}
	id := j.DeviceID
	a.learnFWReach(func(ctx context.Context, l *FWReachLearner) error {
		return l.NoteYes(ctx, id, FWReachSourceOTADownload)
	})
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
		ota.Options{Machine: cfgM, MaxParallel: a.cfg.OTAParallel, Power: a.deviceCmd, Audit: a.svc.Audit, Policy: a.policyApplier(),
			Commands: a.deviceCmd, Principals: principalResolver{local: a.svc.Store, dir: a.svc.Store}},
		a.svc.Store, a.svc.Store, ota.NewDeviceAPI(a.svc.Client), a.cache, a.backups, clock.Real{}, jobNotifier{rt: a},
	)
	a.goBG("ota", func() error { return a.engine.Run(a.bgContext) })
	if a.bus != nil {
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindUpgrade}, a.onUpgradeReport)
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindPower}, a.onPowerReport)
	}
}

// onUpgradeReport forwards stat/<topic>/UPGRADE to the engine as a hint
// ("Failed ..." ends the wait early). It runs on the bus goroutine, so the
// device lookup happens in its own goroutine.
func (a *runtime) onUpgradeReport(m mqttbus.Message) {
	text, ok := upgradeReportText(m.Payload)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.bgContext, 5*time.Second)
		defer cancel()
		d, err := a.svc.Store.GetDeviceByTopic(ctx, m.Topic)
		if err != nil {
			return // not in the inventory: nothing to hint
		}
		a.engine.Hint(d.ID, text)
	}()
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
func (a *runtime) onPowerReport(m mqttbus.Message) {
	if m.Retained {
		return
	}
	relay, ok := powerRelay(m.Suffix)
	if !ok || strings.ToUpper(strings.TrimSpace(string(m.Payload))) != "OFF" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.bgContext, 5*time.Second)
		defer cancel()
		d, err := a.svc.Store.GetDeviceByTopic(ctx, m.Topic)
		if err != nil {
			return
		}
		a.engine.PowerReport(d.ID, relay, false)
	}()
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
	return n, err == nil && n >= 1
}
