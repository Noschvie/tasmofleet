package app

import (
	"context"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// knownIndex answers mqttbus.Options.Known from an in-memory copy of the
// inventory's MQTT topics and MACs, so the bus never queries the database
// for every message (a junk flood would otherwise become a database flood).
type knownIndex struct {
	mu   sync.RWMutex
	keys map[string]bool // mqttbus.DeviceKey values of inventory devices
}

func newKnownIndex() *knownIndex { return &knownIndex{keys: map[string]bool{}} }

func (k *knownIndex) set(devs []model.Device) {
	keys := make(map[string]bool, 2*len(devs))
	for _, d := range devs {
		if d.Topic != "" {
			keys[mqttbus.TopicKey(d.Topic)] = true
		}
		for _, mac := range append([]string{d.MAC}, d.MACs...) {
			if m := mqttbus.MACKey(mac); m != "" {
				keys[m] = true
			}
		}
	}
	k.mu.Lock()
	k.keys = keys
	k.mu.Unlock()
}

// Known reports whether m belongs to an inventory device. It uses
// mqttbus.DeviceKey, the same key the bus reserves cache space under.
func (k *knownIndex) Known(m mqttbus.Message) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.keys[mqttbus.DeviceKey(m)]
}

// runKnownIndex keeps the index current: at start, on every inventory change
// event, and every minute as a safety net.
func (a *runtime) runKnownIndex(ctx context.Context) error {
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		devs, err := a.svc.Inventory.List(rctx)
		if err != nil {
			// Fail closed: without a readable inventory no topic gets a
			// device reservation (docs/security/mqtt-ingest.md).
			a.log.Warn("mqtt known-device index refresh failed; all topics treated as unknown", "err", err)
			devs = nil
		}
		a.mqttKnown.set(devs)
		if r, ok := a.bus.(mqttbus.Reclassifier); ok {
			r.Reclassify()
		}
	}
	refresh()
	ch, unsub := a.svc.Hub.Subscribe(events.TopicDevice)
	defer unsub()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			refresh()
		case <-t.C:
			refresh()
		}
	}
}
