package config

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// Why: the shipped Unraid template is what the container runs from. Its
// defaults must load as a valid configuration without warnings, so a template
// never carries a variable the program has removed.
func TestUnraidTemplateLoads(t *testing.T) {
	data, err := os.ReadFile("../../deploy/unraid/tasmofleet.xml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Configs []struct {
			Type   string `xml:"Type,attr"`
			Target string `xml:"Target,attr"`
			Value  string `xml:",chardata"`
		} `xml:"Config"`
	}
	if err := xml.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	env := []string{"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_OIDC_CLIENT_ID=filled-in-by-the-operator"}
	for _, e := range c.Configs {
		if e.Type == "Variable" && strings.TrimSpace(e.Value) != "" {
			env = append(env, e.Target+"="+strings.TrimSpace(e.Value))
		}
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatalf("template defaults do not load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("template sets removed variables: %v", cfg.Warnings)
	}
}
