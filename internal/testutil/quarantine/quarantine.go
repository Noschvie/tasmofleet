// Package quarantine parks known-flaky tests (tests only). A quarantined test
// is skipped in every run except the nightly quarantine job in ci.yml, which
// sets TASMOFLEET_QUARANTINE=1 and retries. Each quarantined test names its
// GitHub issue (label "flaky"); it is fixed or deleted within about two weeks
// (CLAUDE.md "Running the gates").
package quarantine

import (
	"os"
	"testing"
)

// Env is the variable that lets quarantined tests run.
const Env = "TASMOFLEET_QUARANTINE"

// Test skips t unless Env is "1". issue is the URL of the test's flaky issue.
func Test(t testing.TB, issue string) {
	t.Helper()
	if os.Getenv(Env) != "1" {
		t.Skipf("quarantined as flaky, see %s (runs only with %s=1, the nightly quarantine job)", issue, Env)
	}
}
