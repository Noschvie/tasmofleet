package app

import (
	"context"
	"slices"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// policyApplier is what the OTA engine uses to run a "Make compliant" job: the
// fixer sends and reads back, and the values it read are put into the live
// cache, so the policy page shows the new state without another device read.
type policyApplier struct{ a *runtime }

// policyApplier returns the engine's applier, or nil when the policy is not set up.
func (a *runtime) policyApplier() ota.PolicyApplier {
	if a.fixer == nil || a.policyLive == nil {
		return nil
	}
	return policyApplier{a: a}
}

func (p policyApplier) Apply(ctx context.Context, d model.Device, lines []string) error {
	// Interim (#605): Make compliant keeps the order before writes went MQTT first (HTTP unless it fails) until
	// it moves to MQTT first in its own change (#606), which removes this marker. Verify's reads are not marked.
	err := p.a.fixer.Apply(transport.WithHTTPFirstWrites(ctx), d, lines)
	p.a.settingsWritten(d.ID) // whatever was sent: a read that started before it holds the old values (#662)
	if err == nil && p.a.brokerHost != nil && slices.ContainsFunc(lines, setsBrokerTarget) {
		p.a.brokerHost.later(d.ID) // the device moved to another broker (#598)
	}
	return err
}

func (p policyApplier) Verify(ctx context.Context, d model.Device, lines []string) error {
	vals, err := p.a.fixer.Verify(ctx, d, lines)
	if len(vals) > 0 { // also after a mismatch: the page must show what the device really has
		p.a.policyLive.vals.put(d.ID, vals, p.a.policyLive.clock())
	}
	return err
}
