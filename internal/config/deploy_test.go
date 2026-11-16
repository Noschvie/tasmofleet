package config

import (
	"encoding/xml"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// Why: an install from the repo template (instead of Community Apps) must not
// inherit a proxy trust or container IP that belongs to somebody else's network.
func TestUnraidTemplateHasNoPrefilledNetworkValues(t *testing.T) {
	data, err := os.ReadFile("../../deploy/unraid/tasmofleet.xml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		MyIP    string `xml:"MyIP"`
		Configs []struct {
			Target string `xml:"Target,attr"`
			Value  string `xml:",chardata"`
		} `xml:"Config"`
	}
	if err := xml.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(c.MyIP) != "" {
		t.Errorf("MyIP is prefilled: %q", c.MyIP)
	}
	for _, e := range c.Configs {
		if e.Target == "TASMOFLEET_TRUSTED_PROXIES" && strings.TrimSpace(e.Value) != "" {
			t.Errorf("Trusted proxies is prefilled: %q", e.Value)
		}
	}
}

// Why: whoever pastes a provider guide's variable block into the container
// configuration must get single sign-on from it. Load reads only names that
// start with TASMOFLEET_ from the environment and drops every other name
// without a message (#706: the guide printed short names, the reporter's client
// secret variable was ignored and he could still type the secret on the page).
// Every provider guide has the same block, so each is checked the same way (#722).
func TestProviderGuideBlocksLoadAsContainerEnvironment(t *testing.T) {
	for _, guide := range []string{"authentik", "keycloak"} {
		t.Run(guide, func(t *testing.T) {
			data, err := os.ReadFile("../../deploy/" + guide + "/README.md")
			if err != nil {
				t.Fatal(err)
			}
			_, after, ok := strings.Cut(string(data), "## tasmofleet side")
			if !ok {
				t.Fatal(`no "## tasmofleet side" section in the guide`)
			}
			_, block, ok := strings.Cut(after, "```\n")
			if !ok {
				t.Fatal("no fenced block under the tasmofleet side section")
			}
			block, _, ok = strings.Cut(block, "```")
			if !ok {
				t.Fatal("the fenced block is not closed")
			}
			dir := t.TempDir()
			secretFile := dir + "/oidc-client-secret"
			if err := os.WriteFile(secretFile, []byte("fake-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := []string{"TASMOFLEET_DATA_DIR=" + dir, "TASMOFLEET_CONFIG_FILE=/dev/null"}
			for line := range strings.SplitSeq(block, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if k, _, _ := strings.Cut(line, "="); strings.HasSuffix(k, "_CLIENT_SECRET_FILE") {
					line = k + "=" + secretFile // the guide's path lives in the container's /data
				}
				env = append(env, line)
			}
			cfg, err := Load(env)
			if err != nil {
				t.Fatalf("the guide's block does not load: %v", err)
			}
			if cfg.AuthMode != "oidc" {
				t.Errorf("AuthMode = %q, want oidc: the block's names are not read from the environment", cfg.AuthMode)
			}
			if cfg.OIDCClientSecret != "fake-secret" {
				t.Errorf("the client secret file was not read")
			}
			if !cfg.Locked["OIDC_ISSUER"] || !cfg.Locked["OIDC_CLIENT_SECRET"] {
				t.Errorf("issuer and client secret must be locked by the environment, got %v", cfg.Locked)
			}
		})
	}
}

// Why: Load drops an environment variable without the TASMOFLEET_ prefix
// silently, so a guide that prints a bare key tells the reader to set something
// that has no effect (#706). The prefix is optional only in the env file, which
// the guides must say in words ("the same keys without the prefix"), not by
// printing a bare key.
func TestDeployGuidesNameVariablesWithThePrefix(t *testing.T) {
	var keys []string
	rt := reflect.TypeOf(Config{})
	for i := range rt.NumField() {
		if k := rt.Field(i).Tag.Get("env"); k != "" {
			keys = append(keys, k)
		}
	}
	re := regexp.MustCompile(`(^|[^A-Z0-9_])(` + strings.Join(keys, "|") + `)(_FILE)?($|[^A-Z0-9_])`)
	err := filepath.WalkDir("../../deploy", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for n, line := range strings.Split(string(data), "\n") {
			// FindAll without overlap would miss two keys separated by one character.
			for pos := 0; pos <= len(line); {
				m := re.FindStringSubmatchIndex(line[pos:])
				if m == nil {
					break
				}
				t.Errorf("%s:%d names %q without the TASMOFLEET_ prefix; a bare name in the container environment is ignored", path, n+1, line[pos+m[4]:pos+m[5]])
				pos += m[5]
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
