package tasmotacmd

import (
	"sort"
	"strconv"
	"strings"
)

// FleetDevice is what the check needs to know of a device.
type FleetDevice struct {
	Name    string
	Version string // "16.0.0", "16.0.0.3"; empty or unreadable: the device is not judged
	Env
}

// DeviceNeeds is a device whose build certainly lacks what a command needs.
type DeviceNeeds struct {
	Device string
	Needs  []string // "USE_SUNRISE", "ESP32", ...
}

// VersionVerdict is the result for the devices on one firmware version.
type VersionVerdict struct {
	Version string // the version the devices report, three fields
	Pinned  string // the catalogue version that judged it
	// Older is the pinned catalogue just below Pinned that also lacked the command, set when the
	// version has no pin of its own and "not a command" rests on the two pins around it.
	Older   string
	Match   Match    // how that catalogue stands to Version
	Devices []string // names, sorted
	// Exists: the command (with its index) exists in the catalogue. False is only said when it
	// is sure, see MatchBeyond; Unknown then.
	Exists Tri
	// IndexBad: the command exists but the typed index is outside the range the source states.
	// The range is read heuristically, so this is a warning and never a reason to refuse.
	IndexBad bool
	// Lacking lists the devices whose build lacks a feature; empty unless Exists is True.
	Lacking []DeviceNeeds
}

// Result is the check of one command against the fleet.
type Result struct {
	// Versions has one entry per firmware version in the fleet, newest first.
	Versions []VersionVerdict
	// Refuse is true when the command name exists in none of the versions in the fleet and every
	// device's version was judged (an index out of range does not count, see VersionVerdict.IndexBad;
	// a device without a readable version might know the command). The check
	// warns about everything else and lets the rule be saved.
	Refuse bool
	// Unjudged counts the devices whose version is unknown.
	Unjudged int
	// Range is the index range of the command in the newest judging catalogue; Typed is the index typed.
	Range struct {
		Lo, Hi int
		OK     bool // the command takes an index with a stated range
		Typed  int
		Has    bool // an index was typed
	}
	// Name is the command as the table spells it, "" when no catalogue knows it.
	Name string
}

// Check judges a typed command against the firmware versions of the fleet. Devices without a
// readable version are counted, not judged. With no judged device there is nothing to refuse.
func (s *Set) Check(cmd string, fleet []FleetDevice) Result {
	var res Result
	type group struct {
		ver  [3]int
		devs []FleetDevice
	}
	groups := map[[3]int]*group{}
	for _, d := range fleet {
		v, ok := ParseVersion(d.Version)
		if !ok {
			res.Unjudged++
			continue
		}
		g := groups[v]
		if g == nil {
			g = &group{ver: v}
			groups[v] = g
		}
		g.devs = append(g.devs, d)
	}
	var gs []*group
	for _, g := range groups {
		gs = append(gs, g)
	}
	sort.Slice(gs, func(i, j int) bool { return cmpVer(gs[i].ver, gs[j].ver) > 0 })
	var rangeFrom *Catalogue
	for _, g := range gs {
		cat, m, _ := s.For(versionString(g.ver))
		vv := VersionVerdict{Version: versionString(g.ver), Pinned: cat.Tasmota, Match: m}
		f, found := cat.Find(cmd)
		switch {
		case found:
			vv.Exists = True
			vv.IndexBad = !f.IndexOK()
		case m == MatchExact:
			vv.Exists = False
		case m == MatchNewer:
			// no pin of its own: missing counts only when the next older pin lacks it too
			if old := s.Older(cat); old != nil {
				if _, inOld := old.Find(cmd); !inOld {
					vv.Exists, vv.Older = False, old.Tasmota
				}
			}
		}
		if found && rangeFrom == nil {
			rangeFrom = cat
			res.Name = f.Name
			res.Range.Typed, res.Range.Has = f.Index, f.HasIndex
			res.Range.Lo, res.Range.Hi, res.Range.OK = f.IndexRange()
		}
		for _, d := range g.devs {
			vv.Devices = append(vv.Devices, d.Name)
			if vv.Exists == True {
				if need := cat.Lacks(f, d.Env); len(need) > 0 {
					vv.Lacking = append(vv.Lacking, DeviceNeeds{Device: d.Name, Needs: need})
				}
			}
		}
		sort.Strings(vv.Devices)
		sort.Slice(vv.Lacking, func(i, j int) bool { return vv.Lacking[i].Device < vv.Lacking[j].Device })
		res.Versions = append(res.Versions, vv)
	}
	res.Refuse = len(res.Versions) > 0 && res.Unjudged == 0
	for _, v := range res.Versions {
		if v.Exists != False {
			res.Refuse = false
		}
	}
	if rangeFrom == nil { // unknown everywhere: describe it from the newest catalogue (index range and name stay empty)
		if f, ok := s.Newest().Find(cmd); ok {
			res.Name = f.Name
			res.Range.Typed, res.Range.Has = f.Index, f.HasIndex
			res.Range.Lo, res.Range.Hi, res.Range.OK = f.IndexRange()
		}
	}
	return res
}

func versionString(v [3]int) string {
	return strings.Join([]string{strconv.Itoa(v[0]), strconv.Itoa(v[1]), strconv.Itoa(v[2])}, ".")
}
