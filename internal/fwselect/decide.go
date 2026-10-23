package fwselect

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// decide implements §7.3. An empty policy means the default, only-newer.
// allow-downgrade does not imply reinstall: an equal version is skipped.
func decide(id model.Identity, c Choice) (Decision, error) {
	policy := c.Policy
	if policy == "" {
		policy = model.PolicyOnlyNewer
	}
	if policy != model.PolicyOnlyNewer && policy != model.PolicyReinstall && policy != model.PolicyAllowDowngrade {
		return "", fmt.Errorf("fwselect: unknown update policy %q", policy)
	}
	if id.Variant == variantMinimal || id.Variant == variantSafeboot {
		// The device must leave the transit image whatever the versions say.
		if !isNumericVersion(c.Release) {
			return "", fmt.Errorf("%w: target %q", errBadVersion, c.Release)
		}
		return DecideUpgrade, nil
	}
	cmp, err := compareVersions(id.Version, c.Release)
	if err != nil {
		return "", err
	}
	switch {
	case cmp < 0:
		return DecideUpgrade, nil
	case cmp == 0 && policy == model.PolicyReinstall:
		return DecideReinstall, nil
	case cmp > 0 && policy == model.PolicyAllowDowngrade:
		return DecideDowngrade, nil
	default:
		return DecideSkip, nil
	}
}

// compareVersions compares dotted numeric versions field by field; missing
// fields count as zero.
func compareVersions(a, b string) (int, error) {
	fa, err := versionFields(a)
	if err != nil {
		return 0, err
	}
	fb, err := versionFields(b)
	if err != nil {
		return 0, err
	}
	// Pad to equal length: missing fields are zero.
	n := max(len(fa), len(fb))
	pa, pb := make([]int, n), make([]int, n)
	copy(pa, fa)
	copy(pb, fb)
	return slices.Compare(pa, pb), nil
}

func versionFields(v string) ([]int, error) {
	if !isNumericVersion(v) {
		return nil, fmt.Errorf("%w: %q", errBadVersion, v)
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %w", errBadVersion, v, err)
		}
		out[i] = n
	}
	return out, nil
}
