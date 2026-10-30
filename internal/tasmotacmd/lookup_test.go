package tasmotacmd

import (
	"reflect"
	"strings"
	"testing"
)

// The correlation in WP-A1 accepts a RESULT only if one of its top-level keys is in ReplyKeys, so
// every rule below decides whether a real reply is accepted or ignored.
func testCatalogue() *Catalogue {
	return &Catalogue{Commands: []Command{
		{Command: "OtaUrl", Keys: []string{"OtaUrl"}},
		{Command: "FriendlyName", Keys: []string{"FriendlyName<n>"}},
		{Command: "SetOption", Loose: "keys appended by SetOptionShow are not read",
			Indexed: &IndexedReply{Keys: []string{"SetOption<n>"}}},
		{Command: "Power", Loose: "some paths clear the reply"},
		{Command: "Dup", Keys: []string{"A"}},
		{Command: "Dup", Keys: []string{"B"}},
		{Command: "DupLoose", Keys: []string{"A"}},
		{Command: "DupLoose", Loose: "unknown"},
	}}
}

func TestReplyKeysIgnoresCaseAndArguments(t *testing.T) {
	// Tasmota matches command names case-insensitively and a console line carries arguments.
	got, ok := testCatalogue().ReplyKeys("otaurl http://x/y.bin")
	if !ok || !reflect.DeepEqual(got, []string{"OtaUrl"}) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestReplyKeysFillsTheTypedIndex(t *testing.T) {
	// FriendlyName2 is answered as {"FriendlyName2":...}; without an index Tasmota uses 1.
	c := testCatalogue()
	if got, _ := c.ReplyKeys("FriendlyName2"); !reflect.DeepEqual(got, []string{"FriendlyName2"}) {
		t.Errorf("typed: %v", got)
	}
	if got, _ := c.ReplyKeys("FriendlyName"); !reflect.DeepEqual(got, []string{"FriendlyName1"}) {
		t.Errorf("untyped: %v", got)
	}
}

func TestIndexedReplyOnlyWhenAnIndexIsTyped(t *testing.T) {
	// SetOption19 is derivable although the bare SetOption list is not: the two are different replies.
	c := testCatalogue()
	if got, ok := c.ReplyKeys("SetOption19"); !ok || !reflect.DeepEqual(got, []string{"SetOption19"}) {
		t.Errorf("SetOption19: %v %v", got, ok)
	}
	if _, ok := c.ReplyKeys("SetOption"); ok {
		t.Error("bare SetOption is loose and must not return keys")
	}
}

func TestLooseCommandsReturnNoKeys(t *testing.T) {
	// "No guessing": the reason is available through Lookup, the keys are not.
	c := testCatalogue()
	if keys, ok := c.ReplyKeys("Power1"); ok || keys != nil {
		t.Errorf("Power1: %v %v", keys, ok)
	}
	r, known := c.Lookup("Power1")
	if !known || r.Loose == "" {
		t.Errorf("Lookup(Power1): %+v %v", r, known)
	}
}

func TestUnknownCommandIsNotKnown(t *testing.T) {
	// A command from a newer firmware must not be mistaken for a loose one.
	if _, known := testCatalogue().Lookup("NoSuchCommand"); known {
		t.Error("unknown command reported as known")
	}
	if _, ok := testCatalogue().ReplyKeys(""); ok {
		t.Error("empty command")
	}
}

func TestSameNameInSeveralDriversJoinsKeysUnlessOneIsLoose(t *testing.T) {
	// Two drivers can define the same name (shutter variants); a loose variant makes the whole name loose.
	c := testCatalogue()
	if got, ok := c.ReplyKeys("Dup"); !ok || !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Errorf("Dup: %v %v", got, ok)
	}
	if _, ok := c.ReplyKeys("DupLoose"); ok {
		t.Error("DupLoose should be loose")
	}
}

func TestEmbeddedCatalogueLoads(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if c.Tasmota != "16.0.0" || len(c.Commands) < 500 {
		t.Fatalf("embedded catalogue: version %q, %d commands", c.Tasmota, len(c.Commands))
	}
	if len(c.SourceSHA256) != 64 {
		t.Fatalf("source sha256 not recorded: %q", c.SourceSHA256)
	}
}

// Golden test: the commands TasmoFleet sends today (cmdpolicy, settings, devicecmd, ota, backup,
// restore, policy templates). Expected values were checked by hand against the v16.0.0 source;
// when a regeneration changes one, read the source before editing this table. A loose command
// lists the words its reason must contain, so a different reason does not pass unnoticed.
func TestGoldenCommandsTasmoFleetSends(t *testing.T) {
	keys := map[string][]string{
		"OtaUrl":      {"OtaUrl"},
		"SetOption19": {"SetOption19"},
		"TelePeriod":  {"TelePeriod"},
		"Template":    {"Template", "NAME", "ARCH", "GPIO", "FLAG", "BASE"},
		"Module":      {"Module"},
		"Module2":     {"Module", "Module2"},
		"MqttHost":    {"MqttHost"},
		"Topic":       {"Topic"},
		"FullTopic":   {"FullTopic"},
		"WebPassword": {"WebPassword"},
		"GroupTopic":  {"GroupTopic1", "GroupTopic2", "GroupTopic3", "GroupTopic4"},
		"Module 0":    {"Module"},
		"Upgrade":     {"Upgrade"},
		"Restart 1":   {"Restart"},
		// FriendlyName tests !usridx && !data_len, a form the extractor does not split: the typed reply is the superset
		"FriendlyName2": {"FriendlyName1", "FriendlyName2", "FriendlyName3", "FriendlyName4", "FriendlyName5", "FriendlyName6", "FriendlyName7", "FriendlyName8"},
	}
	for cmd, want := range keys {
		got, ok := ReplyKeys(cmd)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("ReplyKeys(%q) = %v, %v; want %v", cmd, got, ok, want)
		}
	}
	loose := map[string]string{
		"Power1":  "ResponseClear", // the state publisher answers, not the handler
		"Backlog": "ResponseClear", // the queued commands answer
		"Status":  "not read",      // STATUS<N> topics and helpers that append keys
		"Gpio":    "GPIO<#>",       // GPIO0, GPIO1, ... depend on the template
	}
	for cmd, word := range loose {
		r, known := Lookup(cmd)
		if !known || r.Loose == "" || !strings.Contains(r.Loose, word) {
			t.Errorf("Lookup(%q) = %+v, known %v; want loose mentioning %q", cmd, r, known, word)
		}
		if keys, ok := ReplyKeys(cmd); ok || keys != nil {
			t.Errorf("ReplyKeys(%q) must refuse a loose command, got %v", cmd, keys)
		}
	}
}

func TestNoCommandNameEndsInADigit(t *testing.T) {
	// Lookup reads trailing digits as the index (Power1); a command whose own name ended in a
	// digit would never be found.
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range c.Commands {
		if n := cmd.Command; n != "" && n[len(n)-1] >= '0' && n[len(n)-1] <= '9' {
			t.Errorf("command %q ends in a digit", n)
		}
	}
}
