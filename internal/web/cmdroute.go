package web

import (
	"context"
	"strconv"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// A command whose arguments the audit hides may carry a secret, and it crosses the network in plain text over either
// transport (#826, the product owner's rule of 2026-11-14): over HTTP to the device, or over the MQTT broker, which
// also delivers it to every client subscribed to the device's commands unless a broker ACL prevents that. A TLS broker
// URL encrypts only TasmoFleet's leg to the broker. Its confirmation says which, honestly, before anything is sent.

// typedRoutes returns the transport the router sends cmd over now to each of ids, and whether the broker URL uses TLS;
// nil when cmd names no route (transport.ConfirmsRoute).
func (h *handler) typedRoutes(ctx context.Context, cmd string, ids ...model.DeviceID) (map[model.DeviceID]transport.Path, bool) {
	if !transport.ConfirmsRoute(cmd) {
		return nil, false
	}
	if h.d.TypedRoutes == nil {
		out := make(map[model.DeviceID]transport.Path, len(ids))
		for _, id := range ids {
			out[id] = transport.HTTP // a router without MQTT
		}
		return out, false
	}
	return h.d.TypedRoutes(ctx, cmd, ids...)
}

// routeText is how cmd goes over p, after "goes"; whose names the devices whose commands the broker delivers.
func routeText(p transport.Path, tls bool, whose string) string {
	if p != transport.MQTT {
		return "over HTTP, unencrypted"
	}
	leg := "unencrypted"
	if tls {
		leg = "encrypted from TasmoFleet to the broker"
	}
	return "over the MQTT broker, " + leg + "; the broker also delivers it to every client subscribed to " + whose +
		" commands (a broker ACL prevents that)"
}

// consoleRoute is the console's sentence about the route of cmd to device id, and the route; no route when cmd names
// none, and no sentence when it goes nowhere (transport.None: the router refuses it, and the send is announced so,
// which lets nothing through).
func (h *handler) consoleRoute(ctx context.Context, cmd string, id model.DeviceID) (map[model.DeviceID]transport.Path, string) {
	routes, tls := h.typedRoutes(ctx, cmd, id)
	if routes == nil || routes[id] == transport.None {
		return routes, ""
	}
	return routes, "The command goes to the device " + routeText(routes[id], tls, "this device's") + "."
}

// bulkRoute is the bulk preview's rows about the routes of cmd to ids, per transport with how many devices, the
// routes, and the text of the rows (what a run compares); no rows when cmd names no route. Devices it goes to nowhere
// (transport.None) get a row of their own; the run leaves them out.
func (h *handler) bulkRoute(ctx context.Context, cmd string, ids ...model.DeviceID) (map[model.DeviceID]transport.Path, []ui.KVRow, string) {
	routes, tls := h.typedRoutes(ctx, cmd, ids...)
	if routes == nil {
		return nil, nil, ""
	}
	var rows []ui.KVRow
	notice := ""
	for _, p := range []transport.Path{transport.MQTT, transport.HTTP, transport.None} {
		n := 0
		for _, id := range ids {
			if routes[id] == p {
				n++
			}
		}
		if n == 0 {
			continue
		}
		key := map[transport.Path]string{transport.MQTT: "Over MQTT", transport.HTTP: "Over HTTP", transport.None: "Not sent"}[p]
		v := strconv.Itoa(n) + " of " + plural(len(ids), "device") + ". The command goes " + routeText(p, tls, "each device's") + "."
		if p == transport.None {
			v = strconv.Itoa(n) + " of " + plural(len(ids), "device") + ". MQTT does not carry this command and their HTTP is not reachable."
		}
		rows = append(rows, ui.KVRow{Key: key, Value: v})
		notice += key + ": " + v + "\n"
	}
	return routes, rows, notice
}

// sameRoutes reports whether a and b name the same transport for the same devices.
func sameRoutes(a, b map[model.DeviceID]transport.Path) bool {
	if len(a) != len(b) {
		return false
	}
	for id, p := range a {
		if q, ok := b[id]; !ok || q != p {
			return false
		}
	}
	return true
}
