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

// request is one parsed command.
type request struct {
	name    string // as sent, "Power1"
	args    string
	base    string // lower case, no trailing digits, synonyms resolved
	status  string // expected suffix for Status: "STATUS" or "STATUS5"; "" for other commands
	redact  bool   // the reply may echo a credential
	backlog []*request
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
	case r.base == "template" && strings.Contains(strings.ToLower(r.args), "cmnd"):
		// Tasmota runs a template's CMND as a Backlog when it applies the template, and its JSON parser takes the
		// key in any case. Matched on the text, not on parsed JSON, so a spelling the parser would still accept
		// fails closed.
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
	if r.status != "" && m.Kind == mqttbus.KindStatus {
		if m.Suffix == r.status {
			return verdict{reply: m.Payload}, true
		}
		return verdict{}, false
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
	if cmdVal, has := values["Command"]; has {
		var word string
		if json.Unmarshal(cmdVal, &word) != nil || (word != "Unknown" && word != "Error") {
			return verdict{}, false
		}
		var input string
		if in, hasIn := values["Input"]; hasIn && json.Unmarshal(in, &input) == nil {
			first, _, _ := strings.Cut(strings.TrimSpace(input), " ")
			if baseOf(first) != r.base {
				return verdict{}, false // someone else's failed command
			}
		}
		// The Input is not returned: for a secret-setting command it would be the secret.
		if word == "Unknown" {
			return verdict{err: tasmota.ErrUnknownCommand}, true
		}
		return verdict{err: tasmota.ErrCommandError}, true
	}
	if failOnly {
		return verdict{}, false // stat/<t>/COMMAND, or POWER for an indexed Power, carries nothing but a failed command
	}
	if r.status != "" {
		return verdict{}, false // a Status answers on STATUSn; any other RESULT is noise
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
