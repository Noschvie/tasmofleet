package restore

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// lockoutTail ends the lockout sentence: the question is asked only while HTTP does not answer (#607).
const lockoutTail = "; the device's HTTP API does not answer, so TasmoFleet reaches the device only over MQTT and may lose it"

// connCutOffs maps every command that cmdpolicy counts as changing the device's MQTT connection (cmdpolicy.CutsMQTTOff,
// the writes the router publishes only after HTTP answered) to the field of the decoded backup that mqttLockout
// compares for it: a restore writes all of them at once. Keys are names as cmdpolicy.ResolveName reads them (a
// SetOption with its number). "" marks a setting a config dump does not hold, so a restore cannot change it.
// TestEveryCutOffCommandIsComparedByTheRestore fails for a cut-off command missing here, and
// TestEachMQTTConnectionSettingOfABackupIsCompared for a field mqttLockout does not compare.
var connCutOffs = map[string]string{ //nolint:gosec // G101: names of settings and decoded fields, no credential
	"topic":           "mqtt_topic",
	"fulltopic":       "mqtt_fulltopic",
	"grouptopic":      "mqtt_grptopic",
	"mqtthost":        "mqtt_host",
	"mqttport":        "mqtt_port",
	"mqttuser":        "mqtt_user",
	"mqttpassword":    "mqtt_pwd",
	"mqttclient":      "mqtt_client",
	"prefix":          "mqtt_prefix",
	"mqttfingerprint": "mqtt_fingerprint1, mqtt_fingerprint2",
	"setoption3":      "flag.mqtt_enabled",
	"setoption103":    "flag4.mqtt_tls",
	"setoption132":    "flag5.tls_use_fingerprint",
	// Tasmota keeps the TLS client key in its own flash sector (tls_spi_start, xdrv_02_9_mqtt.ino 16.0.0 lines
	// 2225-2237), outside the settings a config dump holds.
	"tlskey": "",
}

// decodedConn is what mqttLockout reads of the decoded backup; nil is a field the backup does not hold.
type decodedConn struct {
	Topic     *string         `json:"mqtt_topic"`
	Hostname  *string         `json:"hostname"`
	FullTopic *string         `json:"mqtt_fulltopic"`
	GrpTopic  *string         `json:"mqtt_grptopic"`
	Host      *string         `json:"mqtt_host"`
	Port      *int            `json:"mqtt_port"`
	User      *string         `json:"mqtt_user"`
	Pwd       *string         `json:"mqtt_pwd"`
	Client    *string         `json:"mqtt_client"`
	Prefix    []string        `json:"mqtt_prefix"`
	FP1       json.RawMessage `json:"mqtt_fingerprint1"`
	FP2       json.RawMessage `json:"mqtt_fingerprint2"`
	Flag      struct {
		MQTTEnabled *int `json:"mqtt_enabled"`
	} `json:"flag"`
	Flag4 struct {
		TLS *int `json:"mqtt_tls"`
	} `json:"flag4"`
	Flag5 struct {
		Fingerprint *int `json:"tls_use_fingerprint"`
	} `json:"flag5"`
}

// mqttLockout says why a restore over MQTT changes the device's MQTT connection: the backup changes how the device
// reaches the broker, under which topics it is addressed, or whether it uses MQTT at all (every setting of
// connCutOffs), or it cannot be compared. "" when nothing in the backup differs. Such a restore can cut TasmoFleet off
// from the device only while its HTTP API does not answer (preflight asks then, with lockoutTail); while HTTP answers
// it is the reason the device is marked after the upload and the wait for the restart also asks HTTP. Names only,
// never values (the password is one of the fields). The device's values are read over the ctx's path (pinned to
// MQTT by preflight); one it does not report counts as not comparable, never as equal.
func (m *Manager) mqttLockout(ctx context.Context, dev model.Device, b model.Backup) string {
	const unreadable = "the backup cannot be read, so its MQTT settings cannot be compared with the device's"
	dec, err := m.decodedOf(ctx, b.ID)
	if err != nil || dec == nil {
		return unreadable
	}
	var d decodedConn
	if json.Unmarshal(dec, &d) != nil {
		return unreadable
	}
	c := connCompare{m: m, ctx: ctx, dev: dev}
	// A field the backup does not hold is not compared; one the device side does not know is not either,
	// except those TasmoFleet reads from the device.
	if d.Topic != nil {
		c.topic(*d.Topic, d.Hostname)
	}
	c.add("FullTopic", d.FullTopic != nil && fullTopic(*d.FullTopic) != fullTopic(dev.FullTopic))
	c.add("GroupTopic", d.GrpTopic != nil && dev.GroupTopic != "" && *d.GrpTopic != dev.GroupTopic)
	// The broker comes from the inventory (Status 6); a device known only from the broker (app.onMQTTSeen) may have none
	// on record, and then the device is asked.
	if d.Host != nil {
		host, ok := dev.MQTTHost, true
		if host == "" {
			host, ok = c.liveText("MqttHost", "MqttHost")
		}
		c.known("MqttHost", ok, !strings.EqualFold(*d.Host, host))
	}
	if d.Port != nil {
		port, ok := dev.MQTTPort, true
		if port == 0 {
			port, ok = c.liveNumber("MqttPort")
		}
		c.known("MqttPort", ok, *d.Port != port)
	}
	if d.User != nil {
		c.text("MqttUser", "MqttUser", *d.User)
	}
	if d.Pwd != nil {
		c.add("MqttPassword", differsPassword(*d.Pwd, dev, m.o.BrokerPassword))
	}
	if d.Client != nil {
		c.text("MqttClient", "MqttClient", *d.Client)
	}
	if d.Prefix != nil {
		c.prefixes(d.Prefix)
	}
	// The device answers on the broker now, so MQTT is on: a backup with MQTT off switches it off (SetOption3 0).
	c.add("SetOption3 (MQTT off)", d.Flag.MQTTEnabled != nil && *d.Flag.MQTTEnabled == 0)
	if d.Flag4.TLS != nil {
		c.option("SetOption103 (MQTT TLS)", 103, *d.Flag4.TLS)
	}
	if d.Flag5.Fingerprint != nil {
		c.option("SetOption132 (TLS fingerprint)", 132, *d.Flag5.Fingerprint)
	}
	// Tasmota checks the fingerprints only with TLS and SetOption132 on (MqttConnected, xdrv_02_9_mqtt.ino 16.0.0
	// line 1613); with either off after the restore they do not decide the connection.
	if d.Flag4.TLS != nil && *d.Flag4.TLS == 1 && d.Flag5.Fingerprint != nil && *d.Flag5.Fingerprint == 1 {
		c.fingerprint(1, d.FP1)
		c.fingerprint(2, d.FP2)
	}
	var parts []string
	if len(c.diff) > 0 {
		parts = append(parts, "this backup changes "+strings.Join(c.diff, ", "))
	}
	if len(c.unread) > 0 {
		parts = append(parts, "the device did not report its "+strings.Join(c.unread, ", ")+
			", so the backup's MQTT settings cannot be compared with the device's")
	}
	return strings.Join(parts, "; ")
}

// connCompare collects mqttLockout's findings: the settings the backup changes and those the device did not report.
type connCompare struct {
	m      *Manager
	ctx    context.Context
	dev    model.Device
	diff   []string
	unread []string
}

func (c *connCompare) add(name string, differs bool) {
	if differs {
		c.diff = append(c.diff, name)
	}
}

// known adds name as changed when the device's value was read (ok) and differs, and as not reported when it was not.
func (c *connCompare) known(name string, ok, differs bool) {
	if !ok {
		c.unread = append(c.unread, name)
		return
	}
	c.add(name, differs)
}

// live asks the device cmd and returns the reply's top-level values; ok is false when it did not answer readably.
func (c *connCompare) live(cmd string) (map[string]json.RawMessage, bool) {
	raw, err := c.m.o.Client.Command(c.ctx, tasmota.TargetFor(c.dev), cmd)
	if err != nil {
		return nil, false
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(raw, &r) != nil {
		return nil, false
	}
	return r, true
}

// liveText reads the text value key of cmd's reply.
func (c *connCompare) liveText(cmd, key string) (string, bool) {
	r, ok := c.live(cmd)
	if !ok {
		return "", false
	}
	var s string
	if raw, has := r[key]; !has || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// liveNumber reads the numeric value of cmd's reply under the key cmd (Tasmota's ResponseCmndNumber).
func (c *connCompare) liveNumber(cmd string) (int, bool) {
	r, ok := c.live(cmd)
	if !ok {
		return 0, false
	}
	var n int
	if raw, has := r[cmd]; !has || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// text compares a text setting with the device's answer to cmd.
func (c *connCompare) text(name, cmd, backup string) {
	live, ok := c.liveText(cmd, cmd)
	c.known(name, ok, live != backup)
}

// topic compares the backup's Topic setting with the device's, raw against raw like MqttClient (#844): a decoded backup
// holds the setting with its placeholders ("tasmota_%06X"), the inventory the topic the device runs under, and the
// device answers Topic with the setting (CmndTopic, xdrv_02_9_mqtt.ino 16.0.0 line 2093). The same setting resolves to
// the same topic on the same device (the restore checked its MAC), except through %hostname%, which the device fills
// from its Hostname setting at boot: with it in the topic, the backup's Hostname is compared with the device's too.
func (c *connCompare) topic(backup string, hostname *string) {
	c.text("Topic", "Topic", backup)
	if !strings.Contains(backup, "%hostname%") {
		return
	}
	const name = "Hostname (in the Topic)"
	if hostname == nil {
		c.unread = append(c.unread, name)
		return
	}
	live, ok := c.liveText("Hostname", "Hostname")
	c.known(name, ok, bootHostname(live) != bootHostname(*hostname))
}

// bootHostname is the Hostname setting a device runs with after a restart: one with a '%' becomes WIFI_HOSTNAME
// (tasmota.ino 16.0.0 lines 701-704; CmndHostname does the same, support_command.ino lines 2545-2547), so every such
// value is the same one.
func bootHostname(s string) string {
	if strings.Contains(s, "%") {
		return "%"
	}
	return s
}

// defaultPrefixes are Tasmota's SUB_PREFIX, PUB_PREFIX and PUB_PREFIX2 (my_user_config.h), which an empty prefix in a
// decoded backup stands for.
var defaultPrefixes = [3]string{"cmnd", "stat", "tele"}

// prefixes compares the backup's Prefix1..3 with the device's (one bare Prefix answers all three).
func (c *connCompare) prefixes(backup []string) {
	r, ok := c.live("Prefix")
	if !ok {
		c.unread = append(c.unread, "Prefix")
		return
	}
	for i := range defaultPrefixes {
		want := ""
		if i < len(backup) {
			want = backup[i]
		}
		if want == "" {
			want = defaultPrefixes[i]
		}
		var got string
		if raw, has := r["Prefix"+strconv.Itoa(i+1)]; !has || json.Unmarshal(raw, &got) != nil {
			c.unread = append(c.unread, "Prefix")
			return
		}
		if got == "" {
			got = defaultPrefixes[i]
		}
		if got != want {
			c.diff = append(c.diff, "Prefix")
			return
		}
	}
}

// option compares a bit of the backup's flags with the device's SetOption<n>, read as a number from the SetOption words
// of Status 3. The answer to SetOption<n> itself is the device's StateText1 or StateText2 (CmndSetoptionBase answers
// GetStateText, support_command.ino 16.0.0 lines 1849-1853), "AUS" and "AN" on a device with German texts: a word that
// cannot be read as on or off (#845).
func (c *connCompare) option(name string, n, backup int) {
	on, ok := c.setOption(n)
	c.known(name, ok, on != (backup != 0))
}

// setOption reads the bit SetOption n from Status 3, whose SetOption array holds flag, param, flag3, flag4, flag5 and
// flag6 in hex (support_command.ino 16.0.0 lines 1012-1028); SetoptionDecode (lines 1614-1643) places the options in
// them. ok is false for an answer that does not hold the bit, and for SetOption32-49, which are numbers, not bits.
func (c *connCompare) setOption(n int) (on, ok bool) {
	word, bit, ok := setOptionWord(n)
	if !ok {
		return false, false
	}
	r, ok := c.live("Status 3")
	if !ok {
		return false, false
	}
	var log struct{ SetOption []string }
	if json.Unmarshal(r["StatusLOG"], &log) != nil || len(log.SetOption) <= word {
		return false, false
	}
	v, err := strconv.ParseUint(log.SetOption[word], 16, 32)
	if err != nil {
		return false, false
	}
	return v>>bit&1 == 1, true
}

// setOptionWord is the place of the bit SetOption n in Status 3's SetOption array (SetoptionDecode).
func setOptionWord(n int) (word, bit int, ok bool) {
	for _, r := range [...]struct{ first, last, word int }{{0, 31, 0}, {50, 81, 2}, {82, 113, 3}, {114, 145, 4}, {146, 177, 5}} {
		if n >= r.first && n <= r.last {
			return r.word, n - r.first, true
		}
	}
	return 0, 0, false
}

// fingerprint compares the backup's MqttFingerprint<n> (decode-config writes 20 "0x.." bytes) with the device's
// (Tasmota answers 20 hex bytes separated by blanks, CmndMqttFingerprint).
func (c *connCompare) fingerprint(n int, backup json.RawMessage) {
	name := "MqttFingerprint" + strconv.Itoa(n)
	want, ok := backupFingerprint(backup)
	if !ok {
		c.unread = append(c.unread, name)
		return
	}
	live, ok := c.liveText(name, name)
	if !ok {
		c.unread = append(c.unread, name)
		return
	}
	got, ok := hexBytes(strings.Fields(live), 16)
	if !ok {
		c.unread = append(c.unread, name)
		return
	}
	c.add(name, string(got) != string(want))
}

func backupFingerprint(raw json.RawMessage) ([]byte, bool) {
	var parts []string
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	return hexBytes(parts, 0)
}

// hexBytes reads 20 bytes written in base (0: with a "0x" prefix).
func hexBytes(parts []string, base int) ([]byte, bool) {
	if len(parts) != 20 {
		return nil, false
	}
	out := make([]byte, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseUint(p, base, 8)
		if err != nil {
			return nil, false
		}
		out = append(out, byte(v))
	}
	return out, true
}

// fullTopic normalises a FullTopic: empty is Tasmota's default, a trailing '/' is not significant.
func fullTopic(s string) string {
	if s == "" {
		s = "%prefix%/%topic%/"
	}
	return strings.TrimRight(s, "/")
}

// differsPassword compares the backup's MqttPassword with the one TasmoFleet knows for the device:
// its own stored one, or the broker password in broker mode. Without either there is nothing to
// compare against, and the transport refuses such a device anyway.
func differsPassword(backup string, dev model.Device, broker func() string) bool {
	switch dev.MQTTPasswordMode {
	case model.MQTTPasswordOwn:
		return backup != dev.MQTTPassword
	case model.MQTTPasswordBroker:
		if broker != nil && broker() != "" {
			return backup != broker()
		}
	case model.MQTTPasswordNone:
		// nothing to compare against
	}
	return false
}
