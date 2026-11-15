package transport

import (
	"sync"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// TopicMarks remembers, in memory only, the devices on whose MQTT topic another device answered (#852): a MAC check
// (Status 5) answered over MQTT alone reported a MAC outside the device's stored set, or the restore's check before its
// upload (UploadConfig) heard another MAC or a second reply. That device listens where this
// device's commands over MQTT go, so it would answer the reads and run the writes too. The facts read the mark
// (MQTTFacts.TopicTakenSince): while it holds, the device's commands, writes, backups and restores go over HTTP while
// HTTP is not failing, and its writes and transfers never cross the broker (Choose).
//
// A mark has no expiry, unlike the router's other marks: between two checks the device's commands would otherwise
// drift back onto the shared topic. It belongs to the topic it was found on:
//   - while the device's stored topic is another one (an edit, a refresh that follows the device's own topic), it does
//     not count: Since compares the topic. It is kept, and counts again when the stored topic returns to it (fail
//     closed); a finding on the new topic replaces it (one entry per device). Reading a mark never ends it: Since is
//     asked with snapshots that can be older than the stored topic;
//   - it ends when a check that hears every reply on the topic hears exactly one, with one of the device's own MACs
//     (Clear, Router.TopicStatus5): the other device left the topic. A check that hears one reply cannot end it, since
//     of two devices on the topic it hears whichever answers first. The broker reader asks the topic of every marked
//     device that way every 10 minutes, on a schedule of its own, also while the mark sends the rest over HTTP;
//   - TasmoFleet restarts (the mark lives in memory).
type TopicMarks struct {
	now func() time.Time

	mu sync.Mutex
	m  map[model.DeviceID]topicMark
}

type topicMark struct {
	topic string
	since time.Time
}

// NewTopicMarks returns an empty set of marks.
func NewTopicMarks(now func() time.Time) *TopicMarks {
	return &TopicMarks{now: now, m: map[model.DeviceID]topicMark{}}
}

// Mark records that another device answered on topic for id. "since" keeps the first finding on the same topic.
func (t *TopicMarks) Mark(id model.DeviceID, topic string) {
	if topic == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.m[id]; ok && m.topic == topic {
		return
	}
	t.m[id] = topicMark{topic: topic, since: t.now()}
}

// Clear ends the mark of id on topic: the device alone answered there again. A mark on another topic stays.
func (t *TopicMarks) Clear(id model.DeviceID, topic string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.m[id]; ok && m.topic == topic {
		delete(t.m, id)
	}
}

// Since reports when another device was first found answering on topic for id; zero when it is not marked there.
// It only reads: a mark on another topic stays (the caller's snapshot may be stale, or the topic may return to it).
func (t *TopicMarks) Since(id model.DeviceID, topic string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.m[id]; ok && m.topic == topic {
		return m.since
	}
	return time.Time{}
}
