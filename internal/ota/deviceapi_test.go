package ota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// fixtureClient answers commands from the captured device fixtures and
// records what was sent, so the tests can prove which commands the OTA
// adapter uses (never Status 0, one at a time).
type fixtureClient struct {
	t       *testing.T
	dir     string
	minimal bool // Status 2..11 answer Unknown, like minimal firmware
	errs    map[string]error
	replies map[string]string
	root    []byte // main page served instead of the fixture's root.html
	sent    []string
}

func (c *fixtureClient) fixture(name string) json.RawMessage {
	b, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		c.t.Fatalf("fixture: %v", err)
	}
	return b
}

func (c *fixtureClient) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	c.sent = append(c.sent, cmnd)
	if err := c.errs[cmnd]; err != nil {
		return nil, err
	}
	if r, ok := c.replies[cmnd]; ok {
		return json.RawMessage(r), nil
	}
	if strings.HasPrefix(cmnd, "OtaUrl ") {
		b, _ := json.Marshal(map[string]string{"OtaUrl": strings.TrimPrefix(cmnd, "OtaUrl ")})
		return b, nil
	}
	if c.minimal && strings.HasPrefix(cmnd, "Status ") {
		return c.fixture("unknown-foobar.json"), nil
	}
	switch cmnd {
	case "Status 2", "Status 4", "Status 11":
		return c.fixture("status-" + strings.TrimPrefix(cmnd, "Status ") + ".json"), nil
	case "OtaUrl":
		return c.fixture("otaurl.json"), nil
	case "Upgrade":
		return c.fixture("upgrade.json"), nil
	}
	c.t.Fatalf("unexpected command %q", cmnd)
	return nil, nil
}

func (c *fixtureClient) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	c.t.Fatal("OTA must not download configs")
	return nil, nil
}

func (c *fixtureClient) RootPage(_ context.Context, _ tasmota.Target) (tasmota.RootInfo, error) {
	c.sent = append(c.sent, "GET /")
	if err := c.errs["GET /"]; err != nil {
		return tasmota.RootInfo{}, err
	}
	if c.root != nil {
		return tasmota.ParseRootPage(c.root)
	}
	return tasmota.ParseRootPage(c.fixture("root.html"))
}

const cabinet = "../../testdata/fixtures/devices/bedroom-cabinet"

func newTestAPI(c *fixtureClient, got *fwselect.Inputs, detectErr error) *deviceAPI {
	return &deviceAPI{c: c, detect: func(in fwselect.Inputs) (model.Identity, error) {
		*got = in
		if detectErr != nil {
			return model.Identity{}, detectErr
		}
		return model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota"}, nil
	}}
}

var cabinetDevice = model.Device{ID: 5, Address: "192.0.2.5", Identity: model.Identity{FlashSizeKB: 1024, ProgramFlashSizeKB: 1024, FreeKB: 352}}

// Why (#686): the main page is HTML for people that changes with the firmware; full firmware is named by
// Status 2 and its language build by the locale ID in Status 4, so the identity probe of the update preview
// (and of a job) reads no page at all. Individual queries in a fixed order; never Status 0 (spliced replies
// on low-heap ESP8266) and no Upgrade probe on normal firmware.
func TestTheUpdatePreviewOfFullFirmwareReadsNoMainPage(t *testing.T) {
	for _, fam := range []model.ChipFamily{model.ChipUnknown, model.ChipESP8266} {
		c := &fixtureClient{t: t, dir: cabinet}
		var in fwselect.Inputs
		id, otaURL, up, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice, fam)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Status 2", "Status 4", "Status 11", "OtaUrl"}
		if !reflect.DeepEqual(c.sent, want) {
			t.Fatalf("job family %q: sent %v, want %v", fam, c.sent, want)
		}
		if !strings.Contains(string(in.Status2), "StatusFWR") || !strings.Contains(string(in.Status4), "StatusMEM") || in.Upgrade != nil || in.Root != nil {
			t.Fatalf("detect inputs %+v", in)
		}
		if up != 4235 || otaURL != "http://tasmoadmin.example.internal:80/data/firmwares/tasmota-minimal.bin.gz" || id.Version != "16.0.0" {
			t.Fatalf("uptime %d otaURL %q id %+v", up, otaURL, id)
		}
	}
	// With the real Detect the language is the locale of Status 4: a German build is German without a page.
	c := &fixtureClient{t: t, dir: cabinet}
	c.replies = map[string]string{"Status 4": strings.Replace(string(c.fixture("status-4.json")), `"Features":["0809",`, `"Features":["0407",`, 1)}
	a := NewDeviceAPI(c).(*deviceAPI)
	a.gap = 0
	id, _, _, err := a.ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown)
	if err != nil || !id.LanguageKnown || id.Language != "DE" {
		t.Fatalf("identity %+v, err %v, want the German build read from Status 4", id, err)
	}
	for _, cmd := range c.sent {
		if cmd == "GET /" {
			t.Fatalf("the main page was read: %v", c.sent)
		}
	}
}

// Minimal firmware knows only five commands: Status 2 is Unknown, the
// version comes from the bare Upgrade probe. Outside a job (the preview, a
// retry's look) nothing else names the image, so the client's main page is read
// (the router reads an INFO1 first there, #686: path 3 of the fallback).
// Flash data cannot be read there, so the last known values stay.
func TestDeviceAPIProbeIdentityMinimal(t *testing.T) {
	for _, viaErr := range []bool{false, true} {
		c := &fixtureClient{t: t, dir: cabinet, minimal: true}
		if viaErr {
			c.errs = map[string]error{"Status 2": tasmota.ErrUnknownCommand}
		}
		var in fwselect.Inputs
		id, _, up, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Status 2", "Upgrade", "GET /", "OtaUrl"}
		if !reflect.DeepEqual(c.sent, want) {
			t.Fatalf("sent %v, want %v", c.sent, want)
		}
		if in.Upgrade == nil || in.Upgrade.Version != "16.0.0" || !strings.Contains(string(in.Status2), "Unknown") || in.Root == nil {
			t.Fatalf("detect inputs %+v", in)
		}
		if up != 0 || id.FlashSizeKB != 1024 || id.FreeKB != 352 || id.ProgramFlashSizeKB != 1024 {
			t.Fatalf("uptime %d id %+v", up, id)
		}
	}
}

func TestDeviceAPIProbeIdentityErrors(t *testing.T) {
	for _, cmd := range []string{"Status 2", "Status 4", "Status 11", "OtaUrl"} {
		c := &fixtureClient{t: t, dir: cabinet, errs: map[string]error{cmd: tasmota.ErrUnreachable}}
		var in fwselect.Inputs
		if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown); !errors.Is(err, tasmota.ErrUnreachable) {
			t.Errorf("%s failing: err %v", cmd, err)
		}
	}
	// Minimal firmware outside a job needs the page; without it the probe fails.
	c0 := &fixtureClient{t: t, dir: cabinet, minimal: true, errs: map[string]error{"GET /": tasmota.ErrUnreachable}}
	var in0 fwselect.Inputs
	if _, _, _, err := newTestAPI(c0, &in0, nil).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Errorf("main page of minimal firmware failing: err %v", err)
	}
	c := &fixtureClient{t: t, dir: cabinet}
	var in fwselect.Inputs
	if _, _, _, err := newTestAPI(c, &in, fwselect.ErrFamilyMismatch).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown); !errors.Is(err, fwselect.ErrFamilyMismatch) {
		t.Errorf("detect error not returned: %v", err)
	}
	c = &fixtureClient{t: t, dir: cabinet, replies: map[string]string{"Status 2": `{"StatusFWR":`}}
	if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown); err == nil {
		t.Error("broken Status 2 accepted")
	}
	c = &fixtureClient{t: t, dir: cabinet, minimal: true, errs: map[string]error{"Upgrade": tasmota.ErrTimeout}}
	if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown); !errors.Is(err, tasmota.ErrTimeout) {
		t.Errorf("upgrade probe error: %v", err)
	}
	c = &fixtureClient{t: t, dir: cabinet, minimal: true, replies: map[string]string{"Upgrade": `{"Upgrade":"what"}`}}
	if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown); err == nil {
		t.Error("unparsable upgrade probe accepted")
	}
}

// Probe is the cheap check while the device flashes: two commands, and a
// device that answers counts as online even when Detect cannot place it.
func TestDeviceAPIProbe(t *testing.T) {
	c := &fixtureClient{t: t, dir: cabinet}
	var in fwselect.Inputs
	id, up, err := newTestAPI(c, &in, errors.New("no language")).Probe(context.Background(), cabinetDevice, model.ChipUnknown)
	if err != nil || up != 4235 || id != (model.Identity{}) {
		t.Fatalf("id %+v up %d err %v", id, up, err)
	}
	if want := []string{"Status 2", "Status 11"}; !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %v, want %v", c.sent, want)
	}
	c = &fixtureClient{t: t, dir: cabinet, minimal: true}
	id, up, err = newTestAPI(c, &in, nil).Probe(context.Background(), cabinetDevice, model.ChipUnknown)
	if err != nil || up != 0 || id.Version != "16.0.0" {
		t.Fatalf("minimal: id %+v up %d err %v", id, up, err)
	}
	if want := []string{"Status 2", "Upgrade", "GET /"}; !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %v, want %v", c.sent, want)
	}
	c = &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"Status 2": tasmota.ErrUnreachable}}
	if _, _, err := newTestAPI(c, &in, nil).Probe(context.Background(), cabinetDevice, model.ChipUnknown); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("offline device: err %v", err)
	}
	c = &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"Status 11": tasmota.ErrTimeout}}
	if _, _, err := newTestAPI(c, &in, nil).Probe(context.Background(), cabinetDevice, model.ChipUnknown); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("status 11 failing: err %v", err)
	}
}

// OtaUrl is checked by its echo: a device that stored something else must
// not get Upgrade.
func TestDeviceAPISetOtaURL(t *testing.T) {
	u := "http://fw.test/fw/16.0.0/tasmota-minimal.bin.gz"
	c := &fixtureClient{t: t, dir: cabinet}
	a := newTestAPI(c, new(fwselect.Inputs), nil)
	if err := a.SetOtaURL(context.Background(), cabinetDevice, u); err != nil {
		t.Fatal(err)
	}
	if c.sent[0] != "OtaUrl "+u {
		t.Fatalf("sent %q", c.sent[0])
	}
	c.replies = map[string]string{"OtaUrl " + u: `{"OtaUrl":"http://truncated"}`}
	if err := a.SetOtaURL(context.Background(), cabinetDevice, u); err == nil {
		t.Fatal("mismatching OtaUrl echo accepted")
	}
	c.replies = map[string]string{"OtaUrl " + u: `{"Command":"Error"}`}
	if err := a.SetOtaURL(context.Background(), cabinetDevice, u); err == nil {
		t.Fatal("error reply accepted")
	}
	c.errs = map[string]error{"OtaUrl " + u: tasmota.ErrAuth}
	if err := a.SetOtaURL(context.Background(), cabinetDevice, u); !errors.Is(err, tasmota.ErrAuth) {
		t.Fatalf("err %v", err)
	}
}

func TestDeviceAPIUpgrade(t *testing.T) {
	cases := []struct {
		name, reply, echo string
		ok                bool
	}{
		{"accepted", `{"Upgrade":"Version 16.0.0 from http://fw.test/fw/16.0.0/tasmota.bin.gz"}`, "http://fw.test/fw/16.0.0/tasmota.bin.gz", true},
		// "1 or >X" means the device did not start (e.g. version not newer).
		{"not started", `{"Upgrade":"1 or >16.0.0 to upgrade"}`, "", false},
		{"unknown", `{"Command":"Unknown","Input":"UPGRADE 1"}`, "", false},
		{"garbage", `not json`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fixtureClient{t: t, dir: cabinet, replies: map[string]string{"Upgrade 1": tc.reply}}
			echo, err := newTestAPI(c, new(fwselect.Inputs), nil).Upgrade(context.Background(), cabinetDevice)
			if (err == nil) != tc.ok || echo != tc.echo {
				t.Fatalf("echo %q err %v", echo, err)
			}
			if !reflect.DeepEqual(c.sent, []string{"Upgrade 1"}) {
				t.Fatalf("sent %v", c.sent)
			}
		})
	}
	c := &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"Upgrade 1": tasmota.ErrTimeout}}
	if _, err := newTestAPI(c, new(fwselect.Inputs), nil).Upgrade(context.Background(), cabinetDevice); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err %v", err)
	}
}

// The production constructor keeps the inter-command gap and real Detect.
func TestNewDeviceAPI(t *testing.T) {
	a, ok := NewDeviceAPI(&fixtureClient{t: t, dir: cabinet}).(*deviceAPI)
	if !ok || a.gap != commandGap || a.detect == nil {
		t.Fatalf("%+v", a)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.cmd(ctx, tasmota.Target{}, "Status 2"); !errors.Is(err, context.Canceled) {
		t.Fatalf("gap ignores cancelled context: %v", err)
	}
}

// With the real fwselect.Detect, every captured device fixture yields a
// known identity: the adapter passes Detect what it needs.
func TestDeviceAPIRealDetectOnFixtures(t *testing.T) {
	dirs, err := filepath.Glob("../../testdata/fixtures/devices/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			a := NewDeviceAPI(&fixtureClient{t: t, dir: dir}).(*deviceAPI)
			a.gap = 0
			id, _, up, err := a.ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown)
			if err != nil {
				t.Fatal(err)
			}
			if !id.Known() || up <= 0 || id.FlashSizeKB == 0 {
				t.Fatalf("identity %+v uptime %d", id, up)
			}
		})
	}
}

// vendorFooterPage is the real footer of a MediaRath build (#872, reporter's page source) in a
// SYNTHETIC page shell with the <html lang="de"> of a stock German build (testdata/fixtures README).
func vendorFooterPage(t *testing.T) []byte {
	t.Helper()
	footer, err := os.ReadFile("../../testdata/fixtures/vendor-footer/mediarath-14.3.0-de.html")
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`<!DOCTYPE html><html lang="de" class=""><head><title>Prusa Plug Main Menu</title></head><body>` + string(footer) + `</body></html>`)
}

// readOnlyCommands are the queries ProbeIdentity may send; anything else (OtaUrl <url>, Upgrade 1)
// would change the device.
var readOnlyCommands = map[string]bool{"Status 2": true, "Status 4": true, "Status 11": true, "Upgrade": true, "GET /": true, "OtaUrl": true}

// #872: a NOUS A1T on a vendor build (Status 2 "14.3.0(tasmota-NOUS_A1T_DE)", SYNTHETIC: the version
// is the one in the issue, the raw reply was not posted) whose main page footer has no image was
// refused before the variant was looked at ("root page: ... no Tasmota footer"), so even the Variant
// override could not help. Why it matters: the device must reach the variant decision, and that
// decision must still refuse until the person chooses a variant (CLAUDE.md safety rule 1): the
// custom image is never guessed to be a stock one. Since #686 the page of full firmware is not read
// at all; the German build is told by the locale in Status 4 (SYNTHETIC: 0407 in the capture's reply).
func TestProbeIdentityVendorFooterRefusesWithoutVariantChoice(t *testing.T) {
	c := &fixtureClient{
		t: t, dir: "../../testdata/fixtures/devices/prusa-plug", root: vendorFooterPage(t),
		replies: map[string]string{"Status 2": `{"StatusFWR":{"Version":"14.3.0(tasmota-NOUS_A1T_DE)","BuildDateTime":"2026-10-16T07:29:08","Boot":31,"Core":"2.7.8","SDK":"2.2.2-dev(38a443e)","CpuFrequency":80,"Hardware":"ESP8266EX","CR":"359/699"}}`},
	}
	c.replies["Status 4"] = strings.Replace(string(c.fixture("status-4.json")), `"Features":["0809",`, `"Features":["0407",`, 1)
	a := NewDeviceAPI(c).(*deviceAPI)
	a.gap = 0
	id, _, _, err := a.ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown)
	if err != nil {
		t.Fatalf("probe of a full-firmware device with a vendor footer: %v", err)
	}
	if id.Family != model.ChipESP8266 || id.Version != "14.3.0" || id.Variant != "unknown" || !id.LanguageKnown || id.Language != "DE" {
		t.Fatalf("identity %+v, want esp8266 14.3.0, variant unknown, language DE read from Status 4", id)
	}
	for _, cmd := range c.sent {
		if !readOnlyCommands[cmd] || cmd == "GET /" {
			t.Fatalf("the probe sent %q", cmd)
		}
	}

	if _, err := fwselect.Select(id, fwselect.Choice{Release: "16.0.0"}); !errors.Is(err, fwselect.ErrVariantChange) {
		t.Fatalf("select without a variant choice: %v, want ErrVariantChange", err)
	}
	chosen := fwselect.Choice{Release: "16.0.0", Variant: "tasmota", AllowVariantChange: true}
	sel, err := fwselect.Select(id, chosen)
	if err != nil || sel.Filename != "tasmota-DE.bin.gz" {
		t.Fatalf("select with variant tasmota: %+v, %v, want tasmota-DE.bin.gz", sel, err)
	}
	art := model.Artifact{Release: "16.0.0", Filename: "tasmota-DE.bin.gz", Family: model.ChipESP8266, Variant: "tasmota", Language: "DE"}
	const otaURL = "http://fleet.example:8080/fw/16.0.0/tasmota-DE.bin.gz"
	if err := fwselect.Guard(id, art, otaURL, fwselect.Choice{Release: "16.0.0"}); !errors.Is(err, fwselect.ErrVariantChange) {
		t.Fatalf("guard without a variant choice: %v, want ErrVariantChange", err)
	}
	if err := fwselect.Guard(id, art, otaURL, chosen); err != nil {
		t.Fatalf("guard with variant tasmota: %v", err)
	}
}

// Why: on minimal firmware the footer image is the only proof of the chip family (Status 2 is
// Unknown there). Reading a footer without an image (#872) must never let such a device be
// identified: the probe must end in ErrUnknownHardware, and nothing that changes the device is sent.
func TestProbeIdentityMinimalPageWithoutImageIsRefused(t *testing.T) {
	page := `<!DOCTYPE html><html lang="en" class=""><body><div style='text-align:center;'><h3>MINIMAL firmware<br>please upgrade</h3></div>` +
		`<div style='text-align:right;font-size:11px;'><hr><a href='https://github.com/arendst/Tasmota' target='_blank' style='color:#aaa;'>Tasmota 16.0.0 by Theo Arends</a></div></body></html>`
	c := &fixtureClient{t: t, dir: cabinet, minimal: true, root: []byte(page)}
	a := NewDeviceAPI(c).(*deviceAPI)
	a.gap = 0
	_, _, _, err := a.ProbeIdentity(context.Background(), cabinetDevice, model.ChipUnknown)
	if !errors.Is(err, fwselect.ErrUnknownHardware) {
		t.Fatalf("minimal page without image: %v, want ErrUnknownHardware", err)
	}
	for _, cmd := range c.sent {
		if !readOnlyCommands[cmd] {
			t.Fatalf("the probe sent %q", cmd)
		}
	}
}

// errRefused marks a test row that only needs a refusal, of any kind.
var errRefused = errors.New("refused")

// info1Client is a fixture client that also offers the router's INFO1-only read (transport.Router.Info1Root).
type info1Client struct {
	*fixtureClient
	info1 tasmota.RootInfo
	err   error
}

func (c *info1Client) Info1Root(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	c.sent = append(c.sent, "INFO1")
	return c.info1, c.err
}

// Why (#686): an update job through minimal (ESP8266) or safeboot (ESP32) must not read the main page, an
// HTML page for people that changes with the firmware: the job read the chip at its preflight, so the bare
// Upgrade reply names the version and the job's family the intermediate image. Neither the liveness probe of
// every poll nor the identity probe of the verify reads the page, and the identity is the one the machine
// needs to go on (minimal: ESP8266, safeboot: family unknown, as before).
func TestAJobThroughMinimalOrSafebootReadsNoMainPage(t *testing.T) {
	for _, tc := range []struct {
		fam     model.ChipFamily
		variant string
		family  model.ChipFamily
	}{
		{model.ChipESP8266, "minimal", model.ChipESP8266},
		{model.ChipESP32, "safeboot", model.ChipUnknown},
		{model.ChipESP32C3, "safeboot", model.ChipUnknown},
	} {
		for _, withRouter := range []bool{false, true} {
			fc := &fixtureClient{t: t, dir: cabinet, minimal: true}
			var c tasmota.Client = fc
			if withRouter {
				c = &info1Client{fixtureClient: fc, err: transport.ErrNoInfo} // MQTT carries the device but sent no INFO1 since its boot
			}
			a := NewDeviceAPI(c).(*deviceAPI)
			a.gap = 0
			id, up, err := a.Probe(context.Background(), cabinetDevice, tc.fam)
			if err != nil || up != 0 || id.Variant != tc.variant || id.Family != tc.family || id.Version != "16.0.0" || id.Image != "" {
				t.Fatalf("%s probe: id %+v up %d err %v", tc.fam, id, up, err)
			}
			id, _, _, err = a.ProbeIdentity(context.Background(), cabinetDevice, tc.fam)
			if err != nil || id.Variant != tc.variant || id.Family != tc.family {
				t.Fatalf("%s identity probe: id %+v err %v", tc.fam, id, err)
			}
			for _, cmd := range fc.sent {
				if cmd == "GET /" {
					t.Fatalf("%s job read the main page: %v", tc.fam, fc.sent)
				}
			}
		}
	}
}

// Why (#686, MQTT first): inside a job the INFO1 a device usable over MQTT published since its last boot names
// the image, and it must be the intermediate image of the job's chip, on every transport: a safeboot INFO1 on an
// ESP8266 job (another device on the topic, a forged INFO1) stops the job instead of passing as minimal. An
// INFO1 the router cannot vouch for (MQTT down or not usable) leaves the job to its own family; any other
// failure of the read fails the probe rather than being guessed around. The page is never read.
func TestAJobTakesTheImageFromTheINFO1AndRefusesOneOfAnotherChip(t *testing.T) {
	minimal := tasmota.RootInfo{Version: "16.0.0", Image: "release-minimal", Minimal: true}
	safeboot := tasmota.RootInfo{Version: "16.0.0", Image: "release-safeboot", Minimal: true}
	for _, tc := range []struct {
		name    string
		info1   tasmota.RootInfo
		err     error
		fam     model.ChipFamily
		image   string
		wantErr error // nil: accepted; errRefused: refused, whatever the kind
	}{
		{"minimal INFO1 on an esp8266 job", minimal, nil, model.ChipESP8266, "release-minimal", nil},
		{"safeboot INFO1 on an esp32 job", safeboot, nil, model.ChipESP32, "release-safeboot", nil},
		{"safeboot INFO1 on an esp8266 job", safeboot, nil, model.ChipESP8266, "", fwselect.ErrFamilyMismatch},
		{"minimal INFO1 on an esp32-s3 job", minimal, nil, model.ChipESP32S3, "", fwselect.ErrFamilyMismatch},
		{"INFO1 of another version than the Upgrade reply", tasmota.RootInfo{Version: "15.0.0", Image: "release-minimal"}, nil, model.ChipESP8266, "", errRefused},
		{"MQTT not usable", tasmota.RootInfo{}, tasmota.ErrUnreachable, model.ChipESP8266, "", nil},
		{"INFO1 unreadable", tasmota.RootInfo{}, tasmota.ErrBadResponse, model.ChipESP8266, "", tasmota.ErrBadResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fixtureClient{t: t, dir: cabinet, minimal: true}
			a := NewDeviceAPI(&info1Client{fixtureClient: fc, info1: tc.info1, err: tc.err}).(*deviceAPI)
			a.gap = 0
			id, _, _, err := a.ProbeIdentity(context.Background(), cabinetDevice, tc.fam)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("err %v", err)
			case tc.wantErr != nil && err == nil:
				t.Fatalf("identity %+v accepted, want an error", id)
			case tc.wantErr != nil && !errors.Is(tc.wantErr, errRefused) && !errors.Is(err, tc.wantErr):
				t.Fatalf("err %v, want %v", err, tc.wantErr)
			}
			if err == nil && id.Image != tc.image {
				t.Fatalf("image %q, want %q", id.Image, tc.image)
			}
			if !slices.Contains(fc.sent, "INFO1") || slices.Contains(fc.sent, "GET /") {
				t.Fatalf("sent %v: the job must ask the INFO1 and never the page", fc.sent)
			}
		})
	}
}
