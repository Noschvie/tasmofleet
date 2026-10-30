package mqttbus

import "testing"

// Tasmota devices in the wild use non-default FullTopic patterns; a decoder
// that only knows "%prefix%/%topic%/" would silently ignore them and the
// health tracker would report them stale.
func TestDecode(t *testing.T) {
	def := [3]string{}
	cases := []struct {
		name, full string
		prefixes   [3]string
		in         string
		kind       Kind
		topic, suf string
		ok         bool
	}{
		{"lwt default", "", def, "tele/kitchen-spotlight/LWT", KindLWT, "kitchen-spotlight", "LWT", true},
		{"state", "", def, "tele/a/STATE", KindState, "a", "STATE", true},
		{"sensor", "", def, "tele/a/SENSOR", KindSensor, "a", "SENSOR", true},
		{"info1", "", def, "tele/a/INFO1", KindInfo, "a", "INFO1", true},
		{"info3", "", def, "tele/a/INFO3", KindInfo, "a", "INFO3", true},
		{"result", "", def, "stat/a/RESULT", KindResult, "a", "RESULT", true},
		{"status plain", "", def, "stat/a/STATUS", KindStatus, "a", "STATUS", true},
		{"status11", "", def, "stat/a/STATUS11", KindStatus, "a", "STATUS11", true},
		{"power", "", def, "stat/a/POWER", KindPower, "a", "POWER", true},
		{"power2", "", def, "stat/a/POWER2", KindPower, "a", "POWER2", true},
		{"upgrade", "", def, "stat/a/UPGRADE", KindUpgrade, "a", "UPGRADE", true},
		{"file download", "", def, "stat/a/FILEDOWNLOAD", KindFile, "a", "FILEDOWNLOAD", true},
		{"file upload", "", def, "stat/a/FILEUPLOAD", KindFile, "a", "FILEUPLOAD", true},
		{"file command topic is not ours", "", def, "cmnd/a/FILEUPLOAD", "", "", "", false},
		{"discovery uses the MAC", "", def, "tasmota/discovery/02005ED22C14/config", KindDiscovery, "02005ED22C14", "config", true},
		{"discovery sensors ignored", "", def, "tasmota/discovery/02005ED22C14/sensors", "", "", "", false},
		{"own cmnd ignored", "", def, "cmnd/a/Status", "", "", "", false},
		{"tele RESULT is not stat RESULT", "", def, "tele/a/RESULT", "", "", "", false},
		{"foreign topic", "", def, "homeassistant/status", "", "", "", false},
		{"too deep", "", def, "tele/a/b/LWT", "", "", "", false},
		{"topic first", "%topic%/%prefix%/", def, "dev1/tele/LWT", KindLWT, "dev1", "LWT", true},
		{"topic first rejects default layout", "%topic%/%prefix%/", def, "tele/dev1/LWT", "", "", "", false},
		{"literal site level", "home/%prefix%/%topic%/", def, "home/stat/dev1/POWER1", KindPower, "dev1", "POWER1", true},
		{"literal level mismatch", "home/%prefix%/%topic%/", def, "other/stat/dev1/POWER1", "", "", "", false},
		{"no trailing slash in pattern", "%prefix%/%topic%", def, "tele/dev1/STATE", KindState, "dev1", "STATE", true},
		{"custom prefixes", "", [3]string{"c", "s", "t"}, "t/dev1/LWT", KindLWT, "dev1", "LWT", true},
		{"custom prefixes reject defaults", "", [3]string{"c", "s", "t"}, "tele/dev1/LWT", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := NewTopicCodec(tc.full, tc.prefixes)
			if !ok {
				t.Fatalf("pattern %q rejected", tc.full)
			}
			kind, topic, suf, ok := c.Decode(tc.in)
			if ok != tc.ok || kind != tc.kind || topic != tc.topic || suf != tc.suf {
				t.Fatalf("Decode(%q) = %q %q %q %v", tc.in, kind, topic, suf, ok)
			}
		})
	}
}

func TestCodecRejectsPatternsWithoutPlaceholders(t *testing.T) {
	for _, p := range []string{"tele/fixed/", "%prefix%/", "%topic%/", "%prefix%-%topic%/", "%prefix%/%topic%/%topic%/"} {
		if _, ok := NewTopicCodec(p, [3]string{}); ok {
			t.Errorf("pattern %q accepted", p)
		}
	}
}

func TestFiltersAndCommandTopic(t *testing.T) {
	c, _ := NewTopicCodec("home/%topic%/%prefix%/", [3]string{})
	f := c.Filters()
	want := []string{"home/+/tele/#", "home/+/stat/#", "tasmota/discovery/+/config"}
	for i := range want {
		if f[i] != want[i] {
			t.Errorf("filter %d = %q, want %q", i, f[i], want[i])
		}
	}
	if got := c.Command("dev1", "Status"); got != "home/dev1/cmnd/Status" {
		t.Errorf("Command = %q", got)
	}
}

func FuzzDecode(f *testing.F) {
	c, _ := NewTopicCodec("", [3]string{})
	f.Add("tele/a/LWT")
	f.Add("//")
	f.Add("tasmota/discovery//config")
	f.Fuzz(func(t *testing.T, in string) {
		kind, topic, _, ok := c.Decode(in)
		if ok && (kind == "" || topic == "") {
			t.Fatalf("ok without kind/topic for %q", in)
		}
	})
}
