package backup

import (
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
)

// fakeFingerprint is the value the synthetic secrets dump gives telegram_fingerprint. A TLS fingerprint is no secret
// (#809), so the diff shows it; the leak searches take it out before they look for "FAKE-SECRET".
const fakeFingerprint = "FAKE-SECRET-tgfp"

// Why (#809): the backup diff, the drift view and the restore preview decided what is secret by a list of their own,
// and it disagreed with the console's: SSId, RgxSSId and MqttUser were redacted when typed but shown in a diff, and
// TmChatId was hidden in a diff but logged when typed. One setting must be secret in both places or in neither, or it
// leaks through the side that misses it. Each row is a Tasmota command, the decode-config settings it writes and, where
// the device reports it, its place in the Status snapshot. A TLS fingerprint is the SHA-1 of a server's public
// certificate and no secret (the coordinator's decision on #809): it must be compared, so a restore that changes
// the broker's fingerprint can be seen.
func TestDiffAndCommandPolicyAgreeOnEverySecret(t *testing.T) {
	rows := []struct {
		cmd      string
		decoded  []string
		snapshot string // "Status6.StatusMQT.MqttUser"; "" when no Status block reports it
		secret   bool
	}{
		{"SSId1", []string{"sta_ssid"}, "", true},
		{"Password1", []string{"sta_pwd"}, "", true},
		{"BSSId1", []string{"wifi_bssid"}, "", true},
		{"WebPassword", []string{"web_password"}, "", true},
		{"MqttUser", []string{"mqtt_user"}, "Status6.StatusMQT.MqttUser", true},
		{"MqttPassword", []string{"mqtt_pwd"}, "", true},
		{"RgxSSId", []string{"rgx_ssid"}, "", true},
		{"RgxPassword", []string{"rgx_pwassword", "rgx_pwassword_esp32"}, "", true},
		{"TmToken", []string{"telegram_token"}, "", true},
		{"TmChatId", []string{"telegram_chatid"}, "", true},
		{"IfxUser", []string{"influxdb_org"}, "", true},
		{"IfxOrg", []string{"influxdb_org"}, "", true},
		{"IfxToken", []string{"influxdb_token"}, "", true},
		{"KeeloqSet", []string{"keeloq_master_lsb", "keeloq_master_msb"}, "", true},
		{"ZbConfig", []string{"zb_precfgkey_h", "zb_precfgkey_l", "zb_ext_panid"}, "", true},
		{"DzKeyIdx1", []string{"domoticz_key_idx"}, "", true},
		{"MqttFingerprint1", []string{"mqtt_fingerprint1"}, "", false},
		{"MqttFingerprint2", []string{"mqtt_fingerprint2"}, "", false},
		{"TmFingerprint", []string{"telegram_fingerprint"}, "", false},
		{"SetOption132", []string{"flag5.tls_use_fingerprint"}, "", false},
		{"MqttHost", []string{"mqtt_host"}, "Status6.StatusMQT.MqttHost", false},
		{"Topic", []string{"mqtt_topic"}, "", false},
	}
	for _, r := range rows {
		v, err := cmdpolicy.Classify(r.cmd + " x")
		if err != nil || v.Redact != r.secret {
			t.Errorf("%s: Classify = %+v, %v; want Redact %v", r.cmd, v, err, r.secret)
		}
		for _, k := range r.decoded {
			a, b := nested(k, `"old-value"`), nested(k, `"new-value"`)
			for name, cmp := range map[string]func(a, b []byte) ([]Change, error){"Diff": Diff, "Compare": Compare} {
				ch, err := cmp(a, b)
				if err != nil || len(ch) != 1 {
					t.Fatalf("%s %s: %+v %v", name, k, ch, err)
				}
				shown := strings.Contains(ch[0].Old+ch[0].New, "value")
				if ch[0].Secret != r.secret || shown == r.secret {
					t.Errorf("%s of %s (%s): %+v; want secret %v", name, k, r.cmd, ch[0], r.secret)
				}
			}
		}
		if r.snapshot == "" {
			continue
		}
		a, b := nested(r.snapshot, `"old-value"`), nested(r.snapshot, `"new-value"`)
		ch, err := snapshotDrift(a, b)
		if err != nil || len(ch) != 1 {
			t.Fatalf("SnapshotDrift %s: %+v %v", r.snapshot, ch, err)
		}
		if shown := strings.Contains(ch[0].Old+ch[0].New, "value"); ch[0].Secret != r.secret || shown == r.secret {
			t.Errorf("SnapshotDrift of %s (%s): %+v; want secret %v", r.snapshot, r.cmd, ch[0], r.secret)
		}
	}
}

// nested builds {"a":{"b":value}} from the path "a.b".
func nested(path, value string) []byte {
	keys := strings.Split(path, ".")
	out := value
	for i := len(keys) - 1; i >= 0; i-- {
		out = `{"` + keys[i] + `":` + out + `}`
	}
	return []byte(out)
}
