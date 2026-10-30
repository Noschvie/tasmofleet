package mqttbus

import (
	"context"
	"sync"
	"sync/atomic"
)

// Switch is a Bus whose connection can be replaced or turned off while its
// consumers keep their subscriptions (the system settings page changes the broker
// live). Handlers subscribed through the switch follow every new connection;
// retained messages are replayed by the new connection as usual. A switch with no
// connection is disabled: Command fails with ErrNotConnected and Connected is false.
type Switch struct {
	// opMu serialises Replace, Subscribe and unsubscribe. It is held while the
	// inner connection is called, which replays retained messages into the
	// handlers synchronously; mu is not, so a handler (or anything it calls) may
	// use the other Switch methods and nothing that holds a lock of its own can
	// deadlock against us. Lock order: opMu, then mu.
	opMu sync.Mutex

	// mu guards the fields below and is never held across a call into a Bus or
	// a handler.
	mu     sync.Mutex
	cur    Bus
	stop   func()
	nextID int
	subs   map[int]*switchSub

	enabled atomic.Bool // mirrors cur != nil; lets Enabled run under foreign locks
}

type switchSub struct {
	kinds []Kind
	h     func(Message)
	inner func() // unsubscribe from the current connection; guarded by opMu
}

// NewSwitch returns a disabled switch.
func NewSwitch() *Switch { return &Switch{subs: map[int]*switchSub{}} }

var _ Bus = (*Switch)(nil)

// Replace makes b the connection (nil turns the bus off). stop, if not nil, ends
// the connection that is replaced the next time Replace is called. Every
// registered handler is moved to b.
func (s *Switch) Replace(b Bus, stop func()) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	subs := make([]*switchSub, 0, len(s.subs))
	for _, sub := range s.subs {
		subs = append(subs, sub)
	}
	oldStop := s.stop
	s.cur, s.stop = b, stop
	s.enabled.Store(b != nil)
	s.mu.Unlock()

	for _, sub := range subs {
		if sub.inner != nil {
			sub.inner()
			sub.inner = nil
		}
	}
	if oldStop != nil {
		oldStop()
	}
	if b == nil {
		return
	}
	for _, sub := range subs {
		sub.inner = b.Subscribe(sub.kinds, sub.h)
	}
}

// Enabled reports whether a connection is configured (it may still be down).
// It takes no lock, so callers may hold their own locks.
func (s *Switch) Enabled() bool { return s.enabled.Load() }

// Subscribe implements Bus.
func (s *Switch) Subscribe(kinds []Kind, h func(Message)) func() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	sub := &switchSub{kinds: append([]Kind(nil), kinds...), h: h}
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = sub
	cur := s.cur
	s.mu.Unlock()
	if cur != nil {
		sub.inner = cur.Subscribe(sub.kinds, sub.h)
	}
	return func() {
		s.opMu.Lock()
		defer s.opMu.Unlock()
		if sub.inner != nil {
			sub.inner()
			sub.inner = nil
		}
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

// Command implements Bus.
func (s *Switch) Command(ctx context.Context, topic, command, payload string) error {
	s.mu.Lock()
	b := s.cur
	s.mu.Unlock()
	if b == nil {
		return ErrNotConnected
	}
	return b.Command(ctx, topic, command, payload)
}

// CommandTopic implements Bus. Without a connection the default shape is assumed; nothing is
// published then.
func (s *Switch) CommandTopic(topic, command string) string {
	s.mu.Lock()
	b := s.cur
	s.mu.Unlock()
	if b == nil {
		return "cmnd/" + topic + "/" + command
	}
	return b.CommandTopic(topic, command)
}

// Connected implements Bus.
func (s *Switch) Connected() bool {
	s.mu.Lock()
	b := s.cur
	s.mu.Unlock()
	return b != nil && b.Connected()
}

// Reclassify implements Reclassifier for the current connection.
func (s *Switch) Reclassify() {
	s.mu.Lock()
	b := s.cur
	s.mu.Unlock()
	if r, ok := b.(Reclassifier); ok {
		r.Reclassify()
	}
}
