package discovery

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// topicTaken records the devices the manager asked the router's check about through Options.OnTopicClaim.
type topicTaken struct {
	mu   sync.Mutex
	hits []string // "<device name>@<topic>"
}

// hook returns the callback; names says which device an ID is (filled once the devices are created).
func (r *topicTaken) hook(names map[model.DeviceID]string) func(model.DeviceID, string) {
	return func(id model.DeviceID, topic string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.hits = append(r.hits, names[id]+"@"+topic)
	}
}

func (r *topicTaken) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.hits)
}

// Why (#876): a board that announces itself on a stored device's topic may listen where the device's commands over MQTT
// go, and would then run the device's writes and transfers. Discovery hears that announcement but cannot tell a second
// board from a forged claim; only the router's check, which hears every reply on the topic, can. So discovery has to
// ask for that check, or the router keeps sending to the topic until some check happens to hear the other board. The
// device's own boot config is no claim (its own MACs would cost every device a check at every boot), and neither are
// a device stored without a MAC and another topic.
func TestAnotherMACAnnouncingAStoredDevicesTopicAsksTheRoutersCheck(t *testing.T) {
	const (
		lamp   = "02:00:5e:d2:2c:14"
		lampEt = "02:00:5e:d2:2c:15"
		other  = "02:00:5e:32:0b:94"
	)
	rows := []struct {
		name string
		mac  string
		ip   string
		top  string
		want []string
	}{
		{name: "another MAC on the topic", mac: "02005E433E52", ip: "172.31.13.50", top: "lamp", want: []string{"lamp@lamp"}},
		{name: "another MAC on the topic from outside the device networks", mac: "02005E433E52", ip: "10.1.1.1", top: "lamp", want: []string{"lamp@lamp"}},
		{name: "the MAC of another stored device on this device's topic", mac: "02005E320B94", ip: "172.31.13.32", top: "lamp", want: []string{"lamp@lamp"}},
		{name: "the device's own MAC", mac: "02005ED22C14", ip: "172.31.13.31", top: "lamp"},
		{name: "the device's own Ethernet MAC", mac: "02005ED22C15", ip: "172.31.13.31", top: "lamp"},
		{name: "another MAC on a topic nobody stored", mac: "02005E433E52", ip: "172.31.13.50", top: "stranger"},
		{name: "any MAC on the topic of a device stored without a MAC", mac: "02005E433E52", ip: "172.31.13.50", top: "nomac"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var taken topicTaken
			names := map[model.DeviceID]string{}
			e := newEnv(t, "172.31.0.0/16", func(o *Options) {
				o.OnTopicClaim = taken.hook(names)
			})
			for _, d := range []model.Device{
				{Name: "lamp", Address: "172.31.13.31", MAC: lamp, MACs: []string{lampEt}, Topic: "lamp"},
				{Name: "other", Address: "172.31.13.32", MAC: other, Topic: "other"},
				{Name: "nomac", Address: "172.31.13.33", Topic: "nomac"},
			} {
				id, err := e.inv.Create(bg, "t", d)
				if err != nil {
					t.Fatal(err)
				}
				names[id] = d.Name
			}
			e.mgr.handleMessage(bg, discoveryMsg(r.mac, r.ip, r.top))
			if got := taken.got(); !slices.Equal(got, r.want) {
				t.Fatalf("asked about %v, want %v", got, r.want)
			}
		})
	}
}

// Why (#876): asking for the check changes nothing about how the claim is judged. The device's record stays as it is
// and the conflict proposal stays the visible record of the other board.
func TestAClaimThatAsksTheCheckStillMakesTheConflictProposal(t *testing.T) {
	var marks int
	e := newEnv(t, "172.31.0.0/16", func(o *Options) { o.OnTopicClaim = func(model.DeviceID, string) { marks++ } })
	id, _ := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"})
	e.mgr.handleMessage(bg, discoveryMsg("02005E320B94", "172.31.13.50", "lamp"))
	if marks != 1 {
		t.Fatalf("asked %d times, want 1", marks)
	}
	d, _ := e.inv.Get(bg, id)
	if d.MAC != "02:00:5e:d2:2c:14" || d.Address != "172.31.13.31" {
		t.Fatalf("record changed: %+v", d)
	}
	if p := mustProposals(t, e); len(p) != 1 || p[0].Kind != KindConflict {
		t.Fatalf("proposals = %+v", p)
	}
}

// flakyList is an inventory whose List can be made to fail.
type flakyList struct {
	Inventory
	fail atomic.Bool
}

func (f *flakyList) List(ctx context.Context) ([]model.Device, error) {
	if f.fail.Load() {
		return nil, errors.New("db down")
	}
	return f.Inventory.List(ctx)
}

// Why (#876): a claim that cannot be compared with the stored devices must ask nothing: a check for a topic no stored
// device has costs a Status 5 window for nothing. The next sighting of the config tries again, and then asks (the same
// claim asks while the inventory is readable).
func TestATopicClaimAsksNothingWhileTheInventoryCannotBeRead(t *testing.T) {
	var marks atomic.Int32
	inv := &flakyList{}
	e := newEnv(t, "172.31.0.0/16", func(o *Options) {
		inv.Inventory = o.Inventory
		o.Inventory = inv
		o.OnTopicClaim = func(model.DeviceID, string) { marks.Add(1) }
	})
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	claim := candidate{mac: "02:00:5e:43:3e:52", topic: "lamp"}
	inv.fail.Store(true)
	e.mgr.noteTopicClaim(bg, claim)
	if n := marks.Load(); n != 0 {
		t.Fatalf("asked %d times with an unreadable inventory", n)
	}
	inv.fail.Store(false)
	e.mgr.noteTopicClaim(bg, claim)
	if n := marks.Load(); n != 1 {
		t.Fatalf("asked %d times once the inventory is readable again, want 1", n)
	}
}
