package fwselect

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const fixturesDir = "../../testdata/fixtures"

// readFixture returns a captured device or firmware fixture. Tests are
// fixture-driven so that the decisions are checked against what real devices
// and real upstream images look like, not against our own assumptions.
func readFixture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return b
}

// wantErr fails unless err wraps want (or both are nil). Every refusal must
// carry its specific sentinel: the UI and the engine branch on it.
func wantErr(t testing.TB, err, want error) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
