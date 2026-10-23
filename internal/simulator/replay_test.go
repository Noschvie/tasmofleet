package simulator

import (
	"os"
	"path/filepath"
	"testing"
)

// Why: the simulator is only a trustworthy stand-in if an unchanged device
// answers exactly what the real one sent (raw text, decimals, key order).
func TestVerbatimReplayIsByteIdentical(t *testing.T) {
	cmds := map[string]string{
		"state.json": "State", "module.json": "Module", "modules.json": "Modules", "template.json": "Template",
		"hostname.json": "Hostname", "topic.json": "Topic", "power.json": "Power", "teleperiod.json": "TelePeriod",
		"setoption-0.json": "SetOption0", "setoption-65.json": "SetOption65", "seriallog.json": "SerialLog",
		"webserver.json": "Webserver", "fallbacktopic.json": "FallbackTopic", "loglevel.json": "WebLog",
		"gpio.json": "Gpio", "gpios.json": "Gpios", "otaurl.json": "OtaUrl", "unknown-foobar.json": "FooBar",
		"backlog-empty.json": "Backlog", "empty-cmnd.json": "",
	}
	for n := 0; n <= 11; n++ {
		cmds["status-"+itoa(n)+".json"] = "Status " + itoa(n)
	}
	devs := map[string]Profile{
		"livingroom-readinglamp": ProfileESP8266_1M, "office-monitor-guest": ProfileESP32,
	}
	f := newTestFleet(t, "", Extra{Verbatim: true})
	for topic, prof := range devs {
		d := add(t, f, DeviceSpec{Name: topic, Profile: prof, Version: "16.0.0"})
		for file, cmnd := range cmds {
			want, err := os.ReadFile(filepath.Join(fixtures, "devices", topic, file))
			if err != nil {
				continue
			}
			got, _, err := cmWith(d, cmnd, 3e9, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s %q:\n got  %s\n want %s", topic, cmnd, got, want)
			}
		}
	}
}

// Why: Upgrade's reply is how a minimal device is identified, so its text
// must follow the device version, not the capture.
func TestUpgradeQueryFollowsDeviceVersion(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_4M, Version: "15.6.0"})
	if got := cm(t, d, "Upgrade"); got != `{"Upgrade":"1 or >15.6.0 to upgrade"}` {
		t.Fatal(got)
	}
	if got := cm(t, d, "Status 2"); !contains(got, `"Version":"15.6.0(release-tasmota)"`) {
		t.Fatal(got)
	}
}

// Why: pollers decide "is this device on minimal" from these replies.
func TestMinimalAnswersOnlyItsFiveCommands(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266Min, Version: "16.0.0"})
	for _, c := range []string{"Status 2", "Status 0", "Power", "Module", "Backlog Power ON", "State"} {
		want := `{"Command":"Unknown","Input":` + jsonString(upperInput(c)) + `}`
		if got := cm(t, d, c); got != want {
			t.Errorf("%s: %s want %s", c, got, want)
		}
	}
	if got := cm(t, d, "Upgrade"); got != `{"Upgrade":"1 or >16.0.0 to upgrade"}` {
		t.Error(got)
	}
	if got := cm(t, d, "Upload"); got != `{"Upload":"1 or >16.0.0 to upgrade"}` {
		t.Error("Upload echoes its own key:", got)
	}
	if d.Variant() != "minimal" {
		t.Error(d.Variant())
	}
}

func upperInput(c string) string {
	_, base, idx, hasIdx, data := splitCmd(c)
	return string(unknownInput(base, idx, hasIdx, data))
}

func unknownInput(base string, idx int, hasIdx bool, data string) []byte {
	b := unknownReply(base, idx, hasIdx, data)
	// {"Command":"Unknown","Input":"X"} -> X
	s := string(b)
	s = s[len(`{"Command":"Unknown","Input":"`) : len(s)-2]
	return []byte(s)
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Why: Time/Uptime/Heap must move or health checks and "uptime reset after
// reboot" assertions would be meaningless.
func TestLiveFieldsAreRecomputedAndIdentityRewritten(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "kitchen", Profile: ProfileESP8266_4M})
	got := cm(t, d, "Status 0")
	for _, want := range []string{`"Topic":"kitchen"`, `"IPAddress":"127.0.0.1"`, `"Mac":"02:00:5E:`} {
		if !contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if contains(got, "172.31.13.28") || contains(got, "kitchen-spotlight") {
		t.Errorf("captured identity leaked: %s", got)
	}
	if !contains(cm(t, d, "Status 4"), `"FlashSize":4096`) {
		t.Error("4 MB profile must report FlashSize 4096")
	}
	d.(*simDevice).mu.Lock()
	d.(*simDevice).bootAt = d.(*simDevice).bootAt.Add(-1000e9)
	d.(*simDevice).mu.Unlock()
	if !contains(cm(t, d, "State"), `"UptimeSec":5`) && contains(cm(t, d, "State"), `"UptimeSec":4200`) {
		t.Error("UptimeSec is not recomputed")
	}
}

func TestHardwareOfSyntheticProfiles(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	for prof, want := range map[Profile]string{
		ProfileESP32C3: "ESP32-C3", ProfileESP32S3: "ESP32-S3", ProfileUnknownChip: "ESP32-X9", ProfileESP8266_1M: "ESP8285N08",
	} {
		d := add(t, f, DeviceSpec{Name: string(prof), Profile: prof})
		if got := cm(t, d, "Status 2"); !contains(got, want) {
			t.Errorf("%s: %s", prof, got)
		}
	}
}
