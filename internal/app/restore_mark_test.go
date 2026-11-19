package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/health"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#607): a restore over MQTT that changed the device's MQTT connection while HTTP answered must reach the router's
// memory, so the device's next commands go over HTTP while it may not be back on the broker (as after such a write,
// #605 T10). Without this wiring the restore's mark would go nowhere and the next commands into the void.
func TestTheRestoreMarksAChangedMQTTConnectionInTheRouter(t *testing.T) {
	rt := transport.NewRouter(transport.Options{})
	a := &runtime{svc: &Services{Client: writerClient{}, Router: rt}}
	mark := a.restoreOptions().MarkMQTTConnChanged
	if mark == nil {
		t.Fatal("the restore has no way to mark the device")
	}
	mark(7)
	if _, _, marked := rt.MQTTConnChanged().HTTPFailure(7); !marked {
		t.Error("the router does not know that the device's MQTT connection changed")
	}
}

// Why (#1005, review): a restore upload that may have reached the device may have changed GroupTopic2 to GroupTopic4
// (the dump carries them). The restore's hook must reach the store and make the stored list unread, or the other
// devices' MQTT writes would go on as if the old list were still true. Its read hook must then store what the device
// answers now, or the verifying backup of a device only MQTT reaches would be held back by the unread list.
func TestTheRestoreMakesTheStoredGroupTopicsUnreadAndReadsThemAgain(t *testing.T) {
	st, id := storeWithKnownGroupTopics(t)
	cl := &gtClient{reply: map[model.DeviceID]string{id: `{"GroupTopic1":"tasmotas","GroupTopic2":"cellar","GroupTopic3":"","GroupTopic4":""}`}}
	a := &runtime{svc: &Services{Client: cl, Router: transport.NewRouter(transport.Options{}), Store: st},
		bgContext: t.Context(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	forget := a.restoreOptions().ForgetGroupTopics
	if forget == nil {
		t.Fatal("the restore has no way to mark the group topics unread")
	}
	forget(id)
	d, _ := st.GetDevice(t.Context(), id)
	if !groupTopicStale(d) {
		t.Errorf("group topics %v read at %v after a restore: they must count as unread", d.GroupTopics(), d.MoreGroupTopicsReadAt)
	}
	read := a.restoreOptions().ReadGroupTopics
	if read == nil {
		t.Fatal("the restore has no way to read the group topics again")
	}
	if err := read(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if d, _ = st.GetDevice(t.Context(), id); groupTopicStale(d) || !slices.Equal(d.GroupTopics(), []string{"tasmotas", "cellar"}) {
		t.Errorf("after the restore's read: %v at %v, want the device's answer, known", d.GroupTopics(), d.MoreGroupTopicsReadAt)
	}
}

// Why (#1005, review): a GroupTopic<n> write from the console, a bulk command or an update job may have changed
// GroupTopic2 to GroupTopic4, and INFO1 refreshes GroupTopic1 only. holdingCmd's hook is the only thing that makes the
// stored list unread after such a write; if the app stops wiring it to the store, the other devices' MQTT writes go on
// as if the old list were still true, and every other test stays green.
func TestTheConsoleServiceMakesTheStoredGroupTopicsUnread(t *testing.T) {
	st, id := storeWithKnownGroupTopics(t)
	ctx, stop := context.WithCancel(t.Context())
	a := &runtime{brokerHost: newBrokerHostReader(nil, nil, nil, nil, slog.New(slog.DiscardHandler)),
		svc: &Services{Hub: events.NewHub(8), Store: st}, restartAsks: newRestartAsks(time.Now),
		tracker: health.New(health.Deps{}, health.Options{}), bgContext: ctx, log: slog.New(slog.DiscardHandler)}
	a.setupDeviceCmd()
	t.Cleanup(func() { stop(); a.bgWG.Wait() })
	h, ok := a.deviceCmd.(holdingCmd)
	if !ok || h.onGroupTopic == nil {
		t.Fatalf("console service %T has no group-topic hook", a.deviceCmd)
	}
	h.onGroupTopic(id)
	if d, _ := st.GetDevice(t.Context(), id); !groupTopicStale(d) {
		t.Errorf("group topics %v read at %v after TasmoFleet's own GroupTopic write: they must count as unread", d.GroupTopics(), d.MoreGroupTopicsReadAt)
	}
}

// storeWithKnownGroupTopics opens a store with one device whose four group topics are read and known.
func storeWithKnownGroupTopics(t *testing.T) (*store.SQLite, model.DeviceID) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	id, err := st.CreateDevice(t.Context(), model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01", Topic: "plug"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateGroupTopics(t.Context(), id, store.ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}, "plug", "tasmotas", []string{"kitchen"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(t.Context(), id); groupTopicStale(d) {
		t.Fatalf("group topics %v read at %v: the rig must start with a known list", d.GroupTopics(), d.MoreGroupTopicsReadAt)
	}
	return st, id
}
