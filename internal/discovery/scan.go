package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// MaxScanAddresses is the most addresses all CIDRs of one scan may span.
const MaxScanAddresses = 1024

const keepScans = 5 // finished scans kept for display

type scan struct {
	mu     sync.Mutex
	st     ScanStatus
	cancel context.CancelFunc
}

func (s *scan) snapshot() ScanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.st
	out.CIDRs = append([]netip.Prefix(nil), s.st.CIDRs...)
	out.Hits = append([]Hit(nil), s.st.Hits...)
	return out
}

// planScan checks the ranges and returns the addresses to probe. It sends
// nothing. Every CIDR must lie inside one device network and all together
// may span MaxScanAddresses. Addresses the dialer would refuse (loopback and
// link-local unless a device network names them explicitly, multicast,
// unspecified, metadata) are skipped, as are the network and broadcast
// address of every CIDR of /30 or wider.
func planScan(cidrs []netip.Prefix, nets []netip.Prefix) ([]netip.Addr, error) {
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("%w: no range given", ErrScanRange)
	}
	var total uint64
	for _, p := range cidrs {
		if !p.IsValid() {
			return nil, fmt.Errorf("%w: invalid range", ErrScanRange)
		}
		p = p.Masked()
		inside := false
		for _, n := range nets {
			if n.Addr().Is4() == p.Addr().Is4() && p.Bits() >= n.Bits() && n.Contains(p.Addr()) {
				inside = true
				break
			}
		}
		if !inside {
			return nil, fmt.Errorf("%w: %s is outside the device networks", ErrScanRange, p)
		}
		host := p.Addr().BitLen() - p.Bits()
		if host > 10 {
			return nil, fmt.Errorf("%w: %s spans more than %d addresses", ErrScanRange, p, MaxScanAddresses)
		}
		total += 1 << host
		if total > MaxScanAddresses {
			return nil, fmt.Errorf("%w: more than %d addresses in total", ErrScanRange, MaxScanAddresses)
		}
	}
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, p := range cidrs {
		p = p.Masked()
		first := p.Addr()
		skipEnds := p.Addr().BitLen()-p.Bits() >= 2
		for a, i := first, uint64(0); p.Contains(a); a, i = a.Next(), i+1 {
			last := !a.Next().IsValid() || !p.Contains(a.Next())
			if skipEnds && (i == 0 || last) {
				continue
			}
			if u := a.Unmap(); seen[u] || !tasmota.IPAllowed(u, nets) {
				continue
			}
			seen[a.Unmap()] = true
			out = append(out, a.Unmap())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no scannable address in the given ranges", ErrScanRange)
	}
	return out, nil
}

// StartScan implements Service.
func (m *Manager) StartScan(ctx context.Context, actor string, cidrs []netip.Prefix) (ScanID, error) {
	if m.o.Client == nil {
		return "", errors.New("discovery: scanning is not configured")
	}
	addrs, err := planScan(cidrs, m.networks()) // before any packet, and before any state
	if err != nil {
		m.audit(ctx, actor, "discovery.scan_refused", "", map[string]any{"cidrs": prefixStrings(cidrs)}, err)
		return "", err
	}
	m.smu.Lock()
	defer m.smu.Unlock()
	if m.base == nil {
		return "", ErrNotRunning
	}
	for _, s := range m.scans {
		if s.snapshot().State == ScanRunning {
			return "", ErrScanRunning
		}
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := ScanID(hex.EncodeToString(raw[:]))
	sctx, cancel := context.WithCancel(m.base)
	s := &scan{cancel: cancel, st: ScanStatus{
		ID: id, Actor: actor, CIDRs: append([]netip.Prefix(nil), cidrs...), State: ScanRunning,
		Total: len(addrs) * len(m.o.Ports), StartedAt: m.clk.Now(),
	}}
	m.scans[id] = s
	m.scanList = append(m.scanList, id)
	for len(m.scanList) > keepScans { // drop the oldest finished one
		old := m.scanList[0]
		m.scanList = m.scanList[1:]
		delete(m.scans, old)
	}
	m.audit(ctx, actor, "discovery.scan_start", "scan:"+string(id),
		map[string]any{"cidrs": prefixStrings(cidrs), "probes": s.st.Total}, nil)
	go m.runScan(sctx, s, addrs)
	return id, nil
}

func prefixStrings(ps []netip.Prefix) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

// CancelScan implements Service.
func (m *Manager) CancelScan(ctx context.Context, actor string, id ScanID) error {
	m.smu.Lock()
	s, ok := m.scans[id]
	m.smu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if s.snapshot().State != ScanRunning {
		return nil
	}
	s.cancel()
	m.audit(ctx, actor, "discovery.scan_cancel", "scan:"+string(id), map[string]any{}, nil)
	return nil
}

// Scan returns a snapshot of one scan.
func (m *Manager) Scan(id ScanID) (ScanStatus, bool) {
	m.smu.Lock()
	s, ok := m.scans[id]
	m.smu.Unlock()
	if !ok {
		return ScanStatus{}, false
	}
	return s.snapshot(), true
}

// Scans returns snapshots of the kept scans, newest first.
func (m *Manager) Scans() []ScanStatus {
	m.smu.Lock()
	ids := append([]ScanID(nil), m.scanList...)
	m.smu.Unlock()
	var out []ScanStatus
	for i := len(ids) - 1; i >= 0; i-- {
		if st, ok := m.Scan(ids[i]); ok {
			out = append(out, st)
		}
	}
	return out
}

type probeTarget struct {
	ip   netip.Addr
	port int
}

func (m *Manager) runScan(ctx context.Context, s *scan, addrs []netip.Addr) {
	jobs := make(chan probeTarget)
	var wg sync.WaitGroup
	for range m.o.Concurrency {
		wg.Go(func() {
			for t := range jobs {
				hit, found := m.probe(ctx, t)
				s.mu.Lock()
				if ctx.Err() == nil {
					s.st.Done++
				}
				if found {
					s.st.Hits = append(s.st.Hits, hit)
				}
				s.mu.Unlock()
			}
		})
	}
feed:
	for _, ip := range addrs {
		for _, port := range m.o.Ports {
			select {
			case jobs <- probeTarget{ip, port}:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wg.Wait()
	s.mu.Lock()
	s.st.FinishedAt = m.clk.Now()
	s.st.State = ScanDone
	if ctx.Err() != nil {
		s.st.State = ScanCancelled
	}
	hits, state := len(s.st.Hits), s.st.State
	s.mu.Unlock()
	s.cancel()
	m.audit(context.WithoutCancel(ctx), s.st.Actor, "discovery.scan_end", "scan:"+string(s.st.ID),
		map[string]any{"state": string(state), "hits": hits}, nil)
}

// probe asks one address for Status 2 and, if a Tasmota answers, Status 5.
// found is false for silent addresses and non-Tasmota hosts.
func (m *Manager) probe(ctx context.Context, t probeTarget) (Hit, bool) {
	dev := model.Device{Address: t.ip.String(), Port: t.port} // no credentials, ever
	target := tasmota.TargetFor(dev)
	hit := Hit{Address: dev.Address, Port: t.port}
	raw2, err := m.command(ctx, target, "Status 2")
	switch {
	case err == nil:
	case errors.Is(err, tasmota.ErrAuth):
		hit.Outcome = "needs password"
		return hit, true
	default:
		return hit, false
	}
	fwr, err := tasmota.ParseStatus2(raw2)
	if err != nil {
		return hit, false
	}
	hit.Version = cleanName(fwr.Version, "00:00:00:00:00:00")
	raw5, err := m.command(ctx, target, "Status 5")
	if err != nil {
		hit.Outcome = "error"
		return hit, true
	}
	var s5 struct {
		StatusNET struct {
			Hostname, Mac string
			Ethernet      struct{ Mac string }
		}
	}
	mac, ethMAC := "", ""
	if json.Unmarshal(raw5, &s5) == nil {
		mac, _ = inventory.NormaliseMAC(s5.StatusNET.Mac)
		ethMAC, _ = inventory.NormaliseMAC(s5.StatusNET.Ethernet.Mac)
	}
	if mac == "" {
		hit.Outcome = outIgnored
		return hit, true
	}
	// The name is Tasmota's DeviceName, as MQTT discovery does; the hostname only when the device
	// has none or does not answer plain "Status" (the Status object, not the heavy "Status 0").
	devName := ""
	if raw, err := m.command(ctx, target, "Status"); err == nil {
		var s struct{ Status struct{ DeviceName string } }
		if json.Unmarshal(raw, &s) == nil {
			devName = s.Status.DeviceName
		}
	}
	c := candidate{via: ViaScan, mac: mac, ip: t.ip, port: t.port, name: cleanName(firstNonEmpty(devName, s5.StatusNET.Hostname), mac)}
	c.host = cleanHostname(s5.StatusNET.Hostname)
	if ethMAC != "" && ethMAC != mac && ethMAC != "00:00:00:00:00:00" {
		c.macs = []string{ethMAC}
	}
	hit.MAC, hit.Name = mac, c.name
	out, err := m.consider(ctx, c)
	if err != nil {
		m.log.Warn("discovery: could not evaluate scan hit", "address", hit.Address, "err", err)
		out = "error"
	}
	hit.Outcome = out
	return hit, true
}

// command sends one command with the probe timeout and retries a timeout
// once (a busy ESP8266 drops a request now and then). A refused connection
// or an authentication answer is final.
func (m *Manager) command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		actx, cancel := context.WithTimeout(ctx, m.o.ProbeTimeout)
		var raw json.RawMessage
		raw, err = m.o.Client.Command(actx, t, cmnd)
		cancel()
		if err == nil || !errors.Is(err, tasmota.ErrTimeout) && !errors.Is(err, context.DeadlineExceeded) {
			return raw, err
		}
	}
	return nil, err
}
