package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// ErrNoInfo is the RootPage error over MQTT when no INFO1 was seen since the device's last boot: a
// device only announces itself once per boot, and an INFO1 from before a restart describes the old
// firmware. It is an ErrBadResponse, so a probe that needs the page fails and nothing is decided on a
// guess.
var ErrNoInfo = fmt.Errorf("%w: no INFO1 seen since the device's last boot, identity unknown", tasmota.ErrBadResponse)

// infoSource is the part of the command channel that keeps the INFO1 cache (mqttcmd.Channel.Info).
type infoSource interface {
	Info(topic string) (mqttcmd.Info, bool)
}

// mqttRootPage builds the RootInfo of a device from its INFO1 (D9): version and image tag
// ("16.0.0(release-minimal)"). Nothing else is known without the web page: LangKnown is false (INFO1
// names no language; full firmware reports it in Status 4, #686), and the title is not part of INFO1.
func (r *Router) mqttRootPage(d model.Device) (tasmota.RootInfo, error) {
	src, ok := r.mqttDoer().(infoSource)
	if !ok {
		return tasmota.RootInfo{}, fmt.Errorf("%w: the MQTT channel keeps no INFO1", tasmota.ErrNotImplemented)
	}
	info, ok := src.Info(d.Topic)
	if !ok || len(info.Info1) == 0 {
		return tasmota.RootInfo{}, ErrNoInfo
	}
	return rootFromInfo1(info.Info1)
}

func rootFromInfo1(payload []byte) (tasmota.RootInfo, error) {
	var m struct {
		Info1 *struct{ Version string }
	}
	if err := json.Unmarshal(payload, &m); err != nil || m.Info1 == nil {
		return tasmota.RootInfo{}, fmt.Errorf("%w: INFO1 is not readable", tasmota.ErrBadResponse)
	}
	semver, rest, ok := strings.Cut(m.Info1.Version, "(")
	tag, _, ok2 := strings.Cut(rest, ")")
	if !ok || !ok2 || semver == "" || tag == "" {
		return tasmota.RootInfo{}, fmt.Errorf("%w: INFO1 version %q is not recognised", tasmota.ErrBadResponse, m.Info1.Version)
	}
	return tasmota.RootInfo{
		Version: semver, Image: tag, LangKnown: false,
		Minimal: strings.HasSuffix(tag, "-minimal") || strings.HasSuffix(tag, "-safeboot"),
	}, nil
}

// RootPage is the main page of a device as far as identity needs it. Since #686 only minimal and safeboot
// firmware need it (their image; the language comes from Status 4), and its callers are those paths: the
// identity probe and the update preview of a device that answers Status 2 with Unknown. A job that pinned
// HTTP reads the page over HTTP and one that pinned MQTT builds it from INFO1 (D5, pins keep their
// meaning). Unpinned, a stored device usable over MQTT that sent an INFO1 since its last boot is read from
// that INFO1 first (MQTT first, #686): nothing is sent to the device. Without one, the page is read over
// HTTP, unless HTTP is known to fail, when only INFO1 can tell (mqttRootPage). An address without a
// stored device (the first add) has no topic and is read over HTTP.
func (r *Router) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	id := t.DeviceID()
	if id == 0 {
		return r.http.RootPage(ctx, t)
	}
	if pin, ok := PathFrom(ctx); ok {
		if pin == HTTP {
			return r.http.RootPage(ctx, t)
		}
		return r.pinnedRootPage(ctx, id)
	}
	if r.mqttDoer() == nil {
		return r.http.RootPage(ctx, t)
	}
	if ri, err := r.pinnedRootPage(ctx, id); err == nil {
		return ri, nil
	}
	if r.o.Facts.HTTPState(id).State == HTTPFailing {
		d, _, p, err := r.decide(ctx, id)
		if err == nil && p.Commands == MQTT {
			return r.mqttRootPage(d)
		}
	}
	return r.http.RootPage(ctx, t)
}

// Info1Root is the main page as far as the device's INFO1 since its last boot tells it (version and image,
// no language), for a stored device usable over MQTT (MQTTBlocker), whatever ctx pins: it only reads the
// INFO1 cache of the MQTT channel, sends nothing to the device and never touches HTTP. An update job reads
// minimal and safeboot firmware this way (#686) instead of the main page. ErrNoInfo without an INFO1 (or
// without a stored device), ErrUnreachable when MQTT is not connected or not usable for the device.
func (r *Router) Info1Root(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	id := t.DeviceID()
	if id == 0 {
		return tasmota.RootInfo{}, ErrNoInfo
	}
	return r.pinnedRootPage(ctx, id)
}

func (r *Router) pinnedRootPage(ctx context.Context, id model.DeviceID) (tasmota.RootInfo, error) {
	if r.mqttDoer() == nil {
		return tasmota.RootInfo{}, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return tasmota.RootInfo{}, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return tasmota.RootInfo{}, err
	}
	if why := MQTTBlocker(f, false); why != "" {
		return tasmota.RootInfo{}, fmt.Errorf("%w: not usable over MQTT: %s", tasmota.ErrUnreachable, why)
	}
	return r.mqttRootPage(d)
}
