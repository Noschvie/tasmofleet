package mqttcmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// fileLog records what the device publishes on stat/<topic>/FILEDOWNLOAD.
type fileLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *fileLog) add(m mqttbus.Message) {
	if m.Suffix != suffixDownload {
		return
	}
	l.mu.Lock()
	l.msgs = append(l.msgs, string(m.Payload))
	l.mu.Unlock()
}

func (l *fileLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}

// Why (#609): the password test must tell an accepted password from a refused one without a backup,
// and must leave the device idle: after "Started" it ends the download with FileDownload 0 before any
// "?" asks for data. Tasmota answers that abort with one more chunk of the dump and then "Aborted";
// that chunk is the test's exposure on the broker, and the next real download must still be whole.
func TestPasswordTestStartsThenAborts(t *testing.T) {
	t.Parallel()
	e := newDownloadEnv(t)
	var log fileLog
	unsub := e.bus.Subscribe([]mqttbus.Kind{mqttbus.KindFile}, log.add)
	defer unsub()
	if err := e.ch.TestPassword(t.Context(), e.dvc, dlPassword); err != nil {
		t.Fatalf("the right password was not accepted: %v", err)
	}
	cmds := fileCmds(e)
	if len(cmds) != 3 || !strings.HasSuffix(cmds[0], " 0") || !strings.HasSuffix(cmds[2], " 0") {
		t.Fatalf("commands %q: want the opening abort, the start, the abort", cmds)
	}
	for _, c := range cmds {
		if strings.HasSuffix(c, " ?") {
			t.Fatalf("the test asked for data: %q", cmds)
		}
	}
	waitFor(t, "the device's Aborted", func() bool {
		msgs := log.all()
		return len(msgs) > 0 && strings.Contains(msgs[len(msgs)-1], `"Aborted"`)
	})
	chunks := 0
	for _, m := range log.all() {
		if strings.Contains(m, `"Data"`) {
			chunks++
		}
	}
	if chunks != 1 {
		t.Fatalf("want the one chunk Tasmota sends on the abort, got %d in %q", chunks, log.all())
	}
	got, err := e.ch.Download(t.Context(), e.dvc, dlPassword)
	if err != nil || !bytes.Equal(got, httpDL(t, e)) {
		t.Fatalf("the download after a test: %d bytes, %v", len(got), err)
	}
}

// Why (#609): the test ends its download early, and Tasmota answers that abort with one more chunk and a late
// "Aborted". Only a transfer that ended with a matching MD5 leaves the topic in a known state, so the download
// after a test must open with FileDownload 0 again, even when the topic was clean before the test; a test that
// marked it clean would let the next download start into the leftovers of the test.
func TestThePasswordTestLeavesTheTopicToBeReset(t *testing.T) {
	t.Parallel()
	e := newDownloadEnv(t)
	if _, err := e.ch.Download(t.Context(), e.dvc, dlPassword); err != nil { // the topic is clean now
		t.Fatal(err)
	}
	before := len(fileCmds(e))
	if err := e.ch.TestPassword(t.Context(), e.dvc, dlPassword); err != nil {
		t.Fatal(err)
	}
	cmds := fileCmds(e)
	if len(cmds) != before+2 || strings.HasSuffix(cmds[before], " 0") {
		t.Fatalf("commands of the test %q: want the start (the topic was clean) and the abort", cmds[before:])
	}
	after := len(cmds)
	if _, err := e.ch.Download(t.Context(), e.dvc, dlPassword); err != nil {
		t.Fatal(err)
	}
	if cmds = fileCmds(e); len(cmds) <= after || !strings.HasSuffix(cmds[after], " 0") {
		t.Fatalf("commands of the download after the test %q: want the opening abort first", cmds[after:])
	}
}

// Why (#609): a refused password is the result the test exists for (the router marks it). The device
// allocated nothing, so no abort follows, and the error never holds the password.
func TestPasswordTestRefusedNeedsNoAbort(t *testing.T) {
	t.Parallel()
	e := newDownloadEnv(t)
	err := e.ch.TestPassword(t.Context(), e.dvc, "not-the-password")
	if !errors.Is(err, ErrPasswordRefused) {
		t.Fatalf("want ErrPasswordRefused, got %v", err)
	}
	if strings.Contains(err.Error(), "not-the-password") {
		t.Fatalf("the error holds the password: %v", err)
	}
	if cmds := fileCmds(e); len(cmds) != 2 || !strings.HasSuffix(cmds[0], " 0") {
		t.Fatalf("commands %q: want the opening abort and the start only", cmds)
	}
}

// Why (#609): a device the last will reports offline gets nothing, the password least of all; a
// password the device's parser might read differently is refused before anything is published.
func TestPasswordTestOfflineSendsNothing(t *testing.T) {
	t.Parallel()
	b := &fakeBus{}
	c := newChan(b)
	if err := c.TestPassword(t.Context(), plug(), `a"b`); !errors.Is(err, ErrRefused) || errors.Is(err, ErrPublished) {
		t.Fatalf("an unsendable password: %v", err)
	}
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Offline", false)
	if err := c.TestPassword(t.Context(), plug(), dlPassword); !errors.Is(err, tasmota.ErrUnreachable) || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}

// Why (#609): an unanswered start leaves the device in an unknown state; the test ends it like a
// download does, and the failure counts as published (the password went out).
func TestPasswordTestUnansweredStartIsAborted(t *testing.T) {
	t.Parallel()
	b := &fakeBus{}
	s := &scriptedDevice{}
	s.attach(b)
	err := New(b, fast()).TestPassword(t.Context(), plug(), dlPassword)
	if !errors.Is(err, tasmota.ErrTimeout) || !errors.Is(err, ErrPublished) {
		t.Fatalf("%v", err)
	}
	if got := s.aborted.Load(); got != 2 {
		t.Fatalf("want the opening abort and the abort after the unanswered start, got %d", got)
	}
}

// Why (#609): a device whose MqttPassword is empty accepts Password "" (Tasmota compares with strcmp).
// Before, the channel refused an empty password, so such a device could never be backed up, restored
// or tested over MQTT, nor send its files on its topic (Type 8, #924); the router decides whether a device has
// a password to send at all.
func TestAnEmptyPasswordIsSent(t *testing.T) {
	t.Parallel()
	e := newSimEnv(t, simulator.DeviceSpec{UfsMQTT: true})
	e.waitConnected(t)
	if err := e.ch.TestPassword(t.Context(), e.dvc, ""); err != nil {
		t.Fatalf("test with the empty password: %v", err)
	}
	got, err := e.ch.Download(t.Context(), e.dvc, "")
	if err != nil || !bytes.Equal(got, httpDL(t, e)) {
		t.Fatalf("download with the empty password: %d bytes, %v", len(got), err)
	}
	file, err := e.ch.DownloadFile(t.Context(), e.dvc, "", "/autoexec.be", 1<<20)
	if want := []byte("print('autoexec')\n"); err != nil || !bytes.Equal(file, want) {
		t.Fatalf("file read with the empty password: %q, %v", file, err)
	}
	dump := bytes.Repeat([]byte{0x5A}, 4096) // the simulator accepts only 4096 byte dumps
	if err := e.ch.Upload(t.Context(), e.dvc, UploadSettings, "Config_x.dmp", int64(len(dump)), bytes.NewReader(dump), "", nil); err != nil {
		t.Fatalf("restore with the empty password: %v", err)
	}
	waitFor(t, "the restored dump", func() bool { return bytes.Equal(httpDL(t, e), dump) })
	if err := e.ch.TestPassword(t.Context(), e.dvc, "some-password"); !errors.Is(err, ErrPasswordRefused) {
		t.Fatalf("a device without a password accepted one: %v", err)
	}
}

// timeoutBus is a fakeBus whose publish of the payloads in lost reports a lost answer (the publish may have
// happened), like a broker that did not acknowledge in time.
type timeoutBus struct {
	*fakeBus
	lost func(payload string) bool
}

func (b *timeoutBus) Command(ctx context.Context, topic, command, payload string) error {
	if err := b.fakeBus.Command(ctx, topic, command, payload); err != nil {
		return err
	}
	if b.lost(payload) {
		return context.DeadlineExceeded
	}
	return nil
}

// Why (#609): the test carries the password, so every case the channel refuses for a download it refuses for
// the test too, before anything is published: a group topic (many devices), a bus that is down, a caller that
// gave up while another command held the topic, a channel that closes, and a firmware upload of unknown
// outcome on the device. Nothing is published in any of them.
func TestPasswordTestRefusesWhatADownloadRefuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(b *fakeBus, c *Channel) (context.Context, func())
		dev   func() model.Device
		want  error
	}{
		"group topic": {dev: func() model.Device { d := plug(); d.Topic = "tasmotas"; return d }, want: ErrRefused},
		"bus down": {setup: func(b *fakeBus, _ *Channel) (context.Context, func()) {
			b.down = true
			return context.Background(), func() {}
		}, want: tasmota.ErrUnreachable},
		"caller gave up in the queue": {setup: func(_ *fakeBus, c *Channel) (context.Context, func()) {
			c.sem("plug") <- struct{}{}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			return ctx, cancel
		}, want: context.DeadlineExceeded},
		"channel closed in the queue": {setup: func(_ *fakeBus, c *Channel) (context.Context, func()) {
			c.sem("plug") <- struct{}{}
			c.Close()
			return context.Background(), func() {}
		}, want: tasmota.ErrUnreachable},
		"firmware upload pending": {setup: func(_ *fakeBus, c *Channel) (context.Context, func()) {
			c.mu.Lock()
			c.fwPending["plug"] = c.reboots["plug"]
			c.mu.Unlock()
			return context.Background(), func() {}
		}, want: ErrFirmwarePending},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := &fakeBus{}
			c := newChan(b)
			ctx, done := t.Context(), func() {}
			if tc.setup != nil {
				ctx, done = tc.setup(b, c)
			}
			defer done()
			d := plug()
			if tc.dev != nil {
				d = tc.dev()
			}
			if err := c.TestPassword(ctx, d, dlPassword); !errors.Is(err, tc.want) || len(b.sent()) != 0 {
				t.Fatalf("%v (want %v), sent %v", err, tc.want, b.sent())
			}
		})
	}
}

// Why (#609): a publish whose acknowledgement was lost may have reached the device. A lost opening abort stops
// the test before the password goes out; a lost start may have started a download, so the test ends it with
// FileDownload 0. Both count as published.
func TestPasswordTestLostPublishes(t *testing.T) {
	t.Parallel()
	b := &timeoutBus{fakeBus: &fakeBus{}, lost: func(p string) bool { return p == "0" }}
	if err := New(b, fast()).TestPassword(t.Context(), plug(), dlPassword); !errors.Is(err, tasmota.ErrTimeout) || !errors.Is(err, ErrPublished) {
		t.Fatalf("lost abort: %v", err)
	}
	if n := len(b.sent()); n != 1 {
		t.Fatalf("the start went out after a lost abort: %v", b.sent())
	}
	b = &timeoutBus{fakeBus: &fakeBus{}, lost: func(p string) bool { return strings.HasPrefix(p, "{") }}
	if err := New(b, fast()).TestPassword(t.Context(), plug(), dlPassword); !errors.Is(err, tasmota.ErrTimeout) || !errors.Is(err, ErrPublished) {
		t.Fatalf("lost start: %v", err)
	}
	if s := b.sent(); len(s) != 3 || s[2].payload != "0" {
		t.Fatalf("want the opening abort, the start and an abort, got %v", s)
	}
}

// Why (#609): a minimal firmware has no FileDownload and echoes the command, the password with it; the test
// must fail without taking that echo for an answer. A leftover of an earlier transfer before "Started" is
// skipped, not read as the answer.
func TestPasswordTestReadsOnlyTheAnswerToItsStart(t *testing.T) {
	t.Parallel()
	b := &fakeBus{}
	(&scriptedDevice{steps: [][]string{{`{"Command":"Unknown"}`}}}).attach(b)
	err := New(b, fast()).TestPassword(t.Context(), plug(), dlPassword)
	if !errors.Is(err, ErrTransfer) || strings.Contains(err.Error(), dlPassword) {
		t.Fatalf("minimal firmware: %v", err)
	}
	b = &fakeBus{}
	(&scriptedDevice{steps: [][]string{{header(7, 4096), started}}}).attach(b)
	if err := New(b, fast()).TestPassword(t.Context(), plug(), dlPassword); err != nil {
		t.Fatalf("a leftover before Started: %v", err)
	}
}

// Why (#609): the facts source asks this before a transfer is chosen, so a password the start command cannot
// carry is shown as unusable instead of failing late; an empty password is carried (the "No password" mode).
func TestCheckFilePasswordIsTheChannelsRule(t *testing.T) {
	t.Parallel()
	for pw, ok := range map[string]bool{"": true, "pw-1": true, `a"b`: false, `a\b`: false, "a\tb": false} {
		err := CheckFilePassword(pw)
		if (err == nil) != ok || (err != nil && (!errors.Is(err, ErrRefused) || (pw != "" && strings.Contains(err.Error(), pw)))) {
			t.Errorf("%q: %v", pw, err)
		}
	}
}
