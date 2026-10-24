package mqttbus

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// testBroker is an embedded broker on a loopback port. It can be stopped and
// started again on the same address to exercise reconnects.
type testBroker struct {
	t    *testing.T
	addr string
	srv  *mqtt.Server
	// subscribeDelay slows SUBSCRIBE processing down, the way a loaded broker
	// does; fixed at creation.
	subscribeDelay time.Duration
}

// delayHook holds every SUBSCRIBE back before the broker registers it.
type delayHook struct {
	mqtt.HookBase
	d time.Duration
}

func (h *delayHook) ID() string { return "subscribe-delay" }

func (h *delayHook) Provides(b byte) bool { return b == mqtt.OnSubscribe }

func (h *delayHook) OnSubscribe(_ *mqtt.Client, pk packets.Packet) packets.Packet {
	time.Sleep(h.d) // simulates a slow broker; the test waits for the effect, not for this
	return pk
}

// testBudget is far above any test here (the slowest takes about a second).
const testBudget = time.Minute

func newTestBroker(t *testing.T) *testBroker { return newTestBrokerDelayed(t, 0) }

func newTestBrokerDelayed(t *testing.T, subscribeDelay time.Duration) *testBroker {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	b := &testBroker{t: t, addr: addr, subscribeDelay: subscribeDelay}
	// Registered first, so it is stopped last: a hang anywhere in the test or in
	// the broker's shutdown must fail within a minute with all goroutine stacks,
	// not run into the 30 minute CI timeout.
	watchdog := time.AfterFunc(testBudget, func() {
		buf := make([]byte, 4<<20)
		buf = buf[:runtime.Stack(buf, true)]
		fmt.Fprintf(os.Stderr, "%s hung for %s (broker shutdown deadlock?)\n%s\n", t.Name(), testBudget, buf)
		os.Exit(2)
	})
	t.Cleanup(func() { watchdog.Stop() })
	b.start()
	t.Cleanup(b.stop)
	return b
}

func (b *testBroker) start() {
	b.t.Helper()
	srv := mqtt.New(&mqtt.Options{InlineClient: true})
	if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
		b.t.Fatal(err)
	}
	if b.subscribeDelay > 0 {
		if err := srv.AddHook(&delayHook{d: b.subscribeDelay}, nil); err != nil {
			b.t.Fatal(err)
		}
	}
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: b.addr})); err != nil {
		b.t.Fatal(err)
	}
	if err := srv.Serve(); err != nil {
		b.t.Fatal(err)
	}
	b.srv = srv
}

// stop closes the broker. mochi's Server.Close waits for every client
// goroutine but only disconnects clients that finished their CONNECT handshake
// before it listed them; a client that connects (or reconnects after being
// kicked) while Close runs is never disconnected, and Close waits for it
// forever. Tests therefore disconnect their bus first (see waitNoClients).
func (b *testBroker) stop() {
	if b.srv != nil {
		_ = b.srv.Close() // test teardown; nothing useful to do with the error
		b.srv = nil
	}
}

// waitSubscribed blocks until some client holds n subscriptions on the broker.
// A connected client is not yet a subscribed one: paho reports the connection
// open before OnConnect has sent SUBSCRIBE and the broker has processed it.
func (b *testBroker) waitSubscribed(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "broker subscriptions", func() bool {
		for _, cl := range b.srv.Clients.GetAll() {
			if cl.State.Subscriptions.Len() >= n {
				return true
			}
		}
		return false
	})
}

// waitNoClients blocks until every client has disconnected from the broker.
// Closing the broker earlier lets paho reconnect into the shutdown (see stop).
func (b *testBroker) waitNoClients(t *testing.T) {
	t.Helper()
	if b.srv == nil {
		return
	}
	// The broker keeps finished sessions (and its own inline client) in Clients,
	// so look at the connections.
	waitFor(t, "bus to disconnect from the broker", func() bool {
		for _, cl := range b.srv.Clients.GetAll() {
			if !cl.Net.Inline && !cl.Closed() {
				return false
			}
		}
		return true
	})
}

func (b *testBroker) url() string { return "tcp://" + b.addr }

func (b *testBroker) publish(topic, payload string, retain bool) {
	b.t.Helper()
	if err := b.srv.Publish(topic, []byte(payload), retain, 0); err != nil {
		b.t.Fatal(err)
	}
}
