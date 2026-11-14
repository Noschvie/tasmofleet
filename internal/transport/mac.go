package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// AskMAC reads the MAC a device reports in Status 5, normalised (NormMAC). It is the one identity question of a backup
// and a restore (#607). Pinned to MQTT (ctx) it asks the device by its topic: there is no address that could receive a
// password. Otherwise it asks the host at dev's address over HTTP without the stored web credentials first, so a host
// that is not the device never sees them unless it demands a password; only a 401 brings the credentials (that host is
// about to receive them with every command anyway). Every identity check runs before the first request that carries
// the credentials.
func AskMAC(ctx context.Context, c tasmota.Client, dev model.Device) (string, error) {
	if pin, ok := PathFrom(ctx); ok && pin == MQTT {
		// The router needs the device ID in the target to find its topic (a target without it is a raw HTTP call).
		raw, err := c.Command(ctx, tasmota.TargetFor(dev), "Status 5")
		if err != nil {
			return "", err
		}
		return macOf(raw)
	}
	ctx = WithPath(ctx, HTTP)
	var lastErr error
	for _, withCreds := range []bool{false, true} {
		t := model.Device{Address: dev.Address, Port: dev.Port} // no ID: a raw HTTP call to the address
		if withCreds {
			t = dev
		}
		raw, err := c.Command(ctx, tasmota.TargetFor(t), "Status 5")
		if errors.Is(err, tasmota.ErrAuth) {
			lastErr = err
			continue
		}
		if err != nil {
			return "", err
		}
		return macOf(raw)
	}
	return "", lastErr
}

// macOf reads the MAC out of a Status 5 reply.
func macOf(raw json.RawMessage) (string, error) {
	var s struct {
		StatusNET struct{ Mac string }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusNET.Mac == "" {
		return "", tasmota.ErrBadResponse
	}
	return NormMAC(s.StatusNET.Mac), nil
}

// NormMAC lowers a MAC and removes its separators; "" stays "".
func NormMAC(s string) string {
	return strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}
