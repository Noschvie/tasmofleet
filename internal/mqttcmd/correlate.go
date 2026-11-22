package mqttcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// otherKeys lists the commands whose RESULT does not carry the command's name as a key. Checked
// against Tasmota v16 (support_command.ino): Template answers {"NAME":..,"GPIO":..,"FLAG":..,
// "BASE":..} (CmndTemplate -> TemplateJson), which a bare Template read matches by shape (templateShape,
// E3 of #327); a Template write answers the same shape, or {"Template":"Invalid JSON or Bad Chip Type"} when refused; Gpios publishes its lines itself (ShowGpios) and
// clears the response. Module, Modules and Gpio do carry their name (S_JSON_COMMAND_NVALUE_SVALUE,
// "Modules", "GPIOn"/"GPIO"), so they are matched like any other command. Only DoLoose, the
// console, takes the first RESULT for these; an automated read needs a key match.
var otherKeys = map[string]bool{"template": true, "gpios": true}

// fileCommands carry the device's MQTT password in their payload and answer on their own topics.
var fileCommands = map[string]bool{"filedownload": true, "fileupload": true, "upload": true}

// executors are commands that run other commands, or publish messages that do: Json (CmndJson ->
// Backlog0, so {"Status":0} is a Status 0), Publish and Publish2 (to the device's own cmnd topic),
// Rule and RuleTimer (DO Status 0), If (runs its command block; Tasmota builds it with rules alone),
// WebSend and WebQuery (to any host, a device included), DevGroupSend (128=<command> runs on the group,
// this device included) and LoRaCommand (on another device), Br (Berry), Script and UfsRun (runs a
// script file). Status 0 is never sent, so a command that can send it on our behalf is
// never published either, alone or inside a Backlog. The console and bulk keep them for devices
// reached over HTTP (cmdpolicy asks an admin to confirm there); over MQTT there is no such path.
var executors = map[string]bool{
	"json": true, "publish": true, "publish2": true, "rule": true, "ruletimer": true,
	"websend": true, "webquery": true, "br": true, "script": true, "ufsrun": true,
	"if": true, "devgroupsend": true, "loracommand": true,
}

// ruleSwitch reports a Rule command that only reads or switches a rule ("Rule1", "Rule1 1",
// "Rule2 off"): no rule text is stored, so nothing new can run.
func ruleSwitch(r *request) bool {
	if r.base != "rule" {
		return false
	}
	switch strings.ToLower(r.args) {
	case "", "0", "1", "2", "4", "5", "on", "off", "toggle", "stop", "once", "stoponce":
		return true
	}
	return false
}

// ReplyKind is how the answer to a command is recognised (Reply).
type ReplyKind int

const (
	// Matched: the answer names the command (a key of its name, STATUSn for Status n, or Command Unknown/Error for
	// it) and the channel takes it.
	Matched ReplyKind = iota
	// Unmatchable: a read whose answer the channel cannot match. State answers without a State key (CmndState,
	// MqttShowState), a bare Gpio under its own key only when GPIO1 is set, Gpios and Modules in several messages
	// (Tasmota v16.0.0 support_command.ino 1220-1232, 2012-2131, 2154-2180). They go HTTP first
	// (transport.MQTTCannotMatch), or they would time out and wrongly mark the device.
	Unmatchable
	// LightState: a light write whose only answer is the whole light state (ResponseLightState), the message every
	// light change publishes: another client's Dimmer, a rotary encoder, the end of a Wakeup. That message proves
	// nothing about the write, and for Channel2, Color2, HSBColor2/3 or Dimmer2/3 it does not even carry the
	// command's key. The channel confirms such a write with ConfirmRead, published right after it (#813).
	LightState
)

// ConfirmRead is the read that confirms a LightState write: its STATUS11 carries the light state (MqttShowState calls
// ResponseLightState, Tasmota v16.0.0 support_tasmota.ino 887-888) and nothing but a Status 11 publishes it. The
// channel returns the object inside StatusSTS, so the reply has the light keys at the top as the HTTP reply has. A
// read-back after a write (#947) can use the same pair: Reply to know which writes need it, ConfirmRead to ask.
const ConfirmRead = "Status 11"

// Reply is the one rule for how the answer to cmd is recognised, for reads and writes alike (#813). Checked against
// the pinned Tasmota sources 12.5.0 to 16.0.0 (docs/research/tasmota-mqtt-transport.md section 8).
func Reply(cmd string) ReplyKind {
	name, args, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	args = strings.TrimSpace(args)
	if args == "?" {
		// Tasmota reads "?" as no argument (CommandHandler, support_command.ino 460): "Gpio ?" is a bare Gpio, "Dimmer ?"
		// a read.
		args = ""
	}
	switch strings.ToLower(name) {
	case "state", "gpios", "modules":
		return Unmatchable
	case "gpio":
		if args == "" {
			return Unmatchable
		}
	}
	if args == "" || !lightWrite(name) {
		return Matched
	}
	return LightState
}

// lightWrite reports a light command that, given an argument, ends in LightPreparePower -> ResponseLightState(0) and
// publishes nothing of its own (v16.0.0 xdrv_04_light.ino: CmndSupportColor 2774 for Color, Color1 and Color2;
// CmndWhite 2800; CmndChannel 2850; CmndHsbColor 2885; CmndColorTemperature 2965 for CT; CmndDimmer 3068-3077, which
// takes an index above 4 as 1). An argument the handler rejects gets its own key (Dimmer 150 answers
// {"Dimmer":50}) or Command Error; the confirmation reads the state either way. Color3 to Color6 store a clock colour
// and answer under their own key (ResponseCmndIdxChar, 2771), Color0 and Color7 and up get Command Error.
func lightWrite(name string) bool {
	switch baseOf(name) {
	case "channel", "dimmer", "hsbcolor", "white", "ct":
		return true
	case "color":
		idx := name[len(strings.TrimRight(name, "0123456789")):]
		n, err := strconv.Atoi(idx)
		return idx == "" || (err == nil && n >= 1 && n <= 2)
	}
	return false
}

// request is one parsed command.
type request struct {
	name    string // as sent, "Power1"
	args    string
	base    string // lower case, no trailing digits, synonyms resolved
	status  string // expected suffix for Status: "STATUS" or "STATUS5"; "" for other commands
	redact  bool   // the reply may echo a credential
	backlog []*request
	// light: a LightState write. Only a Command Unknown/Error for it answers it on its own; success is confirmed
	// by confirm, a Status 11 sent after it (alone), or by the Status 11 appended to its Backlog.
	light    bool
	confirm  *request
	confirms bool // this Status 11 confirms light writes: its reply is the object inside StatusSTS
}

// confirmation is the Status 11 published to confirm r's light writes (req.confirm, or the part confirmLights appended
// to a Backlog), nil when r has none.
func (r *request) confirmation() *request {
	if r.confirm != nil {
		return r.confirm
	}
	if n := len(r.backlog); n > 0 && r.backlog[n-1].confirms {
		return r.backlog[n-1]
	}
	return nil
}

// writeResponse reports a message that can be a light write's own response: a RESULT, or with SetOption4 1 the
// command's stat topic (stat/<t>/DIMMER, or COMMAND for a failed one). Tasmota publishes it before it runs the next MQTT
// command, so the STATUS11 that confirms the write comes after it; one heard earlier was asked before the write ran. The
// waiter never gets a retained message (onMessage).
func (r *request) writeResponse(m mqttbus.Message) bool {
	if m.Kind == mqttbus.KindCommand {
		return strings.EqualFold(m.Suffix, r.typed()) || m.Suffix == "COMMAND"
	}
	return m.Kind == mqttbus.KindResult
}

// ownLightState reports the light state a LightState part of a Backlog publishes when it runs (ResponseLightState): a
// write response that is no Command Unknown/Error and carries a key of the part's command (Dimmer, Channel, under
// SetOption68 Channel<n>, Color, HSBColor, White, CT) or a POWER key. The part keeps it, so it never answers a later
// part whose key it carries as well (#813).
func (r *request) ownLightState(m mqttbus.Message) bool {
	if !r.light || !r.writeResponse(m) {
		return false
	}
	keys, values, _ := topLevel(m.Payload) // a payload that is no object has no keys
	if _, failed := values["Command"]; failed {
		return false
	}
	for _, k := range keys {
		if b := baseOf(k); b == r.base || b == "power" {
			return true
		}
	}
	return false
}

// confirmRequest is ConfirmRead as the channel publishes it after a LightState write.
func confirmRequest() *request {
	name, args, _ := strings.Cut(ConfirmRead, " ")
	return &request{name: name, args: args, base: "status", status: "STATUS" + args, confirms: true}
}

// typed is the command name as Tasmota derives the SetOption4 topic from it: without its index.
func (r *request) typed() string { return strings.TrimRight(r.name, "0123456789") }

// baseOf is the lower-case command name without its index. It resolves Tasmota's synonyms the
// way cmdpolicy does, so SO19 and SetOption19 are the same command.
func baseOf(name string) string {
	b := strings.ToLower(strings.TrimRight(name, "0123456789"))
	if b == "so" {
		return "setoption"
	}
	return b
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// Refuses reports a command the channel never publishes, whatever the device: the refusals of parseRequest (a
// malformed line, a file transfer, a command that runs other commands, Backlog2 and Backlog3, a Status other than 1
// to 13). The transport names HTTP for such a command in a person's confirmation (#826): it never crosses the broker.
func Refuses(cmd string) bool {
	_, err := parseRequest(cmd)
	return err != nil
}

// parseRequest checks cmd with cmdpolicy's grammar and returns what to publish and how to
// recognise the answer. Status 0 and everything that is not a plain Status 1..13 is refused, in
// every spelling and inside a Backlog.
func parseRequest(cmd string) (*request, error) {
	v, err := cmdpolicy.Classify(cmd)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	cmd = strings.Trim(cmd, " ")
	name, args, _ := strings.Cut(cmd, " ")
	r := &request{name: name, args: strings.TrimSpace(args), base: baseOf(name), redact: v.Redact}
	idx := name[len(strings.TrimRight(name, "0123456789")):]
	switch {
	case fileCommands[r.base]:
		return nil, refuse("%s carries the device's MQTT password; file transfers have their own API", r.name)
	case executors[r.base] && !ruleSwitch(r):
		return nil, refuse("%s can run other commands (Status 0 among them), so it is not sent over MQTT", r.name)
	case cmdpolicy.TemplateRunsCommands(cmd):
		// Tasmota runs a template's CMND as a Backlog when it applies the template. cmdpolicy's rule, the one that
		// also masks such a Template in the audit and the log.
		return nil, refuse("%s with CMND runs other commands (Status 0 among them), so it is not sent over MQTT", r.name)
	case r.base == "backlog":
		if idx == "2" || idx == "3" {
			return nil, refuse("%s publishes no results, so nothing could be matched", r.name)
		}
		return parseBacklog(r)
	case r.base == "status":
		arg := r.args
		switch {
		case idx != "" && arg != "":
			return nil, refuse("%s %s: which Status is meant?", r.name, arg)
		case idx != "":
			arg = idx
		}
		if arg == "" {
			r.status = "STATUS"
			return r, nil
		}
		n, err := strconv.Atoi(arg)
		if err != nil || strconv.Itoa(n) != arg || n < 1 || n > 13 {
			return nil, refuse("Status %s is not a plain Status 1 to 13 (Status 0 is never sent)", arg)
		}
		r.status = "STATUS" + arg
	}
	if Reply(cmd) == LightState {
		// The confirmation is a Status 11, which carries the Wi-Fi name: its reply is always scrubbed.
		r.light, r.confirm, r.redact = true, confirmRequest(), true
	}
	return r, nil
}

func parseBacklog(r *request) (*request, error) {
	for _, part := range strings.Split(r.args, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Tasmota strips leading "Backlog" words from every part (CmndBacklog, case-insensitive, no
		// space needed), so "BacklogStatus 0" is Status 0. No part may start that way.
		if len(part) >= len("backlog") && strings.EqualFold(part[:len("backlog")], "backlog") {
			return nil, refuse("a Backlog part starts with \"Backlog\"")
		}
		sub, err := parseRequest(part)
		if err != nil {
			return nil, err
		}
		if sub.base == "backlog" {
			return nil, refuse("a Backlog inside a Backlog")
		}
		r.backlog = append(r.backlog, sub)
		r.redact = r.redact || sub.redact
	}
	if len(r.backlog) == 0 {
		return nil, refuse("an empty Backlog")
	}
	return confirmLights(r)
}

// confirmLights appends ConfirmRead as the last part of a Backlog that holds a LightState write: each part is collected
// by its own answer and a light part has none (Reply), while Tasmota runs the parts in order (CmndBacklog), so the
// trailing STATUS11 shows the state after them. The whole line must stay a line cmdpolicy reads (MaxCommandLen, the
// same split on the device); a longer one is refused and nothing is published (#813).
func confirmLights(r *request) (*request, error) {
	light := false
	for _, sub := range r.backlog {
		if sub.light {
			sub.confirm = nil // confirmed by the Backlog's own last part
			light = true
		}
	}
	if !light {
		return r, nil
	}
	args := strings.TrimRight(r.args, "; ") + "; " + ConfirmRead
	if _, err := cmdpolicy.Classify(r.name + " " + args); err != nil || len(r.name)+1+len(args) > cmdpolicy.MaxCommandLen {
		return nil, refuse("the Backlog has a light command and no room for the %s that would confirm it", ConfirmRead)
	}
	r.args = args
	r.backlog = append(r.backlog, confirmRequest())
	r.redact = true // the confirmation carries the Wi-Fi name
	return r, nil
}

// verdict is the decision about one incoming message that belongs to a command: its reply, or
// the error a Command Unknown/Error reply stands for.
type verdict struct {
	reply []byte
	err   error
}

// match decides whether m is the answer to r. ok is false for a message that belongs to someone
// else (a button press, another poller, a retained message).
func (r *request) match(m mqttbus.Message, loose bool) (v verdict, ok bool) {
	if m.Retained {
		return verdict{}, false
	}
	if r.confirm != nil {
		if v, ok := r.confirm.match(m, false); ok {
			return v, true // the Status 11 sent after the light write
		}
	}
	if r.status != "" && m.Kind == mqttbus.KindStatus {
		if _, values, isObj := topLevel(m.Payload); isObj && values["Command"] != nil {
			// A failed Status is never data (#1209). Tasmota publishes it like every failure: on RESULT, or with
			// SetOption4 1 on the command's name without its index, stat/<t>/STATUS (support_command.ino:517). It is
			// ours only when its Input names this Status, whichever of the two topics it came on.
			if m.Suffix != r.status && m.Suffix != "STATUS" {
				return verdict{}, false
			}
			return r.failure(values)
		}
		if m.Suffix != r.status {
			return verdict{}, false
		}
		if r.confirms {
			return verdict{reply: statusSTS(m.Payload)}, true
		}
		return verdict{reply: m.Payload}, true
	}
	if m.Kind == mqttbus.KindPower && r.base == "power" && (string(m.Payload) == "ON" || string(m.Payload) == "OFF") {
		// SetOption4 1 devices report a switched relay on stat/<t>/POWERn instead of RESULT.
		if keyNames(m.Suffix, r.name, r.base) {
			return verdict{reply: []byte(`{"` + m.Suffix + `":"` + string(m.Payload) + `"}`)}, true
		}
		return verdict{}, false
	}
	// A device with SetOption4 1 answers on stat/<t>/<COMMAND> instead of RESULT (#632). Tasmota names the
	// topic after the command as typed (upper case, index cut off: SO19 is SO), or COMMAND for an unknown one
	// (support_command.ino:517). Such a reply is read like a RESULT, but only on the topic of this command:
	// a rule's own stat topic or another poller's command never answers it.
	failOnly := false // the topic carries nothing but a failed command for r
	switch m.Kind {
	case mqttbus.KindResult:
	case mqttbus.KindCommand:
		if !strings.EqualFold(m.Suffix, r.typed()) {
			if m.Suffix != "COMMAND" {
				return verdict{}, false
			}
			failOnly = true
		}
	case mqttbus.KindPower:
		// Power with SetOption4 1 and SetOption90 1: the JSON {"POWER1":"ON"} alone, on stat/<t>/POWER1.
		if r.base != "power" {
			return verdict{}, false
		}
		if !keyNames(m.Suffix, r.name, r.base) {
			// A failed Power3 (no third relay) is published on the command's name without its index,
			// stat/<t>/POWER (#648); that topic is relay 1's otherwise, so it carries only the failure for r.
			if !strings.EqualFold(m.Suffix, r.typed()) {
				return verdict{}, false
			}
			failOnly = true
		}
	case mqttbus.KindUpgrade:
		// SetOption4 1: Upgrade answers on stat/<t>/UPGRADE (#648), where the device later reports its
		// download too. Only the immediate reply answers; a report never does (safety rule 3: the engine
		// must get the URL echo or nothing).
		if r.base != "upgrade" || !upgradeReply(m.Payload) {
			return verdict{}, false
		}
	case mqttbus.KindLWT, mqttbus.KindState, mqttbus.KindSensor, mqttbus.KindStatus, mqttbus.KindDiscovery, mqttbus.KindInfo, mqttbus.KindFile:
		return verdict{}, false // never a command's reply here (STATUSn was decided above)
	default:
		return verdict{}, false
	}
	keys, values, isObj := topLevel(m.Payload)
	if !isObj {
		return verdict{}, false
	}
	if _, has := values["Command"]; has {
		return r.failure(values)
	}
	if failOnly {
		return verdict{}, false // stat/<t>/COMMAND, or POWER for an indexed Power, carries nothing but a failed command
	}
	if r.status != "" {
		return verdict{}, false // a Status answers on STATUSn; any other RESULT is noise
	}
	if r.light {
		return verdict{}, false // the light state answers every light change; only the confirmation answers this one
	}
	if r.base == "template" {
		// Template answers with the stored template (TemplateJson) and no "Template" key, for the bare read
		// and for a successful write alike (support_command.ino, CmndTemplate); only the refusal of a write
		// is {"Template":"Invalid JSON or Bad Chip Type"} under the command's key. So the shape answers
		// both, and a bare read takes nothing else: a "Template" key there would be someone else's write.
		if templateShape(values) {
			return verdict{reply: m.Payload}, true
		}
		if r.args == "" {
			return verdict{}, false
		}
	}
	for _, k := range keys {
		if keyNames(k, r.name, r.base) {
			return verdict{reply: m.Payload}, true
		}
	}
	if loose && otherKeys[r.base] {
		return verdict{reply: m.Payload}, true
	}
	return verdict{}, false
}

// failure decides a {"Command":"Unknown"|"Error"} object, the one place that says whether a failure is r's (the
// same classification as tasmota's for an HTTP reply). Without an Input it is the command in flight's (nothing says
// whose it is). With one, the Input must name r: its first word, index stripped, is r's command, and for a Status
// it names this Status (named). An Input that is no text, or names another command, is someone else's failure. The
// Input is never returned: for a secret-setting command it would be the secret.
func (r *request) failure(values map[string]json.RawMessage) (verdict, bool) {
	var word string
	if json.Unmarshal(values["Command"], &word) != nil || (word != "Unknown" && word != "Error") {
		return verdict{}, false
	}
	if in, hasIn := values["Input"]; hasIn {
		var input string
		if json.Unmarshal(in, &input) != nil || !r.named(input) {
			return verdict{}, false // someone else's failed command
		}
	}
	if word == "Unknown" {
		return verdict{err: tasmota.ErrUnknownCommand}, true
	}
	return verdict{err: tasmota.ErrCommandError}, true
}

// named reports whether a failure's Input names r. Tasmota writes the Input as the command in upper case, its index
// unless the index is 1, then a space and the argument (CommandHandler, support_command.ino 446-520): "STATUS 9" for
// Status 9, "STATUS9" for Status9, "STATUS" for a bare Status. So a Status is named by the first word's index, or
// else by the second word, and a Status 8 failure never answers a Status 9. Index 1 is left out, so a failed Status1
// would read as a bare Status and time out; the channel sends Status 1 as "Status1" only when asked so, and Status 1
// always answers (StatusPRM), so no special case is kept for it.
func (r *request) named(input string) bool {
	first, rest, _ := strings.Cut(strings.TrimSpace(input), " ")
	if baseOf(first) != r.base {
		return false
	}
	if r.status == "" {
		return true
	}
	sel := first[len(strings.TrimRight(first, "0123456789")):]
	if sel == "" {
		sel, _, _ = strings.Cut(strings.TrimSpace(rest), " ")
	}
	return "STATUS"+sel == r.status
}

// upgradeReply reports an immediate reply of CmndUpgrade (v15.6.0 support_command.ino:1325-1349): "Version X
// from <url>" when the download starts, "1 or >X to upgrade" when it does not, "Safeboot" for Upgrade 2 on an
// ESP32. The download's own reports on the same topic, "Failed ..." and "Successful. Restarting"
// (support_tasmota.ino:1530), are not replies; neither is anything else, so an unknown text is never taken.
func upgradeReply(payload []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil || len(m) != 1 {
		return false
	}
	var s string
	if json.Unmarshal(m["Upgrade"], &s) != nil {
		return false
	}
	return strings.HasPrefix(s, "Version ") || strings.HasPrefix(s, "1 or >") || s == "Safeboot"
}

// templateShape reports the reply of a bare Template (TemplateJson, v16 support.ino): NAME as text and
// GPIO as an array. FLAG and BASE are not required, so an older firmware that leaves one out still reads.
func templateShape(values map[string]json.RawMessage) bool {
	var name string
	if raw, ok := values["NAME"]; !ok || json.Unmarshal(raw, &name) != nil {
		return false
	}
	var pins []json.RawMessage
	raw, ok := values["GPIO"]
	return ok && json.Unmarshal(raw, &pins) == nil
}

// statusSTS is the object inside a STATUS11 reply, the device's state with the light keys at the top as in the HTTP
// reply of a light write. A payload without that object is returned as it came.
func statusSTS(payload []byte) []byte {
	_, values, ok := topLevel(payload)
	if !ok {
		return payload
	}
	inner, has := values["StatusSTS"]
	if !has {
		return payload
	}
	if _, _, isObj := topLevel(inner); !isObj {
		return payload
	}
	return inner
}

// matchesAny reports whether m would have answered r in any mode: a late reply to an abandoned request.
func (r *request) matchesAny(m mqttbus.Message) bool {
	if r.backlog != nil {
		for _, sub := range r.backlog {
			if _, ok := sub.match(m, true); ok {
				return true
			}
		}
		return false
	}
	_, ok := r.match(m, true)
	return ok
}

// keyNames says whether a reply key belongs to the command: "POWER1" and "POWER" answer Power1,
// "SetOption19" answers SO19. Case does not matter. The index matters: POWER1 does not answer
// Power2 (a multi-relay device reports its relays one by one, and a switch must be confirmed by
// its own relay). A missing index means 1, and index 0 ("Power0", all relays) takes any.
func keyNames(key, name, base string) bool {
	if strings.EqualFold(key, name) {
		return true
	}
	if baseOf(key) != base {
		return false
	}
	want, got := indexOf(name), indexOf(key)
	return want == "0" || want == got
}

// indexOf is the numeric suffix of a name, "1" when there is none.
func indexOf(name string) string {
	i := name[len(strings.TrimRight(name, "0123456789")):]
	if i == "" {
		return "1"
	}
	return i
}

// topLevel returns the top-level keys of a JSON object in order, and their raw values.
func topLevel(payload []byte) (keys []string, values map[string]json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, false
	}
	values = map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		k, isStr := t.(string)
		var v json.RawMessage
		if !isStr || dec.Decode(&v) != nil {
			return nil, nil, false
		}
		keys = append(keys, k)
		values[k] = v
	}
	return keys, values, true
}
