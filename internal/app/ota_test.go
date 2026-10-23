package app

import "testing"

// MQTT stat/<t>/UPGRADE reports reach the engine as hints; a "Failed ..."
// report lets a job stop waiting for a download that will never come.
// Anything that is not {"Upgrade": "<text>"} must be ignored, not guessed.
func TestUpgradeReportText(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"Upgrade":"Failed File Not Found (404)"}`, "Failed File Not Found (404)", true},
		{`{"Upgrade":"Successful. Restarting"}`, "Successful. Restarting", true},
		{`{"Upgrade":""}`, "", true},
		{`{"Upload":"Failed"}`, "", false},
		{`{"Upgrade":1}`, "", false},
		{`not json`, "", false},
	}
	for _, c := range cases {
		got, ok := upgradeReportText([]byte(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
