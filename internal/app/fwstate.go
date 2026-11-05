package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// firmwareState is what TasmoFleet has seen of the firmware a device runs right now, in memory
// (#558). The stored Identity.Variant changes only on a probe, so a device that boots into
// safeboot, or leaves it again, would keep the stored value for ever. This state is a hint for
// read suppression only; it never selects firmware (fwselect and its Guard do not use it).
//
// A device is on limited firmware (safeboot or minimal: they answer Unknown to nearly everything)
// when the newest of three says so: the stored identity (with its DetectedAt), an INFO1 version
// seen live (with the time it arrived), and the health poll, which finds minimal firmware over HTTP.
type firmwareState struct {
	mu   sync.Mutex
	seen map[model.DeviceID]fwSeen
	// minimal reports that the last successful health poll found minimal firmware; nil = never.
	minimal func(model.DeviceID) bool
	now     func() time.Time
}

type fwSeen struct {
	variant string // "safeboot", "minimal", or "" for a regular build
	at      time.Time
}

func newFirmwareState(minimal func(model.DeviceID) bool) *firmwareState {
	return &firmwareState{seen: map[model.DeviceID]fwSeen{}, minimal: minimal, now: time.Now}
}

// set records what an INFO1 said about the device at the given time.
func (f *firmwareState) set(id model.DeviceID, variant string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.seen[id]; ok && old.at.After(at) {
		return
	}
	f.seen[id] = fwSeen{variant: variant, at: at}
}

// effective returns d with Identity.Variant replaced by the newest known limited state, or, when a
// live regular build is newer than a stored limited one, by "tasmota". Other fields stay.
func (f *firmwareState) effective(d model.Device) model.Device {
	if f == nil {
		return d
	}
	f.mu.Lock()
	s, ok := f.seen[d.ID]
	f.mu.Unlock()
	if ok && s.at.After(d.Identity.DetectedAt) {
		switch {
		case s.variant != "":
			d.Identity.Variant = s.variant
		case policy.LimitedFirmware(d.Identity.Variant):
			d.Identity.Variant = "tasmota"
		}
		return d
	}
	if f.minimal != nil && !policy.LimitedFirmware(d.Identity.Variant) && f.minimal(d.ID) {
		d.Identity.Variant = "minimal"
	}
	return d
}

// store is the part of the device store the wrappers need.
type deviceStore interface {
	ListDevices(ctx context.Context) ([]model.Device, error)
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// fwDevices serves the devices with their effective firmware variant, for every reader that must
// not send commands to a device on limited firmware.
type fwDevices struct {
	inner deviceStore
	fw    *firmwareState
}

func (s fwDevices) ListDevices(ctx context.Context) ([]model.Device, error) {
	devs, err := s.inner.ListDevices(ctx)
	for i := range devs {
		devs[i] = s.fw.effective(devs[i])
	}
	return devs, err
}

func (s fwDevices) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	d, err := s.inner.GetDevice(ctx, id)
	if err != nil {
		return d, err
	}
	return s.fw.effective(d), nil
}

// list wraps an inventory list function the same way.
func (f *firmwareState) list(inner func(context.Context) ([]model.Device, error)) func(context.Context) ([]model.Device, error) {
	if f == nil {
		return inner
	}
	return func(ctx context.Context) ([]model.Device, error) {
		devs, err := inner(ctx)
		for i := range devs {
			devs[i] = f.effective(devs[i])
		}
		return devs, err
	}
}

// buildTag is the text in the first parentheses of a Tasmota version string: "16.0.0(safeboot)single-core"
// gives "safeboot", "15.0.1(release-minimal)" gives "release-minimal". ok is false without one.
func buildTag(version string) (tag string, ok bool) {
	i := strings.IndexByte(version, '(')
	if i < 0 {
		return "", false
	}
	j := strings.IndexByte(version[i:], ')')
	if j < 0 {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(version[i+1 : i+j])), true
}

// limitedVariant maps a build tag to "safeboot" or "minimal" when it names such a build, whatever
// precedes it ("release-minimal", "<sha>-safeboot") and whatever version, chip or suffix goes with it.
func limitedVariant(tag string) string {
	for _, v := range []string{"safeboot", "minimal"} {
		if tag == v || strings.HasSuffix(tag, "-"+v) {
			return v
		}
	}
	return ""
}

// info1Build reads the version of a live INFO1 message and says what build it names: variant is
// "safeboot" or "minimal" for such a build and "" for a regular one; ok is false when the message
// has no version with a build tag (then it says nothing).
func info1Build(payload []byte) (variant string, ok bool) {
	var m struct {
		Info1 *struct{ Version string }
	}
	if json.Unmarshal(payload, &m) != nil || m.Info1 == nil {
		return "", false
	}
	tag, ok := buildTag(m.Info1.Version)
	if !ok {
		return "", false
	}
	return limitedVariant(tag), true
}
