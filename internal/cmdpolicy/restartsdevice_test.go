package cmdpolicy

import "testing"

// Why: TasmoFleet must know that a restart it sees was its own doing, or the Restart button, a bulk
// restart or an update typed in the console would count towards a boot loop (#325). Only commands that
// really restart the device count: Restart 2 halts it, a bare Restart or Upgrade does nothing.
func TestRestartsDevice(t *testing.T) {
	for cmd, want := range map[string]bool{
		"Restart 1":                      true,
		"restart 1":                      true,
		" Restart 99 ":                   true,
		"Restart 2":                      false, // halts: no restart follows
		"Restart":                        false,
		"Restart 0":                      false,
		"Upgrade 1":                      true,
		"Upgrade":                        false,
		"Reset 1":                        true,
		"Reset":                          false,
		"Backlog Power1 ON; Restart 1":   true,
		"Backlog Delay 20; Upgrade 1":    true,
		"Backlog Power1 ON; Power2 OFF":  false,
		"Backlog Restart 2":              false,
		"Backlog":                        false,
		"Power1 ON":                      false,
		"Status 1":                       false,
		"":                               false,
		"Restart 1\x00":                  false, // refused by the policy: never sent
		"Backlog Power1 ON;;Restart 99":  true,
		"Backlog0 Restart 1":             true,
		"Backlog Restart 1; Restart 2":   true,
		"Backlog Restart 2; Power1 OFF":  false,
		"Backlog Restart 2; Restart 99":  true,
		"Backlog Restart 3; Restart 100": false,
	} {
		if got := RestartsDevice(cmd); got != want {
			t.Errorf("RestartsDevice(%q) = %v, want %v", cmd, got, want)
		}
	}
}
