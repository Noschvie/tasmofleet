// Package events is the in-process publish/subscribe hub that feeds the
// SSE endpoint and any other live view. Publishers never block: slow
// subscribers lose events (and get a "resync" marker) instead of stalling
// the OTA engine or the MQTT bus.
package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// Topic names an event stream.
type Topic string

// Topics.
const (
	TopicHealth Topic = "health" // payload model.Health
	TopicJob    Topic = "job"    // payload model.Job
	TopicDevice Topic = "device" // payload model.Device (inventory change)
	TopicBackup Topic = "backup" // payload model.Backup
)

// Event is one published item.
type Event struct {
	Topic   Topic
	At      time.Time
	Payload any
	Resync  bool // subscriber dropped events and must reload
}

// Hub fans events out to subscribers.
type Hub struct {
	seq  atomic.Uint64 // events published so far (see Seq)
	mu   sync.Mutex
	buf  int
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch     chan Event
	topics map[Topic]struct{} // empty means all topics
}

// NewHub returns an empty hub. buffer is the per-subscriber channel size
// (at least 1, so that a resync marker always fits).
func NewHub(buffer int) *Hub {
	if buffer < 1 {
		buffer = 1
	}
	return &Hub{buf: buffer, subs: make(map[*subscriber]struct{})}
}

// Publish delivers e to every subscriber of e.Topic without blocking. A
// subscriber whose buffer is full loses its queued events and receives a
// single Resync event instead: it cannot patch a gap, it has to reload.
func (h *Hub) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq.Add(1)
	for s := range h.subs {
		if len(s.topics) > 0 {
			if _, ok := s.topics[e.Topic]; !ok {
				continue
			}
		}
		select {
		case s.ch <- e:
			continue
		default:
		}
		// Full: drop the backlog and leave a marker (the lock serialises
		// publishers, so the freed slot cannot be taken by anyone else).
		for drained := false; !drained; {
			select {
			case <-s.ch:
			default:
				drained = true
			}
		}
		s.ch <- Event{Topic: e.Topic, At: e.At, Resync: true}
	}
}

// Seq counts the events published so far. A page reads it before it reads what it shows, hands it
// to its event stream, and the stream says "resync" when more were published since: the page was
// built from older data than the stream starts with (#160).
func (h *Hub) Seq() uint64 { return h.seq.Load() }

// Subscribe returns a channel of events for the given topics (all topics
// when none are given) and a cancel func that closes the channel. cancel is
// idempotent.
func (h *Hub) Subscribe(topics ...Topic) (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, h.buf), topics: make(map[Topic]struct{}, len(topics))}
	for _, t := range topics {
		s.topics[t] = struct{}{}
	}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			close(s.ch)
			h.mu.Unlock()
		})
	}
}
