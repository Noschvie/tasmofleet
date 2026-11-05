package app

import (
	"context"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// Broker aliases (#597). The matcher compares names and resolved addresses, so it cannot see that
// the Home Assistant app's broker name, the HA host's LAN address (what the devices use) and a
// Docker Compose service name are one broker. Evidence settles it: a broker host:port counts as
// ours when a device stored with that host:port is heard on our broker right now. Nothing is
// stored (a patch release has no migration); the set is derived from the inventory and the health
// tracker and recomputed every aliasRefresh. It changes only what is shown (liveBroker.Same), never
// the stored capability (configuredBroker).
const (
	// aliasEvidenceWindow is how recent a device's traffic must be to count. It is the 2 hours of
	// the "Other broker" hint (web silentOnOurBroker, #580): a device on our broker sends telemetry
	// at least every TelePeriod (at most 1 hour), so a device silent for longer is no proof, and
	// one that moved away stops being evidence on the same schedule as it stops being "heard".
	aliasEvidenceWindow = 2 * time.Hour
	// aliasRefresh bounds how often the set is recomputed: Same runs for every device page and
	// probe, and the set needs one inventory read.
	aliasRefresh = 30 * time.Second
	// aliasListTimeout bounds the inventory read.
	aliasListTimeout = 2 * time.Second
)

type brokerAliases struct {
	list   func(context.Context) ([]model.Device, error)
	liveAt func(model.DeviceID) time.Time // last live (not retained) MQTT message of a device
	now    func() time.Time

	mu  sync.Mutex
	set map[string]struct{}
	at  time.Time
}

func newBrokerAliases(list func(context.Context) ([]model.Device, error), liveAt func(model.DeviceID) time.Time, now func() time.Time) *brokerAliases {
	return &brokerAliases{list: list, liveAt: liveAt, now: now}
}

// has reports whether host:port (port 0 = 1883) is a broker that devices are heard on.
func (a *brokerAliases) has(host string, port int) bool {
	if a == nil {
		return false
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.set == nil || now.Sub(a.at) >= aliasRefresh {
		a.refresh(now)
	}
	_, ok := a.set[mqttbus.HostKey(host, port)]
	return ok
}

// refresh recomputes the set (caller holds a.mu). A failed inventory read keeps the previous set
// until the next refresh: the evidence is old, but the window still bounds it.
func (a *brokerAliases) refresh(now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), aliasListTimeout)
	defer cancel()
	devs, err := a.list(ctx)
	a.at = now
	if err != nil && a.set != nil {
		return
	}
	set := map[string]struct{}{}
	if err == nil {
		for _, d := range devs {
			if d.MQTTHost == "" {
				continue
			}
			if at := a.liveAt(d.ID); !at.IsZero() && now.Sub(at) < aliasEvidenceWindow {
				set[mqttbus.HostKey(d.MQTTHost, d.MQTTPort)] = struct{}{}
			}
		}
	}
	a.set = set
}
