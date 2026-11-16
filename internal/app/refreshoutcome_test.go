package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// dataRig is a reader whose data read answers what the test says and whose other reads are not due.
func dataRig(t *testing.T, ids ...model.DeviceID) (*brokerRig, *error) {
	t.Helper()
	g := newBrokerRig(ids...)
	var dataErr error
	g.r.readData = func(context.Context, model.Device) error { return dataErr }
	for _, id := range ids {
		g.r.next[id] = g.clock.Add(100 * time.Hour) // only the data read is due
	}
	return g, &dataErr
}

func (g *brokerRig) outcome(id model.DeviceID) (settings.FailReason, time.Time, time.Time, bool) {
	f, ok := g.r.outcomes.Failure(id)
	return f.Reason, f.At, f.Next, ok
}

// Why (#818): the reader drops the error of a failed background refresh once it has scheduled the retry, so the
// device page could only say how old the data is. Each cause that a person can act on must come out as its own reason,
// with the time of the failure and the very time the reader will try again (not a copy of the retry rule, which would
// drift from it).
func TestAFailedDataRefreshKeepsItsReasonAndTheNextTry(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want settings.FailReason
		next func(g *brokerRig) time.Time
	}{
		{"no answer", fmt.Errorf("read: %w", tasmota.ErrTimeout), settings.FailTimeout, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostRetry) }},
		{"device offline", fmt.Errorf("mqtt: %w: %w", mqttcmd.ErrWentOffline, tasmota.ErrTimeout), settings.FailWentOffline, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostRetry) }},
		{"another device at the address", fmt.Errorf("check: %w", errAnotherDevice), settings.FailOtherDevice, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostEvery) }},
		{"another device on the topic", fmt.Errorf("check: %w", anotherOnTopic{topic: "plug"}), settings.FailOtherDevice, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostRetry) }},
		{"minimal firmware", fmt.Errorf("read: %w", settings.ErrNoMAC), settings.FailNoMAC, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostEvery) }},
		{"credentials refused", tasmota.ErrAuth, settings.FailAuth, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostRetry) }},
		{"unclassified", errors.New("something else"), settings.FailOther, func(g *brokerRig) time.Time { return g.clock.Add(brokerHostRetry) }},
	} {
		g, dataErr := dataRig(t, 1)
		*dataErr = tc.err
		g.r.pass(t.Context())
		reason, at, next, ok := g.outcome(1)
		if !ok || reason != tc.want {
			t.Errorf("%s: reason = %q (recorded %v), want %q", tc.name, reason, ok, tc.want)
			continue
		}
		if !at.Equal(g.clock) || !next.Equal(tc.next(g)) || !next.Equal(g.r.dataNext[1]) {
			t.Errorf("%s: at %v, next %v, the schedule says %v", tc.name, at, next, g.r.dataNext[1])
		}
	}
}

// Why (#818): the identity check that refuses a device (another one answers at its address) also stops the data
// reads for a day. The person looking at the page needs that reason, and the day, more than any other.
func TestARefusedIdentityRefreshIsKeptAsTheReasonTheDataIsOld(t *testing.T) {
	g := newBrokerRig(1)
	g.r.next[1] = g.clock.Add(100 * time.Hour)
	g.r.readData = func(context.Context, model.Device) error { return nil }
	g.r.readIdentity = func(context.Context, model.DeviceID) error { return &RefreshRefusedError{Reason: refusedTaken} }
	g.r.pass(t.Context())
	reason, _, next, ok := g.outcome(1)
	if !ok || reason != settings.FailOtherDevice || !next.Equal(g.clock.Add(identityEvery)) || !next.Equal(g.r.dataNext[1]) {
		t.Errorf("reason %q (recorded %v), next %v, the data schedule says %v", reason, ok, next, g.r.dataNext[1])
	}
}

// Why (#818): a reason with no end is a lie. A read that works takes it back, and so does a failure of a step that
// runs after the Settings values were read: the values the page shows are fresh then, and "refresh failed" over them
// would contradict the age next to it.
func TestADataRefreshThatWorksOrFailsLaterTakesTheReasonBack(t *testing.T) {
	for name, then := range map[string]error{
		"the next read works": nil,
		"a later step fails":  laterStepError{errors.New("policy values not read")},
	} {
		g, dataErr := dataRig(t, 1)
		*dataErr = tasmota.ErrTimeout
		g.r.pass(t.Context())
		if _, _, _, ok := g.outcome(1); !ok {
			t.Fatalf("%s: nothing was recorded for the failed read", name)
		}
		*dataErr = then
		g.advance(brokerHostRetry)
		g.r.pass(t.Context())
		if reason, _, _, ok := g.outcome(1); ok {
			t.Errorf("%s: %q is still recorded", name, reason)
		}
	}
}

// Why (#818): a shutdown is not a finding about the device, and a busy device was not asked at all. Neither may leave
// a reason behind; a busy device that has one loses its next try (it is due at its next visit), not the page a time
// that is wrong.
func TestShutdownAndBusyRecordNoReasonAndBusyForgetsTheNextTry(t *testing.T) {
	g, dataErr := dataRig(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	*dataErr = context.Canceled
	g.r.readDataOf(ctx, model.Device{ID: 1})
	if _, _, _, ok := g.outcome(1); ok {
		t.Error("a shutdown was recorded as a failed refresh")
	}

	*dataErr = tasmota.ErrTimeout
	g.r.readDataOf(t.Context(), model.Device{ID: 1})
	if _, _, next, ok := g.outcome(1); !ok || next.IsZero() {
		t.Fatalf("failed read: recorded %v, next %v", ok, next)
	}
	g.busy[1] = true
	if g.r.readDataOf(t.Context(), model.Device{ID: 1}) {
		t.Fatal("the read ran on a busy device")
	}
	if reason, _, next, ok := g.outcome(1); !ok || reason != settings.FailTimeout || !next.IsZero() {
		t.Errorf("busy: reason %q (recorded %v), next %v; the reason stays, the next try is unknown", reason, ok, next)
	}
}

// Why (#818): a restart moves the data read to the end of the hour (requested). The time the page names is the time
// the reader will use, so it moves with it.
func TestARestartThatMovesTheDataReadMovesTheNextTryShown(t *testing.T) {
	g, dataErr := dataRig(t, 1)
	*dataErr = tasmota.ErrTimeout
	g.r.restartLeft[1] = true // the read a restart left to the end of the hour is the restart's read: its failure is not remembered for a retry (#817)
	g.r.pass(t.Context())
	g.advance(time.Minute)
	g.r.request(1, true)
	g.r.requested(1, g.r.pending[1], g.clock)
	_, _, next, ok := g.outcome(1)
	if want := g.r.restartRead[1].Add(restartRefreshEvery); !ok || !next.Equal(want) || !next.Equal(g.r.dataNext[1]) {
		t.Errorf("next try shown %v (recorded %v), the schedule says %v, want %v", next, ok, g.r.dataNext[1], want)
	}
}

// Why (#818): a deleted device must not keep a reason for ever.
func TestTheReasonOfADeletedDeviceIsForgotten(t *testing.T) {
	g, dataErr := dataRig(t, 1, 2)
	*dataErr = tasmota.ErrTimeout
	g.r.pass(t.Context())
	g.advance(time.Minute)
	g.r.pass(t.Context()) // the second device
	if _, _, _, ok := g.outcome(1); !ok {
		t.Fatal("no reason was recorded")
	}
	g.r.list = func(context.Context) ([]model.Device, error) { return []model.Device{{ID: 2}}, nil }
	g.r.pass(t.Context())
	if _, _, _, ok := g.outcome(1); ok {
		t.Error("the reason of a device that is gone is still there")
	}
}

// Why (#818): the Settings tab gives a reason only when the Settings values are the ones that failed to refresh. A
// later step of the same refresh (the policy values, the template reading) that fails leaves fresh values behind, so
// it must reach the reader as laterStepError; a failure of the Settings read itself must not.
func TestOnlyAFailureAfterTheSettingsReadIsMarkedAsALaterStep(t *testing.T) {
	var later laterStepError
	for name, tc := range map[string]struct {
		mqtt      func() *scriptedMQTT
		wantLater bool
	}{
		"the template reading fails": {func() *scriptedMQTT {
			return &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("Module", tasmota.ErrTimeout)}
		}, true},
		"the Settings read fails": {func() *scriptedMQTT {
			return &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: func(string) (json.RawMessage, error) { return nil, tasmota.ErrTimeout }}
		}, false},
	} {
		g := refreshOverScript(t, tc.mqtt(), otherMAC)
		err := g.a.refreshData(t.Context(), g.d)
		if err == nil {
			t.Errorf("%s: refreshData = nil", name)
			continue
		}
		if got := errors.As(err, &later); got != tc.wantLater {
			t.Errorf("%s: later step = %v, want %v (err %v)", name, got, tc.wantLater, err)
		}
	}
}

// Why (#818): the page reads the outcomes through web.Deps; a wiring that left the field nil would show nothing and
// no test of the reader would notice.
func TestWebDepsCarryTheRefreshOutcomes(t *testing.T) {
	a := &runtime{svc: &Services{}, log: slog.New(slog.DiscardHandler)}
	if a.webDeps(nil, nil).RefreshFailure != nil {
		t.Fatal("a runtime without a reader has no outcomes to give")
	}
	a.brokerHost = newBrokerHostReader(nil, nil, nil, nil, a.log)
	a.brokerHost.outcomes.fail(7, time.Unix(100, 0), time.Unix(200, 0), settings.FailTimeout)
	f, ok := a.webDeps(nil, nil).RefreshFailure(7)
	if !ok || f.Reason != settings.FailTimeout || !f.Next.Equal(time.Unix(200, 0)) {
		t.Errorf("RefreshFailure(7) = %+v, %v", f, ok)
	}
}
