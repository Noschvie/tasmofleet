package health

import (
	"context"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// replayBus behaves like the real connection: Subscribe replays a retained LWT
// synchronously into the handler, and the test can hold it inside Subscribe.
type replayBus struct {
	entered chan struct{}
	release chan struct{}
}

func (b *replayBus) Subscribe(_ []mqttbus.Kind, h func(mqttbus.Message)) func() {
	b.entered <- struct{}{}
	<-b.release
	h(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "a", Suffix: "LWT", Payload: []byte("Offline"), Retained: true})
	return func() {}
}
func (b *replayBus) Command(context.Context, string, string, string) error { return nil }
func (b *replayBus) CommandTopic(topic, command string) string {
	return "cmnd/" + topic + "/" + command
}

func (b *replayBus) Connected() bool { return true }

// Regression (v0.3.1): with a stored broker the pages hung forever. The health
// tick held the health mutex while asking Switch.Enabled (Switch mutex), and
// Switch.Replace held the Switch mutex while the new connection replayed
// retained messages into the health handler (health mutex): a lock-order
// inversion. Neither package may call into the other while holding its lock.
func TestSwitchReplaceAndHealthTickDoNotDeadlock(t *testing.T) {
	sw := mqttbus.NewSwitch()
	h := newHarness(t, false, Options{}, mqttDev(1, "a"))
	h.svc.d.Bus = sw
	h.svc.reload(h.ctx, h.clk.Now())
	sw.Subscribe(nil, h.svc.onMessage)

	inner := &replayBus{entered: make(chan struct{}, 1), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		replaced := make(chan struct{})
		go func() { sw.Replace(inner, nil); close(replaced) }()
		<-inner.entered // Replace is inside the new connection's Subscribe
		tickDone := make(chan struct{})
		go func() { h.svc.tick(h.ctx); h.svc.wg.Wait(); close(tickDone) }()
		time.Sleep(100 * time.Millisecond) // let the tick reach its bus check
		close(inner.release)               // the retained LWT is replayed now
		<-replaced
		<-tickDone
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: health tick and Switch.Replace wait for each other")
	}
	if got := h.svc.Get(1).State; got != model.HealthOffline {
		t.Fatalf("state %s, want offline from the replayed LWT", got)
	}
}
