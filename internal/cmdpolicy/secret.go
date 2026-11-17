package cmdpolicy

import "strings"

// SecretName reports whether name carries a secret: a credential, a key or a network name. name is a Tasmota command
// name ("Password1", "RgxSSId"), a key of a device reply ("MqttUser", "KeyL") or a setting of a decoded backup
// ("sta_pwd", "zb_ext_panid"); letter case, '_' and '-' do not matter. It is the one rule for what is secret (#809):
// Classify marks such a command Redact (its arguments never reach a log, its reply is masked), PolicyReadable refuses
// it as a policy rule, ScrubReply masks the value under such a key, and the backup diff, drift and restore preview
// show only that such a setting changed.
//
// The name is read the way the device reads it (ResolveName: the index at its end, a SetOption synonym as its
// SetOption) and also as it is written, so a name that ResolveName does not accept (a decode-config key with a
// character a command cannot have, a long path) is judged by its text and never counts as harmless only because it
// could not be read. ZbNameKey is SetOption83, a harmless flag, and stays secret by its own name: fail closed.
//
// TLS fingerprints (MqttFingerprint, TmFingerprint; decode-config mqtt_fingerprint1/2, telegram_fingerprint) are not
// secret: each is the SHA-1 of a server's public certificate, which that server shows to every client that connects.
// The diff, drift and restore preview compare them, so a restore that changes the broker's fingerprint can be seen.
func SecretName(name string) bool {
	s := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	if n, ok := ResolveName(s); ok && secretBase(n.Base) {
		return true
	}
	return secretBase(strings.TrimRight(s, "0123456789"))
}

// secretBase judges a lower-case name without '_', '-' and its index.
func secretBase(b string) bool {
	if secretNames[b] || secretExact[b] {
		return true
	}
	for _, p := range secretParts {
		if strings.Contains(b, p) {
			return true
		}
	}
	return false
}

// secretExact are secret names that no part of secretParts catches, matched exactly, each with its source (Tasmota
// 16.0.0 and the decode-config v16 setting definitions). They ask for no confirmation of their own (that is
// secretNames).
var secretExact = map[string]bool{
	// KeeloqSet (xdrv_36_keeloq.ino, CmndKeeloqSet) stores the KeeLoq master key, which decode-config names
	// keeloq_master_msb and keeloq_master_lsb.
	"keeloqset": true, "keeloqmastermsb": true, "keeloqmasterlsb": true,
	// IfxUser and IfxOrg (CmndInfluxDbUser, xdrv_59_influxdb.ino) write the InfluxDB user or organisation,
	// decode-config's influxdb_org.
	"influxdborg": true,
	// The PN532 card reader's PACK (password acknowledge) of its NFC tags, decode-config's pn532_pack, set together
	// with pn532_password.
	"pn532pack": true,
}

// secretParts mark a secret name by a part of it (the name lower case, without '_' and '-'). pass catches password,
// passwd and every *Password command; pwass is Tasmota's own "pwassword" spelling (decode-config rgx_pwassword);
// key catches TLSKey, LoRaWanAppKey, MI32Key, ZbConfig's KeyL and KeyH, apikey and the Zigbee keys (zb_precfgkey_*);
// ssid also BSSId; chatid is Telegram's TmChatId (telegram_chatid); extpanid the Zigbee network's extended PAN ID
// (zb_ext_panid), a part of its join credentials. Fail closed: a harmless name with such a part (DzKeyIdx,
// sen5x_passive_mode) counts as secret.
var secretParts = []string{"pass", "pwd", "pwass", "token", "secret", "key", "cred", "auth", "ssid", "mqttuser", "chatid", "extpanid"}
