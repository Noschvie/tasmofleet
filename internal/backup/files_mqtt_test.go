package backup

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// newMQTTFilesEnv backs up a simulated device through a real transport.Router whose MQTT channel hands out the dump
// (FileDownload) and whose HTTP is the simulator's real HTTP client: the device is live on TasmoFleet's broker and HTTP
// is not known to fail, so the dump goes over MQTT first (#607). mac is the MAC the inventory holds for the device;
// "" takes the simulator's own.
func newMQTTFilesEnv(t *testing.T, mac string) (*filesEnv, *transport.Memory) {
	t.Helper()
	e, mem, _ := newMQTTFilesEnvWith(t, mac, "")
	return e, mem
}

// newMQTTFilesEnvWith is newMQTTFilesEnv for a device whose stored web password is pass; the returned credsSeen records
// every HTTP request that carries it.
func newMQTTFilesEnvWith(t *testing.T, mac, pass string) (*filesEnv, *transport.Memory, *credsSeen) {
	t.Helper()
	seen := &credsSeen{}
	mem := transport.NewMemory(transport.DefaultFailFor, time.Now)
	e := newFilesEnv(t, esp32(nil), func(o *Options) {
		d, err := o.Devices.GetDevice(t.Context(), 7)
		if err != nil {
			t.Fatal(err)
		}
		if mac == "" {
			raw, err := o.Client.Command(t.Context(), tasmota.TargetFor(model.Device{Address: d.Address, Port: d.Port}), "Status 5")
			if err != nil {
				t.Fatal(err)
			}
			var s5 struct{ StatusNET struct{ Mac string } }
			if err := json.Unmarshal(raw, &s5); err != nil || s5.StatusNET.Mac == "" {
				t.Fatalf("Status 5 %s: %v", raw, err)
			}
			mac = s5.StatusNET.Mac
		}
		d.Topic, d.MQTT, d.MQTTPasswordMode, d.MQTTPassword = "plug", model.MQTTOurs, model.MQTTPasswordOwn, "pw"
		d.MAC, d.Identity.Version, d.HTTPPassword = mac, "16.0.0", pass
		seen.Client, seen.fs = o.Client, o.Client.(tasmota.FileSystem)
		rt := transport.NewRouter(transport.Options{HTTP: seen, Devices: devs{d}, Facts: mqttFacts{mem}, Memory: mem})
		rt.SetMQTT(mqttDevice{dump: []byte("MQTT-DUMP")})
		o.Client, o.Devices = rt, devs{d}
	})
	return e, mem, seen
}

// credsSeen is the HTTP client under the Router: it records every request, and separately those that carry the device's
// stored web credentials.
type credsSeen struct {
	tasmota.Client
	fs   tasmota.FileSystem
	mu   sync.Mutex
	all  []string
	seen []string
}

func (c *credsSeen) note(t tasmota.Target, what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.all = append(c.all, what)
	if t.HasCredentials() {
		c.seen = append(c.seen, what)
	}
}

func (c *credsSeen) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func (c *credsSeen) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.all...)
}

func (c *credsSeen) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	c.note(t, cmnd)
	return c.Client.Command(ctx, t, cmnd)
}

func (c *credsSeen) DownloadConfig(ctx context.Context, t tasmota.Target) ([]byte, error) {
	c.note(t, "/dl")
	return c.Client.DownloadConfig(ctx, t)
}

func (c *credsSeen) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	c.note(t, "FSInfo")
	return c.fs.FSInfo(ctx, t)
}

func (c *credsSeen) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	c.note(t, "ListFiles")
	return c.fs.ListFiles(ctx, t, dir)
}

func (c *credsSeen) DownloadFile(ctx context.Context, t tasmota.Target, p string, max int64) ([]byte, error) {
	c.note(t, "DownloadFile")
	return c.fs.DownloadFile(ctx, t, p, max)
}

func (c *credsSeen) UploadFile(ctx context.Context, t tasmota.Target, p string, data []byte) error {
	c.note(t, "UploadFile")
	return c.fs.UploadFile(ctx, t, p, data)
}

// Why (#607): a backup over MQTT first must not lose the files archive that a backup over HTTP kept (#324, 0.26.0):
// Tasmota has no file transfer over MQTT, so after the dump over MQTT the files are read over HTTP while HTTP works.
// The archive holds the device's files, the dump is the one MQTT carried, and the audit names both transports.
func TestABackupOverMQTTFirstKeepsItsFilesArchive(t *testing.T) {
	e, _ := newMQTTFilesEnv(t, "")
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := kindOf(got, model.BackupConfigDump)
	if dmp, _, err := e.m.Open(t.Context(), d.ID); err != nil || string(dmp) != "MQTT-DUMP" {
		t.Fatalf("dump %q, %v: the dump must come over MQTT", dmp, err)
	}
	if _, ok := kindOf(got, model.BackupFiles); !ok {
		t.Fatalf("no files archive after a dump over MQTT; dump note %q", d.Note)
	}
	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil || len(a.Manifest.Files) != 3 {
		t.Fatalf("archive %+v, %v", a, err)
	}
	if d.Note != "" {
		t.Errorf("a complete run carries no note: %q", d.Note)
	}
}

// Why (#607): files read over HTTP after a dump over MQTT come from whatever answers at the device's address, which
// may no longer be this device (a DHCP lease handed on). They are stored only when that host reports this device's
// MAC (Status 5 over HTTP right before the files); otherwise the run keeps the dump and says why it holds no files. That
// question is asked without the stored web password and before every request that carries it, so a host that is not
// the device never sees the password (the review's second finding; a host that demands one gets it, as over HTTP).
func TestFilesOfABackupOverMQTTComeOnlyFromTheSameDevice(t *testing.T) {
	e, _, seen := newMQTTFilesEnvWith(t, "aa:bb:cc:dd:ee:ff", "stored-web-password")
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kindOf(got, model.BackupFiles); ok {
		t.Fatal("files of another device were stored with this device's dump")
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"the device at its address is not this device") {
		t.Errorf("dump note %q", n)
	}
	if len(e.flaky.downloads) != 0 {
		t.Errorf("files were read from the other host: %v", e.flaky.downloads)
	}
	if got := seen.calls(); len(got) != 0 {
		t.Errorf("the other host received the stored web password with %v", got)
	}
}

// Why (#607): while the device's HTTP is known to fail, a backup over MQTT keeps its dump and says that the files need
// HTTP, without one HTTP request (each would wait for a timeout).
func TestABackupOverMQTTWhileHTTPFailsSaysTheFilesNeedHTTP(t *testing.T) {
	e, mem, http := newMQTTFilesEnvWith(t, "", "")
	mem.Fail(7, transport.HTTPFailUnreachable)
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if n := dumpNote(t, got); !strings.Contains(n, NoteFilesPrefix+"they need HTTP") {
		t.Errorf("dump note %q", n)
	}
	if got := http.requests(); len(got) != 0 {
		t.Errorf("HTTP was asked %v: also the identity question before the files would only wait for a timeout", got)
	}
}
