package simulator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
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
	lst  *simListener
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
	l := &simListener{srv: srv, conns: map[net.Conn]struct{}{}}
	if err := srv.AddListener(l); err != nil {
		return nil, fmt.Errorf("simulator: broker listener: %w", err)
	}
	if err := srv.Serve(); err != nil {
		return nil, fmt.Errorf("simulator: broker serve: %w", err)
	}
	return &Broker{srv: srv, addr: l.Address(), lst: l}, nil
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
// The listener is shut down first (simListener.shutdown): no new connection,
// every accepted one ended and waited for. Only then mochi's Server.Close runs;
// its listener close is a no-op by then and its ClientsWg.Wait has nothing left
// to race with. Two mochi v2.7.9 problems are avoided this way: its own client
// closer (Clients.GetByListener) RLocks the clients map recursively and
// deadlocks against a disconnecting client's Delete (hung the v0.13.0 tag run),
// and its TCP listener lets attachClient call ClientsWg.Add while Close is in
// ClientsWg.Wait (race detector, FULL run on 90b2edc).
func (b *Broker) Close() error {
	b.lst.shutdown()
	return b.srv.Close()
}

// simListener is a mochi listener on 127.0.0.1:0 that, unlike listeners.TCP,
// keeps track of every accepted connection until its establish call returned.
type simListener struct {
	srv    *mqtt.Server
	listen net.Listener
	log    *slog.Logger

	mu    sync.Mutex
	ended bool
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup // accepted connections whose establish call has not returned
}

func (l *simListener) ID() string       { return "sim" }
func (l *simListener) Protocol() string { return "tcp" }
func (l *simListener) Address() string  { return l.listen.Addr().String() }

func (l *simListener) Init(log *slog.Logger) error {
	l.log = log
	var err error
	l.listen, err = (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0") // mochi gives Init no context
	return err
}

// Serve accepts until shutdown. A connection is registered (wg.Add) before its
// goroutine starts, under the same lock shutdown takes, so shutdown either sees
// it or it is refused.
func (l *simListener) Serve(establish listeners.EstablishFn) {
	for {
		conn, err := l.listen.Accept()
		if err != nil {
			return // closed by shutdown
		}
		l.mu.Lock()
		if l.ended {
			l.mu.Unlock()
			_ = conn.Close() // shutting down: refused
			continue
		}
		l.conns[conn] = struct{}{}
		l.wg.Add(1)
		l.mu.Unlock()
		go func() {
			defer l.wg.Done()
			if err := establish(l.ID(), conn); err != nil {
				l.log.Debug("simulator broker: connection ended", "err", err)
			}
			l.mu.Lock()
			delete(l.conns, conn)
			l.mu.Unlock()
		}()
	}
}

// Close is what mochi's Server.Close calls; Broker.Close has shut the
// listener down before, so mochi's closer (closeClients) is never used.
func (l *simListener) Close(_ listeners.CloseFn) { l.shutdown() }

// shutdown stops accepting, disconnects the registered clients (closeClientsOf),
// closes every accepted connection (also those that never sent CONNECT) and
// waits until all establish calls returned. It is idempotent.
func (l *simListener) shutdown() {
	l.mu.Lock()
	first := !l.ended
	l.ended = true
	l.mu.Unlock()
	if first {
		_ = l.listen.Close() // stops Accept; an error means it is closed already
		closeClientsOf(l.srv, l.ID())
		l.mu.Lock()
		for c := range l.conns {
			_ = c.Close() // ends attachClient's read; already closed is fine
		}
		l.mu.Unlock()
	}
	l.wg.Wait()
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
