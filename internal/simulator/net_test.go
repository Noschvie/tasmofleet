package simulator

import (
	"strings"
	"testing"
)

// Why: the e2e proof of #1044 (a refresh must store the network facts) needs a device whose subnet mask and
// DNS server can be changed to values no earlier read can have stored. They must show where a reader looks:
// the Ethernet object of a wired board (the top level stays idle), the top level of a Wi-Fi board; "" keeps
// the current value, as SetWifi's 0 does.
func TestSetNetChangesTheActiveInterfacesSubnetAndDNS(t *testing.T) {
	t.Parallel()
	f := newTestFleet(t, "", Extra{})
	wired := add(t, f, DeviceSpec{Name: "rack", Profile: ProfileESP32, Wired: true})
	wifi := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266_4M})

	before := cm(t, wired, "Status 5")
	for _, want := range []string{`"Subnetmask":"255.255.255.0"`, `"DNSServer1":"172.31.13.1"`} {
		if !strings.Contains(before, want) {
			t.Fatalf("the capture no longer reports %s, the test premise is gone: %s", want, before)
		}
	}

	wired.SetNet("255.255.254.0", "203.0.113.7")
	got := cm(t, wired, "Status 5")
	eth := got[strings.Index(got, `"Ethernet":{`):]
	for _, want := range []string{`"Subnetmask":"255.255.254.0"`, `"DNSServer1":"203.0.113.7"`} {
		if !strings.Contains(eth, want) {
			t.Errorf("the wired Ethernet object lacks %s: %s", want, got)
		}
	}
	top := got[:strings.Index(got, `"Ethernet":{`)]
	if !strings.Contains(top, `"Subnetmask":"0.0.0.0"`) || !strings.Contains(top, `"IPAddress":"0.0.0.0"`) {
		t.Errorf("the idle Wi-Fi values of a wired board must stay unset: %s", got)
	}

	wired.SetNet("", "203.0.113.8")
	got = cm(t, wired, "Status 5")
	if !strings.Contains(got, `"Subnetmask":"255.255.254.0"`) || !strings.Contains(got, `"DNSServer1":"203.0.113.8"`) {
		t.Errorf("an empty value must keep the current one, a given one replace it: %s", got)
	}

	wifi.SetNet("255.255.252.0", "203.0.113.9")
	got = cm(t, wifi, "Status 5")
	for _, want := range []string{`"Subnetmask":"255.255.252.0"`, `"DNSServer1":"203.0.113.9"`} {
		if !strings.Contains(got, want) {
			t.Errorf("a Wi-Fi device reports %s at the top level: %s", want, got)
		}
	}
	if strings.Contains(cm(t, add(t, f, DeviceSpec{Name: "other", Profile: ProfileESP8266_4M}), "Status 5"), "203.0.113.9") {
		t.Error("the override of one device leaked into another")
	}
}
