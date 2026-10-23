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
	if in.Root == nil || in.Root.Lang != "en" || in.Root.Image != "release-tasmota" {
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

// Relay state from the captured Status 11 (POWER on a one-relay device), and
// POWERn on multi-relay devices; relays are numbered like Power<n>.
func TestDeviceAPIReadPower(t *testing.T) {
	c := &fixtureClient{t: t, dir: cabinet}
	var in fwselect.Inputs
	api := newTestAPI(c, &in, nil)
	r, err := api.ReadPower(context.Background(), cabinetDevice)
	if err != nil || r.On != model.RelaySet(1) || r.Unknown != 0 {
		t.Fatalf("one-relay capture: %+v %v", r, err)
	}
	c.replies = map[string]string{"Status 11": `{"StatusSTS":{"POWER1":"ON","POWER2":"OFF","POWER3":"on","POWER4":"ON","Wifi":{}}}`}
	r, err = api.ReadPower(context.Background(), cabinetDevice)
	if err != nil || r.On != relaysOn(1, 3, 4) || r.Unknown != 0 {
		t.Fatalf("four relays: %+v %v", r, err)
	}
	if !reflect.DeepEqual(c.sent, []string{"Status 11", "Status 11"}) {
		t.Fatalf("sent %v: reading the relays is Status 11 only", c.sent)
	}
}

// Every uncertain Status 11 fails closed: a relay is never assumed to be in a
// state the device did not state clearly.
func TestDeviceAPIReadPowerUncertainInput(t *testing.T) {
	read := func(reply string, errs map[string]error) (PowerReading, error) {
		c := &fixtureClient{t: t, dir: cabinet, replies: map[string]string{"Status 11": reply}, errs: errs}
		var in fwselect.Inputs
		return newTestAPI(c, &in, nil).ReadPower(context.Background(), cabinetDevice)
	}
	// a custom StateText is neither ON nor OFF: unknown, not "OFF"
	r, err := read(`{"StatusSTS":{"POWER1":"AN","POWER2":"OFF","POWER3":""}}`, nil)
	if err != nil || r.On != 0 || r.Unknown != relaysOn(1, 3) {
		t.Fatalf("custom state text: %+v %v", r, err)
	}
	for name, reply := range map[string]string{
		"POWER0 cannot be attributed":         `{"StatusSTS":{"POWER0":"ON"}}`,
		"relay beyond the range":              `{"StatusSTS":{"POWER99":"ON"}}`,
		"POWER and POWER1 are the same relay": `{"StatusSTS":{"POWER":"ON","POWER1":"OFF"}}`,
		"no StatusSTS":                        `{"Status":{"Module":1}}`,
		"not JSON":                            `<html>`,
		"StatusSTS is not an object":          `{"StatusSTS":"x"}`,
	} {
		if r, err := read(reply, nil); err == nil {
			t.Errorf("%s: got %+v, want an error", name, r)
		}
	}
	// a device without relays has nothing to restore, which is not an error
	if r, err := read(`{"StatusSTS":{"Wifi":{}}}`, nil); err != nil || r.On != 0 || r.Unknown != 0 {
		t.Errorf("no relays: %+v %v", r, err)
	}
	if _, err := read("", map[string]error{"Status 11": tasmota.ErrTimeout}); !errors.Is(err, tasmota.ErrTimeout) {
		t.Errorf("transport error: %v", err)
	}
}

func TestDeviceAPIReadMAC(t *testing.T) {
	var in fwselect.Inputs
	c := &fixtureClient{t: t, dir: cabinet, replies: map[string]string{"Status 5": `{"StatusNET":{"Hostname":"x","Mac":"AA:BB:CC:00:11:22"}}`}}
	if mac, err := newTestAPI(c, &in, nil).ReadMAC(context.Background(), cabinetDevice); err != nil || mac != "aa:bb:cc:00:11:22" {
		t.Fatalf("%q %v", mac, err)
	}
	for _, reply := range []string{`{"StatusNET":{}}`, `{}`, `x`} {
		c.replies["Status 5"] = reply
		if mac, err := newTestAPI(c, &in, nil).ReadMAC(context.Background(), cabinetDevice); err == nil {
			t.Errorf("%s: got %q, want an error", reply, mac)
		}
	}
}

// PulseTime, Interlock and shutter relays are found with bare queries (reads
// in Tasmota).
func TestDeviceAPIRestrictedRelays(t *testing.T) {
	ctx := context.Background()
	var in fwselect.Inputs
	all := relaysOn(1, 2, 3, 4, 5, 6, 7, 8)
	api := func(c *fixtureClient) *deviceAPI { return newTestAPI(c, &in, nil) }
	idle := map[string]string{
		"PulseTime": `{"PulseTime":{"Set":[0,0,0,0,0,0,0,0]}}`, "Interlock": `{"Interlock":"OFF","Groups":""}`, "ShutterRelay": `{}`}
	with := func(over map[string]string) *fixtureClient {
		r := map[string]string{}
		for k, v := range idle {
			r[k] = v
		}
		for k, v := range over {
			r[k] = v
		}
		return &fixtureClient{t: t, dir: cabinet, replies: r}
	}

	c := &fixtureClient{t: t, dir: cabinet, replies: map[string]string{
		"PulseTime":    `{"PulseTime":{"Set":[0,100,0,0,0,0,0,0],"Remaining":[0,0,0,0,0,0,0,0]}}`,
		"Interlock":    `{"Interlock":"ON","Groups":"3,4 5,6"}`,
		"ShutterRelay": `{"ShutterRelay1":7}`,
	}}
	got, err := api(c).RestrictedRelays(ctx, cabinetDevice, all)
	if err != nil || got != relaysOn(2, 3, 4, 5, 6, 7, 8) {
		t.Fatalf("restricted %s %v, want 2 (PulseTime), 3-6 (Interlock), 7-8 (shutter)", got, err)
	}
	if !reflect.DeepEqual(c.sent, []string{"PulseTime", "Interlock", "ShutterRelay"}) {
		t.Fatalf("sent %v, want three bare queries without payload", c.sent)
	}
	// Interlock ON without groups ties all relays together; OFF ties none.
	if got, _ := api(with(map[string]string{"Interlock": `{"Interlock":"ON","Groups":""}`})).RestrictedRelays(ctx, cabinetDevice, all); got != all {
		t.Fatalf("interlock without groups: %s", got)
	}
	if got, err := api(with(nil)).RestrictedRelays(ctx, cabinetDevice, all); err != nil || got != 0 {
		t.Fatalf("nothing restricted: %s %v", got, err)
	}
	// the older PulseTime format is understood, not mistaken for "none"
	old := with(map[string]string{"PulseTime": `{"PulseTime1":{"Set":0,"Remaining":0},"PulseTime2":{"Set":50,"Remaining":0}}`})
	if got, err := api(old).RestrictedRelays(ctx, cabinetDevice, all); err != nil || got != relaysOn(2) {
		t.Fatalf("old PulseTime format: %s %v", got, err)
	}
	// firmware without the shutter driver: no shutters
	c = with(nil)
	c.errs = map[string]error{"ShutterRelay": tasmota.ErrUnknownCommand}
	if got, err := api(c).RestrictedRelays(ctx, cabinetDevice, all); err != nil || got != 0 {
		t.Fatalf("no shutter driver: %s %v", got, err)
	}
}

// Fail closed: a reply that does not have the expected shape is an error, so
// preflight stops (with the opt-out hint) instead of treating "I did not
// understand" as "no restrictions".
func TestDeviceAPIRestrictedRelaysUnexpectedShape(t *testing.T) {
	ctx := context.Background()
	var in fwselect.Inputs
	all := relaysOn(1, 2)
	base := map[string]string{
		"PulseTime": `{"PulseTime":{"Set":[0,0]}}`, "Interlock": `{"Interlock":"OFF"}`, "ShutterRelay": `{}`}
	for name, over := range map[string]map[string]string{
		"PulseTime of an unknown format":     {"PulseTime": `{"Pulse":1}`},
		"PulseTime without Set":              {"PulseTime": `{"PulseTime":{}}`},
		"PulseTime that is not an object":    {"PulseTime": `{"PulseTime":"x"}`},
		"old PulseTime with a bad Set":       {"PulseTime": `{"PulseTime1":{"Set":"x"}}`},
		"Interlock key missing (2 relays)":   {"Interlock": `{"Groups":"1,2"}`},
		"Interlock not JSON (2 relays)":      {"Interlock": `nope`},
		"ShutterRelay value is not a number": {"ShutterRelay": `{"ShutterRelay1":"x"}`},
		"PulseTime not JSON":                 {"PulseTime": `nope`},
	} {
		r := map[string]string{}
		for k, v := range base {
			r[k] = v
		}
		for k, v := range over {
			r[k] = v
		}
		c := &fixtureClient{t: t, dir: cabinet, replies: r}
		if got, err := newTestAPI(c, &in, nil).RestrictedRelays(ctx, cabinetDevice, all); err == nil {
			t.Errorf("%s: got %s, want an error", name, got)
		}
	}
	// a one-relay device cannot have an interlock: a reply Tasmota does not
	// give there is no reason to stop
	c := &fixtureClient{t: t, dir: cabinet, replies: map[string]string{
		"PulseTime": base["PulseTime"], "Interlock": `{}`, "ShutterRelay": `{}`}}
	if got, err := newTestAPI(c, &in, nil).RestrictedRelays(ctx, cabinetDevice, relaysOn(1)); err != nil || got != 0 {
		t.Errorf("one relay, odd Interlock reply: %s %v", got, err)
	}
	// a transport error is an error
	c = &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"Interlock": tasmota.ErrTimeout},
		replies: map[string]string{"PulseTime": base["PulseTime"]}}
	if _, err := newTestAPI(c, &in, nil).RestrictedRelays(ctx, cabinetDevice, all); !errors.Is(err, tasmota.ErrTimeout) {
		t.Errorf("timeout: %v", err)
	}
	// unknown PulseTime is not "no pulse time": every Tasmota has the command
	c = &fixtureClient{t: t, dir: cabinet, errs: map[string]error{"PulseTime": tasmota.ErrUnknownCommand}}
	if _, err := newTestAPI(c, &in, nil).RestrictedRelays(ctx, cabinetDevice, all); err == nil {
		t.Error("unknown PulseTime must be an error")
	}
}

// A device with a light driver reports Dimmer/Color/CT in Status 11; its
// power channel (the last POWERn) is the light.
func TestDeviceAPIReadPowerFindsTheLight(t *testing.T) {
	var in fwselect.Inputs
	c := &fixtureClient{t: t, dir: cabinet, replies: map[string]string{
		"Status 11": `{"StatusSTS":{"POWER":"ON","Dimmer":80,"Color":"FF0000","CT":250}}`}}
	api := newTestAPI(c, &in, nil)
	r, err := api.ReadPower(context.Background(), cabinetDevice)
	if err != nil || r.Light != relaysOn(1) || r.On != relaysOn(1) {
		t.Fatalf("pure light: %+v %v", r, err)
	}
	c.replies["Status 11"] = `{"StatusSTS":{"POWER1":"ON","POWER2":"OFF","POWER3":"ON","Dimmer":50}}`
	r, _ = api.ReadPower(context.Background(), cabinetDevice)
	if r.Light != relaysOn(3) || r.All != relaysOn(1, 2, 3) {
		t.Fatalf("relays and a light: %+v", r)
	}
	c.replies["Status 11"] = `{"StatusSTS":{"POWER1":"ON","POWER2":"OFF","Wifi":{}}}`
	if r, _ = api.ReadPower(context.Background(), cabinetDevice); r.Light != 0 {
		t.Fatalf("plain relays have no light: %+v", r)
	}
}
