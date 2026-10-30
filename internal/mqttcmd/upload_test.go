package mqttcmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

const upPassword = dlPassword

func newUploadEnv(t *testing.T, profile simulator.Profile, faults ...simulator.Fault) *simEnv {
	t.Helper()
	e := newSimEnv(t, simulator.DeviceSpec{MQTTPassword: upPassword, Profile: profile, Faults: faults})
	e.waitConnected(t)
	return e
}

func fileUpCmds(e *simEnv) []string {
	var out []string
	for _, c := range e.dev.Commands() {
		if strings.HasPrefix(strings.ToLower(c), "fileupload") {
			out = append(out, c)
		}
	}
	return out
}

func simImage(t *testing.T) []byte {
	t.Helper()
	img, err := simulator.NewImage("../../testdata/fixtures", "tasmota32.bin", "16.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func upload(ctx context.Context, c *Channel, e *simEnv, typ int, data []byte, pw string) error {
	return c.Upload(ctx, e.dvc, typ, "x.bin", int64(len(data)), bytes.NewReader(data), pw, nil)
}

// Why (restore over MQTT): the dump must end up as the device's settings, applied by the device
// only after the MD5 matched, and progress must count acknowledged bytes.
func TestUploadType2RoundTripAppliesTheDump(t *testing.T) {
	e := newUploadEnv(t, simulator.ProfileESP8266_1M)
	dump := bytes.Repeat([]byte{0x5A}, 4096) // the simulator accepts only 4096 byte dumps
	var last int64
	var calls int
	err := e.ch.Upload(t.Context(), e.dvc, UploadSettings, "Config_x.dmp", int64(len(dump)), bytes.NewReader(dump), upPassword, func(n int64) { last = n; calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if last != 4096 || calls < 2 {
		t.Fatalf("progress ended at %d after %d calls", last, calls)
	}
	waitFor(t, "the restored dump", func() bool { return bytes.Equal(httpDL(t, e), dump) })
	for _, c := range fileUpCmds(e) {
		if strings.Contains(c, upPassword) {
			t.Fatalf("the simulator's log holds the password: %q", c)
		}
	}
}

// Why (D19): Type 1 is refused by the safeboot layout before any data (Error 2, typed, nothing
// flashed) and works on a dual-OTA layout, where the new version boots.
func TestUploadType1SafebootNoRoomAndDualSuccess(t *testing.T) {
	img := simImage(t)
	e := newUploadEnv(t, simulator.ProfileESP32)
	big := bytes.Repeat([]byte{1}, 2234912)
	err := e.ch.Upload(t.Context(), e.dvc, UploadFirmware, "tasmota32.bin", int64(len(big)), bytes.NewReader(big), upPassword, nil)
	if !errors.Is(err, ErrNoRoom) || !errors.Is(err, ErrTransfer) || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("want ErrNoRoom, got %v", err)
	}
	if cmds := fileUpCmds(e); len(cmds) != 2 { // opening abort + start, no abort after the device's own Error
		t.Fatalf("commands %q", cmds)
	}

	d := newUploadEnv(t, simulator.ProfileESP32Dual)
	if err := upload(t.Context(), d.ch, d, UploadFirmware, img, upPassword); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new version", func() bool { return d.dev.Version() == "16.1.0" })
}

// Why: the device drops its connection for a packet of 1200 bytes or more. Every FileUpload
// publish stays within the budget at the announced MaxSize, and a MaxSize that shrinks between
// acks is followed (the latest one wins).
type upDevice struct {
	t       *testing.T
	mu      sync.Mutex
	maxes   []int // MaxSize announced after the start, then after each chunk (last repeats)
	final   string
	dropAck int // 1-based data message whose ack is not published
	idOff   int // added to the Id in acks
	chunks  []int
	widest  int
	aborts  int
	data    bytes.Buffer
}

var idRe = regexp.MustCompile(`"Id":(\d+)`)

func (u *upDevice) attach(b *fakeBus) {
	b.onPub = func(b *fakeBus, p pub) {
		if !strings.EqualFold(p.command, "FileUpload") {
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if budget := 1200 - 5 - len("cmnd/"+p.topic+"/"+p.command) - 1; len(p.payload) > budget {
			u.t.Errorf("payload of %d bytes exceeds the budget %d", len(p.payload), budget)
		}
		u.widest = max(u.widest, len(p.payload))
		emit := func(m string) { b.emit(mqttbus.KindFile, p.topic, "FILEUPLOAD", m, false) }
		max := func() int {
			i := min(len(u.chunks), len(u.maxes)-1)
			return u.maxes[i]
		}
		id := 0
		if m := idRe.FindStringSubmatch(p.payload); m != nil {
			id, _ = strconv.Atoi(m[1])
		}
		switch {
		case p.payload == "0":
			u.aborts++
			emit(`{"FileUpload":"Aborted"}`)
		case strings.Contains(p.payload, "Password"):
			emit(`{"FileUpload":"Started"}`)
			emit(fmt.Sprintf(`{"Id":%d,"MaxSize":%d}`, id+u.idOff, max()))
		case strings.Contains(p.payload, `"Data"`):
			m := regexp.MustCompile(`"Data":"([^"]*)"`).FindStringSubmatch(p.payload)
			raw := decodeB64(u.t, m[1])
			if len(raw) > max() {
				u.t.Errorf("chunk of %d bytes, the device allows %d", len(raw), max())
			}
			u.data.Write(raw)
			u.chunks = append(u.chunks, len(raw))
			if u.dropAck == len(u.chunks) {
				return
			}
			emit("") // the device always publishes; empty payloads are no messages
			emit(fmt.Sprintf(`{"Id":%d,"MaxSize":%d}`, id+u.idOff, max()))
		case strings.Contains(p.payload, `"Md5"`):
			if u.final != "" {
				emit(u.final)
			}
		}
	}
}

func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64Std(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func runUp(t *testing.T, u *upDevice, typ int, data []byte, mod func(*Options)) (*fakeBus, error) {
	t.Helper()
	b := &fakeBus{}
	u.t = t
	u.attach(b)
	o := fast()
	if mod != nil {
		mod(&o)
	}
	err := New(b, o).Upload(t.Context(), plug(), typ, "x.bin", int64(len(data)), bytes.NewReader(data), dlPassword, nil)
	return b, err
}

func TestUploadPayloadStaysInBudgetAndFollowsTheLatestMaxSize(t *testing.T) {
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i * 13)
	}
	// 861 is what a device announces for the topic "plug" ((1174-21)/4*3-2); then it shrinks.
	u := &upDevice{maxes: []int{861, 861, 500, 300}, final: `{"FileUpload":"Done"}`}
	_, err := runUp(t, u, UploadFirmware, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.data.Bytes(), data) {
		t.Fatal("the device did not receive the bytes in order")
	}
	if u.chunks[0] != 861 || u.chunks[1] != 861 || u.chunks[2] != 500 || u.chunks[3] != 300 {
		t.Fatalf("chunks %v do not follow the announced MaxSize", u.chunks)
	}
	if u.widest < 1150 || u.widest > 1174 { // 861 raw bytes: 1148 base64 + 16..20 framing (the Id has 1 to 3 digits); the budget is 1174
		t.Fatalf("widest payload %d", u.widest)
	}
}

// Why: a device that announces more than fits its own buffer must not get a packet that closes
// its connection: the uploader refuses to publish it and ends the transfer.
func TestUploadRefusesAnOversizedChunkAndAborts(t *testing.T) {
	u := &upDevice{maxes: []int{2000}, final: `{"FileUpload":"Done"}`}
	b, err := runUp(t, u, UploadFirmware, make([]byte, 3000), nil)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}
	if u.aborts != 2 || u.data.Len() != 0 {
		t.Fatalf("aborts %d, data %d, sent %d", u.aborts, u.data.Len(), len(b.sent()))
	}
}

// Why: the uploader must not become a generic file pusher, and an unsendable request publishes
// nothing (the start carries the password).
func TestUploadRefusesBeforePublishing(t *testing.T) {
	ok := bytes.Repeat([]byte{1}, 4096)
	cases := []struct {
		name string
		typ  int
		file string
		pw   string
		size int64
	}{
		{"type 3", 3, "x", dlPassword, 4096},
		{"type 0", 0, "x", dlPassword, 4096},
		{"no password", 2, "x", "", 4096},
		{"quote in password", 2, "x", `a"b`, 4096},
		{"quote in name", 2, `a"b`, dlPassword, 4096},
		{"empty name", 2, "", dlPassword, 4096},
		{"dump too big", 2, "x", dlPassword, 20000},
		{"zero size", 1, "x", dlPassword, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBus{}
			err := newChan(b).Upload(t.Context(), plug(), tc.typ, tc.file, tc.size, bytes.NewReader(ok), tc.pw, nil)
			if !errors.Is(err, ErrRefused) || len(b.sent()) != 0 {
				t.Fatalf("%v, sent %v", err, b.sent())
			}
		})
	}
	b := &fakeBus{}
	d := plug()
	d.Topic = "tasmotas"
	if err := newChan(b).Upload(t.Context(), d, 2, "x", 4096, bytes.NewReader(ok), dlPassword, nil); !errors.Is(err, ErrRefused) || len(b.sent()) != 0 {
		t.Fatalf("group topic: %v", err)
	}
}

// Why (never publish a start with a cancelled ctx): the start carries the password.
func TestUploadWithACancelledContextPublishesNothing(t *testing.T) {
	b := &fakeBus{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := newChan(b).Upload(ctx, plug(), 2, "x", 4096, bytes.NewReader(make([]byte, 4096)), dlPassword, nil)
	if err == nil || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}

// Why: Error 1 is the wrong-password signal (the caller marks the password bad); the device
// reset itself, so no abort follows, and the password is in no error.
func TestUploadErrorCodes(t *testing.T) {
	for code, want := range map[int]string{3: "type", 5: "group topic", 7: "Error 7"} {
		u := &upDevice{maxes: []int{861}}
		b := &fakeBus{}
		u.t = t
		u.attach(b)
		b.onPub = wrapStart(b.onPub, fmt.Sprintf(`{"FileUpload":"Error %d"}`, code))
		err := newChan(b).Upload(t.Context(), plug(), 2, "x", 4096, bytes.NewReader(make([]byte, 4096)), dlPassword, nil)
		if err == nil || errors.Is(err, ErrPasswordRefused) || !errors.Is(err, ErrTransfer) || !strings.Contains(err.Error(), want) {
			t.Errorf("Error %d: %v", code, err)
		}
	}
	b := &fakeBus{}
	b.onPub = wrapStart(nil, `{"FileUpload":"Error 1"}`)
	err := newChan(b).Upload(t.Context(), plug(), 2, "x", 4096, bytes.NewReader(make([]byte, 4096)), dlPassword, nil)
	if !errors.Is(err, ErrPasswordRefused) || strings.Contains(err.Error(), dlPassword) {
		t.Fatalf("%v", err)
	}
	aborts := 0
	for _, p := range b.sent() {
		if p.payload == "0" {
			aborts++
		}
	}
	if aborts != 1 { // only the opening one
		t.Fatalf("aborts %d: Error N must not be followed by an abort", aborts)
	}
}

// wrapStart answers the start with reply instead of Started and delegates the rest.
func wrapStart(inner func(*fakeBus, pub), reply string) func(*fakeBus, pub) {
	return func(b *fakeBus, p pub) {
		if strings.Contains(p.payload, "Password") {
			b.emit(mqttbus.KindFile, p.topic, "FILEUPLOAD", reply, false)
			return
		}
		if p.payload == "0" {
			b.emit(mqttbus.KindFile, p.topic, "FILEUPLOAD", `{"FileUpload":"Aborted"}`, false)
			return
		}
		if inner != nil {
			inner(b, p)
		}
	}
}

// Why (fail closed): every irregularity after the start ends with FileUpload 0 (open + close =
// 2 aborts), returns a typed error and never resends.
func TestUploadFaultsAbort(t *testing.T) {
	data := bytes.Repeat([]byte{9}, 2000)
	cases := []struct {
		name string
		u    *upDevice
		typ  int
		data []byte
		want string
		tout bool
	}{
		{"ack lost", &upDevice{maxes: []int{861}, dropAck: 2}, 1, data, "no answer", true},
		{"ack of another transfer", &upDevice{maxes: []int{861}, idOff: 1}, 1, data, "another transfer", false},
		{"no confirmation of the start", &upDevice{maxes: []int{861}}, 1, data, "", true},
		{"md5 mismatch", &upDevice{maxes: []int{861}, final: `{"FileUpload":"MD5 mismatch"}`}, 1, data, "MD5", false},
		{"failed settings", &upDevice{maxes: []int{861}, final: `{"FileUpload":"Failed"}`}, 2, data, "settings dump", false},
		{"failed image", &upDevice{maxes: []int{861}, final: `{"FileUpload":"Failed"}`}, 1, data, "image", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "no confirmation of the start" {
				tc.u.maxes = nil
			}
			b := &fakeBus{}
			tc.u.t = t
			if tc.u.maxes != nil {
				tc.u.attach(b)
			} else {
				b.onPub = wrapStart(nil, "")
				b.onPub = func(b *fakeBus, p pub) {
					if p.payload == "0" {
						b.emit(mqttbus.KindFile, p.topic, "FILEUPLOAD", `{"FileUpload":"Aborted"}`, false)
						tc.u.aborts++
					}
				}
			}
			err := New(b, fast()).Upload(t.Context(), plug(), tc.typ, "x", int64(len(tc.data)), bytes.NewReader(tc.data), dlPassword, nil)
			if err == nil || !errors.Is(err, ErrTransfer) || errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("%v", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks %q", err, tc.want)
			}
			if tc.tout && !errors.Is(err, tasmota.ErrTimeout) {
				t.Fatalf("want a timeout: %v", err)
			}
			if strings.Contains(err.Error(), dlPassword) {
				t.Fatalf("leak: %v", err)
			}
			if tc.u.aborts != 2 {
				t.Fatalf("aborts %d, want the opening one and the closing one", tc.u.aborts)
			}
		})
	}
}

// Why: a reader that is shorter or longer than the announced size would make the device wait
// forever or hash different bytes; both fail and abort.
func TestUploadDataLengthMustMatchSize(t *testing.T) {
	for name, size := range map[string]int64{"short": 3000, "long": 1000} {
		u := &upDevice{maxes: []int{861}, final: `{"FileUpload":"Done"}`}
		b := &fakeBus{}
		u.t = t
		u.attach(b)
		err := New(b, fast()).Upload(t.Context(), plug(), 1, "x", size, bytes.NewReader(make([]byte, 2000)), dlPassword, nil)
		if !errors.Is(err, ErrTransfer) || u.aborts != 2 {
			t.Errorf("%s: %v, aborts %d", name, err, u.aborts)
		}
	}
}

// Why: a lost Done is the nastiest case: the device may have flashed. It must be a typed
// "unknown", never a plain failure the caller could retry, and the simulator proves the flash
// really happened.
func TestUploadLostDoneIsOutcomeUnknownAndTheDeviceAppliedIt(t *testing.T) {
	e := newUploadEnv(t, simulator.ProfileESP32Dual, simulator.Fault{Kind: simulator.FaultMQTTUploadLostDone})
	e.ch = New(e.bus, Options{Gap: 5 * time.Millisecond, ReplyWait: 400 * time.Millisecond})
	err := upload(t.Context(), e.ch, e, UploadFirmware, simImage(t), upPassword)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("want ErrOutcomeUnknown, got %v", err)
	}
	waitFor(t, "the flashed version", func() bool { return e.dev.Version() == "16.1.0" })
	starts := 0
	for _, c := range fileUpCmds(e) {
		if strings.Contains(c, "Password") || strings.Contains(strings.ToLower(c), "\"type\"") {
			starts++
		}
	}
	if starts > 1 {
		t.Fatalf("the upload was resent: %q", fileUpCmds(e))
	}
}

// Why: a link that dies mid-transfer (simulator fault) leaves nothing flashed and the topic idle.
func TestUploadCutMidTransferFailsAndFlashesNothing(t *testing.T) {
	e := newUploadEnv(t, simulator.ProfileESP32Dual, simulator.Fault{Kind: simulator.FaultMQTTUploadCut, Chunks: 1})
	e.ch = New(e.bus, Options{Gap: 5 * time.Millisecond, ReplyWait: 400 * time.Millisecond})
	err := upload(t.Context(), e.ch, e, UploadFirmware, simImage(t), upPassword)
	if !errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("%v", err)
	}
	if e.dev.Version() != "15.1.0" {
		t.Fatalf("version %s", e.dev.Version())
	}
	waitFor(t, "the closing FileUpload 0", func() bool {
		cmds := fileUpCmds(e)
		return strings.HasSuffix(cmds[len(cmds)-1], " 0")
	})
}

// Why (one FMqtt state): the clean flag is shared with Download, so an upload after an unclean
// download opens with FileUpload 0, a second upload after a clean one does not, and a download
// after an unclean upload opens with FileDownload 0.
func TestUploadAndDownloadShareTheCleanState(t *testing.T) {
	zeros := func(b *fakeBus, cmd string) int {
		n := 0
		for _, p := range b.sent() {
			if strings.EqualFold(p.command, cmd) && p.payload == "0" {
				n++
			}
		}
		return n
	}
	good := func(b *fakeBus) *upDevice {
		u := &upDevice{t: t, maxes: []int{861}, final: `{"FileUpload":"Done"}`}
		u.attach(b)
		return u
	}
	data := bytes.Repeat([]byte{4}, 100)
	up := func(c *Channel) error {
		return c.Upload(t.Context(), plug(), 2, "x", 100, bytes.NewReader(data), dlPassword, nil)
	}

	b := &fakeBus{}
	good(b)
	c := newChan(b)
	c.fileClean["plug"] = false // an unclean download
	if err := up(c); err != nil || zeros(b, "FileUpload") != 1 {
		t.Fatalf("%v, FileUpload 0 sent %d times", err, zeros(b, "FileUpload"))
	}
	if err := up(c); err != nil || zeros(b, "FileUpload") != 1 {
		t.Fatalf("a clean device needs no abort: %v, %d", err, zeros(b, "FileUpload"))
	}

	b = &fakeBus{}
	u := good(b)
	u.final = `{"FileUpload":"MD5 mismatch"}`
	c = newChan(b)
	if err := up(c); err == nil {
		t.Fatal("want a failure")
	}
	s := &scriptedDevice{}
	prev := b.onPub
	s.attach(b)
	dlHook := b.onPub
	b.onPub = func(b *fakeBus, p pub) {
		if strings.EqualFold(p.command, "FileDownload") {
			dlHook(b, p)
		} else {
			prev(b, p)
		}
	}
	_, _ = c.Download(t.Context(), plug(), dlPassword) // no steps: times out, only the opening abort matters
	if zeros(b, "FileDownload") < 1 {
		t.Fatal("a download after an unclean upload must open with FileDownload 0")
	}
}

// Why (coordinator notes): empty payloads exist, the "Aborted" of a download is no answer to an
// upload, and an Unknown echo (minimal firmware) carries the password: a clear failure, never repeated.
func TestUploadIgnoresNoiseAndNeverRepeatsAnEcho(t *testing.T) {
	data := bytes.Repeat([]byte{4}, 100)
	b := &fakeBus{}
	u := &upDevice{t: t, maxes: []int{861}, final: `{"FileUpload":"Done"}`}
	u.attach(b)
	inner := b.onPub
	b.onPub = func(b *fakeBus, p pub) {
		if strings.Contains(p.payload, "Password") {
			b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", `{"FileDownload":"Aborted"}`, false)
			b.emit(mqttbus.KindFile, p.topic, "FILEUPLOAD", ``, false)
			b.emit(mqttbus.KindFile, p.topic, "FILEUPLOAD", `{"FileUpload":"Aborted"}`, false)
		}
		inner(b, p)
	}
	if err := New(b, fast()).Upload(t.Context(), plug(), 2, "x", 100, bytes.NewReader(data), dlPassword, nil); err != nil {
		t.Fatal(err)
	}

	b = &fakeBus{}
	echo := fmt.Sprintf(`{"Command":"Unknown","Input":"FILEUPLOAD {\"Password\":\"%s\"}"}`, dlPassword)
	b.onPub = wrapStart(nil, echo)
	err := New(b, fast()).Upload(t.Context(), plug(), 2, "x", 100, bytes.NewReader(data), dlPassword, nil)
	if err == nil || !strings.Contains(err.Error(), "does not support") || strings.Contains(err.Error(), dlPassword) || strings.Contains(err.Error(), "Input") {
		t.Fatalf("%v", err)
	}
}

// Why: a device that is offline per LWT gets nothing published.
func TestUploadOfflineDeviceGetsNothing(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Offline", false)
	err := c.Upload(t.Context(), plug(), 2, "x", 100, bytes.NewReader(make([]byte, 100)), dlPassword, nil)
	if !errors.Is(err, tasmota.ErrUnreachable) || len(b.sent()) != 0 {
		t.Fatalf("%v, sent %v", err, b.sent())
	}
}

func base64Std(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
