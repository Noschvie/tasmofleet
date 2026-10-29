package cmdpolicy

import (
	"strings"
	"testing"
)

// Why: a fleet policy rule makes tasmofleet send the bare command name to every
// device, every hour, with nobody watching. Each name here either answers with a
// plain value (readable) or does something when sent bare (denied); one wrong
// entry means a fleet-wide reset, restart or flash write.
func TestPolicyReadableDenyList(t *testing.T) {
	denied := map[string]string{
		// Status: Status 0 builds a reply large enough to crash an ESP8266, in any spelling.
		"Status": reasonStatus, "Status0": reasonStatus, "STATUS0": reasonStatus, "status00": reasonStatus,
		"Status11": reasonStatus, "StatusSNS": reasonStatus, "StatusFoo": reasonStatus,
		// secrets
		"Password": reasonSecret, "Password1": reasonSecret, "WebPassword": reasonSecret, "MqttPassword": reasonSecret,
		"MqttUser": reasonSecret, "SSId1": reasonSecret, "TmToken": reasonSecret, "IfxToken": reasonSecret,
		"TLSKey1": reasonSecret, "LoRaWanAppKey": reasonSecret, "MyPasswd": reasonSecret, "ApiSecret": reasonSecret,
		"MyApiKey": reasonSecret, "PwdX": reasonSecret, "xtokenx": reasonSecret,
		// commands that act when they are sent bare
		"Backlog": reasonActs, "Backlog0": reasonActs, "BacklogX": reasonActs, "Restart": reasonActs, "Reset": reasonActs,
		"Upgrade": reasonActs, "Upload": reasonActs, "SaveData": reasonActs, "TelePeriod": reasonActs, "Delay": reasonActs,
		"Event": reasonActs, "Publish": reasonActs, "Publish2": reasonActs, "PublishRetain": reasonActs, "Subscribe": reasonActs,
		"Unsubscribe": reasonActs, "Rule": reasonActs, "Rule1": reasonActs, "RuleTimer1": reasonActs, "Timer1": reasonActs,
		"Timers": reasonActs, "Power": reasonActs, "Power1": reasonActs, "Power0": reasonActs, "Sensor13": reasonActs,
		"Sensor": reasonActs, "Driver4": reasonActs, "Wifi": reasonActs, "WifiConfig": reasonActs, "WifiPower": reasonActs,
		"Ping": reasonActs, "Ping4": reasonActs, "WakeUp": reasonActs, "WakeUpDuration": reasonActs, "Script": reasonActs,
		"Script1": reasonActs, "Br": reasonActs, "Json": reasonActs, "If": reasonActs, "I2CScan": reasonActs, "Buzzer": reasonActs,
		"WebSend": reasonActs, "SerialSend5": reasonActs, "TuyaSend": reasonActs, "ZbSend": reasonActs, "ZbPermitJoin": reasonActs,
		"ShutterOpen1": reasonActs, "ShutterClose": reasonActs, "ShutterStop2": reasonActs, "ShutterToggle": reasonActs,
		"ShutterSetOpen1": reasonActs, "ShutterSetClose1": reasonActs, "BrRestart": reasonActs, "Mp3Reset": reasonActs,
		"Mp3Stop": reasonActs, "Mp3Play": reasonActs, "RfKey1": reasonActs, "TCPStart": reasonActs, "TCPConnect": reasonActs,
		"BLEScan": reasonActs, "WcInit": reasonActs,
		// bare form reports, set form restarts, locks out, actuates or corrupts counters
		"Gpio": reasonSetActs, "Gpio4": reasonSetActs, "Gpios": reasonSetActs, "Module": reasonSetActs, "Modules": reasonSetActs,
		"SetSensor1": reasonSetActs, "I2CDriver3": reasonSetActs, "MqttClient": reasonSetActs, "GroupTopic1": reasonSetActs,
		"Prefix1": reasonSetActs, "Prefix3": reasonSetActs, "Emulation": reasonSetActs, "TuyaMCU": reasonSetActs, "Ethernet": reasonSetActs,
		"EthType": reasonSetActs, "MqttFingerprint": reasonSetActs, "CpuFrequency": reasonSetActs, "DeepSleepTime": reasonSetActs,
		"WebQuery": reasonSetActs, "UfsDelete": reasonSetActs, "UfsRename": reasonSetActs, "Exception": reasonSetActs,
		"FlashMode": reasonSetActs, "Cfg": reasonSetActs, "CfgX": reasonSetActs, "Dimmer": reasonSetActs, "Dimmer2": reasonSetActs,
		"Channel1": reasonSetActs, "Color": reasonSetActs, "CT": reasonSetActs, "White": reasonSetActs, "HsbColor1": reasonSetActs,
		"PWM3": reasonSetActs, "PowerSet": reasonSetActs, "PowerCal": reasonSetActs, "EnergyReset3": reasonSetActs,
		"UfsRun": reasonActs, "WebGetConfig": reasonActs, "Telnet": reasonActs, "SendMail": reasonActs, "LoRaCommand": reasonActs,
		// not a plain name, or not the full name
		"": reasonBadName, " ": reasonBadName, "Sleep 50": reasonBadName, " Sleep": reasonBadName, "Sleep ": reasonBadName,
		"Sleep\t1": reasonBadName, "Sl;eep": reasonBadName, "1Sleep": reasonBadName, "Sléep": reasonBadName, "Sleep\n": reasonBadName,
		"SO19": reasonSynonym, "so": reasonSynonym,
		"SetOption": reasonSetOption, "SetOption19": reasonSetOption, "SetOptionX": reasonSetOption,
		strings.Repeat("a", maxNameLen+1): reasonBadName,
	}
	for name, why := range denied {
		ok, got := PolicyReadable(name)
		if ok || got != why {
			t.Errorf("PolicyReadable(%q) = %v, %q; want denied with %q", name, ok, got, why)
		}
	}
}

// Why: the deny list must not swallow the settings people actually want to check;
// a list that refuses everything is safe and useless.
func TestPolicyReadableAllowsPlainSettings(t *testing.T) {
	for _, name := range []string{
		"Sleep", "LedState", "WebLog", "Latitude", "Longitude", "TempOffset", "PowerOnState", "PowerDelta", "Timezone",
		"Hostname", "Pulsetime1", "Interlock", "SwitchMode3", "Baudrate", "Sensor", "Teleperiod", "NtpServer1",
		strings.Repeat("a", maxNameLen),
	} {
		ok, why := PolicyReadable(name)
		want := name != "Sensor" && name != "Teleperiod" // listed on purpose: Sensor<n> acts, TelePeriod is curated
		if ok != want {
			t.Errorf("PolicyReadable(%q) = %v (%s), want %v", name, ok, why, want)
		}
	}
}

// Why: "Status 0 must never be sendable in any spelling" is the one rule that cannot have a
// hole: Tasmota reads "Status 00", "Status +0", "Status 0x0" and "Status off" as 0 as well,
// and matches names without regard to case.
func TestPolicyReadableNeverAllowsStatusInAnySpelling(t *testing.T) {
	suffixes := []string{"", "0", "00", "000", "+0", "-0", "0x0", "0x00", "off", "OFF", "1", "9", "10", "11", "12", "99", "٠", "０", "S", "SNS", "_0"}
	for _, base := range []string{"status", "Status", "STATUS", "sTaTuS", "StAtUs"} {
		for _, suf := range suffixes {
			if ok, _ := PolicyReadable(base + suf); ok {
				t.Errorf("PolicyReadable(%q) is allowed", base+suf)
			}
		}
	}
}

// Why: a rule's set form is what "Make compliant" sends without asking. Whatever the console
// would stop and ask about (Reset, Module, Wifi 0, ...) must never come out of a rule, and a
// credential must never be written or logged.
func TestPolicySetText(t *testing.T) {
	for _, c := range [][3]string{
		{"Sleep", "50", "Sleep 50"}, {"LedState", "3", "LedState 3"}, {"WebLog", "2", "WebLog 2"},
		{"Latitude", "52.5", "Latitude 52.5"}, {"Pulsetime1", "100", "Pulsetime1 100"}, {"Timezone", "99", "Timezone 99"},
		{"Wifi", "1", "Wifi 1"}, {"WebServer", "2", "WebServer 2"}, {"SetOption19", "1", "SetOption19 1"},
		{"FriendlyName1", "x", "FriendlyName1 x"}, {"Sleep", "1 2", "Sleep 1 2"},
	} {
		if got, err := PolicySetText(c[0], c[1]); err != nil || got != c[2] {
			t.Errorf("PolicySetText(%q, %q) = %q, %v; want %q", c[0], c[1], got, err, c[2])
		}
	}
	refused := map[[2]string]string{
		{"Reset", "1"}:         reasonConfirm,
		{"Restart", "2"}:       reasonConfirm,
		{"Template", "1"}:      reasonConfirm,
		{"Template", "x"}:      reasonConfirm,
		{"WifiConfig", "2"}:    reasonConfirm,
		{"Wifi", "0"}:          reasonConfirm,
		{"Wifi", "off"}:        reasonConfirm,
		{"WebServer", "1"}:     reasonConfirm,
		{"IPAddress1", "x"}:    reasonConfirm,
		{"Ethernet", "1"}:      reasonConfirm,
		{"Ap", "1"}:            reasonConfirm,
		{"Upgrade", "1"}:       reasonConfirm,
		{"Upload", "1"}:        reasonConfirm,
		{"OtaUrl", "http://x"}: reasonConfirm,
		{"Status", "0"}:        reasonConfirm,
		{"Status0", ""}:        reasonConfirm,
		{"Password", "x"}:      reasonSecret,
		{"MqttPassword", "x"}:  reasonSecret,
		{"SSId1", "x"}:         reasonSecret,
		{"Sleep", "5;Reset 1"}: "the expected value is not a plain value for this command",
		{"Sleep", "5%3BX"}:     "the expected value is not a plain value for this command",
		{"Sleep", "5;X"}:       "the expected value is not a plain value for this command",
	}
	for in, why := range refused {
		got, err := PolicySetText(in[0], in[1])
		if err == nil || got != "" || err.Error() != why {
			t.Errorf("PolicySetText(%q, %q) = %q, %v; want refused with %q", in[0], in[1], got, err, why)
		}
	}
}

// Why: a command that classifies with Redact keeps its arguments out of every log; it must also
// never be accepted as a rule, even if some future class change dropped its confirmation.
func TestPolicyCommandCombinesReadAndSetChecks(t *testing.T) {
	if err := PolicyCommand("Sleep", "50"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, want, why string }{
		{"Status0", "1", reasonStatus},
		{"MqttPassword", "x", reasonSecret},
		{"Reset", "1", reasonActs},
		{"Template", "1", reasonConfirm},
		{"WebServer", "1", reasonConfirm},
	} {
		if err := PolicyCommand(c.name, c.want); err == nil || err.Error() != c.why {
			t.Errorf("PolicyCommand(%q, %q) = %v, want %q", c.name, c.want, err, c.why)
		}
	}
}

// Why: whatever the name, "readable" must imply that the bare form is not Status and not a
// secret, is one plain token, and classifies without a confirmation. A fuzzer finds the
// spelling a table did not think of.
func FuzzPolicyReadable(f *testing.F) {
	for _, s := range []string{"Sleep", "Status0", "status 0", "STATUS００", "Status+0", "Backlog", "Power1", "SO19", "MqttPassword", "Rule1", "x;y", "", "Sleep\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		ok, why := PolicyReadable(name)
		if !ok {
			if why == "" {
				t.Fatalf("PolicyReadable(%q) refuses without a reason", name)
			}
			return
		}
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "status") {
			t.Fatalf("Status spelling %q is readable", name)
		}
		if strings.ContainsAny(name, " \t\r\n;") || name != strings.TrimSpace(name) {
			t.Fatalf("%q is readable but is not one plain token", name)
		}
		v, err := Classify(name)
		if err != nil || v.Confirm != "" || v.Redact {
			t.Fatalf("%q is readable but its bare form classifies as %+v, %v", name, v, err)
		}
		if canon, err := Canonical(name); err != nil || canon != name {
			t.Fatalf("%q is readable but is not canonical (%q, %v)", name, canon, err)
		}
		for _, secret := range secretParts {
			if strings.Contains(lower, secret) {
				t.Fatalf("%q contains %q and is readable", name, secret)
			}
		}
	})
}
