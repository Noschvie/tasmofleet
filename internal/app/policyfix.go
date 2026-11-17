package app

import (
	"context"
	"slices"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
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
	// The chunks follow the router's write path like any other write (#606): MQTT first for a device usable there
	// under the strict rule; a chunk with a line that changes the MQTT connection goes over MQTT too while HTTP
	// answers the router's Status 11 right before, and is refused (ErrCutOff, reported as skipped) otherwise.
	err := p.a.fixer.Apply(ctx, d, lines)
	p.a.settingsWritten(d.ID) // whatever was sent: a read that started before it holds the old values (#662)
	if err == nil && p.a.brokerHost != nil && slices.ContainsFunc(lines, setsBrokerTarget) {
		p.a.brokerHost.later(d.ID) // the device moved to another broker (#598)
	}
	return err
}

func (p policyApplier) Verify(ctx context.Context, d model.Device, lines []string) error {
	// The fixer returns values only from a device it confirmed to be the stored one (settings.Manager.ReadVerified,
	// #947); a difference is a *policy.DriftError, which the engine reports as drift.
	vals, err := p.a.fixer.Verify(ctx, d, lines)
	if len(vals) > 0 { // also after a mismatch: the page must show what the device really has, as drift
		p.a.policyLive.vals.put(d.ID, vals, p.a.policyLive.clock())
	}
	return err
}
