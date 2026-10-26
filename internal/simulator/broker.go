package simulator

import (
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// Broker is an embedded MQTT broker on a loopback port, for integration
// tests and tasmofleet-devenv. It accepts every client without credentials
// and honours retained messages and last wills like a real broker.
type Broker struct {
	srv  *mqtt.Server
	addr string
	subs atomic.Int64
}

// StartBroker starts a broker on 127.0.0.1 with a free port.
func StartBroker() (*Broker, error) {
	srv := mqtt.New(&mqtt.Options{
		InlineClient: true,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
		return nil, fmt.Errorf("simulator: broker hook: %w", err)
	}
	l := listeners.NewTCP(listeners.Config{ID: "sim", Address: "127.0.0.1:0"})
	if err := srv.AddListener(l); err != nil {
		return nil, fmt.Errorf("simulator: broker listener: %w", err)
	}
	if err := srv.Serve(); err != nil {
		return nil, fmt.Errorf("simulator: broker serve: %w", err)
	}
	return &Broker{srv: srv, addr: l.Address()}, nil
}

// URL is the broker address for paho, e.g. tcp://127.0.0.1:41234.
func (b *Broker) URL() string { return "tcp://" + b.addr }

// Publish injects a message as if a client had published it.
func (b *Broker) Publish(topic string, payload []byte, retain bool) error {
	return b.srv.Publish(topic, payload, retain, 0)
}

// Subscribe registers fn for every message matching filter, including
// retained ones at subscription time. fn runs on the broker's goroutines.
func (b *Broker) Subscribe(filter string, fn func(topic string, payload []byte, retained bool)) error {
	id := int(b.subs.Add(1))
	return b.srv.Subscribe(filter, id, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		fn(pk.TopicName, append([]byte(nil), pk.Payload...), pk.FixedHeader.Retain)
	})
}

// Close stops the broker and disconnects all clients.
//
// The listeners are closed here first, with closeClientsOf, and only then
// mochi's Server.Close runs (it closes each listener only once, so its own
// client closer is skipped). mochi v2.7.9 closes a listener's clients through
// Clients.GetByListener, which RLocks the clients map and RLocks it again in
// Len; a client disconnecting at that moment queues Delete (Lock) between the
// two and the second RLock waits for it forever. That hung the v0.13.0 tag
// run in a test cleanup (TestBrokerCloseWhileClientsDisconnect).
func (b *Broker) Close() error {
	b.srv.Listeners.CloseAll(func(listener string) { closeClientsOf(b.srv, listener) })
	return b.srv.Close()
}

// closeClientsOf disconnects the clients of one listener, reading the clients
// map under a single RLock (Clients.GetAll), never recursively.
func closeClientsOf(srv *mqtt.Server, listener string) {
	for _, cl := range srv.Clients.GetAll() {
		if cl.Net.Listener == listener && !cl.Closed() {
			_ = srv.DisconnectClient(cl, packets.ErrServerShuttingDown) // shutting down: a failed DISCONNECT changes nothing
		}
	}
}
