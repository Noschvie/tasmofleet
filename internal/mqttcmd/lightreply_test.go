package mqttcmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// The light state an RGBCW bulb publishes after every light change (ResponseLightState, Tasmota v16.0.0
// xdrv_04_light.ino 1460-1540): the answer to Dimmer, Color, Channel, HSBColor, White and CT writes, and to a
// rotary encoder, another client's command or the end of a Wakeup alike.
const (
	lightState50 = `{"POWER":"ON","Dimmer":50,"Color":"7F7F7F0000","HSBColor":"0,0,50","White":0,"CT":153,"Channel":[50,50,50,0,0]}`
	lightState30 = `{"POWER":"ON","Dimmer":30,"Color":"4D4D4D0000","HSBColor":"0,0,30","White":0,"CT":153,"Channel":[30,30,30,0,0]}`
	// Status 11 carries the same light state inside StatusSTS (MqttShowState, support_tasmota.ino 887-888), and the
	// Wi-Fi name, which is secret (cmdpolicy, #809).
	status11At50 = `{"StatusSTS":{"Time":"2026-11-17T10:00:00","Uptime":"0T01:00:00","POWER":"ON","Dimmer":50,` +
		`"Color":"7F7F7F0000","HSBColor":"0,0,50","White":0,"CT":153,"Channel":[50,50,50,0,0],"Wifi":{"AP":1,"SSId":"homenet","RSSI":80}}}`
)

// lightBulb scripts a light: own answers the write (nil for a write that publishes only the light state), and every
// Status 11 is answered with status11 (none when status11 is empty).
func lightBulb(own func(p pub) string, status11 string) *fakeBus {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		if strings.EqualFold(p.command, "Status") && p.payload == "11" {
			if status11 != "" {
				b.emit(mqttbus.KindStatus, p.topic, "STATUS11", status11, false)
			}
			return
		}
		b.result(p.topic, own(p))
	}
	return b
}

func sentLines(b *fakeBus) []string {
	var out []string
	for _, p := range b.sent() {
		out = append(out, strings.TrimSpace(p.command+" "+p.payload))
	}
	return out
}

// Why (#813): a light write is answered by the whole light state, and every light change publishes that same message:
// another client's Dimmer, a rotary encoder, the end of a Wakeup. Taking the first such message as the answer reported
// "Dimmer 50" done with Dimmer 30 on the strength of someone else's change. Only the Status 11 published after the
// write, a read whose STATUS11 nothing else answers, may confirm it.
func TestAnUnrelatedLightStateNeverConfirmsALightWrite(t *testing.T) {
	t.Parallel()
	b := lightBulb(func(p pub) string {
		return lightState30 // someone else's change arrives first; the bulb's own state for Dimmer 50 never does
	}, status11At50)
	got, err := newChan(b).Do(t.Context(), plug(), "Dimmer 50")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(got, &m) != nil || string(m["Dimmer"]) != "50" {
		t.Fatalf("reply = %s, want the state Status 11 read after the write (Dimmer 50)", got)
	}
	if s := sentLines(b); len(s) != 2 || s[0] != "Dimmer 50" || s[1] != ConfirmRead {
		t.Fatalf("sent %q, want the write and then %q", s, ConfirmRead)
	}
}

// Why (#813): each command below publishes only the light state after a change (CmndSupportColor 2774, CmndWhite
// 2800, CmndChannel 2850, CmndHsbColor 2885, CmndColorTemperature 2965, CmndDimmer 3068-3077, all ending in
// LightPreparePower -> ResponseLightState(0), v16.0.0 xdrv_04_light.ino; the same in every pinned version from 12.5.0).
// Taking that message proved nothing, and Channel2, Color2, HSBColor2/3 and Dimmer2/3 never matched it at all (the
// state carries Channel, Color, HSBColor and Dimmer without the index), so they timed out and moved the device to
// HTTP for ten minutes. Each is confirmed by the Status 11 that follows it. The rows below them keep their own
// answer: a read, Color3-6 (ResponseCmndIdxChar, their own key) and a Power switch. A light that has no such channel
// answers Command Error (support_command.ino 504-517: the handler wrote no response), which fails the write.
func TestEveryLightStateWriteIsConfirmedByStatus11(t *testing.T) {
	t.Parallel()
	confirmed := []string{
		"Color 1", "Color FF0000", "Color1 FF0000", "Color2 00FF00", "Color1 +",
		"Channel 20", "Channel1 20", "Channel2 20", "Channel5 30", "Channel3 +",
		"Dimmer 50", "Dimmer +", "Dimmer0 50", "Dimmer1 50", "Dimmer2 50", "Dimmer3 50", "Dimmer4 50",
		"HSBColor 120,100,50", "HSBColor 120", "HSBColor1 120", "HSBColor2 50", "HSBColor3 50",
		"White 40", "CT 300", "CT -",
	}
	for _, cmd := range confirmed {
		b := lightBulb(func(pub) string { return lightState50 }, status11At50)
		got, err := newChan(b).Do(t.Context(), plug(), cmd)
		if err != nil {
			t.Errorf("%q: %v", cmd, err)
			continue
		}
		if s := sentLines(b); len(s) != 2 || s[0] != cmd || s[1] != ConfirmRead {
			t.Errorf("%q: sent %q, want the write and then %q", cmd, s, ConfirmRead)
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(got, &m) != nil || m["StatusSTS"] != nil || string(m["Uptime"]) != `"0T01:00:00"` || string(m["Dimmer"]) != "50" {
			t.Errorf("%q: reply = %s, want the state inside StatusSTS", cmd, got)
		}
		if Reply(cmd) != LightState {
			t.Errorf("Reply(%q) = %v, want LightState", cmd, Reply(cmd))
		}
	}

	ownAnswer := map[string]string{
		"Color3 FF0000": `{"Color3":"FF0000"}`,
		"Color6 00FF00": `{"Color6":"00FF00"}`,
		"Dimmer":        `{"Dimmer":50}`,
		"Dimmer ?":      `{"Dimmer":50}`,
		"Channel2":      `{"Channel2":50}`,
		"Power 1":       `{"POWER":"ON"}`,
	}
	for cmd, answer := range ownAnswer {
		b := lightBulb(func(pub) string { return answer }, status11At50)
		got, err := newChan(b).Do(t.Context(), plug(), cmd)
		if err != nil || string(got) != answer {
			t.Errorf("%q: %s, %v; want its own answer %s", cmd, got, err, answer)
		}
		if s := sentLines(b); len(s) != 1 {
			t.Errorf("%q: sent %q, want the command alone", cmd, s)
		}
	}

	b := lightBulb(func(pub) string { return `{"Command":"Error","Input":"CHANNEL5 50"}` }, status11At50)
	if _, err := newChan(b).Do(t.Context(), plug(), "Channel5 50"); !errors.Is(err, tasmota.ErrCommandError) {
		t.Errorf("Channel5 50 on a light without a fifth channel: err = %v, want ErrCommandError, not confirmed by Status 11", err)
	}
}

// Why (#813): one rule says how a command's answer is recognised, for reads and writes alike. The reads whose answer
// cannot be matched go HTTP first (transport.MQTTCannotMatch asks this rule); the light writes are confirmed by a read
// and must never be read as unmatchable, or the router would send them HTTP first against the transport rule.
func TestOneReplyRuleForReadsAndWrites(t *testing.T) {
	t.Parallel()
	for cmd, want := range map[string]ReplyKind{
		"State": Unmatchable, "Gpio": Unmatchable, "Gpios": Unmatchable, "Modules": Unmatchable, "gpios": Unmatchable,
		"Gpio 1": Matched, "Gpio1 0": Matched, "Status 11": Matched, "Hostname": Matched,
		"Dimmer 50": LightState, "Channel2 20": LightState, "Color2 FF0000": LightState, "CT 300": LightState,
		"HSBColor3 40": LightState, "White 10": LightState, "Dimmer5 20": LightState,
		"Gpio ?": Unmatchable, "Dimmer": Matched, "Dimmer ?": Matched, "Channel2": Matched, "HSBColor": Matched, "Color": Matched,
		"Color3 FF0000": Matched, "Color0 1": Matched, "Power 1": Matched, "Scheme 2": Matched, "Wakeup 50": Matched,
	} {
		if got := Reply(cmd); got != want {
			t.Errorf("Reply(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// Why (#813): once the light write is published it may have run. A failure after that point (the Status 11 that would
// confirm it cannot be published) is an unknown outcome, never "not sent": the router would otherwise send the write
// a second time over HTTP (T2, T3).
func TestAFailureAfterTheLightWriteIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	b := lightBulb(func(pub) string { return lightState50 }, status11At50)
	b.failAt = 2
	_, err := newChan(b).Do(t.Context(), plug(), "Dimmer 50")
	if !errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrUnreachable) || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want an unknown outcome (ErrTimeout) and nothing that reads as not sent", err)
	}
	if s := sentLines(b); len(s) != 2 {
		t.Fatalf("publishes attempted %q, want the write and the Status 11", s)
	}
}

// Why (#813): a confirmation that comes too late must not answer the next Status 11 on the topic: that read would show
// a state from before it. The topic is held until the late STATUS11 arrives, as for any other late reply.
func TestALateConfirmationDoesNotAnswerTheNextStatus11(t *testing.T) {
	t.Parallel()
	b := lightBulb(func(pub) string { return lightState50 }, "") // the first Status 11 is not answered in time
	c := newChan(b)
	if _, err := c.Do(t.Context(), plug(), "Dimmer 50"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("first: %v, want a timeout while the confirmation is missing", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.emit(mqttbus.KindStatus, "plug", "STATUS11", `{"StatusSTS":{"Dimmer":10}}`, false) // the late one
	}()
	b.onPub = func(b *fakeBus, p pub) {
		go b.emit(mqttbus.KindStatus, p.topic, "STATUS11", `{"StatusSTS":{"Dimmer":50}}`, false)
	}
	got, err := c.Do(t.Context(), plug(), "Status 11")
	if err != nil || string(got) != `{"StatusSTS":{"Dimmer":50}}` {
		t.Fatalf("second: %s, %v", got, err)
	}
}

// Why (#813, the transport rule): a Backlog with a light write stays MQTT first. Its parts are collected one by one and
// a light part has no answer of its own, so the channel appends a Status 11 as the Backlog's last part: Tasmota runs
// the parts in order (CmndBacklog, support_command.ino 530-575), so that STATUS11 shows the state after them. Without
// it the Backlog is not reported done, however many other parts answered.
func TestABacklogWithALightWriteEndsWithStatus11(t *testing.T) {
	t.Parallel()
	answer := func(status11 bool) func(*fakeBus, pub) {
		return func(b *fakeBus, p pub) {
			go func() {
				b.result(p.topic, `{"POWER":"ON"}`)
				b.result(p.topic, lightState50)
				if status11 {
					b.emit(mqttbus.KindStatus, p.topic, "STATUS11", status11At50, false)
				}
			}()
		}
	}
	b := &fakeBus{onPub: answer(true)}
	got, err := newChan(b).Do(t.Context(), plug(), "Backlog Power 1; Dimmer 50")
	if err != nil {
		t.Fatal(err)
	}
	if s := b.sent(); len(s) != 1 || s[0].command != "Backlog" || s[0].payload != "Power 1; Dimmer 50; Status 11" {
		t.Fatalf("sent %v, want one Backlog that ends with %q", s, ConfirmRead)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(got, &m) != nil || string(m["POWER"]) != `"ON"` || string(m["Uptime"]) != `"0T01:00:00"` || m["StatusSTS"] != nil {
		t.Fatalf("merged = %s, want Power's answer and the state of the trailing Status 11", got)
	}
	if strings.Contains(string(got), "homenet") {
		t.Errorf("the Wi-Fi name of the confirmation is in the reply: %s", got)
	}

	b = &fakeBus{onPub: answer(false)}
	if _, err := newChan(b).Do(t.Context(), plug(), "Backlog Power 1; Dimmer 50"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("without the STATUS11: err = %v, want an unknown outcome (the light part is unconfirmed)", err)
	}
}

// Why (#813): the Status 11 is appended only while the whole line stays within cmdpolicy.MaxCommandLen, the longest
// line the channel publishes (cmdpolicy reads nothing longer). A longer one is refused before anything is published,
// so the router sends it over HTTP once (T2) and the console's confirmation names HTTP (Refuses, #826).
func TestABacklogTooLongForItsStatus11IsRefused(t *testing.T) {
	t.Parallel()
	head := "Backlog Dimmer 50; Hostname "
	cmd := head + strings.Repeat("h", cmdpolicy.MaxCommandLen-5-len(head)) // fits; with "; Status 11" it does not
	if len(cmd) > cmdpolicy.MaxCommandLen || len(cmd)+len("; "+ConfirmRead) <= cmdpolicy.MaxCommandLen {
		t.Fatalf("the test's Backlog is %d bytes long", len(cmd))
	}
	if _, err := cmdpolicy.Classify(cmd); err != nil {
		t.Fatalf("the test's Backlog itself is refused by cmdpolicy: %v", err)
	}
	b := &fakeBus{}
	if _, err := newChan(b).Do(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if s := b.sent(); len(s) != 0 {
		t.Fatalf("published %v", s)
	}
	if !Refuses(cmd) {
		t.Error("Refuses is false, so the console would announce MQTT for a command the channel refuses")
	}
	if Refuses("Backlog Power 1; Dimmer 50") {
		t.Error("a short Backlog with a light write is refused")
	}
}

// Why (#813, #809): the confirmation is a Status 11, which carries the Wi-Fi name. A light write is no secret-setting
// command, so nothing else would mask its reply: the channel scrubs every confirmation.
func TestTheConfirmationReplyIsScrubbed(t *testing.T) {
	t.Parallel()
	b := lightBulb(func(pub) string { return lightState50 }, status11At50)
	got, err := newChan(b).Do(t.Context(), plug(), "Dimmer 50")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Wifi struct{ SSId string }
	}
	if json.Unmarshal(got, &m) != nil || m.Wifi.SSId != "***" || strings.Contains(string(got), "homenet") {
		t.Fatalf("reply = %s, want the Wi-Fi name masked", got)
	}
}

// Why (#813): the Status 11 waits for its turn on the bus (the Gap) after the light write went out. A caller that gives
// up meanwhile, or a channel that closes meanwhile, leaves the write published and unconfirmed: an unknown outcome
// (ErrTimeout), never "not sent" (ErrUnreachable would let the router send the write again over HTTP).
func TestEndingBeforeTheConfirmationIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, closeIt := range []bool{false, true} {
		b := lightBulb(func(pub) string { return lightState50 }, status11At50)
		o := fast()
		o.Gap = 2 * time.Second // the Status 11 waits this long for its turn
		c := New(b, o)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		if closeIt {
			cancel()
			ctx = t.Context()
			go func() { time.Sleep(50 * time.Millisecond); c.Close() }()
		}
		_, err := c.Do(ctx, plug(), "Dimmer 50")
		cancel()
		if !errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrUnreachable) {
			t.Errorf("closed=%v: err = %v, want an unknown outcome only", closeIt, err)
		}
		if s := sentLines(b); len(s) != 1 {
			t.Errorf("closed=%v: sent %q, want the write alone", closeIt, s)
		}
	}
}

// Why (#813): a STATUS11 without the StatusSTS object (nothing Tasmota sends, but a broker client can) is still the
// answer to the Status 11 and is returned as it came: unwrapping must never drop a reply.
func TestAConfirmationWithoutStatusSTSIsReturnedAsItCame(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`{"Uptime":"0T01:00:00"}`, `{"StatusSTS":"x"}`, `[1]`} {
		b := lightBulb(func(pub) string { return lightState50 }, payload)
		got, err := newChan(b).Do(t.Context(), plug(), "Dimmer 50")
		if err != nil || string(got) != payload {
			t.Errorf("%s: %s, %v", payload, got, err)
		}
	}
}

// Why (#813): once a light write's Status 11 is published, its STATUS11 is on the way even when the write itself fails
// (Command Unknown on a plug in a bulk Dimmer, Command Error for a channel the light lacks, a Backlog part's error).
// Releasing the topic at that error let the STATUS11 answer the next light write on the topic, which was then reported
// done with the state from before it. The topic is held until that STATUS11 arrives, as for any late reply
// (TestALateReplyDoesNotAnswerTheNextCommand).
func TestTheStatus11OfAFailedLightWriteNeverAnswersTheNextOne(t *testing.T) {
	t.Parallel()
	stale := `{"StatusSTS":{"POWER":"ON","Dimmer":10}}`
	fresh := `{"StatusSTS":{"POWER":"ON","Dimmer":60}}`
	for _, first := range []string{"Dimmer 50", "Backlog Channel5 50"} {
		b := &fakeBus{}
		late := func(payload string) { // the device answers its Status 11 slower than the Gap
			go func() {
				time.Sleep(150 * time.Millisecond)
				b.emit(mqttbus.KindStatus, "plug", "STATUS11", payload, false)
			}()
		}
		statuses := 0
		b.onPub = func(b *fakeBus, p pub) {
			switch {
			case p.command == "Backlog": // its light part fails; the appended Status 11 still runs
				b.result(p.topic, `{"Command":"Error","Input":"CHANNEL5 50"}`)
				late(stale)
			case p.command == "Dimmer" && p.payload == "50": // a plug: no light
				b.result(p.topic, `{"Command":"Unknown","Input":"DIMMER 50"}`)
			case p.command == "Dimmer":
				b.result(p.topic, `{"POWER":"ON","Dimmer":60}`)
			case p.command == "Status":
				statuses++
				if statuses == 1 && first == "Dimmer 50" {
					late(stale)
				} else {
					late(fresh)
				}
			}
		}
		o := fast()
		o.Gap = 100 * time.Millisecond
		c := New(b, o)
		if _, err := c.Do(t.Context(), plug(), first); !errors.Is(err, tasmota.ErrUnknownCommand) && !errors.Is(err, tasmota.ErrCommandError) {
			t.Fatalf("%s: err = %v, want the device's refusal", first, err)
		}
		got, err := c.Do(t.Context(), plug(), "Dimmer 60")
		var m map[string]json.RawMessage
		if err != nil || json.Unmarshal(got, &m) != nil || string(m["Dimmer"]) != "60" {
			t.Errorf("after %s: Dimmer 60 = %s, %v; want its own confirmation, not the STATUS11 left from before", first, got, err)
		}
		c.Close()
	}
}

// Why (#813): Tasmota publishes a write's own response (the light state, its own key, or a Command Error) before it runs
// the next MQTT command, so the STATUS11 that confirms the write comes after it. A STATUS11 heard before that response
// was asked before the write ran (another client's Status 11, or ours overtaking the write on the other topic) and shows
// the state from before it: it never confirms the write, which then is an unknown outcome.
func TestAStatus11BeforeTheWritesOwnResponseNeverConfirmsIt(t *testing.T) {
	t.Parallel()
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		if p.command == "Status" {
			b.emit(mqttbus.KindStatus, p.topic, "STATUS11", `{"StatusSTS":{"POWER":"ON","Dimmer":30}}`, false)
			b.result(p.topic, `{"POWER":"ON","Dimmer":50}`)
		}
	}
	if got, err := newChan(b).Do(t.Context(), plug(), "Dimmer 50"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("Dimmer 50 = %s, %v; want an unknown outcome (ErrTimeout), not the state from before the write", got, err)
	}
}

// Why (#813): inside a Backlog a light part has no answer of its own, but its light state still comes first. Given to
// the next part whose key it carries (a bare Channel1 when the light is the second relay), that part counted as
// answered and its own Command Error was dropped: the Backlog was reported done although a part failed. The light part
// keeps its light state, so every later part gets only its own reply. That holds under SetOption4 1 too, where the
// replies come on stat/<t>/DIMMER and stat/<t>/CHANNEL. Another client's RESULT that is no light state is not kept.
func TestALightPartKeepsItsLightStateInABacklog(t *testing.T) {
	t.Parallel()
	const state = `{"POWER2":"ON","Dimmer":50,"Color":"7F7F7F","HSBColor":"0,0,50","Channel":[50,50,50]}`
	for _, so4 := range []bool{false, true} {
		b := &fakeBus{}
		b.onPub = func(b *fakeBus, p pub) {
			b.result(p.topic, `{"Hostname":"other"}`) // another client's command, before the Backlog's first part ran
			if so4 {
				b.emit(mqttbus.KindCommand, p.topic, "DIMMER", state, false)
				b.emit(mqttbus.KindCommand, p.topic, "CHANNEL", `{"Command":"Error","Input":"CHANNEL1"}`, false)
			} else {
				b.result(p.topic, state)
				b.result(p.topic, `{"Command":"Error","Input":"CHANNEL1"}`)
			}
			b.emit(mqttbus.KindStatus, p.topic, "STATUS11", `{"StatusSTS":{"POWER2":"ON","Dimmer":50}}`, false)
		}
		if got, err := newChan(b).Do(t.Context(), plug(), "Backlog Dimmer 50; Channel1"); !errors.Is(err, tasmota.ErrCommandError) {
			t.Errorf("SetOption4=%v: Backlog = %s, %v; want Channel1's Command Error", so4, got, err)
		}
	}
}
