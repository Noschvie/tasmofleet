package extract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
)

// The fixture is real Tasmota v16.0.0 source cut down to one case per behaviour; every case below
// says which Tasmota habit it pins and what breaks for the MQTT reply correlation (#268) if the
// extractor gets it wrong.
func fixture(t *testing.T) map[string]tasmotacmd.Command {
	t.Helper()
	cat, err := Extract(os.DirFS("testdata/tasmota"), "16.0.0-fixture", "00")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.SkippedTables) != 0 {
		t.Fatalf("fixture tables skipped: %+v", cat.SkippedTables)
	}
	m := map[string]tasmotacmd.Command{}
	for _, c := range cat.Commands {
		m[c.Command] = c
	}
	return m
}

func keysOf(c tasmotacmd.Command) string { return strings.Join(c.Keys, ",") }

func TestResponseCmndCharKeyIsTheCommandName(t *testing.T) {
	// A RESULT is matched by key; OtaUrl answers {"OtaUrl":"..."}, spelled like the table entry.
	c := fixture(t)["OtaUrl"]
	if keysOf(c) != "OtaUrl" || c.Kind != "text" || c.Loose != "" {
		t.Fatalf("OtaUrl: %+v", c)
	}
}

func TestResponseCmndNumberIsNumber(t *testing.T) {
	// The value kind lets callers tell a number from text without asking the device.
	c := fixture(t)["TelePeriod"]
	if keysOf(c) != "TelePeriod" || c.Kind != "number" || c.Loose != "" {
		t.Fatalf("TelePeriod: %+v", c)
	}
}

func TestPrefixCommandIsPrefixPlusName(t *testing.T) {
	// DecodeCommand glues the table prefix in front of the name, so the user types and the device
	// answers "MqttHost", not "Host".
	c, ok := fixture(t)["MqttHost"]
	if !ok || c.Prefix != "Mqtt" || keysOf(c) != "MqttHost" {
		t.Fatalf("MqttHost: %+v", c)
	}
}

func TestEmptyNameIsThePrefixAlone(t *testing.T) {
	// "InfluxDb||Log": an empty entry is a command (the prefix alone). Dropping it shifts every
	// following name against the handler table and gives every command the wrong handler.
	m := fixture(t)
	if c := m["InfluxDb"]; c.Handler != "CmndInfluxDbState" || c.Kind != "state" {
		t.Fatalf("InfluxDb: %+v", c)
	}
	if c := m["InfluxDbLog"]; c.Handler != "CmndInfluxDbLog" {
		t.Fatalf("InfluxDbLog: %+v", c)
	}
	if c := m["InfluxDbHost"]; c.Handler != "CmndInfluxDbHost" {
		t.Fatalf("InfluxDbHost: %+v", c)
	}
}

func TestResponsePWithOtherKeys(t *testing.T) {
	// Template does not answer {"Template":...} but {"NAME":..,"ARCH":..,"GPIO":[..],..} (TemplateJson).
	// A catalogue that assumed the command name would make the correlation drop the real reply.
	c := fixture(t)["Template"]
	for _, want := range []string{"NAME", "ARCH", "GPIO", "FLAG", "BASE", "Template"} {
		if !strings.Contains(","+keysOf(c)+",", ","+want+",") {
			t.Errorf("Template keys %q miss %s", keysOf(c), want)
		}
	}
	if c.Loose != "" {
		t.Errorf("Template should be derivable, loose: %s", c.Loose)
	}
}

func TestClearedReplyIsLoose(t *testing.T) {
	// Power and Backlog publish their answer from other code (ExecuteCommandPower, the queued
	// commands). Claiming a key would make WP-A1 wait for a reply that never arrives.
	for _, name := range []string{"Power", "Backlog"} {
		c := fixture(t)[name]
		if !strings.Contains(c.Loose, "ResponseClear") {
			t.Errorf("%s should be loose because of ResponseClear: %+v", name, c)
		}
	}
}

func TestStatusAnswersOnItsOwnTopic(t *testing.T) {
	// Status N goes to stat/<topic>/STATUSN, not RESULT; a RESULT key would be a false promise.
	c := fixture(t)["Status"]
	if c.Loose == "" || !strings.Contains(c.Loose, "MqttPublish") {
		t.Fatalf("Status should be loose because of MqttPublish: %+v", c)
	}
}

func TestRunTimeNumberInKeyIsLoose(t *testing.T) {
	// Gpio answers GPIO0, GPIO1, ...: the keys depend on the template, so no fixed list exists.
	c := fixture(t)["GPIO"]
	if !strings.Contains(c.Loose, "GPIO<#>") {
		t.Fatalf("GPIO: %+v", c)
	}
}

func TestIndexedAndPlainRepliesDiffer(t *testing.T) {
	// "SetOption" reports a list, "SetOption19" one option under the key SetOption19. The branch is
	// XdrvMailbox.usridx; without splitting it the single-option case would inherit the loose list
	// case. "SO19" is the same command by a short name: the handler renames XdrvMailbox.command, so
	// the answer is keyed SetOption19, not SO19.
	m := fixture(t)
	for _, name := range []string{"SetOption", "SO"} {
		c := m[name]
		if c.Indexed == nil || strings.Join(c.Indexed.Keys, ",") != "SetOption<n>" || c.Indexed.Loose != "" {
			t.Fatalf("%s indexed: %+v", name, c.Indexed)
		}
		if !strings.Contains(c.Loose, "SetOptionShow") {
			t.Errorf("%s without index should be loose (a helper appends the keys): %s", name, c.Loose)
		}
	}
}

func TestSynonymAnswersUnderItsOwnName(t *testing.T) {
	// WifiNoSleep is SetOption127 by another name; DecodeCommand calls CmndSetoptionBase(0), so the
	// answer is {"WifiNoSleep":"ON"}, no index in the key.
	c := fixture(t)["WifiNoSleep"]
	if c.SynonymOf != "SetOption127" || keysOf(c) != "WifiNoSleep" || c.Loose != "" {
		t.Fatalf("WifiNoSleep: %+v", c)
	}
}

func TestAppendedSuffixIsAnAlternateKey(t *testing.T) {
	// Module2 is answered as {"Module2":..}: the handler does strlcat(XdrvMailbox.command, "2").
	// Listing only "Module" would drop the reply to Module2.
	c := fixture(t)["Module"]
	if keysOf(c) != "Module,Module2" || c.Kind != "object" || c.Loose != "" {
		t.Fatalf("Module: %+v", c)
	}
}

func TestResponseCmndAllWithoutIndex(t *testing.T) {
	// FriendlyName answers FriendlyName1..N without an index and FriendlyName2 with one.
	c := fixture(t)["FriendlyName"]
	if keysOf(c) != "FriendlyName<n>" || c.Loose != "" {
		t.Fatalf("FriendlyName: %+v", c)
	}
}

func TestAlternativesInIfElsePairWithinOneConfiguration(t *testing.T) {
	// Both lists contain the #ifdef and the #else entries; pairing by position across all of them
	// gives the wrong handler. Pairing per configuration keeps every name with its own handler.
	m := fixture(t)
	for name, handler := range map[string]string{
		"ModeSet": "CmndModeSet", "AntiWindup": "CmndAntiWindup", "PerfLevel": "CmndPerfLevel",
		"MaxAction": "CmndMaxAction", "EnableOutput": "CmndEnableOutput",
	} {
		c := m[name]
		if c.Handler != handler || c.Loose != "" {
			t.Errorf("%s: handler %q loose %q, want %s", name, c.Handler, c.Loose, handler)
		}
	}
}

func TestMismatchedTablesAreLooseNotGuessed(t *testing.T) {
	// Upstream's Stcc table has five names and four handlers. Any pairing would be a guess.
	for _, c := range fixture(t) {
		if c.Table != "kStccCommands" {
			continue
		}
		if !strings.Contains(c.Loose, "never have the same length") || c.Handler != "" || len(c.Keys) != 0 {
			t.Errorf("%s: %+v", c.Command, c)
		}
	}
}

func TestRequiresKeepsTheIfConditions(t *testing.T) {
	// The catalogue says which build a command exists in (here: not in the minimal firmware).
	c := fixture(t)["Backlog"]
	if len(c.Requires) != 1 || c.Requires[0] != "!FIRMWARE_MINIMAL" {
		t.Fatalf("Backlog requires: %v", c.Requires)
	}
	if m := fixture(t)["OtaUrl"]; len(m.Requires) != 0 {
		t.Fatalf("OtaUrl is in the minimal firmware too: %v", m.Requires)
	}
}

func TestIndexRangeIsReadFromTheHandler(t *testing.T) {
	// Power1..N: the handler checks 1 <= index <= devices_present, kept as source text.
	c := fixture(t)["Power"]
	if c.Index == nil || c.Index.Min != "1" || c.Index.Max != "TasmotaGlobal.devices_present" {
		t.Fatalf("Power index: %+v", c.Index)
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	// `make catalogue-check` compares a regeneration with the checked-in file.
	a, _ := json.Marshal(mustExtract(t))
	b, _ := json.Marshal(mustExtract(t))
	if string(a) != string(b) {
		t.Fatal("two runs differ")
	}
}

func mustExtract(t *testing.T) *tasmotacmd.Catalogue {
	t.Helper()
	cat, err := Extract(os.DirFS("testdata/tasmota"), "x", "y")
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestEmptySourceIsAnError(t *testing.T) {
	// A wrong -src directory must fail loudly instead of writing an empty catalogue.
	if _, err := Extract(os.DirFS(t.TempDir()), "x", "y"); err == nil {
		t.Fatal("want an error for a tree without source files")
	}
}
