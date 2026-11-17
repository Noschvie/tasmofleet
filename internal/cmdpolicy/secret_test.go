package cmdpolicy

import (
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/tasmotacmd"
)

// Why (#809): what is secret was decided in five lists that disagreed: RgxSSId was redacted in the console but shown
// in a backup diff, TmChatId and WCAuth were hidden in the diff but logged and echoed in the console, IfxOrg (the
// same handler and setting as IfxUser) was secret nowhere. A secret that one side misses reaches a log, a page or
// the audit. Every Tasmota command that carries a secret must therefore be secret everywhere at once: in the one
// rule (SecretName), in the console's verdict (Redact, bare and with a value: the bare reply shows the stored value)
// and as a policy rule (PolicyReadable refuses it). A TLS fingerprint is the SHA-1 of a server's public certificate
// and is no secret, so it stays comparable (the coordinator's decision on #809).
func TestOneSecretRuleCoversTasmotaSecretCommands(t *testing.T) {
	secret := map[string][]string{
		"Wi-Fi":          {"SSId1", "SSID02", "Password1", "BSSId1", "WiFiTest"},
		"web":            {"WebPassword", "WCAuth"},
		"MQTT":           {"MqttUser", "mqttuser", "MqttPassword", "TLSKey1"},
		"range extender": {"RgxSSId", "RgxPassword"},
		"Telegram":       {"TmToken", "TmChatId"},
		"InfluxDB":       {"IfxUser", "IfxOrg", "IfxPassword", "IfxToken"},
		"LoRaWAN":        {"LoRaWanAppKey1"},
		"Zigbee and BLE": {"ZbConfig", "ZbNameKey", "MI32Key", "MI32Keys1"},
		"other keys":     {"DzKeyIdx1", "RfKey1", "KeeloqSet"},
	}
	for area, names := range secret {
		for _, name := range names {
			if !SecretName(name) {
				t.Errorf("%s: SecretName(%s) = false", area, name)
			}
			for _, cmd := range []string{name, name + " x"} {
				if v, err := Classify(cmd); err != nil || !v.Redact {
					t.Errorf("%s: Classify(%q) = %+v, %v: want Redact", area, cmd, v, err)
				}
			}
			if ok, _ := PolicyReadable(name); ok {
				t.Errorf("%s: %s is readable as a policy rule", area, name)
			}
		}
	}
	for _, name := range []string{"MqttFingerprint1", "MqttFingerprint2", "TmFingerprint", "MqttHost", "Topic", "Hostname"} {
		if SecretName(name) {
			t.Errorf("SecretName(%s) = true: not a secret", name)
		}
		for _, cmd := range []string{name, name + " x"} {
			if v, err := Classify(cmd); err != nil || v.Redact {
				t.Errorf("Classify(%q) = %+v, %v: want no Redact", cmd, v, err)
			}
		}
	}
}

// Why (#809): the backup diff and the reply mask judge decode-config keys and reply keys, which are not command
// names: underscores, a character a command cannot have, a long path. Such a name must be judged by its text and
// never count as harmless only because the command reader refuses it (fail closed).
func TestSecretNameJudgesNamesTheCommandReaderRefuses(t *testing.T) {
	for _, k := range []string{"sta_pwd", "rgx_pwassword_esp32", "zb_ext_panid", "keeloq_master_lsb", "pn532_pack",
		"influxdb_org", "telegram_chatid", "wifi_bssid", "mqtt_user", "Key.L", "x" + strings.Repeat("y", 60) + "_token",
		"KeyL", "KeyH", "MIKeys", "client_secret", "http_password"} {
		if !SecretName(k) {
			t.Errorf("SecretName(%q) = false", k)
		}
	}
	for _, k := range []string{"mqtt_host", "tele_period", "friendlyname", "mqtt_fingerprint1", "telegram_fingerprint",
		"tls_use_fingerprint", "keeloq_serial", "StatusMQT", "x" + strings.Repeat("y", 60)} {
		if SecretName(k) {
			t.Errorf("SecretName(%q) = true", k)
		}
	}
}

// Why (#809): IfxOrg and IfxUser are two names of one handler that writes one setting, yet only IfxUser was
// redacted, so the same InfluxDB login reached the log under the other name. Every command of a pinned Tasmota
// catalogue that shares its handler with a secret command must be secret too, so the next shared handler is found
// here, not in a log. SetOption synonyms are left out: their handler is CmndSetoption, shared by every SetOption.
// A name the console refuses as malformed (LD2402_Save) or never sends (Upload, a file transfer, shares Upgrade's
// handler) is left out too.
func TestCommandsOfOneHandlerAreSecretAlike(t *testing.T) {
	set, err := tasmotacmd.Pinned()
	if err != nil {
		t.Fatal(err)
	}
	for _, ver := range set.Versions() {
		cat, _, _ := set.For(ver)
		redact := map[string]map[bool][]string{}
		for _, c := range cat.Commands {
			if c.Handler == "" || c.Handler == "CmndSetoption" || c.SynonymOf != "" {
				continue
			}
			v, err := Classify(c.Command)
			if err != nil || v.Class == ClassForbidden {
				continue
			}
			if redact[c.Handler] == nil {
				redact[c.Handler] = map[bool][]string{}
			}
			redact[c.Handler][v.Redact] = append(redact[c.Handler][v.Redact], c.Command)
		}
		for h, by := range redact {
			if len(by[true]) > 0 && len(by[false]) > 0 {
				t.Errorf("%s: %s runs %v (Redact) and %v (not Redact)", ver, h, by[true], by[false])
			}
		}
	}
}
