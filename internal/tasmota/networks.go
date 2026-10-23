package tasmota

import (
	"net/netip"
	"sync/atomic"
)

// NetworkSet is a device network allowlist that can be replaced while the
// process runs (the system settings page changes DEVICE_NETWORKS live). The
// client, the inventory and discovery share one set, so they always agree on
// where a device may be. The zero value allows nothing.
type NetworkSet struct {
	p atomic.Pointer[[]netip.Prefix]
}

// NewNetworkSet returns a set holding a copy of nets.
func NewNetworkSet(nets []netip.Prefix) *NetworkSet {
	s := &NetworkSet{}
	s.Set(nets)
	return s
}

// Set replaces the networks.
func (s *NetworkSet) Set(nets []netip.Prefix) {
	cp := append([]netip.Prefix(nil), nets...)
	s.p.Store(&cp)
}

// Get returns the current networks. The slice must not be modified.
func (s *NetworkSet) Get() []netip.Prefix {
	if s == nil {
		return nil
	}
	if p := s.p.Load(); p != nil {
		return *p
	}
	return nil
}
