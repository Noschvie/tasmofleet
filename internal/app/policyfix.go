package app

import (
	"context"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
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
	return p.a.fixer.Apply(ctx, d, lines)
}

func (p policyApplier) Verify(ctx context.Context, d model.Device, lines []string) error {
	vals, err := p.a.fixer.Verify(ctx, d, lines)
	if len(vals) > 0 { // also after a mismatch: the page must show what the device really has
		p.a.policyLive.vals.put(d.ID, vals, p.a.policyLive.clock())
	}
	return err
}
