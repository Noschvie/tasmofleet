package quarantine

import "testing"

// Why: the quarantine must really keep a flaky test out of PR and release
// runs, and must really run it in the nightly job; a helper that always
// skips would silently drop the test, one that never skips would keep it
// blocking.
func TestSkipsUnlessEnabled(t *testing.T) {
	for _, tc := range []struct {
		env  string
		skip bool
	}{{"", true}, {"0", true}, {"yes", true}, {"1", false}} {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(Env, tc.env)
			skipped := true
			t.Run("inner", func(t *testing.T) {
				Test(t, "https://github.com/nofuturekid/tasmofleet/issues/0")
				skipped = false
			})
			if skipped != tc.skip {
				t.Errorf("TASMOFLEET_QUARANTINE=%q: skipped=%v, want %v", tc.env, skipped, tc.skip)
			}
		})
	}
}
