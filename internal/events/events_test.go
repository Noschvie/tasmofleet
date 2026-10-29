package events

import (
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return e
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestSubscriberOnlyGetsItsTopics(t *testing.T) {
	h := NewHub(4)
	ch, cancel := h.Subscribe(TopicHealth)
	defer cancel()
	h.Publish(Event{Topic: TopicJob, Payload: 1})
	h.Publish(Event{Topic: TopicHealth, Payload: 2})
	if e := recv(t, ch); e.Payload != 2 {
		t.Fatalf("got %v, want the health event only", e.Payload)
	}
}

func TestNoTopicsMeansAllTopics(t *testing.T) {
	h := NewHub(4)
	ch, cancel := h.Subscribe()
	defer cancel()
	h.Publish(Event{Topic: TopicBackup})
	if e := recv(t, ch); e.Topic != TopicBackup {
		t.Fatalf("got %v", e.Topic)
	}
}

// The OTA engine and the MQTT bus publish from hot paths; a stuck browser
// tab must never stall them.
func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	h := NewHub(2)
	_, cancel := h.Subscribe(TopicHealth) // never read
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Publish(Event{Topic: TopicHealth, Payload: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

// A subscriber that lost events cannot patch the gap; it has to reload, so
// the next thing it reads must be the resync marker.
func TestOverflowYieldsResyncMarker(t *testing.T) {
	h := NewHub(2)
	ch, cancel := h.Subscribe(TopicJob)
	defer cancel()
	for i := 0; i < 10; i++ {
		h.Publish(Event{Topic: TopicJob, Payload: i})
	}
	e := recv(t, ch)
	if !e.Resync || e.Topic != TopicJob {
		t.Fatalf("first event after overflow = %+v, want Resync on topic job", e)
	}
}

func TestFastSubscriberSeesNoResync(t *testing.T) {
	h := NewHub(2)
	ch, cancel := h.Subscribe(TopicJob)
	defer cancel()
	for i := 0; i < 20; i++ {
		h.Publish(Event{Topic: TopicJob, Payload: i})
		if e := recv(t, ch); e.Resync || e.Payload != i {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
}

func TestSlowSubscriberDoesNotAffectOthers(t *testing.T) {
	h := NewHub(1)
	_, cancelSlow := h.Subscribe(TopicJob)
	defer cancelSlow()
	fast, cancelFast := h.Subscribe(TopicJob)
	defer cancelFast()
	for i := 0; i < 5; i++ {
		h.Publish(Event{Topic: TopicJob, Payload: i})
		if e := recv(t, fast); e.Resync || e.Payload != i {
			t.Fatalf("fast subscriber got %+v at %d", e, i)
		}
	}
}

func TestCancelClosesChannelAndIsIdempotent(t *testing.T) {
	h := NewHub(1)
	ch, cancel := h.Subscribe(TopicJob)
	cancel()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed")
	}
	h.Publish(Event{Topic: TopicJob}) // must not panic on the closed channel
}

func TestConcurrentPublishSubscribeCancel(t *testing.T) {
	h := NewHub(8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Publish(Event{Topic: TopicHealth, Payload: j})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, cancel := h.Subscribe(TopicHealth)
				select {
				case <-ch:
				default:
				}
				cancel()
			}
		}()
	}
	wg.Wait()
}

func TestPublishFillsInTimestamp(t *testing.T) {
	h := NewHub(1)
	ch, cancel := h.Subscribe(TopicJob)
	defer cancel()
	h.Publish(Event{Topic: TopicJob})
	if e := recv(t, ch); e.At.IsZero() {
		t.Fatal("At not set; SSE ids and ordering need a timestamp")
	}
}

// Why (#160): a page records Seq before it reads its data; the stream compares it with the hub's
// to learn whether events were published in between. Every publish counts, delivered or not.
func TestSeqCountsEveryPublish(t *testing.T) {
	h := NewHub(1)
	if h.Seq() != 0 {
		t.Fatalf("fresh hub: %d", h.Seq())
	}
	h.Publish(Event{Topic: TopicJob}) // nobody subscribed: still counted
	h.Publish(Event{Topic: TopicHealth})
	if h.Seq() != 2 {
		t.Fatalf("seq %d, want 2", h.Seq())
	}
}
