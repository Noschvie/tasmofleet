package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// edits runs a function (the "admin") at the first command of a probe, i.e.
// while the slow probe is in flight.
type edits struct {
	tasmota.Client
	once bool
	do   func()
}

func (e *edits) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if !e.once && e.do != nil {
		e.once = true
		e.do()
	}
	return e.Client.Command(ctx, t, c)
}

func raceSetup(t *testing.T) (*Prober, *store.SQLite, model.DeviceID, model.Device, *edits) {
	t.Helper()
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
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
	d.Name, d.MAC = "plug", "aa:bb:cc:00:00:01"
	id, err := st.CreateDevice(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	d.ID = id
	// The device answers Status 5 with the stored MAC: it is the stored device (#65).
	e := &edits{Client: override{Client: p.Client, reply: map[string]string{"Status 5": status5Net}}}
	p.Client, p.Devices = e, st
	return p, st, id, d, e
}

// Why: a probe takes seconds. An admin edit made meanwhile (notes, OTA
// exclusion, new password) must survive; the probe only owns identity and
// MQTT facts.
func TestRefreshKeepsConcurrentAdminEdits(t *testing.T) {
	p, st, id, d, e := raceSetup(t)
	e.do = func() {
		cur, _ := st.GetDevice(context.Background(), id)
		cur.Notes, cur.OTAExcluded, cur.HTTPPassword = "edited", true, "new-pass"
		if err := st.UpdateDevice(context.Background(), cur); err != nil {
			t.Error(err)
		}
	}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Notes != "edited" || !got.OTAExcluded || got.HTTPPassword != "new-pass" {
		t.Fatalf("admin edit lost: %+v", got)
	}
	if got.Identity.Version == "" || got.MQTTHost != "mqtt.example.internal" || got.Address != d.Address {
		t.Fatalf("probe result missing: %+v", got)
	}
}

// Why: if the device was moved to another address meanwhile, the answers came
// from the wrong machine and must not be recorded for it.
func TestRefreshDiscardsResultWhenAddressChanged(t *testing.T) {
	p, st, id, _, e := raceSetup(t)
	e.do = func() {
		cur, _ := st.GetDevice(context.Background(), id)
		cur.Address = "127.0.0.2"
		if err := st.UpdateDevice(context.Background(), cur); err != nil {
			t.Error(err)
		}
	}
	err := p.Refresh(context.Background(), id, "")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Identity.Version != "" || got.MQTTHost != "" || got.Address != "127.0.0.2" {
		t.Fatalf("stale probe result stored: %+v", got)
	}
}

func TestRefreshDiscardsResultWhenMACReboundOrDeleted(t *testing.T) {
	for name, mutate := range map[string]func(st *store.SQLite, id model.DeviceID){
		"mac rebound": func(st *store.SQLite, id model.DeviceID) {
			cur, _ := st.GetDevice(context.Background(), id)
			cur.MAC = "aa:bb:cc:00:00:99"
			_ = st.UpdateDevice(context.Background(), cur) // test setup
		},
		"deleted": func(st *store.SQLite, id model.DeviceID) { _ = st.DeleteDevice(context.Background(), id) }, // test setup
	} {
		t.Run(name, func(t *testing.T) {
			p, st, id, _, e := raceSetup(t)
			e.do = func() { mutate(st, id) }
			if err := p.Refresh(context.Background(), id, ""); err == nil {
				t.Fatal("stale probe result accepted")
			}
			if got, err := st.GetDevice(context.Background(), id); err == nil && got.Identity.Version != "" {
				t.Fatalf("stale probe result stored: %+v", got)
			}
		})
	}
}

// Messages seen on our broker beat a failed name match.
func TestRefreshKeepsOursWhenProbeSaysOther(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	cur, _ := st.GetDevice(context.Background(), id)
	cur.MQTT = model.MQTTOurs
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	p.Broker = fixedBroker{"other.lan"}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.MQTT != model.MQTTOurs || got.MQTTHost == "" {
		t.Fatalf("%+v", got)
	}
}

// Why: a refresh and the background network read store identity and network facts without
// touching the inventory, so open pages (device table: firmware, IP, module) never hear of it.
// An operator watching the fleet page must see a new device's properties without reloading,
// so both writes announce the device on the hub (the table reloads on "device") (#46).
func TestRefreshAndReadNetAnnounceTheDevice(t *testing.T) {
	p, _, id, _, _ := raceSetup(t)
	hub := events.NewHub(8)
	p.Hub = hub
	ch, cancel := hub.Subscribe(events.TopicDevice)
	defer cancel()
	announced := func(step string) {
		t.Helper()
		select {
		case ev := <-ch:
			if d, ok := ev.Payload.(model.Device); !ok || d.ID != id || d.HTTPPassword != "" {
				t.Fatalf("%s: payload %+v", step, ev.Payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: no device event, open pages keep the old row until a reload", step)
		}
	}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	announced("refresh")
	if err := p.ReadNet(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	announced("read net")
}
