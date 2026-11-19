package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// recMQTT is the command channel as the router sees it, recording every command handed to it (what the router
// publishes) in order. timeout, when set, makes a command it names come back as a published command nobody
// answered: it is published and runs on the device, the answer is dropped.
type recMQTT struct {
	ch      transport.MQTTDoer
	timeout func(cmd string) bool
	mu      sync.Mutex
	sent    []string
}

func (m *recMQTT) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	m.mu.Lock()
	m.sent = append(m.sent, cmd)
	m.mu.Unlock()
	raw, err := m.ch.Do(ctx, d, cmd)
	if err == nil && m.timeout != nil && m.timeout(cmd) {
		return nil, fmt.Errorf("mqtt: no answer to the Backlog (answer dropped by the test): %w", tasmota.ErrTimeout)
	}
	return raw, err
}

func (m *recMQTT) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return m.Do(ctx, d, cmd)
}
func (m *recMQTT) Connected() bool { return m.ch.Connected() }

func (m *recMQTT) commands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.sent...)
}

// recHTTP is the raw HTTP client, recording every command it was asked to send (sent or refused).
type recHTTP struct {
	tasmota.Client
	mu   sync.Mutex
	sent []string
}

func (h *recHTTP) Command(ctx context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	h.mu.Lock()
	h.sent = append(h.sent, cmd)
	h.mu.Unlock()
	return h.Client.Command(ctx, t, cmd)
}

func (h *recHTTP) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

// backlogs keeps the Backlogs of cmds (the writes Make compliant sends), in order.
func backlogs(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "Backlog ") {
			out = append(out, c)
		}
	}
	return out
}

// fixRig is a simulated device online on a real broker, reached through the real bus, command channel, router,
// transport facts and settings reader, and Make compliant as production wires it (policyApplier around a
// policy.Fixer whose client is the router).
type fixRig struct {
	st     *gtStore
	router *transport.Router
	ctl    simulator.Controller
	dev    simulator.Device
	mq     *recMQTT
	http   *recHTTP
	set    *settings.Manager
	fixer  *policy.Fixer
	apply  policyApplier
}

// newFixRig starts the device with SaveData 0 (a value lives in RAM until a save), so only Make compliant's own
// SaveData puts it into flash. ours false files the device as one on another broker: the router then sends
// everything over HTTP, the order Make compliant had before #606.
func newFixRig(t *testing.T, ours bool, faults ...simulator.Fault) *fixRig {
	t.Helper()
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	fleet, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL(), TelePeriod: time.Hour}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "fix-plug", Profile: simulator.ProfileESP32, Version: "15.1.0",
		Settings: map[string]string{"savedata": "0"}, Faults: faults})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := mqttbus.Connect(t.Context(), mqttbus.Options{BrokerURL: br.URL(), ClientID: "fix-test"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
	}
	ctl, _ := simulator.Control(dev)
	waitReady(t, ctl)
	ch := mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond})
	t.Cleanup(ch.Close)
	host, port := splitAddr(t, dev.Addr())
	capab := model.MQTTOurs
	if !ours {
		capab = model.MQTTOtherBroker
	}
	// own GroupTopic read, the only device: usable under the strict rule
	st := &gtStore{now: time.Now, devs: []model.Device{{ID: 1, Topic: dev.Topic(), Address: host, Port: port, MQTT: capab, MAC: ctl.MAC(),
		FullTopic: "%prefix%/%topic%/", GroupTopic: "tasmotas", GroupTopicReadAt: time.Now(), MoreGroupTopicsReadAt: time.Now()}}}
	h := &recHTTP{Client: tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})}
	router := transport.NewRouter(transport.Options{HTTP: h, Devices: st})
	router.SetFacts(&TransportFacts{Devices: st, Health: fakeFactsHealth{1: {LWT: "Online"}}, Memory: router.Memory(), Unanswered: router.UnansweredReads(),
		BusConnected: bus.Connected, FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now})
	mq := &recMQTT{ch: ch}
	router.SetMQTT(mq)
	set := settings.New(settings.Options{Devices: st, Client: router, ReadTimeout: 30 * time.Second})
	// Verify's waits let the device's queue run (every read pushes it back); the simulator runs at 1/50 of real time
	wait := func(ctx context.Context, d time.Duration) error {
		select {
		case <-time.After(d / 50):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	fixer := &policy.Fixer{Client: router, Reader: set, Wait: wait, VerifyAttempts: 3}
	return &fixRig{st: st, router: router, ctl: ctl, dev: dev, mq: mq, http: h, set: set, fixer: fixer, apply: policyApplier{a: &runtime{fixer: fixer}}}
}

func waitReady(t *testing.T, ctl simulator.Controller) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := ctl.WaitFor(ctx, func(c simulator.Controller) bool { return c.Phase() == simulator.PhaseRun && c.MQTTReady() }); err != nil {
		t.Fatalf("simulated device never became ready: %v", err)
	}
}

func (g *fixRig) device() model.Device { return g.st.devs[0] }

// deviceBacklogs is how many times the device received a Backlog with exactly this text, by whatever transport.
func (g *fixRig) deviceBacklogs(text string) int {
	n := 0
	for _, c := range g.dev.Commands() {
		if strings.Join(strings.Fields(c), " ") == strings.Join(strings.Fields(text), " ") {
			n++
		}
	}
	return n
}

// fixLines are 11 plain settings (no restart, nothing that cuts MQTT off): two chunks of 8 and 3, then the save.
var fixLines = []string{
	"PowerOnState 2", "TelePeriod 123", "LedState 3",
	"SetOption1 1", "SetOption11 1", "SetOption13 1", "SetOption20 1", "SetOption26 1",
	"SetOption30 1", "SetOption31 1", "SetOption51 1",
}

func keysOf(lines []string) []string {
	keys := make([]string, len(lines))
	for i, l := range lines {
		keys[i], _, _ = strings.Cut(l, " ")
	}
	return keys
}

// Why (#606): Make compliant follows the write path, MQTT first for a device usable on our broker, and must end
// where it ends over HTTP: every setting on the device, written to flash by the one SaveData after the chunks
// (a restart must not lose them). Over MQTT nothing of it may go over HTTP while HTTP works, and every chunk
// reaches the device once. Simulated device, real broker, bus, channel, router, facts and settings reader.
func TestMakeCompliantOverMQTTReachesTheSameSettingsAsOverHTTP(t *testing.T) {
	flash := map[bool]map[string]string{}
	for _, ours := range []bool{true, false} {
		g := newFixRig(t, ours)
		before, err := g.set.ReadKeys(t.Context(), 1, keysOf(fixLines))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range fixLines {
			k, v, _ := strings.Cut(l, " ")
			if have, ok := before[k]; ok && strings.EqualFold(have, v) || before[k] == "ON" && v == "1" {
				t.Fatalf("fixture: %s is already %s on the device, the test would prove nothing", k, have)
			}
		}
		g.mq.sent, g.http.sent = nil, nil
		ctx, tr := transport.WithTrace(t.Context())
		if err := g.apply.Apply(ctx, g.device(), fixLines); err != nil {
			t.Fatalf("ours=%v: Apply: %v", ours, err)
		}
		if _, err := g.fixer.Verify(t.Context(), g.device(), fixLines); err != nil {
			t.Fatalf("ours=%v: Verify: %v", ours, err)
		}
		mq, h := backlogs(g.mq.commands()), backlogs(g.http.commands())
		if ours {
			if tr.Via() != "mqtt" || len(h) != 0 || len(g.http.commands()) != 0 {
				t.Fatalf("Make compliant over MQTT first: via %s, HTTP got %v", tr.Via(), g.http.commands())
			}
			if len(mq) != 3 {
				t.Fatalf("published Backlogs %v, want the two chunks and the save", mq)
			}
		} else if tr.Via() != "http" || len(mq) != 0 {
			t.Fatalf("the device off our broker: via %s, published %v", tr.Via(), mq)
		}
		for _, b := range append(mq, h...) {
			if n := g.deviceBacklogs(b); n != 1 {
				t.Errorf("ours=%v: %q reached the device %d times, want once", ours, b, n)
			}
		}
		// a power cycle keeps only what is in flash: the save must have come after the queued lines ran (#559)
		boots := g.ctl.Boots()
		g.ctl.PowerCycle()
		cctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		err = g.ctl.WaitFor(cctx, func(c simulator.Controller) bool { return c.Boots() > boots && c.Phase() == simulator.PhaseRun })
		cancel()
		if err != nil {
			t.Fatalf("the power cycle never ended: %v", err)
		}
		after, err := g.set.ReadKeys(transport.WithPath(t.Context(), transport.HTTP), 1, keysOf(fixLines))
		if err != nil {
			t.Fatal(err)
		}
		flash[ours] = after
	}
	for _, l := range fixLines {
		k, want, _ := strings.Cut(l, " ")
		for _, ours := range []bool{true, false} {
			if got := flash[ours][k]; got != want && (want != "1" || !strings.EqualFold(got, "ON")) {
				t.Errorf("%s after a power cycle (ours=%v): %q, want %s", k, ours, got, want)
			}
		}
	}
}

// Why (#606): over MQTT the chunks are those of always (at most 8 lines, in the plan's order) and the save comes
// last, after them in the device's queue; a save first or in the middle would write flash before the later lines
// ran and lose them at the next restart. Counted on what the router published.
func TestMakeCompliantOverMQTTChunksAndSavesLast(t *testing.T) {
	g := newFixRig(t, true)
	lines := append(append([]string{}, fixLines...), "SetOption8 1", "SetOption12 1", "SetOption16 1", "SetOption17 1", "SetOption21 1", "SetOption24 1")
	g.mq.sent, g.http.sent = nil, nil
	if err := g.apply.Apply(t.Context(), g.device(), lines); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Backlog " + strings.Join(lines[:8], "; "),
		"Backlog " + strings.Join(lines[8:16], "; "),
		"Backlog " + strings.Join(lines[16:], "; "),
		"Backlog SaveData 0",
	}
	if got := backlogs(g.mq.commands()); !slices.Equal(got, want) {
		t.Fatalf("published Backlogs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := g.http.commands(); len(got) != 0 {
		t.Fatalf("HTTP got %v while MQTT carried the run", got)
	}
}

// Why (#606, review of PR #816): MQTT takes a Backlog as long as cmdpolicy accepts, and Make compliant's chunks
// must fit that, not a larger limit of its own: a longer chunk was refused by the MQTT channel and carried by HTTP,
// an HTTP case the product owner's rule does not allow (Tasmota itself takes about 1200 bytes over MQTT). A
// realistic plain plan of firmware and time settings, too long for one Backlog: MQTT carries every chunk, each
// reaches the device once, and HTTP gets nothing.
func TestMakeCompliantOverMQTTKeepsALongPlanOnMQTT(t *testing.T) {
	g := newFixRig(t, true)
	lines := []string{
		"OtaUrl http://firmware.example.lan:8080/tasmota/release/16.0.0/tasmota32-lite-de.bin.gz",
		"NtpServer1 ntp1.example.lan", "NtpServer2 ntp2.example.lan", "NtpServer3 de.pool.ntp.org",
		"TimeStd 0,0,10,1,3,60", "TimeDst 0,0,3,1,2,120", "PowerOnState 3", "SetOption1 1",
	}
	g.mq.sent, g.http.sent = nil, nil
	ctx, tr := transport.WithTrace(t.Context())
	if err := g.apply.Apply(ctx, g.device(), lines); err != nil {
		t.Fatal(err)
	}
	if got := g.http.commands(); len(got) != 0 || tr.Via() != "mqtt" {
		t.Fatalf("via %s, HTTP got %q while MQTT works", tr.Via(), got)
	}
	mq := backlogs(g.mq.commands())
	if len(mq) < 3 || mq[len(mq)-1] != "Backlog SaveData 0" {
		t.Fatalf("published Backlogs %q, want the plan in more than one chunk and the save", mq)
	}
	var sent []string
	for _, b := range mq[:len(mq)-1] {
		sent = append(sent, strings.Split(strings.TrimPrefix(b, "Backlog "), "; ")...)
	}
	if !slices.Equal(sent, lines) {
		t.Fatalf("published lines %q, want %q in order", sent, lines)
	}
	for _, b := range mq {
		if n := g.deviceBacklogs(b); n != 1 {
			t.Errorf("%q reached the device %d times, want once", b, n)
		}
	}
}

// Why (#606, the product owner's transport rule of 2026-11-14): a line that changes the device's MQTT connection
// (here its topic) is no reason for HTTP. While HTTP answers the router's Status 11 right before, the device cannot
// be locked out (HTTP carries its next commands), so the chunk goes over MQTT like the others, without a question
// and without being left out. HTTP carries nothing of the run but that one question. The topic goes in a chunk of
// its own after every plain line, so the question is asked after they all ran (a plain line may close HTTP).
func TestMakeCompliantSendsACutOffLineOverMQTTWhileHTTPAnswers(t *testing.T) {
	g := newFixRig(t, true)
	cut := "Topic " + g.dev.Topic() // its own topic again: a cut-off command that changes nothing, as in the live check
	lines := append(append([]string{}, fixLines[:9]...), cut)
	g.mq.sent, g.http.sent = nil, nil
	ctx, tr := transport.WithTrace(t.Context())
	if err := g.apply.Apply(ctx, g.device(), lines); err != nil {
		t.Fatalf("Apply = %v, want every line sent (no lockout while HTTP answers)", err)
	}
	first, plain, second := "Backlog "+strings.Join(lines[:8], "; "), "Backlog "+lines[8], "Backlog "+cut
	if got := backlogs(g.mq.commands()); !slices.Equal(got, []string{first, plain, second}) {
		t.Errorf("published %v, want every chunk, the topic last in one of its own: %q", got, []string{first, plain, second})
	}
	if got := g.http.commands(); !slices.Equal(got, []string{"Status 11"}) {
		t.Errorf("HTTP was asked %v, want only the router's Status 11 before the topic change", got)
	}
	if tr.Via() != "mqtt" {
		t.Errorf("audit via %q, want mqtt", tr.Via())
	}
	if n := g.deviceBacklogs(second); n != 1 {
		t.Errorf("the chunk with the topic reached the device %d times, want once", n)
	}
}

// Why (#606, guard of T10): when HTTP does not answer the router's Status 11, a line that changes the device's MQTT
// connection could lock it out (MQTT the only way left): it needs a person's consent, which Make compliant never
// has. It is left out and reported (SkippedError), never published and never left undone silently; the rest goes
// over MQTT and is saved. HTTP carries nothing of the run.
func TestMakeCompliantSkipsACutOffLineWhenHTTPDoesNotAnswer(t *testing.T) {
	g := newFixRig(t, true, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	cut := "Topic " + g.dev.Topic()
	lines := append(append([]string{}, fixLines[:9]...), cut)
	g.mq.sent, g.http.sent = nil, nil
	err := g.apply.Apply(t.Context(), g.device(), lines)
	var sk *policy.SkippedError
	if !errors.As(err, &sk) || sk.Applied != 9 || !slices.Equal(sk.SkippedLines(), []string{cut}) {
		t.Fatalf("Apply = %v, want 9 applied and the topic skipped", err)
	}
	for _, c := range append(g.mq.commands(), g.http.commands()...) {
		if strings.Contains(c, "Topic ") {
			t.Fatalf("the topic change was sent: %q", c)
		}
	}
	if got := backlogs(g.http.commands()); len(got) != 0 {
		t.Errorf("HTTP was asked %v, want no line of the run over HTTP", got)
	}
	want := []string{"Backlog " + strings.Join(lines[:8], "; "), "Backlog " + lines[8], "Backlog SaveData 0"}
	if got := backlogs(g.mq.commands()); !slices.Equal(got, want) {
		t.Errorf("published %v, want %v", got, want)
	}
}

// Why (#606): a chunk published over MQTT that nobody confirmed may have run: an unknown outcome. It is never
// sent again, over MQTT or HTTP, and nothing after it goes out but the save of what was sent before it
// (saveAfterFailure); the error says the step may or may not have reached the device. The device's replies are not
// where TasmoFleet listens, so the router sends that save over HTTP while HTTP works (#605), not into the void.
func TestMakeCompliantNeverResendsAChunkThatTimedOutOverMQTT(t *testing.T) {
	g := newFixRig(t, true)
	lines := append(append([]string{}, fixLines...), "SetOption8 1", "SetOption12 1", "SetOption16 1", "SetOption17 1", "SetOption21 1", "SetOption24 1")
	second := "Backlog " + strings.Join(lines[8:16], "; ")
	g.mq.timeout = func(cmd string) bool { return cmd == second }
	g.mq.sent, g.http.sent = nil, nil
	err := g.apply.Apply(t.Context(), g.device(), lines)
	if !errors.Is(err, tasmota.ErrTimeout) || !strings.Contains(err.Error(), "may or may not have reached the device") {
		t.Fatalf("Apply = %v, want the unknown outcome of the timed-out chunk", err)
	}
	want := []string{"Backlog " + strings.Join(lines[:8], "; "), second}
	if got := backlogs(g.mq.commands()); !slices.Equal(got, want) {
		t.Errorf("published %v, want the first chunk and the timed-out one once", got)
	}
	if got := g.http.commands(); !slices.Equal(got, []string{"Backlog SaveData 0"}) {
		t.Errorf("HTTP got %v after a published chunk timed out, want only the save", got)
	}
	if n := g.deviceBacklogs(second); n != 1 {
		t.Errorf("the timed-out chunk reached the device %d times, want once", n)
	}
}

// Why (#606, guard of T10): Tasmota runs MqttTLS as SetOption103, which restarts the device into TLS and cuts it off
// from a plain broker. Taken as a generic rule, "MqttTLS 1" was published over MQTT to a device reached over MQTT
// only, with the save after it, where "SetOption103 1" is left out and reported. Under its synonym it is refused
// before anything is sent, so nothing of the run reaches the device.
func TestMakeCompliantNeverPublishesASetOptionSynonymThatCutsMQTTOff(t *testing.T) {
	g := newFixRig(t, true, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	g.mq.sent, g.http.sent = nil, nil
	err := g.apply.Apply(t.Context(), g.device(), []string{"PowerOnState 2", "MqttTLS 1"})
	if err == nil || !strings.Contains(err.Error(), "write the setting as SetOption103") {
		t.Fatalf("Apply = %v, want MqttTLS refused as SetOption103", err)
	}
	for _, c := range append(g.mq.commands(), g.http.commands()...) {
		if strings.Contains(strings.ToLower(c), "mqtttls") {
			t.Fatalf("MqttTLS was sent: %q", c)
		}
	}
	if got := backlogs(g.mq.commands()); len(got) != 0 {
		t.Errorf("published %v, want nothing of a refused run", got)
	}
}
