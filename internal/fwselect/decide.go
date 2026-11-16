package fwselect

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
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
	if err := migrationStop(id, c.Release); err != nil {
		return "", err
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

// migrationStop is the one implementation of the settings-migration stops (#892), whatever the chip family,
// variant or update policy: a release newer than PathStep is refused for a device running a version older
// than PathFloor (ErrMigrationPath), and a release newer than StepRelease for a device older than
// MigrationFloor (ErrSettingsMigration). It fails closed: a version or release the comparison cannot read
// is an error, never "new enough". Guard (rule 9) is the binding check; Decide only refuses early.
func migrationStop(id model.Identity, release string) error {
	for _, stop := range []struct {
		step, floor string
		err         error
	}{{PathStep, PathFloor, ErrMigrationPath}, {StepRelease, MigrationFloor, ErrSettingsMigration}} {
		past, err := compareVersions(release, stop.step)
		if err != nil {
			return fmt.Errorf("settings migration: target: %w", err)
		}
		if past <= 0 {
			return nil // not past this step, so not past the later one either
		}
		old, err := compareVersions(id.Version, stop.floor)
		if err != nil {
			return fmt.Errorf("settings migration: device version: %w", err)
		}
		if old < 0 {
			return fmt.Errorf("runs %s, target %s: %w", id.Version, release, stop.err)
		}
	}
	return nil
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
