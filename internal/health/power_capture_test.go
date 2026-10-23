package health

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// Why: the Devices list showed "Off" for devices that are on. This replays a
// real capture of the fleet's MQTT traffic (tele STATE, stat POWER) through the
// tracker and requires that the relay state it ends with is the last ON/OFF the
// devices reported, for POWER and POWERn alike, with the strings Tasmota sends.
func TestPowerStateFromRealMQTTCapture(t *testing.T) {
	f, err := os.Open("../../testdata/fixtures/mqtt/capture.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only fixture

	type msg struct {
		kind          mqttbus.Kind
		topic, suffix string
		payload       string
	}
	var msgs []msg
	want := map[string]map[string]string{} // topic -> POWERn -> state
	topics := []string{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "|", 4)
		if len(parts) != 4 {
			continue
		}
		seg := strings.Split(parts[2], "/")
		if len(seg) != 3 {
			continue
		}
		prefix, topic, suffix := seg[0], seg[1], seg[2]
		var k mqttbus.Kind
		switch {
		case prefix == "tele" && suffix == "STATE":
			k = mqttbus.KindState
		case prefix == "stat" && strings.HasPrefix(suffix, "POWER"):
			k = mqttbus.KindPower
		default:
			continue
		}
		if !seen[topic] {
			seen[topic] = true
			topics = append(topics, topic)
		}
		msgs = append(msgs, msg{k, topic, suffix, parts[3]})
		if want[topic] == nil {
			want[topic] = map[string]string{}
		}
		if k == mqttbus.KindPower {
			want[topic][suffix] = strings.ToUpper(strings.TrimSpace(parts[3]))
			continue
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(parts[3]), &raw) == nil {
			for key, v := range raw {
				var s string
				if powerKey.MatchString(key) && json.Unmarshal(v, &s) == nil {
					want[topic][key] = s
				}
			}
		}
	}
	if len(msgs) == 0 {
		t.Fatal("capture has no STATE or POWER lines")
	}
	var devs []model.Device
	ids := map[string]model.DeviceID{}
	for i, tp := range topics {
		ids[tp] = model.DeviceID(i + 1)
		devs = append(devs, mqttDev(model.DeviceID(i+1), tp))
	}
	h := newHarness(t, true, Options{}, devs...)
	h.listen()
	for _, m := range msgs {
		h.bus.emit(m.kind, m.topic, m.suffix, m.payload, false)
	}
	on := 0
	for tp, rel := range want {
		got := h.svc.Get(ids[tp]).Power
		for key, w := range rel {
			if got[key] != w {
				t.Errorf("%s %s = %q, want %q (all: %v)", tp, key, got[key], w, got)
			}
			if w == "ON" {
				on++
			}
		}
	}
	if on == 0 {
		t.Fatal("the capture must contain devices that are ON, or the test proves nothing")
	}
}
