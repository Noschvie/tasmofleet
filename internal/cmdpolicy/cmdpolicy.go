package cmdpolicy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmotacmd"
)

// Class is the authority a command needs.
type Class int

// Classes. The zero value is deliberately none of them: an unclassified
// verdict can never be sent (MinRole reports false).
const (
	ClassRead      Class = iota + 1 // queries; operator
	ClassOperator                   // harmless actions (Power, Restart 1); operator
	ClassAdmin                      // everything else; admin only
	ClassForbidden                  // malformed lines and file transfer commands; never sent
)

func (c Class) String() string {
	switch c {
	case ClassRead:
		return "read"
	case ClassOperator:
		return "operator"
	case ClassAdmin:
		return "admin"
	case ClassForbidden:
		return "forbidden"
	}
	return "unclassified"
}

// MinRole is the lowest role that may send commands of this class. ok is
// false for ClassForbidden and for values that are not a class.
func (c Class) MinRole() (role model.Role, ok bool) {
	switch c {
	case ClassRead, ClassOperator:
		return model.RoleOperator, true
	case ClassAdmin:
		return model.RoleAdmin, true
	case ClassForbidden:
		return model.RoleNone, false
	}
	return model.RoleNone, false
}

// Verdict is the classification of one command line.
type Verdict struct {
	Class  Class
	Name   string // command name as typed, with its index ("Power1"); never the arguments
	Reason string // why it is malformed or needs admin; safe to show
	Redact bool   // the arguments are secret: log Redacted(cmd) only
	// Confirm is set for destructive or lock-out commands: the consequence,
	// in one sentence that is safe to show. Callers with a UI ask for exactly
	// one confirmation that names it before they send; it is no authority.
	Confirm string
}

// Errors. Callers match with errors.Is.
var (
	ErrInvalid   = errors.New("cmdpolicy: malformed command")
	ErrForbidden = errors.New("cmdpolicy: command is never sent from tasmofleet")
	ErrRole      = errors.New("cmdpolicy: command needs a higher role")
	// ErrUpdateJob is the refusal of a firmware command (StartsFirmware) while a TasmoFleet update job holds
	// the device. The caller knows the job state; the policy only names the rule and the reason.
	ErrUpdateJob = errors.New("cmdpolicy: a TasmoFleet update job holds this device")
)

// RejectedText replaces a malformed command in logs and history.
const RejectedText = "[rejected: malformed command]"

// MaxCommandLen is the longest command line, a whole Backlog included, that cmdpolicy reads (Template has a limit of
// its own). The MQTT channel publishes only what cmdpolicy reads, so whoever builds a Backlog for it (Make
// compliant's chunks) stays within this.
const MaxCommandLen = 255

const (
	// maxTemplateLen is the limit of the Template command (a classic ESP32 template is 250-300 bytes).
	// Tasmota 16.0.0 takes less than MQTT_MAX_PACKET_SIZE (1200) bytes: HandleHttpCommand
	// (xdrv_01_9_webserver.ino) for /cm, MqttDataHandler (xdrv_02_9_mqtt.ino, data_len) for MQTT, where
	// topic and header share the 1200-byte client buffer. 512 leaves that room and still fits the
	// longest template of any family (55 pins).
	maxTemplateLen = 512
	maxNameLen     = 40
)

// DefaultOperator are example commands at operator level, for display and
// for allowlists. Classify stays the authority: a test keeps this list true.
var DefaultOperator = []string{
	"Power", "Power1 TOGGLE", "Power1 ON", "Power1 OFF",
	"Status 1", "Status 2", "Status 3", "Status 4", "Status 5", "Status 6",
	"Status 7", "Status 8", "Status 9", "Status 10", "Status 11",
	"State", "Time", "Uptime", "Restart 1",
}

// secretNames carry credentials, keys or network names in their arguments. Admins
// may send them after a confirmation (confirmFor); they are part of the one secret rule
// (SecretName), so their arguments are never logged and their replies are masked
// (ScrubReply). MI32Key and MI32Keys set
// BLE sensor bind keys, ZbConfig the Zigbee network key (its reply shows the key, also
// when sent bare), WiFiTest tries and may store a Wi-Fi name and password ("ssid+pw").
// IfxOrg runs IfxUser's handler (CmndInfluxDbUser) and writes the same setting.
// This list decides only the confirmation: a name that SecretName catches by a part
// (MqttUser, TmChatId, WCAuth, DzKeyIdx, ...) is Redact without one, on purpose. Do not
// merge the two lists: the confirmation names a lock-out that those names do not cause.
var secretNames = map[string]bool{
	"password": true, "webpassword": true, "mqttpassword": true, "ssid": true,
	"tmtoken": true, "ifxpassword": true, "ifxtoken": true, "ifxuser": true, "ifxorg": true,
	"tlskey": true, "lorawanappkey": true, "rgxpassword": true, "rgxssid": true,
	"mi32key": true, "mi32keys": true, "zbconfig": true, "wifitest": true,
}

// Consequences shown on the confirmation step.
const (
	whyReset    = "Reset erases the device configuration; the device has to be set up again and may drop off the network."
	whyWifiCfg  = "WifiConfig puts the device into its Wi-Fi setup mode; it leaves the network until it is configured again."
	whyFirmware = "This starts or redirects a firmware update on the device without tasmofleet's firmware safety checks."
	whyWifiOff  = "Switching Wi-Fi off cuts the device off from tasmofleet."
	whyWebSrv   = "Any WebServer mode but 2 stops the device from accepting commands over HTTP, so tasmofleet can no longer reach it."
	whyCutOff   = "Changing the address or network settings can leave the device unreachable at the address tasmofleet uses."
	whyModule   = "Changing the module or template changes the pin mapping; the device may restart or stop working until it is corrected."
	whyCredCmd  = "Changing a login or the Wi-Fi name can lock tasmofleet out or take the device off the network; tasmofleet does not store the new value."
	whyBindKey  = "This sets or removes a BLE sensor's bind key; with a wrong key the device can no longer read that sensor. tasmofleet does not store the new value."
	whyZigbee   = "A changed Zigbee network setting saves and restarts the device, and a new network key or channel can drop the paired Zigbee devices; tasmofleet does not store the new value."
	whyRestart  = "Restart other than 1 halts the device or erases settings; it may stay down until power is cycled."
	whyMQTTOnly = "this device is reached over MQTT only; this change can cut TasmoFleet off from it"
	whyFile     = "TasmoFleet's own code handles file transfers; the command carries the device's MQTT password."
	whyStatus0  = "Status 0 makes the device build one very large reply; ESP8266 devices can run out of memory. tasmofleet's own code never sends it."
)

// queryNames are read-only when sent without arguments (Tasmota answers with
// the current value). With arguments they change settings: admin. Their
// arguments are harmless to log (see Redacted).
var queryNames = map[string]bool{
	"time": true, "uptime": true, "module": true, "modules": true, "template": true,
	"gpio": true, "gpios": true, "hostname": true, "topic": true, "fulltopic": true,
	"fallbacktopic": true, "teleperiod": true, "setoption": true, "poweronstate": true,
	"webserver": true, "weblog": true, "seriallog": true,
	"friendlyname": true, "devicename": true, "timezone": true, "timedst": true,
	"timestd": true, "ntpserver": true, "mqtthost": true, "mqttport": true,
	"mqttclient": true, "state": true, "sleep": true, "latitude": true,
	"longitude": true, "tempoffset": true, "ledstate": true,
}

// reportsWhenBare are admin-only for the role gate (not queryNames: an operator may not send them and their
// arguments stay out of logs, see P5 and L1) but change nothing when sent without arguments, so the transport may
// route the bare form like a read (#702, #711): MQTT first, and over HTTP a second time when MQTT gets no answer.
// That second send is why only names checked in the Tasmota source belong here, never a rule by pattern: a bare
// command has data_len 0 and payload -99 (CommandHandler, support_command.ino, 16.0.0 lines 458-466, 12.5.0 lines
// 422-430), and each handler below changes something only for data or a payload in range, and otherwise only
// answers. maxIndex is the highest index whose reply carries that index, so the MQTT channel matches it (keyNames
// in mqttcmd); 0 means only the plain name, because the reply comes under the plain key ("OtaUrl" for OtaUrl2).
var reportsWhenBare = map[string]struct{ maxIndex int }{
	// CmndMqttUser (xdrv_02_9_mqtt.ino, 12.5.0 and 16.0.0): stores a value and restarts only when data_len > 0.
	"mqttuser": {},
	// CmndOtaUrl (support_command.ino, 16.0.0 lines 1358-1364, 12.5.0 lines 1100-1106): stores only when
	// data_len > 0, answers {"OtaUrl":"..."}. Status 1 (StatusPRM) carries the same value and is read over MQTT.
	"otaurl": {},
	// CmndPowerRetain (xdrv_02_9_mqtt.ino, 16.0.0 lines 2146-2163, 12.5.0 lines 1643-1660): clears the retained
	// relay states and stores only for payload 0 or 1, answers {"PowerRetain":"OFF"}.
	"powerretain": {},
	// CmndLoghost (support_command.ino, 16.0.0 lines 2388-2394, 12.5.0 lines 1977-1983): stores only when
	// data_len > 0, answers {"LogHost":"..."}.
	"loghost": {},
	// CmndSwitchMode (support_command.ino, 16.0.0 lines 2612-2620, 12.5.0 lines 2162-2170): index 1 to
	// MAX_SWITCHES_SET stores only for a payload in range and answers with its index, {"SwitchMode1":0}; a bare
	// SwitchMode is index 1. MAX_SWITCHES_SET is 28 in 12.5.0 (a higher index answers nothing there) and 32 in
	// 16.0.0, so only 1 to 28 are routed as reads. SwitchMode0 (every switch, answered as an array) stays a write.
	"switchmode": {maxIndex: 28},
	// CmndGroupTopic (xdrv_02_9_mqtt.ino, 16.0.0 lines 2029-2079, 12.5.0 lines 1526-1576): index 1 to MAX_GROUP_TOPICS
	// (4, tasmota.h 153) stores and restarts only when data_len > 0, and always answers all four with
	// ResponseCmndAll, {"GroupTopic1":"tasmotas","GroupTopic2":"",...} (support_command.ino 326-335), so every index
	// carries its own key; a bare GroupTopic is index 1. The GroupTopic reader and the probe read all four this way
	// (#1005); index 0 and above 4 answer nothing and stay writes.
	"grouptopic": {maxIndex: 4},
}

// ReportsOnly reports whether cmd only makes the device answer and changes nothing: a read-class command, or a
// bare name of reportsWhenBare, numbered only where the reply carries the number (written as Tasmota writes it
// back: SwitchMode1, not SwitchMode01). It decides how a command is routed (read or write), never who may send
// it: that is Classify and Authorize. It reads the command exactly like Classify, so callers never parse commands
// themselves (P7).
func ReportsOnly(cmd string) bool {
	p, bad := parse(cmd)
	if bad != "" {
		return false
	}
	if r, ok := reportsWhenBare[p.base]; ok && p.args == "" && (!p.has || p.idx >= 1 && p.idx <= r.maxIndex && p.plainIndex()) {
		return true
	}
	v, bad := classify(p)
	return bad == "" && v.Class == ClassRead
}

// loggedArgs are the commands whose arguments may appear in logs and history
// besides the operator-level ones: the same settings, plus SaveData (which is
// not a pure query: it writes flash, so it is admin even without arguments).
var loggedArgs = func() map[string]bool {
	m := map[string]bool{"savedata": true}
	for k := range queryNames {
		m[k] = true
	}
	// Hosts are where users paste "user:pass@host".
	delete(m, "mqtthost")
	delete(m, "ntpserver")
	return m
}()

// semicolonLookalikes would be read as a command separator by a human or by
// a normalising intermediary even though the device ignores them.
var semicolonLookalikes = map[rune]bool{
	';': true, ';': true, '؛': true, '⁏': true, '⸵': true,
	'︔': true, '﹔': true, '；': true,
}

func invalid(why string) (Verdict, error) {
	return Verdict{Class: ClassForbidden, Name: "?", Reason: "malformed command: " + why, Redact: true},
		fmt.Errorf("%w: %s", ErrInvalid, why)
}

// parsed is one split command line.
type parsed struct {
	name string // as typed, e.g. "Power1"
	base string // as the device runs it (ResolveName): lower case without the index, e.g. "power", "setoption"
	idx  int
	has  bool   // an index was typed, or a SetOption synonym implies one
	args string // trimmed
}

func parse(cmd string) (parsed, string) {
	if !utf8.ValidString(cmd) {
		return parsed{}, "not valid UTF-8"
	}
	cmd = strings.Trim(cmd, " ")
	if cmd == "" {
		return parsed{}, "empty"
	}
	if len(cmd) > MaxCommandLen && (len(cmd) > maxTemplateLen || !isTemplateCommand(cmd)) {
		return parsed{}, "too long"
	}
	// Backlog is the one command whose arguments legitimately hold ';'. Each
	// part is classified on its own (classifyBacklog), so nothing hides in it.
	head, _, _ := strings.Cut(cmd, " ")
	backlog := strings.EqualFold(strings.TrimRight(head, "0123456789"), "backlog")
	if why := unsafeText(cmd, backlog); why != "" {
		return parsed{}, why
	}
	name, args, _ := strings.Cut(cmd, " ")
	if len(name) > maxNameLen || !validName(name) {
		return parsed{}, "bad command name"
	}
	n, ok := ResolveName(name) // as the device runs it: SO19 is SetOption19, MqttTLS is SetOption103
	if !ok {
		return parsed{}, "bad command index"
	}
	return parsed{name: name, base: n.Base, idx: n.Index, has: n.Indexed, args: strings.TrimSpace(args)}, ""
}

// Name is a command name as the device runs it.
type Name struct {
	Base    string // lower case, without the index: "power", "setoption", "mqtthost"
	Index   int    // the index the device's handler gets; 0 when none was typed or implied
	Indexed bool   // an index was typed ("Power1", "SetOption0103") or is implied by a synonym (MqttTLS is SetOption103)
}

// ResolveName reads a command name (no arguments) the way Tasmota does (CommandHandler, support_command.ino 16.0.0
// lines 410-425; DecodeCommand, support.ino lines 1166-1192): any letter case; the digits at its end are the index,
// read with atoi (SetOption0103 is SetOption103); and a SetOption synonym of a command table runs the SetOption's
// handler with the SetOption's number, whatever index was typed (MqttTLS, MqttTLS1 and MQTTTLS7 are SetOption103;
// SO19 is SetOption19). The synonyms come from the pinned Tasmota catalogues (internal/tasmotacmd). It is the one
// reading of a name: Classify, CutsMQTTOff and CutOffName use it, so a rule keyed on the SetOption also holds for
// every spelling the device runs as that SetOption. ok is false for a name the policy does not accept (letters, then
// digits, at most 40 characters, an index that fits an int).
func ResolveName(name string) (Name, bool) {
	if len(name) > maxNameLen || !validName(name) {
		return Name{}, false
	}
	base := strings.ToLower(name)
	end := len(strings.TrimRight(base, "0123456789"))
	n := Name{Base: base[:end]}
	if end < len(base) {
		idx, err := strconv.Atoi(base[end:])
		if err != nil {
			return Name{}, false
		}
		n.Index, n.Indexed = idx, true
	}
	if s, ok := setOptionNames()[n.Base]; ok {
		n.Base = "setoption"
		if s != takesIndex { // a synonym of one SetOption: the device ignores the typed index
			n.Index, n.Indexed = s, true
		}
	}
	return n, true
}

// takesIndex marks the names that take the SetOption's number as their index (SetOption, SO).
const takesIndex = -1

// setOptionNames maps the lower-case names the device runs as SetOption to the SetOption's number, or takesIndex.
var setOptionNames = sync.OnceValue(func() map[string]int { return setOptionTable(tasmotacmd.Pinned()) })

// setOptionTable is setOptionNames from the catalogues set. It panics on a damaged embedded catalogue (tasmotacmd's
// tests catch that): a catalogue that cannot be read must never read as "no synonyms".
func setOptionTable(set *tasmotacmd.Set, err error) map[string]int {
	if err != nil {
		panic("cmdpolicy: the embedded Tasmota catalogues: " + err.Error())
	}
	out := map[string]int{}
	for _, v := range set.Versions() {
		cat, _, _ := set.For(v)
		for _, c := range cat.Commands {
			switch {
			case c.Handler == "CmndSetoption":
				out[strings.ToLower(c.Command)] = takesIndex
			case strings.HasPrefix(c.SynonymOf, "SetOption"):
				s, err := strconv.Atoi(strings.TrimPrefix(c.SynonymOf, "SetOption"))
				if err != nil {
					panic("cmdpolicy: catalogue " + v + ": " + c.Command + " is a synonym of " + c.SynonymOf)
				}
				out[strings.ToLower(c.Command)] = s
			}
		}
	}
	return out
}

// unsafeText says why s may not be part of a command line, or "". Only a space is allowed as white space;
// ';' is allowed when the line is a Backlog (each part is classified on its own).
func unsafeText(s string, backlog bool) string {
	for _, r := range s {
		switch {
		case r == ' ':
		case r == ';' && backlog:
		case semicolonLookalikes[r]:
			return "contains a command separator"
		case !unicode.IsPrint(r):
			return "contains a control or invisible character"
		}
	}
	if strings.Contains(strings.ToLower(s), "%3b") {
		return "contains an encoded command separator"
	}
	return ""
}

// UnsafeText says why s may not be part of a command that code builds from text it did not type itself
// (a template name read from a device), or "" when it may. It is the check Classify applies to every
// character of a command line, so a builder that uses it cannot be more permissive than the console.
func UnsafeText(s string) string {
	if !utf8.ValidString(s) {
		return "not valid UTF-8"
	}
	return unsafeText(s, false)
}

func validName(s string) bool {
	if s == "" || !isLetter(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isLetter(s[i]) && (s[i] < '0' || s[i] > '9') {
			return false
		}
	}
	return true
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

// Classify parses one command line and says what it needs. A malformed line
// returns ErrInvalid and a Forbidden verdict, so a caller that ignores the
// error still cannot send it. Everything else is sendable by an admin; the
// verdict's Confirm names the consequence of a destructive or lock-out
// command. Authorize adds the role check.
func Classify(cmd string) (Verdict, error) {
	p, bad := parse(cmd)
	if bad != "" {
		return invalid(bad)
	}
	v, bad := classify(p)
	if bad != "" {
		return invalid(bad)
	}
	return v, nil
}

func classify(p parsed) (Verdict, string) {
	v := Verdict{Name: p.name, Redact: SecretName(p.name)}
	switch p.base {
	case "status":
		return classifyStatus(p, v), ""
	case "restart":
		if p.args == "1" && !p.has {
			v.Class = ClassOperator
			return v, ""
		}
		v.Class, v.Reason, v.Confirm = ClassAdmin, "changes device state", whyRestart
		return v, ""
	case "power":
		return classifyPower(p, v), ""
	case "filedownload", "fileupload", "upload":
		// Never typed by a person, whatever the role: the payload carries the device's MQTT password
		// and the transfer is a protocol of its own (internal/mqttcmd). Redact: the arguments may
		// hold that password.
		v.Class, v.Reason, v.Redact = ClassForbidden, whyFile, true
		return v, ""
	case "backlog":
		return classifyBacklog(p, v)
	}
	v.Class, v.Reason = ClassAdmin, "changes device state"
	v.Confirm = confirmFor(p)
	if v.Confirm == "" && p.args == "" && queryNames[p.base] {
		v.Class, v.Reason = ClassRead, ""
	}
	return v, ""
}

// StartsFirmware reports whether cmd starts or redirects a firmware update on the device: Upgrade or
// OtaUrl with an argument, alone or inside a Backlog. It is the one place that decides this, so the
// console, the bulk preview and the executing service cannot disagree. Upload with an argument is
// not typed at all (ClassForbidden), so it never reaches a device and needs no lock rule.
// Why it matters: a second update on a device that TasmoFleet is already updating can brick it.
func StartsFirmware(cmd string) bool {
	v, err := Classify(cmd)
	return err == nil && strings.Contains(v.Confirm, whyFirmware)
}

// RunsStatus0 reports whether the device would run a Status 0 for cmd: Status 0 in any spelling Tasmota reads as 0
// (00, off, an unclear index), alone or inside a Backlog. It reads the command exactly like Classify, so the Status 0
// rule stays in one place (classifyStatus). The MQTT channel never publishes such a command (safety rule 2); the
// console uses this to refuse it up front where HTTP is not known to work (#1001).
func RunsStatus0(cmd string) bool {
	v, err := Classify(cmd)
	return err == nil && strings.Contains(v.Confirm, whyStatus0)
}

// StartsFirmwareUpgrade is StartsFirmware restricted to the Upgrade family: the command makes the device
// flash and restart (Upgrade with an argument, alone or inside a Backlog). A bare OtaUrl only stores the
// address and is not one. It reads the command exactly like Classify (same parse, same Backlog split, same
// case and index handling), so callers never parse commands themselves.
func StartsFirmwareUpgrade(cmd string) bool {
	if !StartsFirmware(cmd) {
		return false
	}
	p, bad := parse(cmd)
	return bad == "" && upgradeIn(p)
}

// RestartsDevice reports whether the command makes the device restart: Restart 1 (saves) or Restart 99
// (does not), Upgrade or Reset with an argument, alone or inside a Backlog. Restart 2 halts the device and
// no restart follows. It reads the command exactly like Classify, so callers never parse commands
// themselves. TasmoFleet uses it to know that a restart it sees was its own doing (#325).
func RestartsDevice(cmd string) bool {
	p, bad := parse(cmd)
	return bad == "" && restartsIn(p)
}

// SetsGroupTopic reports whether cmd sets a group topic of the device (#1005): GroupTopic with any index and an
// argument, alone or inside a Backlog (every part counts, not only the first). Tasmota stores only when an argument
// came (CmndGroupTopic, data_len > 0) and then moves the set ones to the front, so any of GroupTopic1 to GroupTopic4
// may change; a bare GroupTopic only reads. It reads the command exactly like Classify, so callers never parse
// commands themselves.
func SetsGroupTopic(cmd string) bool {
	p, bad := parse(cmd)
	return bad == "" && setsGroupTopicIn(p)
}

func setsGroupTopicIn(p parsed) bool {
	switch p.base {
	case "grouptopic":
		return p.args != ""
	case "backlog":
		for _, part := range strings.Split(p.args, ";") {
			if sub, bad := parse(stripBacklogPrefix(part)); bad == "" && setsGroupTopicIn(sub) {
				return true
			}
		}
	}
	return false
}

func restartsIn(p parsed) bool {
	switch p.base {
	case "restart":
		return p.args == "1" || p.args == "99"
	case "upgrade", "reset":
		return p.args != ""
	case "backlog":
		for _, part := range strings.Split(p.args, ";") {
			part = stripBacklogPrefix(part)
			if part == "" {
				continue
			}
			if sub, bad := parse(part); bad == "" && restartsIn(sub) {
				return true
			}
		}
	}
	return false
}

// upgradeIn reports whether p, or a part of a Backlog p, is an Upgrade with an argument.
func upgradeIn(p parsed) bool {
	switch p.base {
	case "upgrade":
		return p.args != "" // as in confirmFor
	case "backlog":
		for _, part := range strings.Split(p.args, ";") {
			part = stripBacklogPrefix(part)
			if part == "" {
				continue
			}
			if sub, bad := parse(part); bad == "" && upgradeIn(sub) {
				return true
			}
		}
	}
	return false
}

// confirmFor names the consequence of the destructive or lock-out commands,
// or returns "" for everything else.
func confirmFor(p parsed) string {
	switch p.base {
	case "reset":
		return whyReset
	case "wificonfig":
		return whyWifiCfg
	case "upgrade", "otaurl":
		if p.args != "" {
			return whyFirmware
		}
	case "wifi":
		// Tasmota reads "0", "off", "00", "+0" and more as off.
		if p.args != "" && unclearOrOff(p.args) {
			return whyWifiOff
		}
	case "webserver":
		if p.args != "" && p.args != "2" {
			return whyWebSrv
		}
	case "ipaddress", "ethernet", "ap":
		if p.args != "" {
			return whyCutOff
		}
	case "module", "template":
		if p.args != "" {
			return whyModule
		}
	case "mi32key", "mi32keys": // secret keys, but no login: their own consequence
		if p.args != "" {
			return whyBindKey
		}
	case "zbconfig":
		if p.args != "" {
			return whyZigbee
		}
	default:
		if secretNames[p.base] && p.args != "" {
			return whyCredCmd
		}
	}
	return ""
}

// classifyBacklog judges every part of a Backlog on its own, so a Reset or a
// password inside it asks for the same confirmation and is redacted the same
// way as when sent alone.
func classifyBacklog(p parsed, v Verdict) (Verdict, string) {
	v.Class, v.Reason = ClassAdmin, "changes device state"
	seen := map[string]bool{}
	for _, part := range strings.Split(p.args, ";") {
		// Tasmota's CmndBacklog (support_command.ino 15.1.0 lines 534-542) joins a piece with the next one while
		// their '{' and '}' counts differ, so a ';' inside a Template's JSON hides its CMND from a split on every
		// ';'. Refused: when every piece is balanced the device joins nothing, so each Backlog accepted here splits
		// the same way in TasmoFleet (Redacted, ShownInFull, the MQTT channel, the router) and on the device.
		if strings.Count(part, "{") != strings.Count(part, "}") {
			return v, "a Backlog part with unbalanced braces"
		}
		part = stripBacklogPrefix(part)
		if part == "" {
			continue
		}
		sub, bad := parse(part)
		if bad != "" {
			return v, bad
		}
		sv, bad := classify(sub)
		if bad != "" {
			return v, bad
		}
		if sv.Class == ClassForbidden { // a part that is never sent makes the whole Backlog never sent
			v.Class, v.Reason, v.Redact = ClassForbidden, sv.Reason, true
			return v, ""
		}
		if sv.Confirm != "" && !seen[sv.Confirm] {
			seen[sv.Confirm] = true
			if v.Confirm != "" {
				v.Confirm += " "
			}
			v.Confirm += sv.Confirm
		}
		v.Redact = v.Redact || sv.Redact
	}
	return v, ""
}

// stripBacklogPrefix removes the leading "Backlog" words of one Backlog part the way Tasmota's
// CmndBacklog does (support_command.ino): trim, strip "backlog" case-insensitively with or
// without a space after it, repeat. "BacklogStatus 0" and "BACKLOGBacklogStatus 0" are Status 0.
func stripBacklogPrefix(part string) string {
	for {
		part = strings.TrimSpace(part)
		if len(part) < len("backlog") || !strings.EqualFold(part[:len("backlog")], "backlog") {
			return part
		}
		part = part[len("backlog"):]
	}
}

// unclearOrOff reports whether arg is anything but a plain positive number or
// ON/TRUE. Tasmota reads "0", "off", "false", "00", "+0", "0x0" and more as
// off, so only the clearly-on spellings pass.
func unclearOrOff(arg string) bool {
	switch strings.ToLower(arg) {
	case "", "on", "true":
		return false
	}
	n, ok := canonicalNumber(arg)
	return !ok || n == 0
}

func classifyStatus(p parsed, v Verdict) Verdict {
	arg := p.args
	unclear := false
	switch {
	case p.has && arg != "":
		unclear = true // "Status1 2": which one does the device take?
	case p.has:
		arg = strconv.Itoa(p.idx)
		if p.idx == 0 {
			arg = "0"
		}
	}
	if arg == "" {
		v.Class = ClassRead // bare Status is the abbreviated object only
		return v
	}
	n, ok := canonicalNumber(arg)
	switch {
	case unclear || !ok || n == 0:
		// Tasmota's atoi reads "00", "+0", "0x0" and "off" as 0 as well, so
		// anything that is not a plain Status N is treated as Status 0.
		v.Class, v.Reason, v.Confirm = ClassAdmin, "changes device state", whyStatus0
	case n <= 11:
		v.Class = ClassRead
	default:
		v.Class, v.Reason = ClassAdmin, "only Status 1 to 11 are operator queries"
	}
	return v
}

// canonicalNumber accepts 0 and plain decimals without sign, zeros or other
// notation, so that what we classify is what Tasmota's atoi would read.
func canonicalNumber(s string) (int, bool) {
	if s == "0" {
		return 0, true
	}
	if s == "" || s[0] < '1' || s[0] > '9' || len(s) > 3 {
		return 0, false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func classifyPower(p parsed, v Verdict) Verdict {
	const maxRelay = 8
	// Power0 means "all relays", including ones above 8: not an operator action.
	if p.has && (p.idx < 1 || p.idx > maxRelay) {
		v.Class, v.Reason = ClassAdmin, "only relays 1 to 8 are operator actions"
		return v
	}
	switch strings.ToLower(p.args) {
	case "":
		v.Class = ClassRead
	case "on", "off", "toggle", "0", "1", "2":
		v.Class = ClassOperator
	default:
		v.Class, v.Reason = ClassAdmin, "only ON, OFF and TOGGLE are operator actions"
	}
	return v
}

// Authorize classifies cmd and checks it against role. It returns the verdict
// and nil when the role may send it; otherwise ErrInvalid, ErrForbidden or
// ErrRole (wrapped, with a safe reason).
func Authorize(cmd string, role model.Role) (Verdict, error) {
	v, err := Classify(cmd)
	if err != nil {
		return v, err
	}
	min, ok := v.Class.MinRole()
	if !ok {
		return v, fmt.Errorf("%w: %s", ErrForbidden, v.Reason)
	}
	if role < min || role > model.RoleAdmin {
		return v, fmt.Errorf("%w: %s needs the %s role", ErrRole, v.Name, roleLabel(min))
	}
	return v, nil
}

func roleLabel(r model.Role) string {
	if r == model.RoleAdmin {
		return "admin"
	}
	return "operator"
}

// Redacted is the only form of cmd that may be written to logs, the audit
// log and the console history. It is an allowlist: arguments are shown only
// for operator-level commands and the harmless settings in loggedArgs (with
// credential patterns scrubbed); every other command, refused ones included,
// shows as "Name ***", and a malformed line as RejectedText. A secret wrapped
// in another command (Backlog, Json, Rule, Publish, ...) therefore never
// reaches a log.
func Redacted(cmd string) string {
	p, bad := parse(cmd)
	if bad != "" {
		return RejectedText
	}
	if p.args == "" {
		return p.name
	}
	// An allowlist: arguments are shown only where they are known to be
	// harmless. Everything else, refused commands included, may hold a secret
	// (directly or wrapped in another command) and shows as "Name ***".
	v, bad := classify(p)
	if bad != "" {
		return RejectedText
	}
	if p.base == "backlog" {
		// Each part is redacted on its own, so a log shows what was chained
		// without the secrets inside.
		var parts []string
		for _, part := range strings.Split(p.args, ";") {
			if part = strings.TrimSpace(part); part != "" {
				parts = append(parts, Redacted(part))
			}
		}
		return p.name + " " + strings.Join(parts, "; ")
	}
	if c := v.Class; c != ClassRead && c != ClassOperator && !loggedArgs[p.base] {
		return p.name + " ***"
	}
	if TemplateRunsCommands(cmd) { // it can carry any command and its secret (WebPassword ...)
		return p.name + " ***"
	}
	p.args = scrubText(p.args)
	return p.canonical()
}

// TemplateRunsCommands reports a Template (one command, not a Backlog) whose arguments hold "cmnd" in any letter
// case: a CMND key the device runs as a Backlog when it applies the template (support.ino 15.1.0 1935-1945).
// Tasmota's JSON parser compares keys without case and does not unescape them, so the test on the text catches
// every spelling of the key, and fails closed for "cmnd" anywhere else. It is the one rule for such a Template:
// Redacted masks it, and the MQTT channel refuses it as a command that runs other commands.
func TemplateRunsCommands(cmd string) bool {
	p, bad := parse(cmd)
	return bad == "" && p.base == "template" && strings.Contains(strings.ToLower(p.args), "cmnd")
}

// ShownInFull reports whether Redacted shows cmd exactly as it is sent: it parses, nothing is hidden or masked,
// and a Backlog shows every part unchanged (judged part by part, so "Backlog A;B" counts as shown). It is the
// fail-closed rule for "may hold a secret" of what a person types: the console history and bulk's input echo
// keep only such commands. Since Redacted is an allowlist, a command it does not know counts as secret. It
// decides no transport: a secret is no reason for HTTP (#605).
func ShownInFull(cmd string) bool {
	p, bad := parse(cmd)
	if bad != "" {
		return false
	}
	if p.base != "backlog" || p.args == "" {
		c := p.canonical()
		return Redacted(cmd) == c && !strings.Contains(c, mask)
	}
	if _, bad := classify(p); bad != "" {
		return false
	}
	for _, part := range strings.Split(p.args, ";") {
		if part = strings.TrimSpace(part); part != "" && !ShownInFull(part) {
			return false
		}
	}
	return true
}

// Canonical is the exact text that is classified and, if allowed, sent: the
// command name, one blank, the arguments, without surrounding blanks.
// Classify(Canonical(x)) always equals Classify(x), so there is no gap between
// what was judged and what reaches the device. Callers send Canonical's
// result, never their original string.
func Canonical(cmd string) (string, error) {
	p, bad := parse(cmd)
	if bad != "" {
		return "", fmt.Errorf("%w: %s", ErrInvalid, bad)
	}
	return p.canonical(), nil
}

// plainIndex reports whether the index is written the way Tasmota writes it in a reply: no leading zero.
func (p parsed) plainIndex() bool {
	return p.name[len(strings.TrimRight(p.name, "0123456789")):] == strconv.Itoa(p.idx)
}

func (p parsed) canonical() string {
	if p.args == "" {
		return p.name
	}
	return p.name + " " + p.args
}

// PrepareOpts carries what the policy needs to know about the device.
type PrepareOpts struct {
	// MQTTOnly: tasmofleet reaches the device over MQTT only (HTTP fails). A change of its MQTT
	// identity or connection can then cut it off, which an HTTP-reachable device would survive.
	MQTTOnly bool
}

// cutsMQTTOff reports whether p changes how the device talks MQTT: its topics, prefixes, host, port,
// login, client name, TLS fingerprint or client key (Prefix<n>, MqttFingerprint, TLSKey<n>, SetOption103 TLS, SetOption132
// fingerprint check), or switches MQTT off (SetOption3 0). Queries (no argument) do not.
func cutsMQTTOff(p parsed) bool {
	if p.args == "" {
		return false
	}
	switch p.base {
	case "topic", "fulltopic", "grouptopic", "mqtthost", "mqttport", "mqttuser", "mqttpassword", "mqttclient",
		"prefix", "mqttfingerprint", "tlskey":
		return true
	case "setoption":
		switch {
		case !p.has:
			return false
		case p.idx == 3:
			return unclearOrOff(p.args)
		case p.idx == 103 || p.idx == 132:
			return true
		}
	}
	return false
}

// CutOffName returns the name of the first command in cmd (a Backlog is judged part by part) that can
// cut a device off that tasmofleet reaches over MQTT only, "" when none can.
func CutOffName(cmd string) string {
	p, bad := parse(cmd)
	if bad != "" {
		return ""
	}
	if p.base != "backlog" {
		if cutsMQTTOff(p) {
			return p.name
		}
		return ""
	}
	for _, part := range strings.Split(p.args, ";") {
		if sub, bad := parse(stripBacklogPrefix(part)); bad == "" && cutsMQTTOff(sub) {
			return sub.name
		}
	}
	return ""
}

// CutsMQTTOff reports whether cmd can cut an MQTT-only device off (see CutOffName).
func CutsMQTTOff(cmd string) bool { return CutOffName(cmd) != "" }

// WithMQTTOnly adds the consequence of cutting an MQTT-only device off to the verdict of cmd. It never
// lowers the class and never removes a confirmation.
func WithMQTTOnly(cmd string, v Verdict) Verdict {
	if !CutsMQTTOff(cmd) || v.Class == ClassForbidden || strings.Contains(v.Confirm, whyMQTTOnly) {
		return v
	}
	if v.Confirm != "" {
		v.Confirm += " "
	}
	v.Confirm += whyMQTTOnly
	return v
}

// Prepare authorizes cmd for role and returns the canonical text to send. It
// is the single entry point for code that sends free-form commands.
func Prepare(cmd string, role model.Role, opts PrepareOpts) (send string, v Verdict, err error) {
	if v, err = Authorize(cmd, role); err != nil {
		return "", v, err
	}
	if opts.MQTTOnly {
		v = WithMQTTOnly(cmd, v)
	}
	send, err = Canonical(cmd)
	if err != nil {
		return "", v, err
	}
	return send, v, nil
}

// isTemplateCommand reports whether cmd is the Template command by name (any case, no index), which has
// its own length limit.
func isTemplateCommand(cmd string) bool {
	head, _, _ := strings.Cut(strings.Trim(cmd, " "), " ")
	return strings.EqualFold(head, "template")
}
