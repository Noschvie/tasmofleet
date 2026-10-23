package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/audit"
)

// Change is one differing setting between two JSON documents. Path is
// "key.sub[2]". Old and New are the JSON encoding of the scalar ("300",
// "\"text\"", "null"); the empty string means the key is absent on that side.
// For a Secret row both are always empty: the UI only says that a secret
// value changed (spec 12.3). A string value that carries a credential
// (see scrub) is treated as secret: the row only says it changed. Body marks free text (rules, scripts)
// that can hold credentials no pattern recognises; HideBodies blanks those
// values for callers who may not read them and sets Hidden.
type Change struct {
	Path, Old, New string
	Secret         bool
	Body           bool
	Hidden         bool
	// Same marks a setting both documents hold with the same value; only Compare and
	// SnapshotCompare return such rows ("Show all settings" of the comparison and the drift view).
	Same bool
}

// secretRe is the fallback rule: the spec 12.3 expression, widened with the
// spellings real firmware uses (pwd, Tasmota's own "pwassword" typo,
// credentials, auth tokens, TLS fingerprints). It is a net under the explicit
// list below, not the primary defence.
var secretRe = regexp.MustCompile(`(?i)pass|pwd|pwassword|ssid.*pass|token|secret|key|cred|auth|fingerprint`)

// secretFields are the decode-config v16.0.0 settings that hold secrets or
// identify a secret, derived from its setting definitions (decoded output of
// a 15.6 dump has 316 keys; these are the ones that matter). Matched by
// exact path, so a field is covered even if a future rename slips past
// secretRe. "_esp32" twins are listed explicitly.
var secretFields = map[string]bool{
	"sta_pwd": true, "mqtt_pwd": true, "web_password": true,
	"rgx_pwassword": true, "rgx_pwassword_esp32": true,
	"influxdb_token": true, "telegram_token": true, "telegram_chatid": true,
	"mqtt_fingerprint1": true, "mqtt_fingerprint2": true, "telegram_fingerprint": true,
	"pn532_password": true, "pn532_pack": true,
	"zb_precfgkey_h": true, "zb_precfgkey_l": true, "zb_ext_panid": true,
	"keeloq_master_lsb": true, "keeloq_master_msb": true,
}

// SecretFields returns the explicit list (for tests and documentation).
func SecretFields() []string {
	out := make([]string, 0, len(secretFields))
	for k := range secretFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bodyKeys are free-text settings (ignoring case and index): rules, scripts
// and Berry code may contain credentials in any shape; mems (Mem1..16) hold
// the API keys rules use; rf_code holds replayable RF codes.
var bodyKeys = map[string]bool{"mems": true, "rf_code": true, "rules": true, "rule1": true, "rule2": true, "rule3": true, "script": true, "script_pram": true, "berry": true}

func isBodyKey(k string) bool { return bodyKeys[strings.ToLower(k)] }

// secretKey reports whether the value under a JSON key must never be shown:
// the fallback rule plus the audit redaction keys (matched ignoring "_" and "-").
func secretKey(k string) bool {
	if secretRe.MatchString(k) {
		return true
	}
	nk := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
	for _, rk := range audit.RedactKeys {
		if strings.Contains(nk, strings.NewReplacer("_", "", "-", "").Replace(rk)) {
			return true
		}
	}
	return false
}

// Credential patterns for free-text values, applied in order. They are
// deliberately greedy: when in doubt the whole value counts as credential. A
// value that any pattern changes is never displayed (see walk and
// diffFiltered), the scrubbed text only serves as a second line of defence.
var scrubbers = []struct {
	re   *regexp.Regexp
	repl string
}{
	// URL userinfo up to the LAST "@" of the line: passwords may hold "@", "/" or spaces.
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\n]*@`), `${1}***@`},
	// [user:pass@host] (WebSend) and user:pass@host without a scheme
	{regexp.MustCompile(`\[[^\]\n]*@`), `[***@`},
	{regexp.MustCompile(`([\s"'(=]|^)[^\s/@:\[\]"']+:[^\s/@:\[\]"']*@`), `${1}***@`},
	// header lines
	{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key|api-key|x-auth-token|x-access-token|cookie|set-cookie)\s*:\s*[^\r\n]*`), `${1}: ***`},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{3,}`), `${1} ***`},
	// name=value with any prefix or suffix on the name; quoted values may hold spaces
	{regexp.MustCompile(`(?i)([\w-]*(?:pass|pwd|token|secret|key|auth|sig|cred|user|login|sid)[\w-]*)\s*=\s*("[^"]*"|'[^']*'|[^&\s,;\]}]*)`), `${1}=***`},
	// JSON members, string or not
	{regexp.MustCompile(`(?i)"([^"]*(?:pass|pwd|token|secret|key|auth|cred|user|login)[^"]*)"\s*:\s*("(?:[^"\\]|\\.)*"|[^,}\s]+)`), `"${1}":"***"`},
	// Tasmota commands that take a secret: Password1 x, RgxPassword x, TelegramToken x, InfluxDbPassword x
	{regexp.MustCompile(`(?i)\b\w*(?:password|pwd|token|key|secret)\d*\s+[^\s;"']+`), `***`},
	// tokens carried in URL paths
	{regexp.MustCompile(`(?i)/bot\d{5,}:[\w-]+`), `/bot***`},
	{regexp.MustCompile(`/services/[A-Z0-9]+/[A-Z0-9]+/\w+`), `/services/***`},
	{regexp.MustCompile(`(?i)/api/webhooks/\d+/[\w-]+`), `/api/webhooks/***`},
}

// scrub removes credentials from a free-text value and reports whether it
// found any.
func scrub(s string) (string, bool) {
	out := s
	for _, sc := range scrubbers {
		out = sc.re.ReplaceAllString(out, sc.repl)
	}
	return out, out != s
}

// leaf is one flattened scalar (or empty container). raw is the JSON
// encoding used to compare and display; secret is set when the key marks it
// secret or the value itself carries a credential.
type leaf struct {
	raw    string
	secret bool
	body   bool
}

// flatten decodes doc and returns path -> leaf.
func flatten(doc []byte) (map[string]leaf, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber() // keep large numbers exact
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("backup: diff: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("backup: diff: trailing data after JSON document")
	}
	out := map[string]leaf{}
	walk(out, "", v, false, false)
	return out, nil
}

func encodeJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // scalars decoded from JSON always encode
		return "null"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func walk(out map[string]leaf, path string, v any, secret, body bool) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[path] = leaf{"{}", secret, body}
			return
		}
		for k, c := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walk(out, p, c, secret || secretFields[p] || secretKey(k), body || isBodyKey(k))
		}
	case []any:
		if len(t) == 0 {
			out[path] = leaf{"[]", secret, body}
			return
		}
		for i, c := range t {
			walk(out, path+"["+strconv.Itoa(i)+"]", c, secret, body)
		}
	case string:
		_, cred := scrub(t)
		out[path] = leaf{encodeJSON(t), secret || cred, body}
	default:
		s := encodeJSON(t)
		out[path] = leaf{s, secret, body}
	}
}

// Diff compares two decoded JSON documents and returns the settings that
// differ, sorted by path. Values of secret settings (key names matching the
// secret rule, anywhere on the path, and URLs with credentials) are never
// returned; such a row only has Secret set.
func Diff(a, b []byte) ([]Change, error) {
	return diffFiltered(a, b, nil, false)
}

// Compare is Diff plus the settings both documents share unchanged (Same
// set), sorted by path: the full comparison of two backups. The secret rule
// is the same as Diff's: a secret row never carries a value, changed or not.
func Compare(a, b []byte) ([]Change, error) {
	return diffFiltered(a, b, nil, true)
}

// diffFiltered compares a and b, skipping the paths ignore names; with all it lists the equal
// settings too (Same set), with the same redaction.
func diffFiltered(a, b []byte, ignore func(path string) bool, all bool) ([]Change, error) {
	fa, err := flatten(a)
	if err != nil {
		return nil, err
	}
	fb, err := flatten(b)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(fa)+len(fb))
	for p := range fa {
		paths = append(paths, p)
	}
	for p := range fb {
		if _, ok := fa[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []Change
	for _, p := range paths {
		if ignore != nil && ignore(p) {
			continue
		}
		la, oka := fa[p]
		lb, okb := fb[p]
		same := oka && okb && la.raw == lb.raw
		if same && !all {
			continue
		}
		c := Change{Path: p, Old: la.raw, New: lb.raw, Secret: la.secret || lb.secret, Body: la.body || lb.body, Same: same}
		if c.Secret {
			c.Old, c.New = "", ""
		}
		out = append(out, c)
	}
	return out, nil
}

// HideBodies blanks the values of free-text rows (rules, scripts) for
// callers who are not admins; the row stays and says that the text changed.
func HideBodies(in []Change) []Change {
	out := make([]Change, len(in))
	copy(out, in)
	for i := range out {
		if out[i].Body {
			out[i].Old, out[i].New, out[i].Hidden = "", "", true
		}
	}
	return out
}

// volatileDecoded are the decoded settings the device rewrites by itself
// (checksums, counters, save flags). A restore verification ignores them.
var volatileDecoded = map[string]bool{
	"cfg_crc": true, "cfg_crc32": true, "cfg_timestamp": true,
	"bootcount": true, "bootcount_reset_time": true, "save_flag": true,
}

// IsVolatile reports whether a decoded-JSON path changes by itself.
func IsVolatile(path string) bool {
	return volatileDecoded[path] || strings.HasPrefix(path, "header.")
}

// FilterVolatile drops the changes IsVolatile names.
func FilterVolatile(in []Change) []Change {
	var out []Change
	for _, c := range in {
		if !IsVolatile(c.Path) {
			out = append(out, c)
		}
	}
	return out
}

// volatileSnapshotKeys are leaf names in the Status snapshot that change
// without anybody touching the configuration; volatileSnapshotPrefixes are
// whole status groups that are live readings (time, sensors, Status 11).
var (
	volatileSnapshotKeys = map[string]bool{
		"Time": true, "Uptime": true, "UptimeSec": true, "Heap": true, "LoadAvg": true, "MqttCount": true,
		"RSSI": true, "Signal": true, "StartupUTC": true, "BootCount": true, "SaveCount": true, "SaveAddress": true,
		"RestartReason": true, "StackLowMark": true, "PsrFreeMem": true, "FreeMem": true, "Free": true,
		"UTC": true, "Local": true, "Sunrise": true, "Sunset": true, "Sleep": true, "SleepMode": true,
	}
	volatileSnapshotPrefixes = []string{"Status7.", "Status8.", "Status11."}
)

func volatileSnapshot(path string) bool {
	for _, p := range volatileSnapshotPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	last := path
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		last = path[i+1:]
	}
	if i := strings.IndexByte(last, '['); i >= 0 {
		last = last[:i]
	}
	return volatileSnapshotKeys[last]
}

// SnapshotDrift compares a stored snapshot (Status 1..11 JSON) with a live
// one and returns the differences in settings, ignoring clocks, uptime,
// memory and sensor readings. Secrets are redacted like in Diff.
func SnapshotDrift(stored, live []byte) ([]Change, error) {
	return diffFiltered(stored, live, volatileSnapshot, false)
}

// SnapshotCompare is SnapshotDrift with the unchanged settings listed too (Same set): the
// drift view's "Show all". Secrets stay redacted on both kinds of rows.
func SnapshotCompare(stored, live []byte) ([]Change, error) {
	return diffFiltered(stored, live, volatileSnapshot, true)
}
