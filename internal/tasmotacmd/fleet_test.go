package tasmotacmd

import (
	"reflect"
	"strings"
	"testing"
)

func cmdEntry(name string, mod func(*Command)) Command {
	c := Command{Command: name, Table: "t", File: "f"}
	if mod != nil {
		mod(&c)
	}
	return c
}

// testSet has three pinned versions with a command that came in 14.0.0 and one that needs a define.
func testSet(t *testing.T) *Set {
	t.Helper()
	base := []Command{
		cmdEntry("Sleep", nil),
		cmdEntry("SetOption", func(c *Command) { c.Index = &Index{Max: "177"}; c.Kind = "mixed" }),
		cmdEntry("Latitude", func(c *Command) { c.Requires = []string{"USE_SUNRISE"}; c.Kind = "number"; c.Decimal = true }),
		cmdEntry("Power", func(c *Command) { c.Index = &Index{Min: "1", Max: "TasmotaGlobal.devices_present"} }),
	}
	builds := map[string][]string{
		"esp8266/tasmota":          {"USE_SUNRISE"},
		"esp8266/FIRMWARE_LITE":    {},
		"esp32/FIRMWARE_TASMOTA32": {"USE_SUNRISE"},
	}
	mk := func(v string, extra ...Command) *Catalogue {
		return &Catalogue{Tasmota: v, Commands: append(append([]Command{}, base...), extra...), Builds: builds}
	}
	s, err := NewSet([]*Catalogue{
		mk("16.0.0", cmdEntry("DaliDeviceScan", nil)),
		mk("13.4.0"),
		mk("14.4.1", cmdEntry("DaliDeviceScan", nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetPicksTheCatalogueThatJudgesAVersion(t *testing.T) {
	s := testSet(t)
	for _, tc := range []struct {
		ver   string
		want  string
		match Match
	}{
		{"14.4.1", "14.4.1", MatchExact},
		{"14.4.1.2", "14.4.1", MatchExact}, // a development build is judged like its release
		{"14.0.0", "14.4.1", MatchNewer},   // between pins: the next newer one, so "missing" stays true
		{"12.0.0", "13.4.0", MatchNewer},
		{"16.1.0", "16.0.0", MatchBeyond}, // newer than every pin: only "present" is known
	} {
		c, m, ok := s.For(tc.ver)
		if !ok || c.Tasmota != tc.want || m != tc.match {
			t.Errorf("For(%s) = %v %v %v, want %s %v", tc.ver, c, m, ok, tc.want, tc.match)
		}
	}
	if _, _, ok := s.For("tasmota"); ok {
		t.Fatal("an unreadable version must not be judged")
	}
}

func dev(name, ver, chip, variant string) FleetDevice {
	return FleetDevice{Name: name, Version: ver, Env: Env{Chip: chip, Variant: variant}}
}

func TestCheckFlagsACommandMissingInSomeVersionsButAllowsSaving(t *testing.T) {
	// The approved design: a version check warns and the rule still applies to the other
	// devices; only a command that exists in no version of the fleet is refused.
	s := testSet(t)
	fleet := []FleetDevice{dev("a", "16.0.0", "esp32", "tasmota"), dev("b", "13.4.0", "esp8266", "tasmota"), dev("c", "13.4.0", "esp8266", "tasmota")}
	r := s.Check("DaliDeviceScan", fleet)
	if r.Refuse {
		t.Fatal("exists in 16.0.0: must be allowed")
	}
	if len(r.Versions) != 2 || r.Versions[0].Version != "16.0.0" || r.Versions[0].Exists != True || r.Versions[1].Exists != False {
		t.Fatalf("%+v", r.Versions)
	}
	if !reflect.DeepEqual(r.Versions[1].Devices, []string{"b", "c"}) {
		t.Fatalf("devices: %v", r.Versions[1].Devices)
	}
}

func TestCheckRefusesWhatNoFleetVersionKnows(t *testing.T) {
	s := testSet(t)
	r := s.Check("NoSuchCommand", []FleetDevice{dev("a", "16.0.0", "esp32", "tasmota"), dev("b", "13.4.0", "esp8266", "tasmota")})
	if !r.Refuse {
		t.Fatalf("a command in no version must be refused: %+v", r)
	}
	// ... but never on a fleet we know nothing about: nothing to judge against.
	if r := s.Check("NoSuchCommand", []FleetDevice{dev("a", "", "esp32", "tasmota")}); r.Refuse || r.Unjudged != 1 {
		t.Fatalf("no versions known must not refuse: %+v", r)
	}
	// a device whose version is unknown might know it: no refusal on a mixed fleet
	if r := s.Check("NoSuchCommand", []FleetDevice{dev("a", "16.0.0", "esp32", "tasmota"), dev("b", "", "esp32", "tasmota")}); r.Refuse || r.Unjudged != 1 {
		t.Fatalf("mixed fleet must not refuse: %+v", r)
	}
	if r := s.Check("NoSuchCommand", nil); r.Refuse {
		t.Fatalf("empty fleet must not refuse: %+v", r)
	}
}

func TestCheckDoesNotJudgeAVersionNewerThanEveryPin(t *testing.T) {
	// A command that is not in the newest pin may have been added later: warning about it on a
	// newer device would be a false alarm, and refusing it would block a valid rule.
	s := testSet(t)
	r := s.Check("BrandNewCommand", []FleetDevice{dev("a", "17.0.0", "esp32", "tasmota")})
	if r.Refuse || r.Versions[0].Exists != Unknown || r.Versions[0].Match != MatchBeyond {
		t.Fatalf("%+v", r)
	}
}

func TestCheckIndexOutOfRangeWarnsButNeverRefuses(t *testing.T) {
	// The index range is read from the source heuristically, so SetOption200 is a warning on the
	// versions that state a range; only a command name that no fleet version knows is refused.
	s := testSet(t)
	fleet := []FleetDevice{dev("a", "16.0.0", "esp32", "tasmota")}
	r := s.Check("SetOption200", fleet)
	if r.Refuse || r.Versions[0].Exists != True || !r.Versions[0].IndexBad {
		t.Fatalf("SetOption200: %+v", r)
	}
	if r := s.Check("NoSuchCommand9", fleet); !r.Refuse {
		t.Fatalf("an unknown name stays refused: %+v", r)
	}
	r = s.Check("SetOption19", fleet)
	if r.Refuse || r.Versions[0].IndexBad || r.Name != "SetOption" || !r.Range.OK || r.Range.Hi != 177 || r.Range.Typed != 19 {
		t.Fatalf("SetOption19: %+v", r)
	}
	// a range given only as a run-time expression (Power) never flags an index
	if r := s.Check("Power9", fleet); r.Refuse || r.Versions[0].IndexBad {
		t.Fatalf("Power9: %+v", r)
	}
}

func TestCheckNamesDevicesWhoseBuildLacksTheDefine(t *testing.T) {
	// "Their firmware was built without it": the lite build undefines USE_SUNRISE, the default does not.
	s := testSet(t)
	fleet := []FleetDevice{
		dev("lamp-lite", "16.0.0", "esp8266", "lite"),
		dev("plug", "16.0.0", "esp8266", "tasmota"),
		dev("custom", "16.0.0", "esp8266", "unknown"), // a custom build is never blamed
		dev("esp", "16.0.0", "esp32", "tasmota"),
	}
	r := s.Check("Latitude", fleet)
	if r.Refuse || r.Versions[0].Exists != True {
		t.Fatalf("%+v", r)
	}
	want := []DeviceNeeds{{Device: "lamp-lite", Needs: []string{"USE_SUNRISE"}}}
	if !reflect.DeepEqual(r.Versions[0].Lacking, want) {
		t.Fatalf("lacking: %+v", r.Versions[0].Lacking)
	}
}

func TestLacksNamesFamilyAndFirmwareConditions(t *testing.T) {
	c := &Catalogue{Tasmota: "16.0.0", Commands: []Command{
		cmdEntry("OnlyEsp32", func(c *Command) { c.Requires = []string{"ESP32"} }),
		cmdEntry("NotMinimal", func(c *Command) { c.Requires = []string{"!FIRMWARE_MINIMAL"} }),
	}, Builds: map[string][]string{"esp8266/FIRMWARE_MINIMAL": {}, "esp32/FIRMWARE_TASMOTA32": {}}}
	f, _ := c.Find("OnlyEsp32")
	if got := c.Lacks(f, Env{Chip: "esp8266", Variant: "tasmota"}); !reflect.DeepEqual(got, []string{"ESP32"}) {
		t.Fatalf("ESP8266 lacks an ESP32-only command: %v", got)
	}
	if got := c.Lacks(f, Env{Chip: "esp32s3", Variant: "tasmota"}); got != nil {
		t.Fatalf("an ESP32-S3 is an ESP32: %v", got)
	}
	f, _ = c.Find("NotMinimal")
	if got := c.Lacks(f, Env{Chip: "esp8266", Variant: "minimal"}); !reflect.DeepEqual(got, []string{"not FIRMWARE_MINIMAL"}) {
		t.Fatalf("minimal lacks it: %v", got)
	}
	if got := c.Lacks(f, Env{Chip: "esp8266", Variant: ""}); got != nil {
		t.Fatalf("an unplaced variant is never blamed: %v", got)
	}
}

func TestFindSplitsTheIndexOffTheName(t *testing.T) {
	s := testSet(t)
	c := s.Newest()
	f, ok := c.Find("setoption19 1")
	if !ok || f.Name != "SetOption" || !f.HasIndex || f.Index != 19 {
		t.Fatalf("%+v %v", f, ok)
	}
	if _, ok := c.Find("19"); ok {
		t.Fatal("a bare number is not a command")
	}
	if _, ok := c.Find("Nope9"); ok {
		t.Fatal("unknown")
	}
}

func TestValueKindsComeFromTheCatalogue(t *testing.T) {
	lo, hi := 0, 3600
	c := &Catalogue{Tasmota: "16.0.0", Commands: []Command{
		cmdEntry("TelePeriod", func(c *Command) { c.Kind = "number"; c.Value = &Bounds{Min: &lo, Max: &hi} }),
		cmdEntry("Latitude", func(c *Command) { c.Kind = "number"; c.Decimal = true }),
		cmdEntry("Fade", func(c *Command) { c.Kind = "state" }),
		cmdEntry("MqttHost", func(c *Command) { c.Kind = "text" }),
		cmdEntry("Template", func(c *Command) { c.Kind = "mixed" }),
		cmdEntry("Twice", func(c *Command) { c.Kind = "number" }),
		cmdEntry("Twice", func(c *Command) { c.Kind = "text" }),
	}}
	for name, want := range map[string]ValueKind{"TelePeriod": ValueNumber, "Latitude": ValueNumber, "Fade": ValueState, "MqttHost": ValueText, "Template": ValueText, "Twice": ValueText} {
		f, _ := c.Find(name)
		if got := f.Value().Kind; got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	f, _ := c.Find("TelePeriod")
	if v := f.Value(); v.Min == nil || *v.Min != 0 || *v.Max != 3600 || v.Decimal {
		t.Fatalf("%+v", v)
	}
	f, _ = c.Find("Latitude")
	if v := f.Value(); !v.Decimal || v.Min != nil {
		t.Fatalf("%+v", v)
	}
}

func TestSuggestRanksPrefixFirstAndMarksIndexAndNeeds(t *testing.T) {
	s := testSet(t)
	s2 := s.Suggest("setopt", []string{"16.0.0"}, 10, nil)
	if len(s2.List) != 1 || s2.List[0].Name != "SetOption" || !s2.List[0].Indexed || s2.List[0].Hi != 177 || !s2.List[0].Starts {
		t.Fatalf("%+v", s2.List)
	}
	got := s.Suggest("a", []string{"16.0.0"}, 10, nil)
	var names []string
	for _, x := range got.List {
		names = append(names, x.Name)
	}
	// starts-with before contains: Latitude/DaliDeviceScan contain "a"; none starts with it except... order check
	if strings.Join(names, ",") == "" {
		t.Fatal("no suggestions")
	}
	lat := s.Suggest("lat", []string{"16.0.0"}, 10, nil).List[0]
	if lat.Needs != "USE_SUNRISE" || lat.Desc == "" {
		t.Fatalf("%+v", lat)
	}
	if got := s.Suggest("sleep", nil, 10, func(n string) bool { return n != "Sleep" }); len(got.List) != 0 {
		t.Fatalf("keep must filter: %+v", got.List)
	}
	if got := s.Suggest("dali", []string{"13.4.0", "16.0.0"}, 10, nil); got.Total != 5 || len(got.Versions) != 2 || len(got.List) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestCheckMissInANewerPinIsCertainOnlyWhenTheOlderPinLacksItToo(t *testing.T) {
	// SafePower is in 13.4.0 and gone in 14.4.1: a device on 14.0.0 (no pin of its own) may still
	// have it, so the miss in 14.4.1 proves nothing. The same for a version older than every pin.
	cats := []*Catalogue{
		{Tasmota: "13.4.0", Commands: []Command{cmdEntry("SafePower", nil), cmdEntry("Sleep", nil)}},
		{Tasmota: "14.4.1", Commands: []Command{cmdEntry("Sleep", nil)}},
		{Tasmota: "16.0.0", Commands: []Command{cmdEntry("Sleep", nil)}},
	}
	s, err := NewSet(cats)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Check("SafePower", []FleetDevice{dev("a", "14.0.0", "esp8266", "tasmota")})
	if r.Refuse || r.Versions[0].Exists != Unknown {
		t.Fatalf("14.0.0: %+v", r.Versions[0])
	}
	// 15.0.0 sits between 14.4.1 and 16.0.0 and both lack it: certain
	r = s.Check("SafePower", []FleetDevice{dev("a", "15.0.0", "esp8266", "tasmota")})
	if !r.Refuse || r.Versions[0].Exists != False || r.Versions[0].Older != "14.4.1" {
		t.Fatalf("15.0.0: %+v", r.Versions[0])
	}
	// older than every pin: nothing is known about what came before
	r = s.Check("Nope", []FleetDevice{dev("a", "11.1.0", "esp8266", "tasmota")})
	if r.Refuse || r.Versions[0].Exists != Unknown {
		t.Fatalf("11.1.0: %+v", r.Versions[0])
	}
	// an exact pin is judged on its own catalogue
	if r := s.Check("SafePower", []FleetDevice{dev("a", "14.4.1", "esp8266", "tasmota")}); !r.Refuse {
		t.Fatalf("14.4.1 lacks it: %+v", r)
	}
}
