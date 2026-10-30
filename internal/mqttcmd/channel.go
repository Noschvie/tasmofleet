package mqttcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Defaults of Options (D3 of the 0.20.0 plan; live replies took 40-100 ms).
const (
	DefaultGap          = 100 * time.Millisecond
	DefaultReplyWait    = 8 * time.Second
	DefaultBacklogQuiet = 600 * time.Millisecond
	DefaultBacklogMax   = 10 * time.Second

	maxTopics = 1024 // bound of the LWT and INFO tables; known devices are far fewer
	waitDepth = 32   // messages kept for one waiting command; more are dropped
)

// ErrRefused marks a command the channel will not publish. Nothing was sent.
var ErrRefused = errors.New("mqttcmd: command refused")

// Doer is what consumers (devicecmd, identity, later the transport router) depend on.
type Doer interface {
	// Do sends cmd to d over MQTT and returns the matching reply.
	Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
	// Connected reports whether the broker connection is usable for a command right now.
	Connected() bool
}

// Options tune the channel. Zero values select the defaults.
type Options struct {
	Gap          time.Duration // between two publishes on the whole bus
	ReplyWait    time.Duration // for the reply to one command
	BacklogQuiet time.Duration // after the last collected Backlog reply
	BacklogMax   time.Duration // for a whole Backlog
	// Known, if set, limits the LWT and INFO tables to inventory devices.
	Known func(mqttbus.Message) bool
}

// Channel implements Doer on a bus.
type Channel struct {
	bus   mqttbus.Bus
	o     Options
	unsub func()

	mu      sync.Mutex
	lwt     map[string]string // topic -> "Online" | "Offline"
	info    map[string]*Info
	sems    map[string]chan struct{}
	waiters map[string]*waiter
	nextPub time.Time
}

// waiter receives the live replies of the one command in flight on a topic.
type waiter struct {
	ch  chan mqttbus.Message
	off chan struct{} // LWT Offline arrived
}

// New creates the channel and subscribes before it returns, so the first Do cannot miss a reply.
func New(bus mqttbus.Bus, o Options) *Channel {
	if o.Gap == 0 {
		o.Gap = DefaultGap
	}
	if o.ReplyWait == 0 {
		o.ReplyWait = DefaultReplyWait
	}
	if o.BacklogQuiet == 0 {
		o.BacklogQuiet = DefaultBacklogQuiet
	}
	if o.BacklogMax == 0 {
		o.BacklogMax = DefaultBacklogMax
	}
	c := &Channel{bus: bus, o: o, lwt: map[string]string{}, info: map[string]*Info{}, sems: map[string]chan struct{}{}, waiters: map[string]*waiter{}}
	c.unsub = bus.Subscribe([]mqttbus.Kind{mqttbus.KindLWT, mqttbus.KindInfo, mqttbus.KindResult, mqttbus.KindStatus}, c.onMessage)
	return c
}

// Close stops listening.
func (c *Channel) Close() { c.unsub() }

// Connected reports whether the bus is connected and subscribed.
func (c *Channel) Connected() bool { return c.bus.Connected() }

// Do sends cmd and returns the reply that belongs to it (see the package comment). Automated
// reads and writes use this; a reply without a key that names the command is not accepted.
func (c *Channel) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return c.do(ctx, d, cmd, false)
}

// DoLoose is Do for the console: a command whose reply carries other keys than its name
// (Template, Gpios; checked against v16 source, docs/research/tasmota-mqtt-transport.md section 8)
// takes the first RESULT. Every other command is matched exactly as in Do.
func (c *Channel) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return c.do(ctx, d, cmd, true)
}

func (c *Channel) do(ctx context.Context, d model.Device, cmd string, loose bool) (json.RawMessage, error) {
	req, err := parseRequest(cmd)
	if err != nil {
		return nil, err
	}
	if err := checkTopic(d); err != nil {
		return nil, err
	}
	if !c.bus.Connected() {
		return nil, fmt.Errorf("%w: %w", tasmota.ErrUnreachable, mqttbus.ErrNotConnected)
	}
	sem := c.sem(d.Topic)
	select {
	case sem <- struct{}{}: // one command in flight per topic; blocked callers are served in order
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-sem }()

	if c.offline(d.Topic) { // checked after the wait: the device may have gone while we queued
		return nil, fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	}
	if err := c.waitTurn(ctx); err != nil {
		return nil, err
	}
	w := &waiter{ch: make(chan mqttbus.Message, waitDepth), off: make(chan struct{}, 1)}
	c.setWaiter(d.Topic, w)
	defer c.setWaiter(d.Topic, nil)
	if err := c.bus.Command(ctx, d.Topic, req.name, req.args); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s not sent: %w", tasmota.ErrUnreachable, req.name, err)
	}
	var raw json.RawMessage
	if req.backlog != nil {
		raw, err = c.collectBacklog(ctx, w, req)
	} else {
		raw, err = c.collect(ctx, w, req, loose)
	}
	if err != nil {
		return nil, err
	}
	if req.redact {
		raw = cmdpolicy.ScrubReply(raw)
	}
	return raw, nil
}

// collect waits for the one reply of a single command.
func (c *Channel) collect(ctx context.Context, w *waiter, req *request, loose bool) (json.RawMessage, error) {
	timer := time.NewTimer(c.o.ReplyWait)
	defer timer.Stop()
	for {
		select {
		case m := <-w.ch:
			v, ok := req.match(m, loose)
			if !ok {
				continue
			}
			if v.err != nil {
				return nil, v.err
			}
			return v.reply, nil
		case <-w.off:
			return nil, fmt.Errorf("mqtt: the device went offline before it answered %s (outcome unknown): %w", req.name, tasmota.ErrTimeout)
		case <-timer.C:
			return nil, fmt.Errorf("mqtt: no answer to %s: %w", req.name, tasmota.ErrTimeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func checkTopic(d model.Device) error {
	switch {
	case d.Topic == "":
		return fmt.Errorf("%w: the device has no MQTT topic", ErrRefused)
	case strings.ContainsAny(d.Topic, "/+#") || strings.TrimSpace(d.Topic) != d.Topic:
		return fmt.Errorf("%w: the topic contains characters that are not allowed", ErrRefused)
	case d.GroupTopic != "" && d.Topic == d.GroupTopic:
		return fmt.Errorf("%w: the topic is the group topic, which would reach many devices", ErrRefused)
	}
	return nil
}

func (c *Channel) sem(topic string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sems[topic]
	if s == nil {
		s = make(chan struct{}, 1)
		c.sems[topic] = s
	}
	return s
}

func (c *Channel) setWaiter(topic string, w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w == nil {
		delete(c.waiters, topic)
		return
	}
	c.waiters[topic] = w
}

func (c *Channel) offline(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lwt[topic] == "Offline"
}

// waitTurn keeps Gap between two publishes on the whole bus.
func (c *Channel) waitTurn(ctx context.Context) error {
	now := time.Now()
	c.mu.Lock()
	turn := now
	if c.nextPub.After(turn) {
		turn = c.nextPub
	}
	c.nextPub = turn.Add(c.o.Gap)
	c.mu.Unlock()
	wait := turn.Sub(now)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// onMessage runs on the bus goroutine and never blocks.
func (c *Channel) onMessage(m mqttbus.Message) {
	switch m.Kind {
	case mqttbus.KindLWT:
		c.onLWT(m)
	case mqttbus.KindInfo:
		c.onInfo(m)
	case mqttbus.KindResult, mqttbus.KindStatus:
		if m.Retained {
			return // old news, never an answer
		}
		c.mu.Lock()
		w := c.waiters[m.Topic]
		c.mu.Unlock()
		if w != nil {
			select {
			case w.ch <- m:
			default: // a flood must not block the bus; the real reply is lost, the caller times out
			}
		}
	case mqttbus.KindState, mqttbus.KindSensor, mqttbus.KindPower, mqttbus.KindDiscovery, mqttbus.KindUpgrade, mqttbus.KindFile:
		// not subscribed
	}
}

func (c *Channel) onLWT(m mqttbus.Message) {
	state := string(m.Payload)
	if state != "Online" && state != "Offline" {
		return
	}
	c.mu.Lock()
	_, have := c.lwt[m.Topic]
	c.mu.Unlock()
	if !have && c.o.Known != nil && !c.o.Known(m) { // outside the lock: may consult the inventory
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !have && len(c.lwt) >= maxTopics {
		return
	}
	c.lwt[m.Topic] = state
	if state == "Offline" {
		// Every software restart publishes Offline (the will, no DISCONNECT): what the device
		// announced before it is stale, and a command waiting for its answer lost it.
		delete(c.info, m.Topic)
		if w := c.waiters[m.Topic]; w != nil {
			select {
			case w.off <- struct{}{}:
			default:
			}
		}
	}
}
