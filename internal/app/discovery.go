package app

import (
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// scanConcurrency and scanTimeout are the spec limits of the HTTP autoscan.
const (
	scanConcurrency = 16
	scanTimeout     = 2 * time.Second
)

// setupDiscovery starts MQTT discovery (with a broker) and prepares the
// admin-started HTTP scan. The scan has its own device client: the shared one
// allows 8 requests in flight and 10 s per request, the scan needs 16 and 2 s.
func (a *runtime) setupDiscovery() error {
	scanClient := tasmota.NewHTTPClient(tasmota.Options{
		Networks: a.svc.Networks, Timeout: scanTimeout, MaxConcurrent: scanConcurrency, Logger: a.log,
	})
	m, err := discovery.New(discovery.Options{
		Inventory: a.svc.Inventory, Store: a.svc.Store, Audit: a.svc.Audit, Client: scanClient,
		Verifier: macVerifier{client: a.svc.Client}, NetworkSet: a.svc.Networks,
		Concurrency: scanConcurrency, ProbeTimeout: scanTimeout, Logger: a.log,
		OnDropped: func(kind string) { a.svc.Std.DiscoveryDropped.With(kind).Inc() },
	})
	if err != nil {
		return err
	}
	a.discovery = m
	a.discoveryFollowsSetting()
	a.goBG("discovery", func() error { return m.Run(a.bgContext) })
	return nil
}
