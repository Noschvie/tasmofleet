package simulator

import (
	"testing"
	"time"
)

func newBrokerFleet(t *testing.T, x Extra) (*Broker, *collector, Fleet) {
	t.Helper()
	b, err := StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	c := &collector{}
	if err := b.Subscribe("#", c.add); err != nil {
		t.Fatal(err)
	}
	return b, c, newTestFleet(t, b.URL(), x)
}

// Why: tasmofleet's health relies on retained LWT, retained discovery and
// periodic STATE; a simulator that skips them hides health bugs.
func TestMQTTBasics(t *testing.T) {
	b, c, f := newBrokerFleet(t, Extra{})
	d := add(t, f, DeviceSpec{Name: "livingroom-readinglamp", Profile: ProfileESP8266_1M})
	c.waitFor(t, "[R]tele/livingroom-readinglamp/LWT=Online")
	c.waitFor(t, "[R]tasmota/discovery/")
	c.waitFor(t, "tele/livingroom-readinglamp/INFO1=")
	c.waitFor(t, "tele/livingroom-readinglamp/STATE=")
	if err := b.Publish("cmnd/livingroom-readinglamp/Power", []byte("ON"), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "stat/livingroom-readinglamp/POWER=ON")
	c.waitFor(t, `stat/livingroom-readinglamp/RESULT={"POWER":"ON"}`)
	if !d.Power() {
		t.Error("MQTT command must change state")
	}
	cm(t, d, "Status 2")
	c.waitFor(t, `stat/livingroom-readinglamp/STATUS2={"StatusFWR"`)
}

// Why: a software restart disconnects cleanly, so the broker must NOT see an
// LWT Offline; the retained LWT Online and INFO1 reappear after the boot.
func TestMQTTUpgradeShowsNoOfflineAndReportsResult(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	_, c, f := newBrokerFleet(t, Extra{})
	d := newDirect(t, f, "plug")
	c.waitFor(t, "[R]tele/plug/LWT=Online")
	before := c.count("tele/plug/INFO1=")
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	settled(t, d, 1)
	c.waitFor(t, `stat/plug/UPGRADE={"Upgrade":"Successful. Restarting"}`)
	for i := 0; i < 200 && c.count("tele/plug/INFO1=") == before; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if c.count("tele/plug/INFO1=") != before+1 {
		t.Error("INFO1 must be published once per boot")
	}
	if c.has("[R]tele/plug/LWT=Offline") || c.has("tele/plug/LWT=Offline") {
		t.Error("clean restart must not produce LWT Offline")
	}
	c.waitFor(t, `tele/plug/INFO1={"Info1":{"Module":"Sonoff Basic","Version":"16.1.0(release-tasmota)"`)
}

func TestMQTTFailureAndWrongFamilyAreReported(t *testing.T) {
	fw := newFirmwareServer(t, map[string]string{"tasmota32.bin": "16.1.0"})
	_, c, f := newBrokerFleet(t, Extra{})
	d := newDirect(t, f, "a")
	startUpgrade(t, d, fw, "tasmota32.bin")
	c.waitFor(t, `stat/a/UPGRADE={"Upgrade":"Failed Verify Bin Header Failed"}`)
	cm(t, d, "OtaUrl "+fw.URL+"/nothing.bin.gz")
	cm(t, d, "Upgrade 1")
	c.waitFor(t, `stat/a/UPGRADE={"Upgrade":"Failed File Not Found (404)"}`)
}

func TestFlapFault(t *testing.T) {
	_, c, f := newBrokerFleet(t, Extra{})
	newDirect(t, f, "a", Fault{Kind: FaultFlap, Duration: 60 * time.Millisecond})
	for i := 0; i < 300 && (c.count("[R]tele/a/LWT=Offline")+c.count("tele/a/LWT=Offline") < 2); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if c.count("[R]tele/a/LWT=Offline")+c.count("tele/a/LWT=Offline") < 2 {
		c.mu.Lock()
		defer c.mu.Unlock()
		t.Fatalf("flap produced no repeated Offline: %v", c.msgs)
	}
}

// Why: devices without MQTT must work in the same fleet as MQTT ones.
func TestHTTPOnlyDeviceStaysSilent(t *testing.T) {
	_, c, f := newBrokerFleet(t, Extra{})
	d := newDirect(t, f, "quiet", Fault{Kind: FaultHTTPOnly})
	newDirect(t, f, "chatty")
	c.waitFor(t, "[R]tele/chatty/LWT=Online")
	time.Sleep(250 * time.Millisecond)
	cm(t, d, "Status 2")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if contains(m, "/quiet/") {
			t.Errorf("HTTP-only device published %s", m)
		}
	}
}

// Why: the design fixture needs devices the app has seen online and then lost,
// the way a unplugged device is lost: the broker fires the will (LWT Offline)
// and the HTTP port refuses. Close is a clean shutdown and says nothing, so
// the app would keep showing the device as online until its telemetry went stale.
func TestPowerOffFiresTheWillAndStopsHTTP(t *testing.T) {
	_, c, f := newBrokerFleet(t, Extra{})
	d := add(t, f, DeviceSpec{Name: "unplugged", Profile: ProfileESP8266_4M})
	c.waitFor(t, "[R]tele/unplugged/LWT=Online")
	d.PowerOff()
	c.waitFor(t, "[R]tele/unplugged/LWT=Offline") // retained, like the will the device registered
	if _, _, err := cmWith(d, "Status", 500*time.Millisecond, nil); err == nil {
		t.Error("HTTP still answers after PowerOff")
	}
}
