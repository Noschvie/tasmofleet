package web

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// templateWant is the value an admin types into a template whose Want is blank, in the form
// the device reports (docs/research/tasmota-policy-templates.md).
var templateWant = map[string]string{
	"ntp":            "ntp.lab.home",
	"latitude":       "52.5200",
	"longitude":      "13.4050",
	"otaurl-esp8266": "http://ota.tasmota.com/tasmota/release/tasmota.bin.gz",
	"otaurl-esp32":   "http://ota.tasmota.com/tasmota32/release/tasmota32.bin",
}

// v16Bare is the shape of the bare query as the Tasmota 16 source builds it (the response macro
// and the Cmnd function are in the research note). TelePeriod and PowerOnState are read through
// Status blocks, never bare, so they have no entry.
var v16Bare = map[string]*regexp.Regexp{
	"Timezone":    regexp.MustCompile(`^\{"Timezone":(99|"[+-]\d\d:\d\d")\}$`),
	"NtpServer1":  regexp.MustCompile(`^\{"NtpServer1":"[^"]*"\}$`),
	"Latitude":    regexp.MustCompile(`^\{"Latitude":-?\d+\.\d{6}\}$`),
	"Longitude":   regexp.MustCompile(`^\{"Longitude":-?\d+\.\d{6}\}$`),
	"OtaUrl":      regexp.MustCompile(`^\{"OtaUrl":"[^"]*"\}$`),
	"MqttHost":    regexp.MustCompile(`^\{"MqttHost":"[^"]*"\}$`),
	"PowerRetain": regexp.MustCompile(`^\{"PowerRetain":"(ON|OFF)"\}$`),
	"LedState":    regexp.MustCompile(`^\{"LedState":\d\}$`),
	"Sleep":       regexp.MustCompile(`^\{"Sleep":\{"\d+":\{"Active":"\d+"\}\}\}$`),
}

func loopback() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }

// Why: a template that can never match is a bug that only shows on a real fleet (Sleep answered
// a nested object, Latitude answers float32 noise, and both looked fine with a made-up reply).
// Each template must be accepted as a rule, be sendable by Make compliant without a
// confirmation, be answered by the simulator in the Tasmota 16 format, and, after the device
// was set to the rule's value, be read back as a value the policy judges equal.
func TestTemplatesReadableAndComparable(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopback()})
	ctx := context.Background()

	for _, tp := range policyTemplates("mqtt.lab.home") {
		t.Run(tp.ID, func(t *testing.T) {
			spec := tp.Spec
			if spec.Want == "" {
				spec.Want = templateWant[tp.ID]
			}
			if spec.Want == "" {
				t.Fatal("a template with a blank want needs a documented sample want in templateWant")
			}
			data, err := json.Marshal([]sysconfig.RuleSpec{spec})
			if err != nil {
				t.Fatal(err)
			}
			rules, err := policy.ParseRules(data, settings.Queryable)
			if err != nil {
				t.Fatalf("the rule parser refuses the template: %v", err)
			}
			text, err := policy.CommandText(spec.Command, spec.Want)
			if err != nil {
				t.Fatalf("Make compliant cannot set it: %v", err)
			}
			if _, curated := settings.Curated(spec.Command); !curated {
				// generic: nothing that needs a console confirmation may be sent from a rule
				if _, err := cmdpolicy.PolicySetText(spec.Command, spec.Want); err != nil {
					t.Fatalf("PolicySetText: %v", err)
				}
			}

			profile, family := simulator.ProfileESP8266_4M, model.ChipESP8266
			if len(rules[0].Families) > 0 && rules[0].Families[0].IsESP32() {
				profile, family = simulator.ProfileESP32, model.ChipESP32
			}
			dev, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "t-" + tp.ID, Profile: profile})
			if err != nil {
				t.Fatal(err)
			}
			host, portS, _ := net.SplitHostPort(dev.Addr())
			port, _ := strconv.Atoi(portS)
			tgt := tasmota.TargetFor(model.Device{Address: host, Port: port})

			if re := v16Bare[spec.Command]; re != nil {
				first, err := cl.Command(ctx, tgt, spec.Command)
				if err != nil || !re.Match(first) {
					t.Fatalf("bare %s -> %s, %v; want the Tasmota 16 format %s", spec.Command, first, err, re)
				}
				second, _ := cl.Command(ctx, tgt, spec.Command)
				if string(first) != string(second) {
					t.Errorf("a bare query changed the answer: %s then %s", first, second)
				}
			}

			if _, err := cl.Command(ctx, tgt, text); err != nil {
				t.Fatalf("send %q: %v", text, err)
			}
			rd := settings.New(settings.Options{Devices: oneDevice{host, port}, Client: cl})
			have, err := rd.ReadKeys(ctx, 1, []string{spec.Command})
			if err != nil || have[spec.Command] == "" {
				t.Fatalf("read %s: %v, %v", spec.Command, have, err)
			}
			d := model.Device{ID: 1, Name: "d", Identity: model.Identity{Family: family}}
			out := policy.Outcomes(rules, []model.Device{d}, map[model.DeviceID]map[string]string{1: have})
			if len(out) != 1 || out[0].Kind != policy.OutcomeMatch {
				t.Fatalf("after %q the device reads %q, which the policy does not judge equal to %q: %+v", text, have[spec.Command], spec.Want, out)
			}
		})
	}
}

type oneDevice struct {
	host string
	port int
}

func (o oneDevice) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	return model.Device{ID: id, Address: o.host, Port: o.port}, nil
}

// Why: Make compliant orders the writes of a device so that commands which restart it go last,
// but it only knows the restart of a curated setting. Every template's command was read in
// the Tasmota 16 source for restart_flag; MqttHost is the only one that sets it (xdrv_02_9_mqtt.ino
// CmndMqttHost), so a drift in either direction would reorder or lose writes.
func TestTemplatesRestartLikeTasmota16(t *testing.T) {
	restartsInSource := map[string]bool{"MqttHost": true}
	for _, tp := range policyTemplates("mqtt.lab.home") {
		want := tp.Spec.Want
		if want == "" {
			want = templateWant[tp.ID]
		}
		if got := settings.RestartsDevice(tp.Spec.Command, want); got != restartsInSource[tp.Spec.Command] {
			t.Errorf("%s: RestartsDevice = %v, Tasmota 16 restarts = %v", tp.Spec.Command, got, restartsInSource[tp.Spec.Command])
		}
	}
}

// Why: the float32 round trip of Latitude/Longitude is the reason their rules are stated with
// four decimals. A device set to any four-decimal coordinate must read back as exactly that
// string (over the whole range, both signs), or the rule would deviate forever.
func TestCoordinateRulesSurviveTheFloat32RoundTrip(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "geo", Profile: simulator.ProfileESP8266_4M})
	if err != nil {
		t.Fatal(err)
	}
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopback()})
	host, portS, _ := net.SplitHostPort(dev.Addr())
	port, _ := strconv.Atoi(portS)
	tgt := tasmota.TargetFor(model.Device{Address: host, Port: port})
	rd := settings.New(settings.Options{Devices: oneDevice{host, port}, Client: cl})
	for i := 0; i < 400; i++ {
		deg := -179.9 + float64(i)*0.8973 // spreads over sign, magnitude and fractions
		for _, cmd := range []string{"Latitude", "Longitude"} {
			want := strconv.FormatFloat(deg, 'f', 4, 64)
			if _, err := cl.Command(t.Context(), tgt, cmd+" "+want); err != nil {
				t.Fatal(err)
			}
			have, err := rd.ReadKeys(t.Context(), 1, []string{cmd})
			if err != nil || have[cmd] != want {
				t.Fatalf("%s %s reads back as %q (%v)", cmd, want, have[cmd], err)
			}
		}
	}
}
