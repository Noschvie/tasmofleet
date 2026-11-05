package ota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
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

func TestDeviceAPIProbeIdentityNormalFirmware(t *testing.T) {
	c := &fixtureClient{t: t, dir: cabinet}
	var in fwselect.Inputs
	id, otaURL, up, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice)
	if err != nil {
		t.Fatal(err)
	}
	// Individual queries in a fixed order; never Status 0 (spliced replies
	// on low-heap ESP8266) and no Upgrade probe on normal firmware.
	want := []string{"Status 2", "GET /", "Status 4", "Status 11", "OtaUrl"}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %v, want %v", c.sent, want)
	}
	if !strings.Contains(string(in.Status2), "StatusFWR") || !strings.Contains(string(in.Status4), "StatusMEM") || in.Upgrade != nil {
		t.Fatalf("detect inputs %+v", in)
	}
	if in.Root == nil || in.Root.Lang != "en" || !in.Root.LangKnown || in.Root.Image != "release-tasmota" {
		t.Fatalf("root info %+v", in.Root)
	}
	if up != 4235 || otaURL != "http://tasmoadmin.example.internal:80/data/firmwares/tasmota-minimal.bin.gz" || id.Version != "16.0.0" {
		t.Fatalf("uptime %d otaURL %q id %+v", up, otaURL, id)
	}
}

// Minimal firmware knows only five commands: Status 2 is Unknown, the
// version comes from the bare Upgrade probe, image and language from GET /.
// Flash data cannot be read there, so the last known values stay.
func TestDeviceAPIProbeIdentityMinimal(t *testing.T) {
	for _, viaErr := range []bool{false, true} {
		c := &fixtureClient{t: t, dir: cabinet, minimal: true}
		if viaErr {
			c.errs = map[string]error{"Status 2": tasmota.ErrUnknownCommand}
		}
		var in fwselect.Inputs
		id, _, up, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice)
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
	for _, cmd := range []string{"Status 2", "GET /", "Status 4", "Status 11", "OtaUrl"} {
		c := &fixtureClient{t: t, dir: cabinet, errs: map[string]error{cmd: tasmota.ErrUnreachable}}
		var in fwselect.Inputs
		if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice); !errors.Is(err, tasmota.ErrUnreachable) {
			t.Errorf("%s failing: err %v", cmd, err)
		}
	}
	c := &fixtureClient{t: t, dir: cabinet}
	var in fwselect.Inputs
	if _, _, _, err := newTestAPI(c, &in, fwselect.ErrFamilyMismatch).ProbeIdentity(context.Background(), cabinetDevice); !errors.Is(err, fwselect.ErrFamilyMismatch) {
		t.Errorf("detect error not returned: %v", err)
	}
	c = &fixtureClient{t: t, dir: cabinet, replies: map[string]string{"Status 2": `{"StatusFWR":`}}
	if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice); err == nil {
		t.Error("broken Status 2 accepted")
	}
	c = &fixtureClient{t: t, dir: cabinet, minimal: true, errs: map[string]error{"Upgrade": tasmota.ErrTimeout}}
	if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice); !errors.Is(err, tasmota.ErrTimeout) {
		t.Errorf("upgrade probe error: %v", err)
	}
	c = &fixtureClient{t: t, dir: cabinet, minimal: true, replies: map[string]string{"Upgrade": `{"Upgrade":"what"}`}}
	if _, _, _, err := newTestAPI(c, &in, nil).ProbeIdentity(context.Background(), cabinetDevice); err == nil {
		t.Error("unparsable upgrade probe accepted")
	}
}

// Probe is the cheap check while the device flashes: two commands, and a
// device that answers counts as online even when Detect cannot place it.
func TestDeviceAPIProbe(t *testing.T) {
	c := &fixtureClient{t: t, dir: cabinet}
	var in fwselect.Inputs
	id, up, err := newTestAPI(c, &in, errors.New("no language")).Probe(context.Background(), cabinetDevice)
	if err != nil || up != 4235 || id != (model.Identity{}) {
		t.Fatalf("id %+v up %d err %v", id, up, err)
	}
	if want := []string{"Status 2", "Status 11"}; !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %v, want %v", c.sent, want)
	}
	c = &fixtureClient{t: t, dir: cabinet, minimal: true}
	id, up, err = newTestAPI(c, &in, nil).Probe(context.Background(), cabinetDevice)
	if err != nil || up != 0 || id.Version != "16.0.0" {
		t.Fatalf("minimal: id %+v up %d err %v", id, up, err)
	}
	if want := []string{"Status 2", "Upgrade", "GET /"}; !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %v, want %v", c.sent, want)
	}
	c = &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"Status 2": tasmota.ErrUnreachable}}
	if _, _, err := newTestAPI(c, &in, nil).Probe(context.Background(), cabinetDevice); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("offline device: err %v", err)
	}
	c = &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"Status 11": tasmota.ErrTimeout}}
	if _, _, err := newTestAPI(c, &in, nil).Probe(context.Background(), cabinetDevice); !errors.Is(err, tasmota.ErrTimeout) {
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
			id, _, up, err := a.ProbeIdentity(context.Background(), cabinetDevice)
			if err != nil {
				t.Fatal(err)
			}
			if !id.Known() || up <= 0 || id.FlashSizeKB == 0 {
				t.Fatalf("identity %+v uptime %d", id, up)
			}
		})
	}
}
