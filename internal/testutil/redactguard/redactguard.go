// Package redactguard checks TasmoFleet's own reads in tests (tests only): a read that compares or shows a device
// value must never send the bare form of a command whose reply the router masks.
//
// The router masks the reply of every command cmdpolicy marks Redact (cmdpolicy.ScrubReply, the one secret rule of
// #809) in transport.Router.Command, whichever transport carried it (MQTT or HTTP). A bare MqttUser therefore answers "***" in production, and a comparison with it says
// "differs" or, worse, "equal" against a masked value. Such a value is read from its Status block instead (MqttUser
// from Status 6, StatusMQT). The Settings read and the restore's lockout check use this guard; so must every new path
// that reads a value to compare it (the read-back of #947).
package redactguard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Client wraps the client of a test. Command fails the test for a bare command that cmdpolicy marks Redact and masks
// the reply of every command the router masks, so the code under test sees what it would see in production.
type Client struct {
	tasmota.Client
	t testing.TB
}

// Wrap returns c guarded for t.
func Wrap(t testing.TB, c tasmota.Client) *Client { return &Client{Client: c, t: t} }

// Command implements tasmota.Client.
func (c *Client) Command(ctx context.Context, tg tasmota.Target, cmnd string) (json.RawMessage, error) {
	v, verr := cmdpolicy.Classify(cmnd)
	if verr == nil && v.Redact && !strings.Contains(strings.TrimSpace(cmnd), " ") {
		c.t.Errorf("redactguard: the read sent the bare %q, whose reply the router masks (cmdpolicy: Redact); read the value from its Status block", cmnd)
	}
	raw, err := c.Client.Command(ctx, tg, cmnd)
	if verr != nil || v.Redact { // the router's rule (transport: classifyCommand, Router.Command)
		raw = cmdpolicy.ScrubReply(raw)
	}
	return raw, err
}
