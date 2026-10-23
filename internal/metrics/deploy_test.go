package metrics

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

// The deploy/ files are examples that people copy; these tests keep them
// consistent with the code so they cannot silently rot.

func deployFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type panel struct {
	Type    string                   `json:"type"`
	Title   string                   `json:"title"`
	GridPos struct{ X, Y, W, H int } `json:"gridPos"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

func loadDashboard(t *testing.T) []panel {
	t.Helper()
	var d struct {
		Panels []panel `json:"panels"`
	}
	if err := json.Unmarshal(deployFile(t, "grafana/tasmofleet.json"), &d); err != nil {
		t.Fatalf("grafana json: %v", err)
	}
	return d.Panels
}

var metricRef = regexp.MustCompile(`tasmofleet_[a-z0-9_]+`)

// A dashboard that queries a renamed metric shows "no data" forever; so every
// referenced name must be in the contract and every contract metric must be shown.
func TestGrafanaReferencesOnlyContractMetrics(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range loadDashboard(t) {
		for _, tg := range p.Targets {
			for _, name := range metricRef.FindAllString(tg.Expr, -1) {
				if _, ok := ContractMetrics[name]; !ok {
					t.Errorf("panel %q references %q, not in the metrics contract", p.Title, name)
				}
				seen[name] = true
			}
		}
	}
	for name := range ContractMetrics {
		if !seen[name] {
			t.Errorf("contract metric %s has no panel", name)
		}
	}
}

// The layout rule: at most two graphs side by side.
func TestGrafanaAtMostTwoGraphsPerRow(t *testing.T) {
	perRow := map[int]int{}
	for _, p := range loadDashboard(t) {
		if p.Type == "timeseries" || p.Type == "graph" {
			perRow[p.GridPos.Y]++
			if p.GridPos.W < 12 {
				t.Errorf("graph %q is narrower than half a row", p.Title)
			}
		}
	}
	for y, n := range perRow {
		if n > 2 {
			t.Errorf("row at y=%d has %d graphs", y, n)
		}
	}
}

type unraidConfig struct {
	Name   string `xml:"Name,attr"`
	Target string `xml:"Target,attr"`
	Type   string `xml:"Type,attr"`
}

func configKeys() map[string]bool {
	keys := map[string]bool{}
	rt := reflect.TypeOf(config.Config{})
	for i := 0; i < rt.NumField(); i++ {
		if k := rt.Field(i).Tag.Get("env"); k != "" {
			keys[k] = true
		}
	}
	return keys
}

// A template variable the app does not read is a silent misconfiguration.
func TestUnraidTemplateWellFormedAndKeysKnown(t *testing.T) {
	var c struct {
		Name    string         `xml:"Name"`
		Network string         `xml:"Network"`
		MyIP    string         `xml:"MyIP"`
		WebUI   string         `xml:"WebUI"`
		Configs []unraidConfig `xml:"Config"`
	}
	if err := xml.Unmarshal(deployFile(t, "unraid/tasmofleet.xml"), &c); err != nil {
		t.Fatalf("unraid xml: %v", err)
	}
	if c.Name != "tasmofleet" || c.Network != "br0" || c.MyIP == "" || c.WebUI == "" {
		t.Errorf("template header incomplete: %+v", c)
	}
	keys := configKeys()
	hasData := false
	for _, cfg := range c.Configs {
		switch cfg.Type {
		case "Variable":
			k, ok := strings.CutPrefix(cfg.Target, config.Prefix)
			if !ok || !keys[k] {
				t.Errorf("template variable %q is not a config key", cfg.Target)
			}
		case "Path":
			hasData = hasData || cfg.Target == "/data"
		}
	}
	if !hasData {
		t.Error("template lacks the /data mapping")
	}
}

func TestEnvExampleKeysKnown(t *testing.T) {
	keys := configKeys()
	var bad []string
	for _, line := range strings.Split(string(deployFile(t, "unraid/tasmofleet.env.example")), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "# "))
		k, _, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(k, " ()") {
			continue
		}
		k = strings.TrimPrefix(strings.TrimSuffix(k, "_FILE"), config.Prefix)
		if !keys[k] {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("unknown keys in env example: %v", bad)
	}
}

func TestTraefikExampleMatchesDeployment(t *testing.T) {
	s := string(deployFile(t, "traefik/tasmofleet.yml"))
	for _, want := range []string{"websecure", "172.31.12.61:8080", "/healthz"} {
		if !strings.Contains(s, want) {
			t.Errorf("traefik example lacks %q", want)
		}
	}
}
