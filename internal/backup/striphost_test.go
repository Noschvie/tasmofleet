package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// Stored decoded backups must not carry details of the host that ran
// decode-config (kernel, platform): they leak host information into backup
// downloads and make every diff noisy after a host update.
func TestStripHostInfoRemovesDecoderEnvOnly(t *testing.T) {
	in := `{"header":{"data":{"version":{"name":"15.6.0.3"}},"env":{"platform":"Linux-7.2.10","system":"Linux x86_64"},"timestamp":"t"},"power_on_state":0}`
	out, err := stripHostInfo([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Linux") || strings.Contains(string(out), `"env"`) {
		t.Fatalf("host info kept: %s", out)
	}
	var got struct {
		Header struct {
			Data      map[string]any `json:"data"`
			Timestamp string         `json:"timestamp"`
		} `json:"header"`
		PowerOnState *int `json:"power_on_state"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Header.Data == nil || got.Header.Timestamp != "t" || got.PowerOnState == nil {
		t.Fatalf("device content lost: %s", out)
	}
}

func TestStripHostInfoLeavesOtherOutputAlone(t *testing.T) {
	for _, in := range []string{`{"a":1}`, `{"header":"x"}`, `{"header":{"data":{}}}`} {
		out, err := stripHostInfo([]byte(in))
		if err != nil || string(out) != in {
			t.Errorf("%s -> %s, %v", in, out, err)
		}
	}
	if _, err := stripHostInfo([]byte(`[1,2]`)); err == nil {
		t.Error("non-object output must be rejected")
	}
}
