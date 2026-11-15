package mqttbus

import (
	"os"
	"strings"
	"testing"
)

// The broker ACL guide (docs/mqtt-broker-acl.md, #840) tells people which topics to open for TasmoFleet. If it is
// wrong the way that hurts is silent: a rule too narrow and TasmoFleet's reads, commands or backups stop working; a rule
// too wide and the ACL the page promises keeps nobody out. These tests read the guide's ACL blocks as they stand and
// hold them against the topics this package really subscribes to and publishes to, and against what the page says
// Home Assistant never gets. (tools/mqtt-acl-check.sh runs the same blocks against a real broker.)

type aclRule struct{ access, topic string } // access: read, write, readwrite, deny

// guideACL returns the rules per login of the fenced block with the given info string.
func guideACL(t *testing.T, info string) map[string][]aclRule {
	t.Helper()
	b, err := os.ReadFile("../../docs/mqtt-broker-acl.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "\n```"+info+"\n")
	if !ok {
		t.Fatalf("docs/mqtt-broker-acl.md has no ```%s block", info)
	}
	block, _, _ := strings.Cut(after, "\n```")
	rules := map[string][]aclRule{}
	user := ""
	for line := range strings.SplitSeq(block, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch f[0] {
		case "user":
			user = f[1]
		case "topic":
			if user == "" || len(f) != 3 {
				t.Fatalf("%s: %q is not a rule of a user", info, line)
			}
			rules[user] = append(rules[user], aclRule{f[1], f[2]})
		case "pattern":
			// applies to every login with %u as its name: not a rule of one user
		default:
			t.Fatalf("%s: unknown line %q", info, line)
		}
	}
	return rules
}

// covers reports whether the ACL topic pattern allows everything the MQTT filter f can match.
func covers(pat, f []string) bool {
	switch {
	case len(pat) == 0:
		return len(f) == 0
	case pat[0] == "#":
		return true
	case len(f) == 0 || f[0] == "#":
		return false
	case pat[0] == "+":
		return covers(pat[1:], f[1:])
	case f[0] == "+":
		return false
	}
	return pat[0] == f[0] && covers(pat[1:], f[1:])
}

// allows reports whether the login may do access (read or write) on the topic or filter: some rule of the right kind
// covers it and no deny rule does.
func allows(rules []aclRule, access, topic string) bool {
	f := strings.Split(topic, "/")
	ok := false
	for _, r := range rules {
		if !covers(strings.Split(r.topic, "/"), f) {
			continue
		}
		if r.access == "deny" {
			return false
		}
		if r.access == access || r.access == "readwrite" {
			ok = true
		}
	}
	return ok
}

func TestGuideACLCoversExactlyWhatTasmoFleetSubscribesAndPublishes(t *testing.T) {
	codec, _ := NewTopicCodec("", [3]string{})
	for _, info := range []string{"acl", "acl-strict"} {
		fleet := guideACL(t, info)["tasmofleet"]
		for _, filter := range codec.Filters() {
			if !allows(fleet, "read", filter) {
				t.Errorf("%s: TasmoFleet subscribes to %q but the guide's rules do not allow reading it", info, filter)
			}
		}
		for _, cmd := range []string{"Power", "FileDownload", "FileUpload", "Backlog", "WebPassword"} {
			if topic := codec.Command("kitchen", cmd); !allows(fleet, "write", topic) {
				t.Errorf("%s: TasmoFleet publishes to %q but the guide's rules do not allow writing it", info, topic)
			}
		}
		// it never reads commands, so the guide must not hand it that (least privilege)
		if allows(fleet, "read", "cmnd/kitchen/Power") || allows(fleet, "read", "#") {
			t.Errorf("%s: the guide lets TasmoFleet read commands or everything", info)
		}
	}
}

// Why: the page's promise is that Home Assistant may send commands but never reads one, and never receives the dump of
// a backup or restore. A line that gives it back `#` or `cmnd/#` would leave the page's check passing nowhere and the
// secrets readable by every automation in Home Assistant.
func TestGuideACLKeepsHomeAssistantAwayFromCommandsAndTransfers(t *testing.T) {
	for _, info := range []string{"acl", "acl-strict"} {
		ha := guideACL(t, info)["homeassistant"]
		if !allows(ha, "write", "cmnd/kitchen/Power") {
			t.Errorf("%s: Home Assistant cannot send commands", info)
		}
		for _, topic := range []string{"cmnd/kitchen/WebPassword", "cmnd/#", "#", "stat/kitchen/FILEDOWNLOAD", "stat/kitchen/FILEUPLOAD"} {
			if allows(ha, "read", topic) {
				t.Errorf("%s: Home Assistant may read %q", info, topic)
			}
		}
		for _, topic := range []string{"tele/#", "stat/kitchen/RESULT", "tasmota/discovery/AABBCCDDEEFF/config"} {
			if !allows(ha, "read", topic) {
				t.Errorf("%s: the Tasmota integration needs to read %q", info, topic)
			}
		}
	}
}

// Why: the devices' login reads commands, and nothing else; if it could read all of stat/ or tele/ one stolen device
// would see every device's replies.
func TestGuideACLDevicesReadOnlyCommands(t *testing.T) {
	dev := guideACL(t, "acl")["tasmota"]
	if !allows(dev, "read", "cmnd/kitchen/#") || !allows(dev, "write", "stat/kitchen/RESULT") || !allows(dev, "write", "tele/kitchen/LWT") ||
		!allows(dev, "write", "tasmota/discovery/AABBCCDDEEFF/config") {
		t.Error("the devices' login lacks what Tasmota needs")
	}
	for _, topic := range []string{"stat/#", "tele/#", "#"} {
		if allows(dev, "read", topic) {
			t.Errorf("the devices' login may read %q", topic)
		}
	}
	for _, topic := range []string{"cmnd/kitchen/Power"} {
		if allows(dev, "write", topic) {
			t.Errorf("the devices' login may write %q", topic)
		}
	}
}

func TestCoversTopicPatterns(t *testing.T) {
	for _, tc := range []struct {
		pat, f string
		want   bool
	}{
		{"tele/#", "tele/+/#", true}, {"tele/#", "tele/a/LWT", true}, {"tele/#", "#", false}, {"tele/#", "tele", true},
		{"tasmota/discovery/+/config", "tasmota/discovery/+/config", true}, {"tasmota/discovery/+/config", "tasmota/discovery/+/sensors", false},
		{"+/a", "+/a", true}, {"x/a", "+/a", false}, {"#", "cmnd/#", true},
	} {
		if got := covers(strings.Split(tc.pat, "/"), strings.Split(tc.f, "/")); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.pat, tc.f, got, tc.want)
		}
	}
}
