package firmware

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Position is where a device's running version stands against a release.
type Position string

// Positions.
const (
	PosBelow   Position = "below"   // the device runs an older version: an update is possible
	PosAt      Position = "at"      // same version
	PosAbove   Position = "above"   // the device runs a newer version
	PosUnknown Position = "unknown" // version not detected yet
)

// ComparePosition places the running version of id against release.
func ComparePosition(release string, id model.Identity) Position {
	if id.Version == "" {
		return PosUnknown
	}
	switch c := compareVersions(id.Version, release); {
	case c < 0:
		return PosBelow
	case c > 0:
		return PosAbove
	default:
		return PosAt
	}
}

// Latest returns the newest release version of rels (the "recommended" one;
// the index holds stable releases only), or "".
func Latest(rels []Release) string {
	best := ""
	for _, r := range rels {
		if best == "" || compareVersions(r.Version, best) > 0 {
			best = r.Version
		}
	}
	return best
}

// UpdateAvailable is the one definition of "this device has a newer stable
// release available" shared by the firmware page and the device list: the
// device runs a known version older than latest. A device on minimal or
// safeboot firmware still counts (it must leave that image anyway).
func UpdateAvailable(latest string, id model.Identity) bool {
	return latest != "" && ComparePosition(latest, id) == PosBelow
}

// DeviceNeed is what one device needs from a release.
type DeviceNeed struct {
	DeviceID model.DeviceID
	Name     string
	Position Position
	// Files are the upstream file names the device would use for this
	// release: its own build and, for an ESP8266, tasmota-minimal. Empty when
	// Skip is set.
	Files []string
	Skip  string // why the device needs nothing: excluded, or no build can be chosen
}

// ReleaseNeeds is the fleet's view of one release.
type ReleaseNeeds struct {
	Release string
	Devices []DeviceNeed
	// Files is the sorted union of all devices' files that the release
	// offers; Missing lists wanted files the release index does not offer.
	Files   []string
	Missing []string
}

// FleetNeeds computes, per device, the files an update to rel would use:
// fwselect.Select on the device's identity (keeping its variant and
// language) plus the tasmota-minimal image for every ESP8266. A device on
// minimal or safeboot firmware gets the standard build. Devices excluded
// from OTA and devices whose hardware or version is not known need nothing.
// Nothing here is a safety decision: the OTA engine runs the full guard
// before every flash.
func FleetNeeds(rel Release, devs []model.Device) ReleaseNeeds {
	out := ReleaseNeeds{Release: rel.Version}
	offered := map[string]bool{}
	for _, f := range rel.Files {
		offered[f] = true
	}
	union := map[string]bool{}
	missing := map[string]bool{}
	for _, d := range devs {
		n := DeviceNeed{DeviceID: d.ID, Name: d.Name, Position: ComparePosition(rel.Version, d.Identity)}
		switch files, err := deviceFiles(rel.Version, d); {
		case d.OTAExcluded:
			n.Skip = "excluded from OTA"
		case err != nil:
			n.Skip = err.Error()
		default:
			n.Files = files
			for _, f := range files {
				if offered[f] {
					union[f] = true
				} else {
					missing[f] = true
				}
			}
		}
		out.Devices = append(out.Devices, n)
	}
	sort.SliceStable(out.Devices, func(i, j int) bool { return lessFold(out.Devices[i].Name, out.Devices[j].Name) })
	out.Files, out.Missing = keys(union), keys(missing)
	return out
}

func deviceFiles(release string, d model.Device) ([]string, error) {
	id := d.Identity
	c := fwselect.Choice{Release: release, Policy: model.PolicyOnlyNewer}
	if id.Variant == "minimal" || id.Variant == "safeboot" {
		c.Variant, c.AllowVariantChange = "tasmota", true
	}
	sel, err := fwselect.Select(id, c)
	if err != nil {
		return nil, fmt.Errorf("no build can be chosen: %w", err)
	}
	files := []string{sel.Filename}
	if sel.Minimal != "" {
		files = append(files, sel.Minimal)
	}
	return files, nil
}

// UsedBy returns the devices of needs that would use file.
func (n ReleaseNeeds) UsedBy(file string) []DeviceNeed {
	var out []DeviceNeed
	for _, d := range n.Devices {
		for _, f := range d.Files {
			if f == file {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lessFold(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}
