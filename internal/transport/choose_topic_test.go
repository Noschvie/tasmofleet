package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Why (#852): a MAC check answered over MQTT found another device on the device's topic. That device answers the reads
// and runs the writes sent there, so the device's next commands must not go to the topic: reads go over HTTP while
// HTTP works (like the unanswered mark), and a write, a backup or a restore never crosses the broker to that topic,
// also not when HTTP fails (it would change, back up or overwrite the other device). The reason names the topic.
func TestAnotherDeviceOnTheTopicKeepsTheCommandsOffIt(t *testing.T) {
	taken := func(f Facts) Facts { f.MQTT.TopicTakenSince = t0.Add(-time.Minute); return f }
	for _, c := range []struct {
		name                           string
		f                              Facts
		reads, writes, backup, restore Path
	}{
		{"HTTP works", taken(base()), HTTP, HTTP, HTTP, HTTP},
		{"HTTP unknown", func() Facts { f := taken(base()); f.HTTP = HTTPState{}; return f }(), HTTP, HTTP, HTTP, HTTP},
		{"HTTP failing: a read is the only way left, nothing that changes the device", taken(httpDown(base())), MQTT, None, None, None},
		{"no mark: MQTT first", base(), MQTT, MQTT, MQTT, MQTT},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := Choose(c.f)
			for _, a := range []struct {
				act       Action
				got, want Path
			}{{ActCommands, p.Commands, c.reads}, {ActWrites, p.Writes, c.writes}, {ActBackup, p.Backup, c.backup}, {ActRestore, p.Restore, c.restore}} {
				if a.got != a.want {
					t.Errorf("%s = %s, want %s (%s)", a.act, a.got, a.want, p.Reason(a.act))
				}
				if c.f.MQTT.TopicTakenSince.IsZero() || a.got == MQTT {
					continue
				}
				if r := p.Reason(a.act); !strings.Contains(r, "another device answers on the device's MQTT topic kitchen") {
					t.Errorf("%s reason %q does not say which topic another device answers on", a.act, r)
				}
			}
		})
	}
	// A job that pinned MQTT asks the same: no write there, but the read that checks the topic again may go.
	if why := MQTTBlocker(taken(base()), true); why == "" {
		t.Error("a pinned write may go to a topic another device answers on")
	}
	if why := MQTTBlocker(taken(base()), false); why != "" {
		t.Errorf("a pinned read is refused (%s): nothing could check the topic again and end the mark", why)
	}
}

// Why (#852): the mark holds until the topic is the device's own again: it must not expire on its own (the commands
// would drift back to the shared topic between two checks), it ends when the device itself answers there (Clear) and
// it belongs to the topic it was found on, so a device whose stored topic changed is not held off its new one. It
// still counts when the stored topic returns to it (fail closed: the next check on the topic ends it if the other
// device has left).
func TestTheTopicMarkEndsOnlyWhenTheTopicIsTheDevicesOwn(t *testing.T) {
	now := t0
	m := NewTopicMarks(func() time.Time { return now })
	m.Mark(1, "kitchen")
	now = now.Add(48 * time.Hour)
	if got := m.Since(1, "kitchen"); !got.Equal(t0) {
		t.Fatalf("Since after two days = %v, want the first finding %v: the mark expired on its own", got, t0)
	}
	m.Mark(1, "kitchen") // found again: still the first finding
	if got := m.Since(1, "kitchen"); !got.Equal(t0) {
		t.Errorf("Since = %v after a second finding, want the first %v", got, t0)
	}
	m.Clear(1, "hall") // the device answered on another topic: says nothing about this one
	if m.Since(1, "kitchen").IsZero() {
		t.Error("an answer on another topic ended the mark")
	}
	if !m.Since(1, "hall").IsZero() {
		t.Error("the device's new topic counts as taken")
	}
	if m.Since(1, "kitchen").IsZero() {
		t.Error("the stored topic went back to kitchen, where another device was found, and the mark was gone: asking for another topic ended it")
	}
	m.Mark(2, "porch")
	m.Clear(2, "porch")
	if !m.Since(2, "porch").IsZero() {
		t.Error("the device answered on its topic again and is still held off it")
	}
	m.Mark(3, "")
	if !m.Since(3, "").IsZero() {
		t.Error("a device without a topic was marked")
	}
}

// Why (#852, security check): Since is a read, and it is asked with device snapshots that can be older than the stored
// topic (a broker reader's device list, a page's device). A read must never end a mark: a snapshot with the old topic
// would erase the mark on the topic the router sends to now, and the device's writes, backups and restores would go
// back to the topic another device answers on. Only a check that hears the device alone on the topic ends it (Clear).
func TestReadingATopicMarkNeverEndsIt(t *testing.T) {
	m := NewTopicMarks(func() time.Time { return t0 })
	m.Mark(1, "new")
	_ = m.Since(1, "old") // a stale snapshot asks; its answer (not marked there) is not under test
	if m.Since(1, "new").IsZero() {
		t.Error("the mark on the device's current topic was erased by a read with a stale topic")
	}
	m.Mark(2, "a")
	_ = m.Since(2, "b") // the stored topic moved to b for a while
	if m.Since(2, "a").IsZero() {
		t.Error("back on topic a, where another device was found: the mark is gone")
	}
}

// Why (#852): the router's memory of taken topics is what its facts read, so a device the identity checks found
// another device on is held off the topic by the very router that sends its commands: its next read and its next
// write go over HTTP, and once the device answers on its topic again (Clear) they go over MQTT again.
func TestTheRouterSendsNothingToATopicItsMemoryHoldsAsTaken(t *testing.T) {
	g := newRig(t)
	g.f.httpOK = true
	g.r.TopicTaken().Mark(g.devs.d.ID, g.devs.d.Topic)
	for _, cmd := range []string{"Status 6", "Power1 ON"} {
		if _, err := g.r.Command(t.Context(), g.target(), cmd); err != nil {
			t.Fatal(err)
		}
	}
	if g.mq.n() != 0 || g.http.n() != 2 {
		t.Fatalf("while the topic is taken: MQTT %v, HTTP %v; want both commands over HTTP", g.mq.sent, g.http.calls)
	}
	g.r.TopicTaken().Clear(g.devs.d.ID, g.devs.d.Topic)
	if _, err := g.r.Command(t.Context(), g.target(), "Status 6"); err != nil {
		t.Fatal(err)
	}
	if g.mq.n() != 1 {
		t.Errorf("after the device answered on its topic again: MQTT %v, want the read there", g.mq.sent)
	}
}

// Why (#852, review): a mark of a taken topic may end only on a check that hears every device on the topic, since a
// check that hears one reply hears whichever of two devices answers first. TopicStatus5 is that check: it asks the
// device's stored topic, returns every reply within the identity window (the collect-all the restore uses) and the
// snapshot whose topic it asked, so the mark is set or ended on that topic. It is a read: a marked topic may be asked
// (else nothing could end the mark), a device not usable over MQTT is not asked, and it never goes over HTTP.
func TestTopicStatus5HearsEveryReplyOnTheStoredTopic(t *testing.T) {
	g := newUpRig(t, Password{})
	g.up.extraMAC = "aa:bb:cc:dd:ee:99"
	g.r.TopicTaken().Mark(g.devs.d.ID, g.devs.d.Topic)
	tctx, trace := WithTrace(t.Context())
	d, replies, err := g.r.TopicStatus5(tctx, g.devs.d.ID)
	if err != nil {
		t.Fatalf("TopicStatus5 = %v: a marked topic must still be asked, or nothing could end the mark", err)
	}
	if d.ID != g.devs.d.ID || d.Topic != "plug" {
		t.Errorf("asked for %+v, want the stored device and its topic", d)
	}
	if len(replies) != 2 {
		t.Errorf("%d replies, want both devices on the topic", len(replies))
	}
	if via := trace.Via(); via != "mqtt" {
		t.Errorf("the check went over %q, want mqtt", via)
	}
	if g.http.n() != 0 {
		t.Errorf("HTTP calls %v: the check is about the topic", g.http.calls)
	}
	t.Run("MQTT not usable: nothing is asked", func(t *testing.T) {
		g.f.lwtOffline = true
		defer func() { g.f.lwtOffline = false }()
		if _, replies, err := g.r.TopicStatus5(t.Context(), g.devs.d.ID); !errors.Is(err, tasmota.ErrUnreachable) || len(replies) != 0 {
			t.Errorf("TopicStatus5 = %d replies, %v; want ErrUnreachable and nothing heard", len(replies), err)
		}
	})
	t.Run("announced over HTTP: not sent over MQTT", func(t *testing.T) {
		if _, replies, err := g.r.TopicStatus5(WithRoute(t.Context(), HTTP), g.devs.d.ID); !errors.Is(err, ErrRouteChanged) || len(replies) != 0 {
			t.Errorf("TopicStatus5 = %d replies, %v; want ErrRouteChanged", len(replies), err)
		}
	})
	t.Run("no facts: nothing is asked", func(t *testing.T) {
		g.r.o.Facts = brokenFacts{g.f}
		defer func() { g.r.o.Facts = pwFacts{g.f, Password{}} }()
		if _, replies, err := g.r.TopicStatus5(t.Context(), g.devs.d.ID); err == nil || len(replies) != 0 {
			t.Errorf("TopicStatus5 = %d replies, %v; want an error and nothing heard", len(replies), err)
		}
	})
	t.Run("MQTT not connected", func(t *testing.T) {
		g.r.SetMQTT(nil)
		if _, _, err := g.r.TopicStatus5(t.Context(), g.devs.d.ID); !errors.Is(err, tasmota.ErrUnreachable) {
			t.Errorf("TopicStatus5 = %v, want ErrUnreachable", err)
		}
	})
}

// cutShortMQTT is the command channel when the caller's context ends inside the reply window: like
// mqttcmd.Channel.DoAll it returns the replies heard so far (the device's own, which came first) and no error.
type cutShortMQTT struct{ *upMQTT }

func (m cutShortMQTT) DoAll(ctx context.Context, d model.Device, c string, _ time.Duration) ([]json.RawMessage, error) {
	raw, err := m.Do(ctx, d, c)
	if err != nil {
		return nil, err
	}
	<-ctx.Done() // the window is not over; a second device would answer later in it
	return []json.RawMessage{raw}, nil
}

// Why (#852, fourth review): TopicStatus5 is the only check that may end a topic mark, because it hears every reply
// in the window. When the caller's context ends inside the window (a person reloads the page during Refresh identity,
// the reader's budget runs out after a slow first reply) the channel returns the first reply without an error. Taken
// as a full window, the device's own reply ended the mark although the second device would have answered later, and
// the device's writes, backups and restores went to the shared topic again. A window cut short heard nothing; the
// restore's check before its upload follows the same rule.
func TestATopicCheckWhoseWindowWasCutShortHeardNothing(t *testing.T) {
	g := newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	g.r.SetMQTT(cutShortMQTT{g.up})
	g.r.TopicTaken().Mark(g.devs.d.ID, g.devs.d.Topic)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, replies, err := g.r.TopicStatus5(ctx, g.devs.d.ID); !errors.Is(err, tasmota.ErrTimeout) || len(replies) != 0 {
		t.Errorf("TopicStatus5 = %d replies, %v; want ErrTimeout and nothing heard: the window was cut short", len(replies), err)
	}
	t.Run("the restore's check", func(t *testing.T) {
		g.r.TopicTaken().Clear(g.devs.d.ID, g.devs.d.Topic)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if err := g.r.UploadConfig(ctx, g.target(), make([]byte, 100), g.bind()); err == nil {
			t.Fatal("UploadConfig = nil after a check whose window was cut short")
		}
		if g.up.uploads != 0 {
			t.Errorf("%d uploads after a check that did not hear its whole window", g.up.uploads)
		}
	})
}

// Why (#852, fourth review): a channel that hears one reply hears whichever of two devices on the topic answers first,
// so through it one reply would always look like the device alone on its topic and end the mark. Production wires the
// collect-all channel; anything else (a wrapper added later) must make the check refuse, never fall back to one reply.
func TestATopicCheckNeedsAChannelThatHearsEveryReply(t *testing.T) {
	g := newRig(t)
	g.mq.reply = `{"StatusNET":{"Mac":"AA:BB:CC:DD:EE:01"}}`
	g.r.TopicTaken().Mark(g.devs.d.ID, g.devs.d.Topic)
	if _, replies, err := g.r.TopicStatus5(t.Context(), g.devs.d.ID); !errors.Is(err, tasmota.ErrNotImplemented) || len(replies) != 0 {
		t.Fatalf("TopicStatus5 = %d replies, %v; want ErrNotImplemented and nothing heard", len(replies), err)
	}
	if g.mq.n() != 0 {
		t.Errorf("sent %v over a channel that cannot hear every reply", g.mq.sent)
	}
}
