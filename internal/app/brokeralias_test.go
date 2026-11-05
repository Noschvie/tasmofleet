package app

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// noLookup resolves nothing: the HA app's broker name has no address outside the HA network.
func noLookup(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no DNS") }

// aliasFixture is a liveBroker for the Home Assistant app: TasmoFleet connects to
// core-broker:1883, the devices are configured with the HA host's LAN address.
type aliasFixture struct {
	mu   sync.Mutex
	now  time.Time
	devs []model.Device
	live map[model.DeviceID]time.Time
	b    *liveBroker
}

func newAliasFixture(devs ...model.Device) *aliasFixture {
	f := &aliasFixture{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), devs: devs, live: map[model.DeviceID]time.Time{}}
	f.b = &liveBroker{}
	f.b.p.Store(mqttbus.NewBrokerMatcher("tcp://core-broker:1883", noLookup)) // no DNS in unit tests
	f.b.aliases.Store(newBrokerAliases(
		func(context.Context) ([]model.Device, error) { return f.devs, nil },
		func(id model.DeviceID) time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.live[id] },
		func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	))
	return f
}

func (f *aliasFixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }
func (f *aliasFixture) heard(id model.DeviceID) { f.mu.Lock(); f.live[id] = f.now; f.mu.Unlock() }

func haDev(id model.DeviceID, host string) model.Device {
	return model.Device{ID: id, MQTTHost: host, MQTTPort: 1883, MQTT: model.MQTTOtherBroker}
}

// Why: the HA app's users enter core-broker, their devices use the HA host's address; names
// and resolved addresses never match, yet a device heard on our broker proves the address is
// ours (#597).
func TestBrokerAliasLearnedFromADeviceHeardThere(t *testing.T) {
	f := newAliasFixture(haDev(1, "192.168.1.10"), haDev(2, "192.168.1.10"))
	if f.b.Same("192.168.1.10", 1883) {
		t.Fatal("alias before any device was heard")
	}
	f.heard(1)
	f.advance(time.Minute) // the set is recomputed, not stored
	if !f.b.Same("192.168.1.10", 1883) {
		t.Fatal("a device configured for 192.168.1.10 is heard on our broker, so that address is ours")
	}
	if !f.b.Same("192.168.1.10", 0) {
		t.Fatal("port 0 is Tasmota's default 1883")
	}
	if f.b.Same("192.168.1.10", 1884) {
		t.Fatal("another port is another broker")
	}
}

// Why: a retained LWT outlives a device that moved to another broker; health reports no live
// traffic for it, so its new broker must not become an alias of ours.
func TestBrokerAliasNotFromADeviceThatWasNeverHeardLive(t *testing.T) {
	f := newAliasFixture(haDev(1, "other.lan"))
	f.advance(time.Minute)
	if f.b.Same("other.lan", 1883) {
		t.Fatal("a device without live traffic made its broker an alias")
	}
}

// Why: the window is the 2 hours of the "Other broker" hint (#580): a device on our broker sends at
// least every TelePeriod (at most 1 hour), and a device that moved away stays evidence that long.
// Literal times, so that a changed window fails here.
func TestBrokerAliasExpiresWhenNoDeviceIsHeardWithinTheWindow(t *testing.T) {
	f := newAliasFixture(haDev(1, "192.168.1.10"))
	f.heard(1)
	f.advance(time.Hour + 59*time.Minute)
	if !f.b.Same("192.168.1.10", 1883) {
		t.Fatal("heard 1 h 59 min ago: still evidence that the address is ours")
	}
	f.advance(2 * time.Minute) // more than aliasRefresh: the set is recomputed
	if f.b.Same("192.168.1.10", 1883) {
		t.Fatal("heard 2 h 1 min ago: the alias outlived its evidence, the device may have moved away")
	}
}

// Why: what the probe decides is stored, the store keeps "ours" over a later "other", and devicecmd
// sends Power over MQTT, without a fallback, to every device stored as ours. An alias is evidence
// about other devices and can be wrong for a while: device 999 is stored with other.lan and heard on
// our broker (it moved away to other.lan within the window, or it moved here and its stored host is
// stale). A device that really reports to other.lan must stay "other" when it is added and when it
// is re-read (#598); only what is shown follows the alias. Built through openServices, so the test
// fails when the Prober is handed the alias-aware broker again.
func TestBrokerAliasNeverMakesAProbedDeviceOurs(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M,
		Settings: map[string]string{"mqtthost": "other.lan"}})
	if err != nil {
		t.Fatal(err)
	}
	svc := importServices(t)
	svc.Broker.p.Store(mqttbus.NewBrokerMatcher("tcp://core-broker:1883", noLookup)) // no DNS in unit tests
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc.Broker.aliases.Store(newBrokerAliases(
		func(context.Context) ([]model.Device, error) {
			return []model.Device{{ID: 999, MQTTHost: "other.lan", MQTTPort: 1883}}, nil
		},
		func(id model.DeviceID) time.Time {
			if id == 999 {
				return now
			}
			return time.Time{}
		},
		func() time.Time { return now },
	))
	if !svc.Broker.Same("other.lan", 1883) {
		t.Fatal("precondition: other.lan is an alias for what is shown")
	}
	host, port := splitAddr(t, sim.Addr())
	desc, err := svc.Prober.Describe(t.Context(), model.Device{Address: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if desc.MQTTHost != "other.lan" || desc.MQTT != model.MQTTOtherBroker {
		t.Fatalf("adding the device: host %q, capability %q, want other.lan and %q", desc.MQTTHost, desc.MQTT, model.MQTTOtherBroker)
	}
	id, err := svc.Store.CreateDevice(t.Context(), desc)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Prober.ReadBroker(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if d, _ := svc.Store.GetDevice(t.Context(), id); d.MQTT != model.MQTTOtherBroker {
		t.Fatalf("after the broker re-read: capability %q, want %q", d.MQTT, model.MQTTOtherBroker)
	}
}

func TestBrokerAliasDoesNotMakeUnrelatedBrokersOurs(t *testing.T) {
	f := newAliasFixture(haDev(1, "192.168.1.10"), haDev(2, "other.lan"))
	f.heard(1)
	f.advance(time.Minute)
	if f.b.Same("other.lan", 1883) {
		t.Fatal("an unrelated broker became ours")
	}
	if !f.b.Same("core-broker", 1883) {
		t.Fatal("the configured name stopped matching")
	}
}

// Why: no MQTT URL means there is no broker of ours; evidence must not create one.
func TestBrokerAliasNeedsAConfiguredBroker(t *testing.T) {
	f := newAliasFixture(haDev(1, "192.168.1.10"))
	f.b.p.Store(mqttbus.NewBrokerMatcher("", noLookup))
	f.heard(1)
	f.advance(time.Minute)
	if f.b.Same("192.168.1.10", 1883) {
		t.Fatal("alias without a broker")
	}
}

func TestBrokerAliasHostIsCaseAndDotInsensitive(t *testing.T) {
	f := newAliasFixture(haDev(1, "MQTT.Example."))
	f.heard(1)
	f.advance(time.Minute)
	if !f.b.Same("mqtt.example", 1883) {
		t.Fatal("host spelling must not matter")
	}
}
