package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// forgetDuringRead answers the bare GroupTopic with the list the device had when it answered, and marks the list
// unread before the answer comes back: TasmoFleet's own GroupTopic<n> write (holdingCmd) or a restore upload ran
// between the device's answer and the store write.
type forgetDuringRead struct {
	tasmota.Client
	st     *store.SQLite
	id     model.DeviceID
	answer string
}

func (f forgetDuringRead) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if c != groupTopicRead {
		return f.Client.Command(ctx, t, c)
	}
	if err := f.st.ForgetGroupTopics(ctx, f.id); err != nil {
		return nil, err
	}
	return json.RawMessage(f.answer), nil
}

const oldGroupTopics = `{"GroupTopic1":"tasmotas","GroupTopic2":"","GroupTopic3":"","GroupTopic4":""}`

// Why (#1005, security review "toctou-fail-open"): a group topic list the device answered before TasmoFleet changed it
// is old. If the reader stored it after the change was marked (ForgetGroupTopics), with a stamp of the store write, the
// old list would count as known, and another device's MQTT writes would also run on this one (the new GroupTopic2 is
// that device's topic). A list read before the mark must leave the group topics unread; the next read stores them.
func TestAGroupTopicListReadBeforeTheMarkStaysUnread(t *testing.T) {
	st := newTestStore(t)
	id, err := st.CreateDevice(t.Context(), model.Device{Name: "lamp", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01", Topic: "lamp"})
	if err != nil {
		t.Fatal(err)
	}
	c := forgetDuringRead{Client: &gtClient{}, st: st, id: id, answer: oldGroupTopics}
	r := newGroupTopicReader(st.ListDevices, online, c, st, slog.New(slog.DiscardHandler))
	r.tick(t.Context())
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !groupTopicStale(d) {
		t.Fatalf("a list read before the mark is stored as known (read at %v)", d.MoreGroupTopicsReadAt)
	}
	// a read that starts after the mark is a valid read
	r.client, r.tried = &gtClient{}, map[model.DeviceID]groupTry{}
	r.tick(t.Context())
	if d, _ = st.GetDevice(t.Context(), id); groupTopicStale(d) {
		t.Fatal("a read after the mark must store the group topics as known")
	}
}

// Why: the same race for the probe, which reads the bare GroupTopic too and stores the list with its OtaUrl.
func TestAProbeGroupTopicListReadBeforeTheMarkStaysUnread(t *testing.T) {
	p, st, id, _ := factsSetup(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	p.Client = forgetDuringRead{Client: p.Client, st: st, id: id, answer: oldGroupTopics}
	if err := p.Refresh(t.Context(), id, ""); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !groupTopicStale(d) {
		t.Fatalf("a list the probe read before the mark is stored as known (read at %v)", d.MoreGroupTopicsReadAt)
	}
}

// Why (#1005, review): the restore reads the list itself before its verifying backup. When the store refuses that
// read (the list was marked unread while it was in flight), the list stays unread and holds the backup over MQTT back;
// the read must report that, or the restore says nothing and the backup is held back without a reason in the log.
func TestTheRestoresReadReportsAListTheStoreRefused(t *testing.T) {
	st, id := storeWithKnownGroupTopics(t)
	d, _ := st.GetDevice(t.Context(), id)
	a := &runtime{svc: &Services{Client: forgetDuringRead{Client: &gtClient{}, st: st, id: id, answer: oldGroupTopics}, Store: st},
		log: slog.New(slog.DiscardHandler)}
	if err := a.readGroupTopics(t.Context(), d); err == nil {
		t.Fatal("the read reports success, but the store kept nothing")
	}
	if d, _ = st.GetDevice(t.Context(), id); !groupTopicStale(d) {
		t.Fatal("a list read before the mark must stay unread")
	}
}
