package sysconfig

import "testing"

// Why: every editable system setting explains itself on the page (the hint
// under the field); a new setting without one would be a bare input.
func TestEveryFieldHasHelp(t *testing.T) {
	for _, f := range fields {
		if f.Help == "" {
			t.Errorf("setting %s has no help text", f.Key)
		}
	}
}
