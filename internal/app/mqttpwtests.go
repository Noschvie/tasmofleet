package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	"github.com/hellwand/tasmofleet/internal/audit"
	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web"
)

// mqttPWBulk is what the fleet service needs of the store (*store.SQLite).
type mqttPWBulk interface {
	SetMQTTPasswordsWhereUnset(ctx context.Context, ids []model.DeviceID, mode model.MQTTPasswordMode) ([]model.DeviceID, error)
}

// pwTestJob is one queued test and the admin who asked for it.
type pwTestJob struct {
	id    model.DeviceID
	actor string
}

// mqttPasswordFleet is the fleet side of the device MQTT passwords (#609, web.MQTTPasswordFleet): the reasons a
// device has no password from a backup (the learner's), the bulk "use the broker password" (one conditional store
// write), and the password tests that follow it, one device at a time in the background.
//
// The tests are the only state it keeps, and it is runtime state only. Start: a confirmed bulk set with the tests
// ticked queues the devices it wrote. A set that meets a busy run joins its queue, so a set is never applied
// without its tests. End: the runner clears its flag in the same step in which it finds the queue empty, and a
// deferred reset covers a panic; the run also ends with the app's context (a shutdown), dropping what is queued.
// Nothing is persisted: after a restart the untested devices simply show "Not tested", and no notice outlives the
// process. A device that is offline, blocked or answers with an error stays "Not tested" and the run goes on; a
// device deleted meanwhile is skipped. Every test is bounded (transport.PasswordTestBound), and the result of a
// test is written by the router for the password that was tested, so a password changed during the run is never
// marked. Each test is one audit entry with the admin who started the run; nothing in it holds a password.
type mqttPasswordFleet struct {
	learner *mqttPasswordLearner
	st      mqttPWBulk
	tester  web.PasswordTester
	audit   audit.Recorder
	clk     clock.Clock
	log     *slog.Logger
	ctx     context.Context //nolint:containedctx // the app's context: a run ends with the app
	spawn   func(func())    // starts the runner; the app waits for it at shutdown

	mu      sync.Mutex
	queue   []pwTestJob
	running bool
	current bool // a test is in flight
}

var _ web.MQTTPasswordFleet = (*mqttPasswordFleet)(nil)

// Gaps asks the learner why each of ids has no password from a backup.
func (f *mqttPasswordFleet) Gaps(ctx context.Context, ids []model.DeviceID) map[model.DeviceID]web.PasswordGap {
	return f.learner.Gaps(ctx, ids)
}

// UseBroker sets broker mode on those of ids that are still not set and, with test, queues a test for each of
// them. Only the ids the store wrote are tested: a device that was learned or typed meanwhile is none of its business.
func (f *mqttPasswordFleet) UseBroker(ctx context.Context, actor string, ids []model.DeviceID, test bool) ([]model.DeviceID, error) {
	applied, err := f.st.SetMQTTPasswordsWhereUnset(ctx, ids, model.MQTTPasswordBroker)
	if err != nil {
		return nil, err
	}
	if test && len(applied) > 0 {
		f.enqueue(actor, applied)
	}
	return applied, nil
}

// Testing is the number of devices queued or in flight.
func (f *mqttPasswordFleet) Testing() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.queue)
	if f.current {
		n++
	}
	return n
}

func (f *mqttPasswordFleet) enqueue(actor string, ids []model.DeviceID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.queue = append(f.queue, pwTestJob{id: id, actor: actor})
	}
	if !f.running {
		f.running = true
		f.spawn(f.run)
	}
}

// run tests the queue until it is empty or the app ends. The emptiness check and the flag are one step under the
// lock, so a job that arrives while the runner leaves starts a new runner instead of waiting for nobody.
func (f *mqttPasswordFleet) run() {
	finished := false
	defer func() { // a panic must not leave the flag set: the next bulk set would queue for nobody
		if finished {
			return
		}
		f.mu.Lock()
		f.running, f.current, f.queue = false, false, nil
		f.mu.Unlock()
	}()
	for {
		f.mu.Lock()
		if len(f.queue) == 0 || f.ctx.Err() != nil {
			f.running, f.current, f.queue = false, false, nil
			finished = true
			f.mu.Unlock()
			return
		}
		job := f.queue[0]
		f.queue = f.queue[1:]
		f.current = true
		f.mu.Unlock()
		f.test(job)
		f.mu.Lock()
		f.current = false
		f.mu.Unlock()
	}
}

// test runs one device's test and audits its outcome (never the value).
func (f *mqttPasswordFleet) test(job pwTestJob) {
	ctx, cancel := context.WithTimeout(f.ctx, transport.PasswordTestBound)
	defer cancel()
	err := f.tester.TestMQTTPassword(ctx, job.id)
	if errors.Is(err, store.ErrNotFound) {
		return // deleted since it was queued
	}
	outcome := transport.PasswordTestOutcome(err)
	f.log.Info("device MQTT password test", "device", int64(job.id), "outcome", outcome, "run", "bulk")
	if f.audit == nil {
		return
	}
	b, jerr := json.Marshal(map[string]string{"result": outcome, "run": "bulk"})
	if jerr != nil {
		b = []byte("{}")
	}
	res := "ok"
	if err != nil {
		res = "failed"
	}
	f.audit.Record(context.WithoutCancel(f.ctx), model.AuditEvent{At: f.clk.Now(), Actor: job.actor, Action: "device.mqtt_password_test",
		Target: "device:" + strconv.FormatInt(int64(job.id), 10), Detail: string(b), Result: res})
}
