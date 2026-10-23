package simulator

import "testing"

// Why: the engine's family guard and the device check must agree on the
// header format captured from the real release images.
func TestNewImageHeaderMatchesFamily(t *testing.T) {
	for file, want := range map[string]string{
		"tasmota.bin.gz": "esp8266", "tasmota-minimal.bin.gz": "esp8266", "tasmota32.bin": "esp32",
		"tasmota32c3.bin": "esp32c3", "tasmota32s3.bin": "esp32s3", "tasmota32solo1.bin": "esp32",
		"tasmota32-safeboot.bin": "esp32", "tasmota-zbbridge.bin.gz": "esp8266",
	} {
		b, err := NewImage(fixtures, file, "16.1.0")
		if err != nil {
			t.Fatal(file, err)
		}
		raw, err := unpack(b)
		if err != nil {
			t.Fatal(err)
		}
		fam, err := imageFamily(raw)
		if err != nil || fam != want {
			t.Errorf("%s: family %q err %v, want %q", file, fam, err, want)
		}
		if v, ok := findVersion(raw); !ok || v != "16.1.0" {
			t.Errorf("%s: version %q", file, v)
		}
	}
	if _, err := NewImage(fixtures, "tasmota32xyz.bin", "1"); err == nil {
		t.Error("unknown image name must be rejected")
	}
}

// Why: Upgrade parameters decide whether a device starts flashing; the matrix
// is from the research doc (OBS/SRC).
func TestUpgradeParameterMatrix(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	for param, accept := range map[string]bool{"": false, "0": false, "2": false, "01": false, "ON": false,
		"16.0.0": false, "15.0.0": false, "abc": false, "1": true, "17.0.0": true, "16.0.1": true} {
		d := add(t, f, DeviceSpec{Name: "u" + param + "x", Profile: ProfileESP8266Min})
		cmd := "Upgrade"
		if param != "" {
			cmd += " " + param
		}
		got := cm(t, d, cmd)
		started := d.Phase() == PhasePending
		if started != accept {
			t.Errorf("%q: started=%v reply %s", param, started, got)
		}
	}
}

func TestMinimalURLRewrite(t *testing.T) {
	for in, want := range map[string]string{
		"http://h/tasmota.bin.gz": "http://h/tasmota-minimal.bin.gz", "http://h/tasmota-lite.bin": "http://h/tasmota-minimal.bin",
		"http://h/x/tasmota": "http://h/x/tasmota-minimal",
	} {
		if got, ok := minimalURL(in); !ok || got != want {
			t.Errorf("%s -> %s (%v)", in, got, ok)
		}
	}
	if _, ok := minimalURL("http://h/firmware.bin"); ok {
		t.Error("non-tasmota names are not rewritten")
	}
}

func TestUnknownReplyShape(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "a", Profile: ProfileESP8266_4M})
	for in, want := range map[string]string{
		"FooBar":      `{"Command":"Unknown","Input":"FOOBAR"}`,
		"FooBar2 x y": `{"Command":"Unknown","Input":"FOOBAR2 x y"}`,
		"Status 12":   `{"Command":"Error","Input":"STATUS 12"}`,
		"status%200":  "",
	} {
		if want == "" {
			continue
		}
		if got := cm(t, d, in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	if got, _, _ := cmWith(d, "", 3e9, nil); got != `{"WARNING":"Enter command cmnd="}` {
		t.Error(got)
	}
	if got := cm(t, d, "Backlog Power ON; Power"); got != `{"POWER":"ON","POWER":"ON"}` {
		t.Error(got)
	}
}
