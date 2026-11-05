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
	t.Parallel()
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

// Why (#632): a device with SetOption4 1 publishes a command's reply on stat/<t>/<COMMAND> (upper
// case, index cut), an unknown command on COMMAND and a relay on stat/<t>/POWERn, never on RESULT
// (Tasmota support_command.ino:517, xdrv_02_9_mqtt.ino:947). The SetOption4 tests of mqttcmd and
// transport read such a device through this simulator: if it answered on RESULT they would pass
// without testing anything. Switched off over MQTT, the reply comes on RESULT again.
func TestMQTTSetOption4RepliesOnTheCommandTopic(t *testing.T) {
	t.Parallel()
	b, c, f := newBrokerFleet(t, Extra{})
	add(t, f, DeviceSpec{Name: "so4-plug", Profile: ProfileESP8266_1M, Relays: 2, Settings: map[string]string{"setoption4": "1"}})
	c.waitFor(t, "[R]tele/so4-plug/LWT=Online")
	for _, tc := range []struct{ cmd, payload, want string }{
		{"TelePeriod", "60", `stat/so4-plug/TELEPERIOD={"TelePeriod":60}`},
		{"SetOption19", "", `stat/so4-plug/SETOPTION={"SetOption19":`},
		{"NoSuchCommand", "", `stat/so4-plug/COMMAND={"Command":"Unknown"`},
		{"Power2", "ON", `stat/so4-plug/POWER2={"POWER2":"ON"}`}, // named after the relay, not the command
	} {
		if err := b.Publish("cmnd/so4-plug/"+tc.cmd, []byte(tc.payload), false); err != nil {
			t.Fatal(err)
		}
		c.waitFor(t, tc.want)
	}
	if n := c.count("stat/so4-plug/RESULT="); n != 0 {
		t.Fatalf("%d replies on RESULT while SetOption4 is 1", n)
	}
	if err := b.Publish("cmnd/so4-plug/SetOption4", []byte("0"), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, `stat/so4-plug/RESULT={"SetOption4":"OFF"}`)
}

// Why (#272): a real device is subscribed to its cmnd topics before it announces itself, so a
// controller that sends a command the moment the device shows up is answered. The simulator
// used to fire its SUBSCRIBEs without waiting for the SUBACKs (and the tests raced them).
func TestMQTTDeviceIsSubscribedWhenItAnnouncesItself(t *testing.T) {
	t.Parallel()
	b, c, f := newBrokerFleet(t, Extra{})
	d := newDirect(t, f, "plug")
	ctl, _ := Control(d)
	c.waitFor(t, "[R]tele/plug/LWT=Online")
	if !ctl.MQTTReady() {
		t.Fatal("LWT Online published but the device is not subscribed to its command topic")
	}
	if err := b.Publish("cmnd/plug/Power", []byte("ON"), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "stat/plug/POWER=ON")
}

// Why: Tasmota's PubSubClient::disconnect() sends no DISCONNECT packet by
// default, so EspRestart (WifiShutdown -> MqttDisconnect) makes the broker fire
// the will: LWT Offline before every reboot, seen live on 2026-10-30 (see
// docs/research/tasmota-mqtt-transport.md). Health and the OTA engine must be
// tested against that, not against a silent restart. The retained LWT Online
// and INFO1 reappear after the boot.
func TestMQTTUpgradeShowsOfflineAndReportsResult(t *testing.T) {
	t.Parallel()
	fw := newFirmwareServer(t, map[string]string{"tasmota.bin.gz": "16.1.0"})
	_, c, f := newBrokerFleet(t, Extra{})
	d := newDirect(t, f, "plug")
	c.waitFor(t, "[R]tele/plug/LWT=Online")
	// LWT Online comes before INFO1-3 within a boot: sample only once the first boot's INFO3
	// (its last message) is in, or its INFO1 would be counted as the second boot's.
	c.waitFor(t, "tele/plug/INFO3=")
	before := c.count("tele/plug/INFO1=")
	before3 := c.count("tele/plug/INFO3=")
	startUpgrade(t, d, fw, "tasmota.bin.gz")
	settled(t, d, 1)
	c.waitFor(t, `stat/plug/UPGRADE={"Upgrade":"Successful. Restarting"}`)
	// INFO3 is the last message of a boot, so once it is in, the boot's INFO1 count is final.
	c.waitForCount(t, "tele/plug/INFO3=", before3+1)
	if c.count("tele/plug/INFO1=") != before+1 {
		t.Error("INFO1 must be published once per boot")
	}
	if !c.has("[R]tele/plug/LWT=Offline") && !c.has("tele/plug/LWT=Offline") {
		t.Error("a software restart must produce LWT Offline (no DISCONNECT packet)")
	}
	c.waitFor(t, `tele/plug/INFO1={"Info1":{"Module":"Sonoff Basic","Version":"16.1.0(release-tasmota)"`)
}

func TestMQTTFailureAndWrongFamilyAreReported(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	_, c, f := newBrokerFleet(t, Extra{})
	d := add(t, f, DeviceSpec{Name: "unplugged", Profile: ProfileESP8266_4M})
	c.waitFor(t, "[R]tele/unplugged/LWT=Online")
	d.PowerOff()
	c.waitFor(t, "[R]tele/unplugged/LWT=Offline") // retained, like the will the device registered
	if _, _, err := cmWith(d, "Status", 500*time.Millisecond, nil); err == nil {
		t.Error("HTTP still answers after PowerOff")
	}
}
