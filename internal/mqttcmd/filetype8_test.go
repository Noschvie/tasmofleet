package mqttcmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// type8Header is the header the firmware sends for Type 8: the name without its leading slash, JSON-escaped
// (xdrv_02_1_mqtt_file.ino, MqttFileDownloadValidate: EscapeJSONString(file_name + 1)).
func type8Header(name string, id, size int) string {
	return fmt.Sprintf(`{"File":%q,"Id":%d,"Type":8,"Size":%d}`, name, id, size)
}

func runType8(t *testing.T, steps [][]string, mod func(*Options), path string, max int64) (*scriptedDevice, *fakeBus, []byte, error) {
	t.Helper()
	b := &fakeBus{}
	s := &scriptedDevice{steps: steps}
	s.attach(b)
	o := fast()
	if mod != nil {
		mod(&o)
	}
	got, err := New(b, o).DownloadFile(t.Context(), plug(), dlPassword, path, max)
	return s, b, got, err
}

func waitAborts(s *scriptedDevice, n int32) int32 {
	deadline := time.Now().Add(5 * time.Second)
	for s.aborted.Load() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return s.aborted.Load()
}

// Why (#924): a device built with USE_UFILESYS_MQTT hands over a file of its file system on its topic, so the
// files of a backup need no HTTP; the bytes must be exactly the file's, checked against the device's MD5, as
// the simulator (modelled on the firmware) serves them.
func TestType8DownloadReturnsExactlyTheNamedFile(t *testing.T) {
	e := newSimEnv(t, simulator.DeviceSpec{MQTTPassword: dlPassword, UfsMQTT: true})
	e.waitConnected(t)
	got, err := e.ch.DownloadFile(t.Context(), e.dvc, dlPassword, "/autoexec.be", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte("print('autoexec')\n"); !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Why: a header that names another file than the one asked for (a stale transfer, a second device on the
// topic) must not be stored under the asked path: the archive would hold the wrong content for a file that a
// restore writes back. The firmware names the file without its leading slash, so that form is the match and
// nothing else is.
func TestType8HeaderMustNameTheAskedFile(t *testing.T) {
	body := []byte("x")
	for _, tc := range []struct {
		name, header string
		ok           bool
	}{
		{"the firmware's form", type8Header("lib/x.be", 3, 1), true},
		{"another file", type8Header("lib/y.be", 3, 1), false},
		{"with the slash the firmware never sends", type8Header("/lib/x.be", 3, 1), false},
		{"a settings header", header(3, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, got, err := runType8(t, [][]string{{started, tc.header}, {data(3, body)}, {fmt.Sprintf(`{"Id":3,"Md5":"%s"}`, md5hex(body))}}, nil, "/lib/x.be", 100)
			if tc.ok {
				if err != nil || !bytes.Equal(got, body) {
					t.Fatalf("%q, %v", got, err)
				}
				if start := b.sent()[1].payload; start != `{"Password":"`+dlPassword+`","Type":8,"File":"/lib/x.be"}` {
					t.Fatalf("start %q", start)
				}
				return
			}
			if err == nil || got != nil || !errors.Is(err, ErrTransfer) {
				t.Fatalf("got %q, %v", got, err)
			}
			if n := waitAborts(s, 2); n != 2 {
				t.Fatalf("the refused transfer must be ended with FileDownload 0, aborts %d", n)
			}
		})
	}
}

// Why (#924): Error 3 to a Type 8 start is how a stock firmware (built without USE_UFILESYS_MQTT) says it sends
// no files over MQTT; the backup then takes the files over HTTP, which is safe only because the device refused
// before it sent anything. The same Error 3 after a header is not that answer: data may have started, so it
// must never read as "this firmware has no file transfer" (T2/T3).
func TestType8ErrorThreeMeansNoFileTransferInThisFirmware(t *testing.T) {
	e := newSimEnv(t, simulator.DeviceSpec{MQTTPassword: dlPassword}) // stock: no USE_UFILESYS_MQTT
	e.waitConnected(t)
	_, err := e.ch.DownloadFile(t.Context(), e.dvc, dlPassword, "/autoexec.be", 1<<20)
	if !errors.Is(err, ErrNoFileTransfer) {
		t.Fatalf("stock firmware: want ErrNoFileTransfer, got %v", err)
	}
	if _, err := e.ch.DownloadFile(t.Context(), e.dvc, "wrong", "/autoexec.be", 1<<20); !errors.Is(err, ErrPasswordRefused) || errors.Is(err, ErrNoFileTransfer) {
		t.Fatalf("a wrong password is checked first and is no 'no file transfer': %v", err)
	}

	_, _, _, err = runType8(t, [][]string{{started, type8Header("x.be", 3, 10)}, {`{"FileDownload":"Error 3"}`}}, nil, "/x.be", 100)
	if err == nil || errors.Is(err, ErrNoFileTransfer) {
		t.Fatalf("Error 3 after the header: %v", err)
	}
	_, _, _, err = runType8(t, [][]string{{`{"FileDownload":"Error 2"}`}}, nil, "/x.be", 100)
	if !errors.Is(err, ErrFileUnavailable) || errors.Is(err, ErrNoFileTransfer) {
		t.Fatalf("Error 2: %v", err)
	}
}

// Why: the backup stores at most its limit per file; a file the device announces larger is refused before a
// single "?" asks for data (each chunk is a publish to the device and a few kB on the broker), and the
// device is left idle with FileDownload 0.
func TestType8HeaderOverTheLimitAbortsBeforeAnyData(t *testing.T) {
	s, b, got, err := runType8(t, [][]string{{started, type8Header("big.be", 3, 5000)}}, nil, "/big.be", 4999)
	if !errors.Is(err, tasmota.ErrFileTooLarge) || got != nil {
		t.Fatalf("got %d bytes, %v", len(got), err)
	}
	if n := waitAborts(s, 2); n != 2 {
		t.Fatalf("want the opening abort and the one that ends the refused transfer, got %d", n)
	}
	for _, p := range b.sent() {
		if p.payload == "?" {
			t.Fatalf("asked for data of a file over the limit: %v", b.sent())
		}
	}
}

// Why: the file name travels inside the start command next to the password; a name the device's JSON parser
// could read differently (quote, backslash, control character), or one that breaks the path rules of the
// file calls (a dot file such as the settings, no leading slash), is refused before anything is published.
func TestType8PathThatCannotBeSentVerbatimPublishesNothing(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	for _, p := range []string{"", "x.be", `/a"b`, `/a\b`, "/a\nb", "/.settings", "/lib/../x"} {
		if _, err := c.DownloadFile(t.Context(), plug(), dlPassword, p, 100); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: %v", p, err)
		}
	}
	if _, err := c.DownloadFile(t.Context(), plug(), "", "/x.be", 100); !errors.Is(err, ErrRefused) {
		t.Errorf("no password: %v", err)
	}
	if _, err := c.DownloadFile(t.Context(), plug(), dlPassword, "/x.be", 0); !errors.Is(err, ErrRefused) {
		t.Errorf("no room for a byte: %v", err)
	}
	if n := len(b.sent()); n != 0 {
		t.Fatalf("published %d commands: %v", n, b.sent())
	}
}

// Why (#924): a path the firmware cannot take is the same refusal over MQTT as over HTTP (tasmota.ErrBadPath), so a
// backup lists that file as "name not accepted" whichever transport read the files, not as "could not be read".
func TestType8PathTheFirmwareCannotTakeIsANameRefusalAsOverHTTP(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	for _, p := range []string{"/aaaaaaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.be", "x.be", `/a"b`} {
		_, err := c.DownloadFile(t.Context(), plug(), dlPassword, p, 100)
		if !errors.Is(err, ErrRefused) || !errors.Is(err, tasmota.ErrBadPath) {
			t.Errorf("%q: %v, want a refusal that is tasmota.ErrBadPath", p, err)
		}
	}
	if n := len(b.sent()); n != 0 {
		t.Fatalf("published %d commands: %v", n, b.sent())
	}
}

// Why: a file is far larger than a settings dump, so the dump's 30 s total does not fit it, but a device that
// answers each "?" slowly (yet within ReplyWait) must still not hold its topic, and the backup the device
// lock, for minutes: the bound follows the announced size.
func TestType8SlowDeviceCannotHoldTheTopicPastItsBound(t *testing.T) {
	b := &fakeBus{}
	var questions atomic.Int32
	b.onPub = func(b *fakeBus, p pub) {
		if p.payload == "0" {
			b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", `{"FileDownload":"Aborted"}`, false)
			return
		}
		go func() {
			time.Sleep(60 * time.Millisecond)
			if strings.Contains(p.payload, "Password") {
				b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", started, false)
				b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", type8Header("slow.be", 4, 2000), false)
				return
			}
			questions.Add(1)
			b.emit(mqttbus.KindFile, p.topic, "FILEDOWNLOAD", data(4, []byte{1}), false)
		}()
	}
	o := fast()
	o.ReplyWait, o.FileTotal = 200*time.Millisecond, 300*time.Millisecond
	start := time.Now()
	_, err := New(b, o).DownloadFile(t.Context(), plug(), dlPassword, "/slow.be", 1<<20)
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	// 2000 bytes are one chunk: the bound is ReplyWait plus one chunk's allowance (above the FileTotal floor),
	// far below the 2000 one-byte answers this device would take.
	if el := time.Since(start); el > 3*time.Second || questions.Load() < 2 {
		t.Fatalf("took %v with %d data answers", el, questions.Load())
	}
}
