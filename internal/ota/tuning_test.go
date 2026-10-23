package ota

import (
	"testing"
	"time"
)

// Why: the update settings can change on the settings page while jobs run. The
// engine must use the new values from the next step and ignore an invalid one.
func TestSetTuningChangesTheEngineLive(t *testing.T) {
	e := NewEngine(Options{MaxParallel: 2}, nil, nil, nil, nil, nil, nil, nil)
	if e.maxParallel() != 2 || !e.machine().RestoreOtaURL {
		t.Fatalf("start values: %d %v", e.maxParallel(), e.machine().RestoreOtaURL)
	}
	e.SetTuning(Tuning{MaxParallel: 5, RestoreOtaURL: false, PowerRestoreMaxAge: 3 * time.Minute})
	if e.maxParallel() != 5 || e.machine().RestoreOtaURL || e.machine().PowerRestoreMaxAge != 3*time.Minute {
		t.Fatalf("after SetTuning: %d %+v", e.maxParallel(), e.machine())
	}
	if e.machine().CommandTimeout != DefaultConfig().CommandTimeout {
		t.Fatal("the other machine settings stay")
	}
	e.SetTuning(Tuning{})
	if e.maxParallel() != 5 {
		t.Fatal("an invalid tuning (MaxParallel < 1) is ignored")
	}
}
