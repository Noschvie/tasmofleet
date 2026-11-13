package cmdpolicy

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/tasmotacmd"
)

// Why (#605, the coordinator's decision of 2026-11-14 17:15): the policy must read a command name the way the
// device runs it, or a check keyed on the name misses a spelling the device treats as the same command. Tasmota
// upper-cases the name, reads the digits at its end as the index with atoi (SetOption0103 is SetOption103), and
// runs a SetOption synonym of its command tables as that SetOption whatever index was typed (support.ino
// DecodeCommand: XdrvMailbox.index = the synonym's byte; MqttTLS7 is SetOption103).
func TestResolveNameReadsANameAsTheDeviceDoes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Name
	}{
		{"Power", Name{Base: "power"}},
		{"Power1", Name{Base: "power", Index: 1, Indexed: true}},
		{"POWER01", Name{Base: "power", Index: 1, Indexed: true}},
		{"SetOption0103", Name{Base: "setoption", Index: 103, Indexed: true}},
		{"so19", Name{Base: "setoption", Index: 19, Indexed: true}},
		{"SO", Name{Base: "setoption"}},
		{"MqttTLS", Name{Base: "setoption", Index: 103, Indexed: true}},
		{"mqtttls1", Name{Base: "setoption", Index: 103, Indexed: true}},
		{"MQTTTLS99", Name{Base: "setoption", Index: 103, Indexed: true}},
		{"MqttTLSFingerprint", Name{Base: "setoption", Index: 132, Indexed: true}},
		{"WifiNoSleep", Name{Base: "setoption", Index: 127, Indexed: true}},
		{"MqttHost", Name{Base: "mqtthost"}},
		{strings.Repeat("a", maxNameLen), Name{Base: strings.Repeat("a", maxNameLen)}},
	} {
		if got, ok := ResolveName(tc.in); !ok || got != tc.want {
			t.Errorf("ResolveName(%q) = %+v, %v; want %+v", tc.in, got, ok, tc.want)
		}
	}
	// What the policy refuses to read as a name stays unread: nothing to resolve, nothing to send.
	for _, in := range []string{"", strings.Repeat("a", maxNameLen+1), "1Power", "_MqttTLS", "Mqtt TLS", "MqttTLS 1", "Power99999999999999999999", "SetOptión3"} {
		if got, ok := ResolveName(in); ok {
			t.Errorf("ResolveName(%q) = %+v, want not ok", in, got)
		}
	}
}

// Why: every SetOption synonym of every pinned Tasmota catalogue runs the SetOption's handler on the device, so the
// policy must judge it exactly like that SetOption: the same class, the same confirmation, the same secrecy, and the
// same answer to "does it change the MQTT connection". A synonym the policy judged by its own name would slip past a
// rule written for the SetOption (MqttTLS 1 changed the MQTT connection without the router's HTTP question).
func TestEveryCatalogueSynonymIsJudgedLikeItsSetOption(t *testing.T) {
	set, err := tasmotacmd.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, v := range set.Versions() {
		cat, _, _ := set.For(v)
		for _, c := range cat.Commands {
			if !strings.HasPrefix(c.SynonymOf, "SetOption") {
				continue
			}
			seen++
			n, err := strconv.Atoi(strings.TrimPrefix(c.SynonymOf, "SetOption"))
			if err != nil {
				t.Fatalf("%s: synonym of %q", c.Command, c.SynonymOf)
			}
			for _, spelling := range []string{c.Command, strings.ToLower(c.Command) + "1", strings.ToUpper(c.Command) + "007"} {
				if got, ok := ResolveName(spelling); !ok || got != (Name{Base: "setoption", Index: n, Indexed: true}) {
					t.Errorf("%s (Tasmota %s): ResolveName(%q) = %+v, %v; want SetOption%d", c.Command, v, spelling, got, ok, n)
				}
				for _, arg := range []string{"", " 0", " 1"} {
					syn, so := spelling+arg, c.SynonymOf+arg
					if CutsMQTTOff(syn) != CutsMQTTOff(so) {
						t.Errorf("CutsMQTTOff(%q) = %v, but %q gives %v", syn, CutsMQTTOff(syn), so, CutsMQTTOff(so))
					}
					a, aerr := Classify(syn)
					b, berr := Classify(so)
					if (aerr == nil) != (berr == nil) || a.Class != b.Class || a.Confirm != b.Confirm || a.Redact != b.Redact {
						t.Errorf("Classify(%q) = %+v %v, Classify(%q) = %+v %v; want the same verdict", syn, a, aerr, so, b, berr)
					}
					if a.Name != spelling {
						t.Errorf("Classify(%q).Name = %q, want the name as typed", syn, a.Name)
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no SetOption synonym in the pinned catalogues: the resolver would read none")
	}
}

// Why (#605 review, the coordinator's decision of 2026-11-14 17:15): the router asks HTTP one question before a change
// of the MQTT connection and refuses it without consent when HTTP is silent. MqttTLS is SetOption103 (TLS on or off)
// and MqttTLSFingerprint is SetOption132 (the fingerprint check); under either name, in any case and with any index,
// alone or in a Backlog, the device changes how it connects to the broker.
func TestCutsMQTTOffUnderEverySpellingTheDeviceRuns(t *testing.T) {
	for cmd, want := range map[string]string{
		"MqttTLS 1":                   "MqttTLS",
		"mqtttls1 1":                  "mqtttls1",
		"MqttTLSFingerprint 0":        "MqttTLSFingerprint",
		"Backlog Power ON; MqttTLS 1": "MqttTLS",
		"SetOption0103 1":             "SetOption0103",
		"so132 0":                     "so132",
		"SetOption003 0":              "SetOption003",
		"Backlog backlogMqttTLS 0":    "MqttTLS",
	} {
		if !CutsMQTTOff(cmd) || CutOffName(cmd) != want {
			t.Errorf("%q: CutsMQTTOff %v, CutOffName %q; want true, %q (the name as typed)", cmd, CutsMQTTOff(cmd), CutOffName(cmd), want)
		}
	}
	for _, cmd := range []string{"MqttTLS", "MqttTLSFingerprint", "WifiNoSleep 1", "SetOption003 1", "MqttNoRetain 1"} {
		if CutsMQTTOff(cmd) {
			t.Errorf("%q does not change the MQTT connection", cmd)
		}
	}
}

// Why: the policy's reading of a name depends on the embedded catalogues. One that cannot be read, or a synonym whose
// SetOption number cannot be read, must stop the program at its first use, never read as "no synonyms": the policy
// would then judge MqttTLS by its own name again and let a change of the MQTT connection pass unasked.
func TestADamagedCatalogueIsNeverReadAsNoSynonyms(t *testing.T) {
	bad, err := tasmotacmd.NewSet([]*tasmotacmd.Catalogue{{Tasmota: "16.0.0", Commands: []tasmotacmd.Command{{Command: "MqttTLS", SynonymOf: "SetOptionX"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, build := range map[string]func(){
		"unreadable catalogues":   func() { setOptionTable(nil, errors.New("damaged")) },
		"unreadable synonym name": func() { setOptionTable(bad, nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: the table was built", name)
				}
			}()
			build()
		}()
	}
}
