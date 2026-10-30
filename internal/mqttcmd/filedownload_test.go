package mqttcmd

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: the Tasmota file transfer protocol mandates MD5
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

const dlPassword = "s3cret-mqtt-pw"

func newDownloadEnv(t *testing.T) *simEnv {
	t.Helper()
	e := newSimEnv(t, simulator.DeviceSpec{MQTTPassword: dlPassword})
	e.waitConnected(t)
	return e
}

func httpDL(t *testing.T, e *simEnv) []byte {
	t.Helper()
	resp, err := http.Get("http://" + e.dev.Addr() + "/dl") //nolint:noctx // test against the loopback simulator
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fileCmds(e *simEnv) []string {
	var out []string
	for _, c := range e.dev.Commands() {
		if strings.HasPrefix(strings.ToLower(c), "filedownload") {
			out = append(out, c)
		}
	}
	return out
}

// Why: backups from both paths must be interchangeable for restore and diff, so the MQTT bytes
// are exactly what /dl serves. One "?" is sent per Data message and none after Started.
func TestDownloadEqualsHTTPDlAndSendsOneQuestionMarkPerData(t *testing.T) {
	e := newDownloadEnv(t)
	got, err := e.ch.Download(t.Context(), e.dvc, dlPassword)
	if err != nil {
		t.Fatal(err)
	}
	if want := httpDL(t, e); !bytes.Equal(got, want) {
		t.Fatalf("MQTT dump differs from /dl (%d vs %d bytes)", len(got), len(want))
	}
	cmds := fileCmds(e)
	q := 0
	for _, c := range cmds {
		if strings.HasSuffix(c, " ?") {
			q++
		}
	}
	// 0 (first transfer), start, one ? for the single Data message, one ? for the Md5.
	if q != 2 || len(cmds) != 4 {
		t.Fatalf("commands %q: want abort, start, ?, ?", cmds)
	}
	if !strings.HasSuffix(cmds[0], " 0") {
		t.Fatalf("the first transfer after start must open with FileDownload 0: %q", cmds)
	}
	if strings.Contains(cmds[1], dlPassword) {
		t.Fatalf("the simulator's command log must not hold the password: %q", cmds[1])
	}
}

// Why: a dump larger than one chunk is reassembled in order and checked against the MD5.
func TestDownloadReassemblesSeveralChunks(t *testing.T) {
	e := newDownloadEnv(t)
	dump := make([]byte, 10000)
	for i := range dump {
		dump[i] = byte(i * 7)
	}
	e.ctl.SetConfigDump(dump)
	got, err := e.ch.Download(t.Context(), e.dvc, dlPassword)
	if err != nil || !bytes.Equal(got, dump) {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	q := 0
	for _, c := range fileCmds(e) {
		if strings.HasSuffix(c, " ?") {
			q++
		}
	}
	if q != 4 { // 3 data messages + the Md5
		t.Fatalf("want 4 question marks, got %d in %q", q, fileCmds(e))
	}
}

// Why: after a clean transfer the device is idle, so the next one does not need the abort; the
// abort is only for "state unknown" (live test: a stray state produced truncated files).
func TestSecondDownloadSkipsTheOpeningAbort(t *testing.T) {
	e := newDownloadEnv(t)
	for i := range 2 {
		if _, err := e.ch.Download(t.Context(), e.dvc, dlPassword); err != nil {
			t.Fatal(i, err)
		}
	}
	zeros := 0
	for _, c := range fileCmds(e) {
		if strings.HasSuffix(c, " 0") {
			zeros++
		}
	}
	if zeros != 1 {
		t.Fatalf("want exactly one FileDownload 0 over two transfers, got %q", fileCmds(e))
	}
}

// Why (D6): Error 1 is the wrong-password signal the router turns into the bad mark; the error
// must be typed, must not contain the password, and nothing may be left half-open.
func TestDownloadWrongPasswordIsTypedAndDoesNotLeakIt(t *testing.T) {
	e := newDownloadEnv(t)
	_, err := e.ch.Download(t.Context(), e.dvc, "not-the-password")
	if !errors.Is(err, ErrPasswordRefused) {
		t.Fatalf("want ErrPasswordRefused, got %v", err)
	}
	if strings.Contains(err.Error(), "not-the-password") {
		t.Fatalf("error leaks the password: %v", err)
	}
	if _, err := e.ch.Download(t.Context(), e.dvc, dlPassword); err != nil {
		t.Fatalf("the right password works afterwards: %v", err)
	}
}

// Why: no password, nothing published (acceptance: "No password set -> unavailable; nothing is
// published"). Characters the device's parser might read differently are refused, not escaped.
func TestDownloadRefusesUnsendablePasswordsWithoutPublishing(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	for _, pw := range []string{"", `a"b`, `a\b`, "a\nb"} {
		_, err := c.Download(t.Context(), plug(), pw)
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%q: %v", pw, err)
		}
		if err != nil && pw != "" && strings.Contains(err.Error(), pw) {
			t.Errorf("error repeats the password: %v", err)
		}
	}
	if n := len(b.sent()); n != 0 {
		t.Fatalf("published %d commands", n)
	}
}

// Why: a group topic would run FileDownload on many devices (same rule as Do).
func TestDownloadRefusesAGroupTopic(t *testing.T) {
	b := &fakeBus{}
	d := plug()
	d.Topic = "tasmotas"
	if _, err := newChan(b).Download(t.Context(), d, dlPassword); !errors.Is(err, ErrRefused) || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}

// scriptedDevice answers FileDownload on a fakeBus step by step. steps[n] is what the device
// publishes in answer to the n-th command whose payload is not "0" ("?" or the start).
type scriptedDevice struct {
	steps   [][]string
	n       atomic.Int32
	aborted atomic.Int32
}

func (s *scriptedDevice) attach(b *fakeBus) {
	b.onPub = func(b *fakeBus, p pub) {
		if !strings.EqualFold(p.command, "FileDownload") {
			return
		}
		if p.payload == "0" {
			s.aborted.Add(1)
			b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", `{"FileDownload":"Aborted"}`, false) // in publish order, like a broker
			return
		}
		i := int(s.n.Add(1)) - 1
		if i >= len(s.steps) {
			return
		}
		msgs := s.steps[i]
		for _, m := range msgs { // synchronously: a broker keeps a publisher's order
			b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", m, false)
		}
	}
}

func md5hex(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // see import
	return hex.EncodeToString(s[:])
}

func data(id int, b []byte) string {
	return fmt.Sprintf(`{"Id":%d,"Data":"%s"}`, id, base64.StdEncoding.EncodeToString(b))
}

const started = `{"FileDownload":"Started"}`

func header(id, size int) string {
	return fmt.Sprintf(`{"File":"Config_x.dmp","Id":%d,"Type":2,"Size":%d}`, id, size)
}

func runScript(t *testing.T, steps [][]string, mod func(*Options)) (*scriptedDevice, *fakeBus, []byte, error) {
	t.Helper()
	b := &fakeBus{}
	s := &scriptedDevice{steps: steps}
	s.attach(b)
	o := fast()
	if mod != nil {
		mod(&o)
	}
	c := New(b, o)
	got, err := c.Download(t.Context(), plug(), dlPassword)
	// the abort after a failure is asynchronous to the caller only through the bus hook
	return s, b, got, err
}

// Why (fail closed): every irregularity returns an error, hands back no bytes (nothing is
// stored) and ends the transfer with FileDownload 0 so the device is not left mid-transfer.
func TestDownloadFaultsFailClosedAndAbort(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB}, 100)
	good := md5hex(body)
	cases := []struct {
		name  string
		steps [][]string
		want  string
	}{
		{"md5 mismatch", [][]string{{started, header(5, 100)}, {data(5, body)}, {fmt.Sprintf(`{"Id":5,"Md5":"%s"}`, md5hex([]byte("x")))}}, "MD5"},
		{"short file (live: stray ? truncates)", [][]string{{started, header(5, 200)}, {data(5, body)}, {fmt.Sprintf(`{"Id":5,"Md5":"%s"}`, good)}}, "100 of 200"},
		{"empty data (live: stray ? gives an empty chunk)", [][]string{{started, header(5, 100)}, {`{"Id":5,"Data":""}`}}, "empty"},
		{"id changes", [][]string{{started, header(5, 100)}, {data(6, body)}}, "another transfer"},
		{"more data than announced", [][]string{{started, header(5, 50)}, {data(5, body)}}, "more than"},
		{"size too small", [][]string{{started, header(5, 1)}}, "announced"},
		{"size too large", [][]string{{started, header(5, 1<<20)}}, "announced"},
		{"bad base64", [][]string{{started, header(5, 100)}, {`{"Id":5,"Data":"!!!"}`}}, "base64"},
		{"header is not a header", [][]string{{started, `{"Foo":1}`}}, "header"},
		{"no header", [][]string{{started}}, "no answer"},
		{"no data answer", [][]string{{started, header(5, 100)}}, "no answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, got, err := runScript(t, tc.steps, nil)
			if err == nil || got != nil || !errors.Is(err, ErrTransfer) {
				t.Fatalf("got %d bytes, %v", len(got), err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks %q", err, tc.want)
			}
			if strings.Contains(err.Error(), dlPassword) {
				t.Fatalf("error leaks the password: %v", err)
			}
			// one abort opens the transfer (state unknown), a second one closes the failed one.
			deadline := time.Now().Add(5 * time.Second)
			for s.aborted.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if s.aborted.Load() != 2 {
				t.Fatalf("want the failed transfer ended with FileDownload 0, aborts %d", s.aborted.Load())
			}
		})
	}
}

// Why: after a bad transfer the device state is unknown, so the next Download opens with
// FileDownload 0 again, even though this process did transfer before.
func TestDownloadAfterAFailureOpensWithAbortAgain(t *testing.T) {
	b := &fakeBus{}
	body := bytes.Repeat([]byte{1}, 64)
	good := [][]string{{started, header(9, 64)}, {data(9, body)}, {fmt.Sprintf(`{"Id":9,"Md5":"%s"}`, md5hex(body))}}
	bad := [][]string{{started, header(9, 64)}, {`{"Id":9,"Data":""}`}}
	s := &scriptedDevice{steps: append(append(append([][]string{}, good...), bad...), good...)}
	s.attach(b)
	c := newChan(b)
	for i, wantErr := range []bool{false, true, false} {
		_, err := c.Download(t.Context(), plug(), dlPassword)
		if (err != nil) != wantErr {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	// opening abort, (good), bad's abort, then the next opening abort: 3 in total before the third start.
	var zeros int
	for _, p := range b.sent() {
		if p.payload == "0" {
			zeros++
		}
	}
	if zeros != 3 {
		t.Fatalf("want 3 aborts (open, after the failure, open again), got %d in %v", zeros, b.sent())
	}
}

// Why: leftovers of an earlier transfer (the late "Aborted", an extra chunk) must not be taken
// for the answer to the new start.
func TestDownloadIgnoresMessagesBeforeStarted(t *testing.T) {
	body := bytes.Repeat([]byte{7}, 40)
	steps := [][]string{
		{`{"FileDownload":"Aborted"}`, data(3, []byte("old")), ``, started, header(9, 40)},
		{data(9, body)},
		{fmt.Sprintf(`{"Id":9,"Md5":"%s"}`, md5hex(body))},
	}
	_, _, got, err := runScript(t, steps, nil)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("%v, %d bytes", err, len(got))
	}
}

// Why (coordinator note): the device always publishes, an empty response too, so empty payloads
// on the topic are no messages at all.
func TestDownloadIgnoresEmptyPayloads(t *testing.T) {
	body := bytes.Repeat([]byte{7}, 40)
	steps := [][]string{
		{started, header(9, 40)},
		{``, data(9, body)},
		{``, fmt.Sprintf(`{"Id":9,"Md5":"%s"}`, md5hex(body))},
	}
	if _, _, got, err := runScript(t, steps, nil); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("%v, %d bytes", err, len(got))
	}
}

// Why (coordinator note): a firmware without FileDownload answers {"Command":"Unknown"} with the
// whole payload, password included, in Input. It is a clear failure and the echo goes nowhere.
func TestDownloadUnknownEchoFailsWithoutLeakingIt(t *testing.T) {
	echo := fmt.Sprintf(`{"Command":"Unknown","Input":"FILEDOWNLOAD {\"Password\":\"%s\",\"Type\":2}"}`, dlPassword)
	_, _, got, err := runScript(t, [][]string{{echo}}, nil)
	if err == nil || got != nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), dlPassword) || strings.Contains(err.Error(), "Input") {
		t.Fatalf("error carries the echo: %v", err)
	}
}

// Why: Error 5 (group topic) and others are named; only Error 1 is the password signal.
func TestDownloadErrorCodes(t *testing.T) {
	for code, want := range map[int]string{3: "type", 5: "group topic", 7: "Error 7"} {
		_, _, _, err := runScript(t, [][]string{{fmt.Sprintf(`{"FileDownload":"Error %d"}`, code)}}, nil)
		if err == nil || errors.Is(err, ErrPasswordRefused) || !strings.Contains(err.Error(), want) {
			t.Errorf("Error %d: %v", code, err)
		}
	}
	_, _, _, err := runScript(t, [][]string{{`{"FileDownload":"Error 1"}`}}, nil)
	if !errors.Is(err, ErrPasswordRefused) {
		t.Errorf("Error 1: %v", err)
	}
}

// Why: a silent device must not hold the topic forever: ReplyWait per message and FileTotal for
// the whole transfer, both ending in ErrTimeout.
func TestDownloadTimeouts(t *testing.T) {
	_, _, _, err := runScript(t, nil, func(o *Options) { o.ReplyWait = 50 * time.Millisecond })
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("per message: %v", err)
	}
	// A device that answers every ? slowly but within ReplyWait still hits the overall limit.
	b := &fakeBus{}
	body := bytes.Repeat([]byte{2}, 30)
	var questions atomic.Int32
	b.onPub = func(b *fakeBus, p pub) {
		if p.payload == "0" {
			b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", `{"FileDownload":"Aborted"}`, false)
			return
		}
		go func() {
			time.Sleep(40 * time.Millisecond)
			switch {
			case strings.Contains(p.payload, "Password"):
				b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", started, false)
				b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", header(4, 1000), false)
			default:
				questions.Add(1)
				b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", data(4, body), false)
			}
		}()
	}
	o := fast()
	o.ReplyWait, o.FileTotal = time.Second, 300*time.Millisecond
	if _, err := New(b, o).Download(t.Context(), plug(), dlPassword); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("overall: %v", err)
	}
	if questions.Load() < 1 {
		t.Fatal("the overall limit must be hit by the transfer, not spent before it started")
	}
}

// Why: a start that was (or may have been) published and never answered leaves the device in an
// unknown state, so it is ended with FileDownload 0; an Error N means the device reset itself.
func TestDownloadAbortsAfterAnUnansweredStartButNotAfterAnErrorAnswer(t *testing.T) {
	s, _, _, err := runScript(t, nil, nil) // nothing ever answers the start
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatal(err)
	}
	if got := s.aborted.Load(); got != 2 {
		t.Fatalf("want opening abort + abort after the unanswered start, got %d", got)
	}
	s, _, _, err = runScript(t, [][]string{{`{"FileDownload":"Error 1"}`}}, nil)
	if !errors.Is(err, ErrPasswordRefused) || s.aborted.Load() != 1 {
		t.Fatalf("%v, aborts %d (only the opening one)", err, s.aborted.Load())
	}
}

// Why: a caller whose context is already spent must not publish the start (it carries the password).
func TestDownloadWithACancelledContextPublishesNothing(t *testing.T) {
	b := &fakeBus{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := newChan(b).Download(ctx, plug(), dlPassword); err == nil || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}

// Why: a caller that gives up before it is its turn sends nothing; the topic stays usable.
func TestDownloadQueuedCallerCanCancelWithoutSending(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	c.sem("plug") <- struct{}{} // another command holds the topic
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Download(ctx, plug(), dlPassword); !errors.Is(err, context.DeadlineExceeded) || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}

// Why: a device that is offline per LWT gets nothing published (same as Do).
func TestDownloadOfflineDeviceGetsNothing(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Offline", false)
	if _, err := c.Download(t.Context(), plug(), dlPassword); !errors.Is(err, tasmota.ErrUnreachable) || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}
