package discovery

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Explain answers one question of the Add page (#713): why is the device behind this MAC, IP address
// or MQTT topic not offered as new? It reads the inventory, the stored proposals and the topic table
// and stores nothing: no mark, no cache, no timer. It sends nothing to a device or the broker.

// maxLookupLen bounds a lookup text; a topic is at most maxTopicLen, a MAC or an IP much less.
const maxLookupLen = 128

// ErrBadLookup is a lookup text that names nothing the page can look for (empty, longer than
// maxLookupLen or with control characters). The page words the refusal itself.
var ErrBadLookup = errors.New("discovery: not a MAC, IP address or MQTT topic to look up")

// What a Finding says.
const (
	// FindingInDeviceList: the device is in the device list (Finding.DeviceID, DeviceName, By).
	FindingInDeviceList = "in-device-list"
	// FindingIgnored: a proposal about the device is ignored (Finding.Proposal); Restore brings it back.
	FindingIgnored = "ignored"
	// FindingListed: a proposal about the device is open, under New devices, Address changed or
	// Conflicts, by Finding.Proposal.Kind.
	FindingListed = "listed"
	// FindingUnidentified: its topic is on the broker, but its MAC is not known (Finding.Unidentified.Reason).
	FindingUnidentified = "unidentified"
)

// Finding is one place the device was found.
type Finding struct {
	Kind string
	// DeviceID and DeviceName are the stored record of FindingInDeviceList; By says what matched
	// it ("MAC", "topic" or "address"), in the order discovery itself looks.
	DeviceID   model.DeviceID
	DeviceName string
	By         string
	// Proposal is the proposal of FindingIgnored and FindingListed, as the Add page shows it.
	Proposal Proposal
	// Unidentified is the topic of FindingUnidentified.
	Unidentified Unidentified
}

// Explanation lists the findings, most telling first: the device list, ignored, listed, not identified.
// No finding means TasmoFleet knows nothing about the lookup text (since its start for the topic side,
// in the last ProposalTTL for the proposals).
type Explanation struct {
	Findings []Finding
}

func (k matchKind) String() string {
	switch k {
	case matchMAC:
		return "MAC"
	case matchTopic:
		return "topic"
	case matchAddress:
		return "address"
	case matchNone:
	}
	return ""
}

// lookup is what one lookup text stands for after the topic table named the facts it knows.
type lookup struct {
	macs, topics, ips map[string]struct{}
	cands             []candidate
}

// Explain looks the text up (a MAC, else an IP address, else an MQTT topic, matched exactly like
// discovery itself matches a claim, see match). It never changes anything; an inventory or store error
// is returned as it is, a text that cannot be looked up as ErrBadLookup.
func (m *Manager) Explain(ctx context.Context, query string) (Explanation, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > maxLookupLen || strings.IndexFunc(query, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return Explanation{}, ErrBadLookup
	}
	lk := m.expand(query)
	devs, err := m.o.Inventory.List(ctx)
	if err != nil {
		return Explanation{}, err
	}
	var out Explanation
	seen := map[model.DeviceID]bool{}
	for _, c := range lk.cands {
		if d, how := match(devs, c); how != matchNone && !seen[d.ID] {
			seen[d.ID] = true
			out.Findings = append(out.Findings, Finding{Kind: FindingInDeviceList, DeviceID: d.ID, DeviceName: d.Name, By: how.String()})
		}
	}
	if err := m.explainProposals(ctx, lk, &out); err != nil {
		return Explanation{}, err
	}
	for _, u := range m.Unidentified() {
		if _, ok := lk.topics[u.Topic]; ok || (u.IP != "" && hasKey(lk.ips, u.IP)) {
			out.Findings = append(out.Findings, Finding{Kind: FindingUnidentified, Unidentified: u})
		}
	}
	rank := map[string]int{FindingInDeviceList: 0, FindingIgnored: 1, FindingListed: 2, FindingUnidentified: 3}
	sort.SliceStable(out.Findings, func(i, j int) bool { return rank[out.Findings[i].Kind] < rank[out.Findings[j].Kind] })
	return out, nil
}

func hasKey(m map[string]struct{}, k string) bool { _, ok := m[k]; return ok }

// expand turns the lookup text into the MACs, topics and IPs it stands for. The topic table is asked
// once (an entry whose MAC, IP or topic is the text gives all three, newest sighting first), then
// released: nothing else is called with tmu held.
func (m *Manager) expand(query string) lookup {
	lk := lookup{macs: map[string]struct{}{}, topics: map[string]struct{}{}, ips: map[string]struct{}{}}
	var q candidate
	var by func(topic string, s *topicState) bool
	if mac, err := inventory.NormaliseMAC(query); err == nil && mac != "" {
		q.mac = mac
		by = func(_ string, s *topicState) bool { return s.mac == mac }
	} else if ip, err := netip.ParseAddr(query); err == nil {
		q.ip = ip.Unmap()
		by = func(_ string, s *topicState) bool { return s.ip == q.ip.String() }
	} else {
		q.topic = query
		by = func(topic string, _ *topicState) bool { return topic == query }
	}

	type entry struct {
		topic, mac, ip string
		seen           time.Time
	}
	var hits []entry
	m.tmu.Lock()
	for t, s := range m.topics {
		if by(t, s) {
			hits = append(hits, entry{t, s.mac, s.ip, s.seen})
		}
	}
	m.tmu.Unlock()
	// Several entries can share one IP (a lease handed on, a stale entry next to a fresh one): newest
	// first, the topic as the tie-break, so the answer does not depend on map order.
	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].seen.Equal(hits[j].seen) {
			return hits[i].seen.After(hits[j].seen)
		}
		return hits[i].topic < hits[j].topic
	})
	for _, h := range hits {
		c := candidate{mac: h.mac, topic: h.topic, ip: q.ip}
		if ip, err := netip.ParseAddr(h.ip); err == nil && !c.ip.IsValid() {
			c.ip = ip.Unmap()
		}
		lk.cands = append(lk.cands, c)
	}
	if len(lk.cands) == 0 {
		lk.cands = []candidate{q}
	}
	for _, c := range lk.cands {
		if c.mac != "" {
			lk.macs[c.mac] = struct{}{}
		}
		if c.topic != "" {
			lk.topics[c.topic] = struct{}{}
		}
		if c.ip.IsValid() {
			lk.ips[c.ip.String()] = struct{}{}
		}
	}
	return lk
}

// explainProposals adds the open and the ignored proposals of the device, as the Add page lists them
// (visible drops the rows the inventory has overtaken, as it does at every render of the page).
func (m *Manager) explainProposals(ctx context.Context, lk lookup, out *Explanation) error {
	for _, src := range []struct {
		kind string
		list func(context.Context) ([]store.ProposalRow, error)
	}{{FindingListed, m.o.Store.ListProposals}, {FindingIgnored, m.o.Store.ListDismissedProposals}} {
		rows, err := src.list(ctx)
		if err != nil {
			return err
		}
		props, err := m.visible(ctx, rows)
		if err != nil {
			return err
		}
		for _, p := range props {
			d := p.Device
			_, byMAC := lk.macs[d.MAC]
			_, byTopic := lk.topics[d.Topic]
			if byMAC || (d.Topic != "" && byTopic) || hasKey(lk.ips, d.Address) {
				out.Findings = append(out.Findings, Finding{Kind: src.kind, Proposal: p})
			}
		}
	}
	return nil
}
