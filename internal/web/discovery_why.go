package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/discovery"
)

// The lookup of the Add page (#713): "why is this device not offered as new?". discoveryData asks
// discovery.Explain only for GET ?why=text; the answer is words and links, nothing is stored.

// whyView is the answer box.
type whyView struct {
	Query string
	// Message is the whole answer when there is no finding to show: a refusal, an error, or "nothing seen".
	Message string
	// Limit says how far the answer reaches.
	Limit string
	Lines []whyLineView
}

// whyLineView is one finding. RestoreID and RestoreVersion belong to an ignored proposal: the version is
// the one shown, so a number that names another device by the time of the click is refused (#745).
type whyLineView struct {
	Name, Href     string
	Topic          bool // Name is an MQTT topic, not a device name
	Lead, Detail   string
	Anchor         string // a link to the section of the page the device is listed in
	RestoreID      int64
	RestoreVersion string
}

// anchorID is the id of a section of the Discovered box, from its title.
func anchorID(title string) string {
	return "disc-" + strings.ReplaceAll(strings.ToLower(title), " ", "-")
}

// proposalDays is the number of days after which a proposal without a sighting is deleted.
func proposalDays() int { return int(discovery.ProposalTTL / (24 * time.Hour)) }

// whyOf asks discovery about text and words the answer. A text it cannot look up, and an error, become
// a sentence in the box; neither fails the page, and the text itself is never logged (it is free input).
func (h *handler) whyOf(r *http.Request, base, text string, off bool) *whyView {
	a := &whyView{Query: text}
	x, err := h.d.Discovery.Explain(r.Context(), text)
	switch {
	case errors.Is(err, discovery.ErrBadLookup):
		a.Message = "Enter an IP address, a MAC or an MQTT topic of up to 128 characters."
		return a
	case err != nil:
		a.Message = "The device could not be looked up."
		h.log.ErrorContext(r.Context(), "discovery lookup", "err", err)
		return a
	}
	days := strconv.Itoa(proposalDays())
	a.Limit = "The lookup covers the device list, the MQTT topics TasmoFleet has seen since it started and the proposals of the last " + days + " days."
	for _, f := range x.Findings {
		a.Lines = append(a.Lines, h.whyLineOf(base, f, days))
	}
	if len(a.Lines) == 0 {
		a.Message = "TasmoFleet has not seen this on MQTT since it started, and has no proposal for it from the last " + days + " days, and no device in the device list matches it."
		if off {
			a.Message += " MQTT discovery is switched off, so topics are not tracked at all."
		}
	}
	return a
}

func (h *handler) whyLineOf(base string, f discovery.Finding, days string) whyLineView {
	switch f.Kind {
	case discovery.FindingInDeviceList:
		return whyLineView{Name: f.DeviceName, Href: base + "/devices/" + strconv.FormatInt(int64(f.DeviceID), 10),
			Lead: "is already in your device list.", Detail: "TasmoFleet matched it by its " + byWords(f.By) + ", so it is not offered as new. It keeps the name and settings stored there."}
	case discovery.FindingIgnored:
		p := f.Proposal
		return whyLineView{Name: dash(p.Device.Name), Lead: "is ignored" + kindSuffix(p.Kind) + ".", RestoreID: p.ID, RestoreVersion: p.Version,
			Detail: "It stays hidden while the data it announces stays the same. It comes back when that changes, or when it announces itself after " + days + " days without a sighting."}
	case discovery.FindingListed:
		p := f.Proposal
		title, lead := "New devices", "is listed under New devices."
		switch p.Kind {
		case discovery.KindAddressChange:
			title, lead = "Address changed", "is listed under Address changed: a known device announced a new address."
		case discovery.KindConflict:
			title, lead = "Conflicts", "is listed under Conflicts: it claims the address or topic of a known device."
		}
		anchor := "#disc-new"
		if p.Kind != discovery.KindNew {
			anchor = "#" + anchorID(title)
		}
		return whyLineView{Name: dash(p.Device.Name), Lead: lead, Anchor: anchor}
	default: // FindingUnidentified
		u := f.Unidentified
		why := capitalize(u.Reason)
		if why != "" {
			why += "."
		}
		return whyLineView{Name: u.Topic, Topic: true, Lead: "is on the broker, but TasmoFleet does not know its MAC yet.", Detail: why}
	}
}

func byWords(by string) string {
	if by == "topic" {
		return "MQTT topic"
	}
	return by
}

func kindSuffix(kind string) string {
	switch kind {
	case discovery.KindAddressChange:
		return " (address change)"
	case discovery.KindConflict:
		return " (conflict)"
	}
	return ""
}
