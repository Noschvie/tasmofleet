// Package mqttbus connects to the MQTT broker (MQTT 3.1.1, the protocol
// Tasmota speaks), subscribes to the topics tasmofleet needs and publishes
// commands. It decodes topics into (device topic, kind) using the FullTopic
// pattern and hands payloads to subscribers; it does not interpret them.
//
// Subscriptions (default FullTopic %prefix%/%topic%/):
//
//	tele/+/LWT, tele/+/STATE, tele/+/SENSOR, tele/+/INFO+, stat/+/RESULT,
//	stat/+/STATUS+, stat/+/POWER+, stat/+/UPGRADE, stat/+/FILE*, tasmota/discovery/+/config (retained)
//
// MQTT is optional: with no broker configured Connect is never called and
// every consumer works from HTTP alone.
//
// Dependency: github.com/eclipse/paho.mqtt.golang (mature 3.1.1 client with
// auto-reconnect). Tests use an embedded broker (github.com/mochi-mqtt/server/v2,
// test-only import) on 127.0.0.1:0, never a real broker.
package mqttbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ErrNotImplemented marks skeleton code.
var ErrNotImplemented = errors.New("mqttbus: not implemented")

// ErrNotConnected is returned by Command while the broker is unreachable.
var ErrNotConnected = errors.New("mqttbus: not connected")

// Kind classifies an incoming message.
type Kind string

// Message kinds.
const (
	KindLWT       Kind = "lwt"
	KindState     Kind = "state"
	KindSensor    Kind = "sensor"
	KindResult    Kind = "result"
	KindStatus    Kind = "status" // stat/<topic>/STATUSn, Suffix = "STATUS11"
	KindPower     Kind = "power"  // stat/<topic>/POWERn
	KindDiscovery Kind = "discovery"
	KindUpgrade   Kind = "upgrade" // stat/<topic>/UPGRADE {"Upgrade":"Successful. Restarting"|"Failed ..."}
	KindInfo      Kind = "info"    // tele/<topic>/INFO1..3, published once per boot
	KindFile      Kind = "file"    // stat/<topic>/FILEDOWNLOAD and FILEUPLOAD (file transfer over MQTT, #79)
)

// Message is one decoded MQTT message.
type Message struct {
	Kind     Kind
	Topic    string // Tasmota %topic% (for discovery: the MAC from the topic)
	Suffix   string // last topic level, e.g. "STATE", "POWER1", "STATUS11"
	Payload  []byte
	Retained bool
	At       time.Time
}

// Bus is the interface consumers (health, discovery, web console) use.
type Bus interface {
	// Subscribe registers a handler for messages of the given kinds. The
	// handler must not block; it runs on the bus goroutine. Retained messages
	// received so far are replayed to the new handler before Subscribe returns.
	Subscribe(kinds []Kind, h func(Message)) (unsubscribe func())
	// Command publishes cmnd/<topic>/<command> with payload.
	Command(ctx context.Context, topic, command, payload string) error
	Connected() bool
}

// Options configure the connection.
type Options struct {
	BrokerURL string // tcp://mqtt.example.internal:1883 or ssl://...
	Username  string
	Password  string
	ClientID  string    // default "tasmofleet-<hostname>"
	FullTopic string    // default "%prefix%/%topic%/"
	Prefixes  [3]string // cmnd, stat, tele
	// Known reports whether a message belongs to a device in the inventory.
	// Known topics get their own retained-cache budget that unknown topics
	// cannot crowd out. nil treats every topic as unknown.
	Known func(Message) bool
	// OnDropped, if set, is called for every message or cache entry the bus
	// drops (reason: payload_too_large, known_cache_full, unknown_evicted,
	// unknown_too_large), for metric tasmofleet_mqtt_dropped_total{reason}.
	OnDropped func(reason string)
	// AllowAddr says which broker addresses may be dialled: Probe refuses all when it
	// is nil; Connect checks every connection attempt against it when set (the
	// address is checked on the resolved IP right before connect, so DNS tricks do not help).
	AllowAddr func(netip.Addr) bool
}

// Connect starts the connection to the broker and keeps reconnecting until
// ctx is done. It returns as soon as the options are valid: an unreachable
// broker is not an error, because MQTT is optional and the rest of the
// application works from HTTP alone. Connected reports the current state;
// subscriptions are (re)established on every connect.
func Connect(ctx context.Context, o Options) (Bus, error) {
	u, err := url.Parse(o.BrokerURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("mqttbus: invalid broker URL %q", redactURL(o.BrokerURL))
	}
	if err := refuseWebsocket(u); err != nil {
		return nil, err
	}
	codec, ok := NewTopicCodec(o.FullTopic, o.Prefixes)
	if !ok {
		return nil, fmt.Errorf("mqttbus: unsupported FullTopic %q (needs one %%prefix%% and one %%topic%% level)", o.FullTopic)
	}
	if o.ClientID == "" {
		host, _ := os.Hostname() // only used to build a default client ID
		o.ClientID = "tasmofleet-" + host
	}
	b := &bus{codec: codec, subs: make(map[int]*sub), retained: newRetainedCache(), known: o.Known, onDropped: o.OnDropped}
	po := paho.NewClientOptions().
		AddBroker(o.BrokerURL).
		SetClientID(o.ClientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetMaxReconnectInterval(time.Minute).
		SetKeepAlive(30 * time.Second).
		SetOrderMatters(true)
	if o.Username != "" {
		po.SetUsername(o.Username).SetPassword(o.Password)
	}
	if o.AllowAddr != nil {
		allow := o.AllowAddr
		po.SetDialer(&net.Dialer{
			Timeout: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(address)
				if err != nil || !allow(ap.Addr().Unmap()) {
					return errors.New("mqttbus: broker address not allowed")
				}
				return nil
			},
		})
	}
	po.SetOnConnectHandler(func(c paho.Client) {
		for _, f := range codec.Filters() {
			if tok := c.Subscribe(f, 0, b.dispatch); tok.Wait() && tok.Error() != nil {
				slog.Warn("mqtt subscribe failed", "filter", f, "err", tok.Error())
			}
		}
		b.subscribed.Store(true) // after the attempts, so a refused filter (logged above) does not keep the bus "down" for good
		slog.Info("mqtt connected", "broker", redactURL(o.BrokerURL))
	})
	po.SetConnectionLostHandler(func(_ paho.Client, err error) {
		b.subscribed.Store(false)
		slog.Warn("mqtt connection lost", "err", err)
	})
	b.client = paho.NewClient(po)
	b.client.Connect() // result is reported through the OnConnect handler; retries run in the background
	go func() {
		<-ctx.Done()
		b.client.Disconnect(250)
	}()
	return b, nil
}

// refuseWebsocket rejects ws/wss broker URLs: paho dials them through its own
// websocket dialer, so the address rule installed with SetDialer would not apply.
func refuseWebsocket(u *url.URL) error {
	if u.Scheme == "ws" || u.Scheme == "wss" {
		return errors.New("mqttbus: websocket brokers are not supported (the address filter cannot be applied); use tcp:// or ssl://")
	}
	return nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparsable>"
	}
	return u.Scheme + "://" + u.Host
}

type sub struct {
	kinds map[Kind]struct{}
	h     func(Message)
	// mu serialises the handler: Subscribe holds it while replaying, and dispatch
	// takes it per call, so a live message never overtakes the replay (#161).
	mu sync.Mutex
}

type bus struct {
	client paho.Client
	codec  TopicCodec
	// subscribed is set once the OnConnect handler has tried every filter and cleared when the connection is lost.
	subscribed atomic.Bool

	mu     sync.RWMutex
	nextID int
	subs   map[int]*sub
	// retained keeps the latest retained message per topic. The broker hands
	// them over once per connect, usually before consumers have subscribed;
	// Subscribe replays them so that no consumer misses the retained LWTs.
	retained  *retainedCache
	known     func(Message) bool
	onDropped func(string)
}

// Connected is true when the connection is open AND the subscriptions of the current connection
// are in place. paho reports the connection open before the OnConnect handler has subscribed, and a
// command published in that window would be answered into the void (#272).
func (b *bus) Connected() bool { return b.client.IsConnectionOpen() && b.subscribed.Load() }

func (b *bus) Subscribe(kinds []Kind, h func(Message)) func() {
	s := &sub{kinds: make(map[Kind]struct{}, len(kinds)), h: h}
	for _, k := range kinds {
		s.kinds[k] = struct{}{}
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = s
	s.mu.Lock() // held until the replay is done; a concurrent dispatch waits here
	var replay []Message
	for _, m := range b.retained.messages() {
		if _, ok := s.kinds[m.Kind]; ok {
			replay = append(replay, m)
		}
	}
	b.mu.Unlock()
	for _, m := range replay {
		b.call(h, m)
	}
	s.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *bus) dispatch(_ paho.Client, m paho.Message) {
	kind, topic, suffix, ok := b.codec.Decode(m.Topic())
	if !ok {
		return
	}
	if len(m.Payload()) > maxPayload {
		slog.Warn("mqtt message dropped: payload too large", "topic", m.Topic(), "bytes", len(m.Payload()))
		b.dropped(dropPayloadTooBig)
		return
	}
	msg := Message{Kind: kind, Topic: topic, Suffix: suffix, Payload: append([]byte(nil), m.Payload()...), Retained: m.Retained(), At: time.Now()}
	// Tasmota publishes its LWT retained, but the broker clears the flag for
	// subscribers that were connected when it arrived. A late consumer must still
	// replay the latest LWT, so every LWT is cached, whatever the flag says (#161).
	cacheIt := msg.Retained || kind == KindLWT
	known := false
	if cacheIt && len(msg.Payload) > 0 && b.known != nil {
		known = b.known(msg) // outside the lock: may consult the inventory index
	}
	b.mu.Lock()
	var drops []string
	if cacheIt {
		cm := msg
		cm.Retained = true // replayed as retained, as the broker would have delivered it
		drops = b.retained.put(m.Topic(), cm, known)
	}
	subs := make([]*sub, 0, len(b.subs))
	for _, s := range b.subs {
		if _, ok := s.kinds[kind]; ok {
			subs = append(subs, s)
		}
	}
	b.mu.Unlock()
	for _, r := range drops {
		b.dropped(r)
	}
	for _, s := range subs {
		s.mu.Lock() // waits for a replay still running for this subscriber
		b.call(s.h, msg)
		s.mu.Unlock()
	}
}

// Reclassifier is implemented by the bus returned from Connect. The caller
// calls Reclassify after the set of known devices changed, so cached entries
// move between the device reservations and the unknown pool (a device removed
// from the inventory loses its reservation; a new one gains it).
type Reclassifier interface {
	Reclassify()
}

// Reclassify re-evaluates Options.Known for every cached retained message.
func (b *bus) Reclassify() {
	if b.known == nil {
		return
	}
	type item struct {
		topic string
		e     *retEntry
	}
	b.mu.Lock()
	items := make([]item, 0, b.retained.len())
	for t, e := range b.retained.entries {
		items = append(items, item{t, e})
	}
	b.mu.Unlock()
	known := make([]bool, len(items))
	for i, it := range items {
		known[i] = b.known(it.e.msg) // outside the lock: may consult the inventory index
	}
	var drops []string
	b.mu.Lock()
	for i, it := range items {
		if b.retained.entries[it.topic] != it.e || (it.e.key != "") == known[i] {
			continue // replaced meanwhile, or class unchanged
		}
		drops = append(drops, b.retained.put(it.topic, it.e.msg, known[i])...)
	}
	b.mu.Unlock()
	for _, r := range drops {
		b.dropped(r)
	}
}

func (b *bus) dropped(reason string) {
	if b.onDropped != nil {
		b.onDropped(reason)
	}
}

func (b *bus) call(h func(Message), msg Message) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mqtt handler panicked", "kind", msg.Kind, "panic", r)
		}
	}()
	h(msg)
}

func (b *bus) Command(ctx context.Context, topic, command, payload string) error {
	if topic == "" || command == "" || strings.ContainsAny(topic+command, "/+#") {
		return fmt.Errorf("mqttbus: invalid topic %q or command %q", topic, command)
	}
	if !b.client.IsConnectionOpen() {
		return ErrNotConnected
	}
	tok := b.client.Publish(b.codec.Command(topic, command), 0, false, payload)
	select {
	case <-tok.Done():
		return tok.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}
