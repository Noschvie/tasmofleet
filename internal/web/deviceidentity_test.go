package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

const crFull = "650/699"

// withFWR gives device 1 the Status 2 facts a probe stored.
func withFWR(e *env, fwr model.FWRFacts) {
	d := e.inv.devs[1]
	d.Identity.Hardware = "ESP32-S3 v0.2"
	d.Identity.Variant, d.Identity.Image, d.Identity.FlashSizeKB = "tasmota32", "release-tasmota32", 4096
	d.Identity.FWR = fwr
	e.inv.devs[1] = d
}

var sampleFWR = model.FWRFacts{Version: "16.0.0(tasmota32)", BuildDateTime: "2026-10-16T18:34:59", Core: "3.3.12", SDK: "5.5.5", CPUFrequency: 240, CR: "448/699"}

// Why (#289): users compare the card with `Status 2` on the device, so the StatusFWR fields appear
// under Tasmota's own names with the raw values, each explained, and what TasmoFleet derives
// itself is kept apart and named as its reading instead of being mixed into those fields.
func TestIdentityCardShowsStatusFWRRawAndTasmoFleetsReadingApart(t *testing.T) {
	e := setup(t)
	withFWR(e, sampleFWR)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	card := body[strings.Index(body, `id="identity"`):]
	card = card[:strings.Index(card, "</section>")]
	for _, want := range []string{
		"<dt>Version", `<dd class="mono">16.0.0(tasmota32)</dd>`,
		"<dt>BuildDateTime", `<dd class="mono">2026-10-16T18:34:59</dd>`,
		"<dt>Core", `<dd class="mono">3.3.12</dd>`,
		"<dt>SDK", `<dd class="mono">5.5.5</dd>`,
		"<dt>CpuFrequency", `<dd class="mono">240</dd>`,
		"<dt>Hardware", `<dd class="mono">ESP32-S3 v0.2</dd>`,
		"<dt>CR", `<dd class="mono">448/699</dd>`,
		"TasmoFleet&#39;s reading", "<dt>Variant", "<dt>Flash size",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("Identity card lacks %q:\n%s", want, shortDump(card))
		}
	}
	if n := strings.Count(card, `class="info"`); n != 7 {
		t.Errorf("%d info icons, want one per StatusFWR field (7)", n)
	}
	if strings.Index(card, "TasmoFleet&#39;s reading") < strings.Index(card, "<dt>CR") {
		t.Error("TasmoFleet's reading must come after, not inside, the StatusFWR fields")
	}
}

// Why (#289): a device identified before 0.22 has no raw StatusFWR; the card says so instead of
// showing empty or invented values, and still shows the hardware it knows.
func TestIdentityCardWithoutStoredStatusFWRSaysSo(t *testing.T) {
	e := setup(t)
	withFWR(e, model.FWRFacts{})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "Refresh identity") || !strings.Contains(body, "not read yet") {
		t.Errorf("no hint that Refresh identity reads Status 2:\n%s", shortDump(body))
	}
	if strings.Contains(body, "<dt>BuildDateTime") {
		t.Error("BuildDateTime shown without a value")
	}
}

// Why (#289): CR is the settings text area; once it is nearly full a longer OtaUrl silently does
// not stick. The warning names the numbers and the consequence, and appears only above the
// configured threshold (default 90 %).
func TestIdentityCardWarnsWhenTheSettingsTextAreaIsAlmostFull(t *testing.T) {
	e := setup(t)
	withFWR(e, model.FWRFacts{Version: "16.0.0(tasmota32)", CR: crFull})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "Settings text area almost full (650/699): longer values such as OtaUrl may not be saved") {
		t.Errorf("no CR warning at 93 %%:\n%s", shortDump(body))
	}
	withFWR(e, sampleFWR) // 448/699 = 64 %
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, "Settings text area almost full") {
		t.Error("warned at 64 %")
	}
}

// Why (#289): the threshold is a system setting, not a constant.
func TestCRWarningFollowsTheConfiguredThreshold(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.CRWarnPercent = func() int { return 60 } })
	withFWR(e, sampleFWR)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "Settings text area almost full (448/699)") {
		t.Errorf("64 %% must warn at a 60 %% threshold:\n%s", shortDump(body))
	}
	e = setupWith(t, func(d *Deps) { d.CRWarnPercent = func() int { return 100 } })
	withFWR(e, model.FWRFacts{CR: "699/699"})
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, "Settings text area almost full") {
		t.Error("exactly full is not more than 100 %, the threshold is exclusive")
	}
}

// Why (#289): the header names the device by its hostname too (part of #290), so a device is
// recognisable on every tab.
func TestDeviceHeaderShowsTheHostname(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	d.Net.Hostname = "kitchen-lamp-1234"
	e.inv.devs[1] = d
	for _, p := range []string{"/devices/1", "/devices/1/backups"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		if !strings.Contains(body, `<span data-visual-mask>kitchen-lamp-1234</span>`) {
			t.Errorf("%s: header lacks the hostname", p)
		}
	}
}

// Why (#289): Make compliant writes text values into the same small area; the preview must warn
// before the admin confirms a plan that no longer fits.
func TestPolicyPreviewWarnsWhenATextValueWouldNotFit(t *testing.T) {
	rule := policy.Rule{ID: "r1", Command: "OtaUrl", Want: "http://fw.example.internal/tasmota32.bin"} // 40 bytes
	preview := func(cr string) string {
		rep := PolicyReport{Rules: []policy.Rule{rule},
			Devices:    []model.Device{{ID: 1, Name: "kitchen", Identity: model.Identity{FWR: model.FWRFacts{CR: cr}}}},
			Findings:   []policy.Finding{{DeviceID: 1, Rule: rule, Have: "off"}},
			Provenance: map[model.DeviceID]map[string]ValueSource{1: {"otaurl": {Kind: "live"}}}}
		e := setupWith(t, func(d *Deps) { d.Policy, d.PolicyFix = &fakePolicy{rep: rep}, &fakeFix{} })
		return previewFor(t, e, url.Values{"only": {"1"}})
	}
	if body := preview("690/699"); !strings.Contains(body, "Settings text area almost full (690/699)") || !strings.Contains(body, "may not be saved") {
		t.Errorf("preview lacks the text-area warning:\n%s", shortDump(body[strings.Index(body, "pol-fix-grid"):]))
	}
	if body := preview("100/699"); strings.Contains(body, "may not be saved") {
		t.Error("warned although the value fits")
	}
	if body := preview(""); strings.Contains(body, "may not be saved") {
		t.Error("warned for a device whose CR was never read")
	}
}

// clip keeps a failing assertion's page dump short.
func shortDump(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "..."
	}
	return s
}

// Why (#289 review): the growth of a text value is its new length minus the old one; the time
// zone settings are not in the text area at all.
func TestTextGrowth(t *testing.T) {
	for _, tt := range []struct {
		cmd, have, want string
		grow            int
	}{
		{"OtaUrl", "http://a", "http://abcdef", 5},
		{"OtaUrl", "", "http://abc", 10},
		{"OtaUrl", "http://abcdef", "http://a", -5},
		{"TimeDst", "0,0,3,1,2,120", "0,0,3,1,2,121", 0},
		{"Timezone", "1", "+01:30", 0},
		{"SetOption0", "1", "0", 0},
	} {
		if got := textGrowth(tt.cmd, tt.have, tt.want); got != tt.grow {
			t.Errorf("%s %q -> %q: %d, want %d", tt.cmd, tt.have, tt.want, got, tt.grow)
		}
	}
}
