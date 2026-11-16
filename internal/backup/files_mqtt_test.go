package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/simulator"
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
	return newMQTTFilesEnvSpec(t, esp32(nil), mac, pass)
}

// noStoredMAC is the mac argument of the env helpers for a device whose inventory entry holds no MAC (a device added by
// topic and not probed yet).
const noStoredMAC = "none"

// newMQTTFilesEnvSpec is newMQTTFilesEnvWith for a simulated device of the given spec.
func newMQTTFilesEnvSpec(t *testing.T, spec simulator.DeviceSpec, mac, pass string, opts ...func(*mqttDevice)) (*filesEnv, *transport.Memory, *credsSeen) {
	t.Helper()
	return newMQTTFilesEnvDev(t, spec, mac, pass, nil, opts...)
}

// newMQTTFilesEnvDev is newMQTTFilesEnvSpec whose stored device edit changes last (nil: no change).
func newMQTTFilesEnvDev(t *testing.T, spec simulator.DeviceSpec, mac, pass string, edit func(*model.Device), opts ...func(*mqttDevice)) (*filesEnv, *transport.Memory, *credsSeen) {
	t.Helper()
	seen := &credsSeen{}
	mem := transport.NewMemory(transport.DefaultFailFor, time.Now)
	var asked []string
	e := newFilesEnv(t, spec, func(o *Options) {
		d, err := o.Devices.GetDevice(t.Context(), 7)
		if err != nil {
			t.Fatal(err)
		}
		switch mac {
		case noStoredMAC:
			mac = ""
		case "":
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
		if edit != nil {
			edit(&d)
		}
		seen.Client, seen.fs = o.Client, o.Client.(tasmota.FileSystem)
		rt := transport.NewRouter(transport.Options{HTTP: seen, Devices: devs{d}, Facts: mqttFacts{mem}, Memory: mem})
		md := mqttDevice{dump: []byte("MQTT-DUMP"), dev: o.Client, asked: &asked}
		for _, f := range opts {
			f(&md)
		}
		rt.SetMQTT(md)
		o.Client, o.Devices = rt, devs{d}
	})
	e.overMQTT = &asked
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

// status5Says is an HTTP client whose host answers Status 5 with raw (another host at the device's address).
type status5Says struct {
	tasmota.Client
	raw string
}

func (s status5Says) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	if cmnd == "Status 5" {
		return json.RawMessage(s.raw), nil
	}
	return s.Client.Command(ctx, t, cmnd)
}

// Why (the security check of #865): the MAC check is the one thing that keeps the stored web password and the files of
// a backup over MQTT away from a host that is not the device. A host that reports a MAC made only of separators (":")
// reports no MAC at all; it must not match an inventory entry that holds no MAC either ("-", a leftover), or the walk
// sends that host the web password and stores its files as this device's. Both sides are compared as normalised MACs,
// and an empty one proves nothing.
func TestAHostThatReportsAnEmptyMACIsNotTheDevice(t *testing.T) {
	e, _, seen := newMQTTFilesEnvDev(t, esp32(nil), "AA:BB:CC:DD:EE:01", "stored-web-password",
		func(d *model.Device) { d.MACs = []string{"AA:BB:CC:DD:EE:01", "-"} })
	seen.Client = status5Says{Client: seen.Client, raw: `{"StatusNET":{"Mac":":"}}`}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kindOf(got, model.BackupFiles); ok {
		t.Fatal("files of a host that reported no MAC were stored with this device's dump")
	}
	if n := dumpNote(t, got); FilesNoteOf(n) != "the device at its address did not report its MAC" {
		t.Errorf("dump note %q", n)
	}
	if got := seen.calls(); len(got) != 0 {
		t.Errorf("the host that reported no MAC received the stored web password with %v", got)
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

// Why (#846): a device added by topic and not probed yet has no MAC in the inventory, so the host at its address can never
// be told to be it. The note must say what the person can do about it (the restore says the same: probe it first), not
// that the device at its address "is not this device", which sends them looking for a DHCP lease that did not move. No
// HTTP question is asked either: nothing it could answer would be comparable.
func TestABackupOverMQTTOfADeviceWithoutAStoredMACSaysSoAndAsksNothingOverHTTP(t *testing.T) {
	for _, httpFails := range []bool{false, true} {
		// While HTTP is known to fail the sentence is the same: "they need HTTP" would send the person to repair a
		// connection, and with HTTP repaired the files would still not be stored without a MAC.
		t.Run(fmt.Sprintf("HTTP known to fail: %v", httpFails), func(t *testing.T) {
			e, mem, http := newMQTTFilesEnvWith(t, noStoredMAC, "")
			if httpFails {
				mem.Fail(7, transport.HTTPFailUnreachable)
			}
			got, err := e.m.Backup(t.Context(), "alice", 7)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := kindOf(got, model.BackupFiles); ok {
				t.Fatal("files were stored for a device that cannot be told from the host at its address")
			}
			n := dumpNote(t, got)
			if FilesNoteOf(n) != "the inventory has no MAC for this device (probe it first)" { // the restore's sentence too
				t.Errorf("dump note %q", n)
			}
			if got := http.requests(); len(got) != 0 {
				t.Errorf("HTTP was asked %v for a device whose identity cannot be checked", got)
			}
		})
	}
}

// Why (#855): the run note and the backups page said "files need HTTP" while HTTP was known to fail, also for a device
// that has no file system: there are no files, and "need HTTP" sends the person to repair a connection for nothing. The
// device on its topic answers whether it has a file system (UfsType over MQTT, "Unknown" in a build without one), so
// the backup says "no filesystem" and asks nothing over HTTP at all: no MAC check, no web password, no file call, also
// while HTTP works.
func TestABackupOverMQTTOfADeviceWithoutAFileSystemAsksNothingOverHTTP(t *testing.T) {
	for _, httpFails := range []bool{true, false} {
		t.Run(fmt.Sprintf("HTTP known to fail: %v", httpFails), func(t *testing.T) {
			e, mem, http := newMQTTFilesEnvSpec(t, simulator.DeviceSpec{Profile: simulator.ProfileESP32, NoFS: true}, "", "stored-web-password")
			if httpFails {
				mem.Fail(7, transport.HTTPFailUnreachable)
			}
			got, err := e.m.Backup(t.Context(), "alice", 7)
			if err != nil {
				t.Fatal(err)
			}
			if n := dumpNote(t, got); FilesNoteOf(n) != "no filesystem" {
				t.Errorf("files note %q, want %q (dump note %q)", FilesNoteOf(n), "no filesystem", n)
			}
			if got := http.requests(); len(got) != 0 {
				t.Errorf("HTTP was asked %v about a device that said on its topic it has no file system", got)
			}
			if got := fmt.Sprint(*e.overMQTT); got != "[UfsType]" {
				t.Errorf("asked over MQTT %s, want [UfsType]", got)
			}
		})
	}
}

// ufsUnreachable is the device on its topic while MQTT provably delivers nothing for the file system question (offline
// by LWT between the dump and the question): every Ufs command fails as unreachable, the rest answers as the device.
type ufsUnreachable struct{ tasmota.Client }

func (u ufsUnreachable) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	if strings.HasPrefix(cmnd, "Ufs") {
		return nil, fmt.Errorf("%w: offline by LWT", tasmota.ErrUnreachable)
	}
	return u.Client.Command(ctx, t, cmnd)
}

// Why (T15, the security check of #865): the file system question of a backup over MQTT carries no web password and
// needs no identity check only because it stays on the topic. UfsType, UfsSize and UfsFree count as writes, and a write
// that MQTT provably did not deliver fails over to HTTP once: there it would reach the host at the device's address
// with the stored web password before the MAC check. So the question must never leave MQTT: when the topic leaves it
// unanswered, the first HTTP request is the MAC check (Status 5), and no Ufs command goes over HTTP at all.
func TestTheFileSystemQuestionOfABackupOverMQTTNeverFailsOverToHTTP(t *testing.T) {
	e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "stored-web-password",
		func(m *mqttDevice) { m.dev = ufsUnreachable{m.dev} })
	if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
		t.Fatal(err)
	}
	reqs := seen.requests()
	if len(reqs) == 0 || reqs[0] != "Status 5" {
		t.Errorf("HTTP requests %v: the MAC check (Status 5) must be the first HTTP request", reqs)
	}
	for _, c := range reqs {
		if strings.HasPrefix(c, "Ufs") {
			t.Errorf("the file system question went over HTTP (%s) before the MAC check; requests %v, with the web password %v", c, reqs, seen.calls())
		}
	}
}

// Why (#855): the audit "via" of a backup named "http+mqtt" because the MAC check and UfsType went over HTTP, although
// the dump (the settings, the part with the secrets) came over MQTT. "via" names the transport of the dump and its
// status snapshot; the HTTP calls beside it (the files cannot come over MQTT) are listed apart, so the audit still
// shows every HTTP request of the run without calling the whole backup an HTTP one.
func TestBackupAuditNamesTheDumpsTransportAndListsTheHTTPCallsBesideIt(t *testing.T) {
	detailOf := func(t *testing.T, e *filesEnv) map[string]any {
		t.Helper()
		if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
			t.Fatal(err)
		}
		if len(e.audit.ev) != 1 {
			t.Fatalf("audit: %+v", e.audit.ev)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(e.audit.ev[0].Detail), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	t.Run("a device with files: the file system over MQTT, the MAC check and the files over HTTP", func(t *testing.T) {
		e, _, http := newMQTTFilesEnvWith(t, "", "")
		d := detailOf(t, e)
		if d["via"] != "mqtt" {
			t.Errorf("via = %v, want mqtt", d["via"])
		}
		if got, want := fmt.Sprint(d["http_side"]), "[mac check files]"; got != want {
			t.Errorf("http_side = %v, want %v", got, want)
		}
		if got, want := fmt.Sprint(*e.overMQTT), "[UfsType UfsSize UfsFree]"; got != want {
			t.Errorf("asked over MQTT %s, want %s", got, want)
		}
		for _, r := range http.requests() {
			if r == "FSInfo" || strings.HasPrefix(r, "Ufs") {
				t.Errorf("the file system was asked over HTTP (%v) although the device answered on its topic", http.requests())
			}
		}
	})
	t.Run("a device without a file system: nothing over HTTP", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvSpec(t, simulator.DeviceSpec{Profile: simulator.ProfileESP32, NoFS: true}, "", "")
		d := detailOf(t, e)
		if d["via"] != "mqtt" {
			t.Errorf("via = %v, want mqtt", d["via"])
		}
		if side, ok := d["http_side"]; ok {
			t.Errorf("http_side = %v, want none", side)
		}
	})
	t.Run("the file system unanswered over MQTT: asked over HTTP after the MAC check", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvSpec(t, esp32(nil), "", "", func(m *mqttDevice) { m.dev = nil })
		d := detailOf(t, e)
		if d["via"] != "mqtt" {
			t.Errorf("via = %v, want mqtt", d["via"])
		}
		if got, want := fmt.Sprint(d["http_side"]), "[mac check file system files]"; got != want {
			t.Errorf("http_side = %v, want %v", got, want)
		}
	})
	t.Run("HTTP known to fail: nothing was sent, so nothing is listed", func(t *testing.T) {
		e, mem, _ := newMQTTFilesEnvWith(t, "", "")
		mem.Fail(7, transport.HTTPFailUnreachable)
		d := detailOf(t, e)
		if d["via"] != "mqtt" {
			t.Errorf("via = %v, want mqtt", d["via"])
		}
		if side, ok := d["http_side"]; ok {
			t.Errorf("http_side = %v, want none", side)
		}
	})
}

// reportOf takes a backup with a Report attached, as the device page and the engine's backup job do, and returns what the
// run reported with the "via" its audit entry holds.
func reportOf(t *testing.T, e *filesEnv) (rep Report, auditVia string) {
	t.Helper()
	ctx, r := WithReport(t.Context())
	if _, err := e.m.Backup(ctx, "alice", 7); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(e.audit.ev[len(e.audit.ev)-1].Detail), &d); err != nil {
		t.Fatal(err)
	}
	via, _ := d["via"].(string)
	return *r, via
}

// Why (#926): the result of a backup is worded from what the run measured, the same measurement the audit keeps, not from
// a trace of every call of the run: that trace reads "http+mqtt" for a dump over MQTT whose files came over HTTP, and says
// nothing about which part used which. The dump's transport is the audit's "via"; the files' transport is reported only
// when a files archive was stored, because a result that says "files over HTTP" for a run that stored none would be wrong.
func TestTheRunReportMatchesTheAuditForDumpAndFiles(t *testing.T) {
	t.Run("MQTT dump with files: the files came over HTTP", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvWith(t, "", "")
		rep, via := reportOf(t, e)
		if rep.Dump != "mqtt" || rep.Files != "http" || via != rep.Dump {
			t.Errorf("report %+v, audit via %q; want dump mqtt, files http, the audit's via", rep, via)
		}
	})
	t.Run("MQTT dump on a device without a file system: no files to name", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvSpec(t, simulator.DeviceSpec{Profile: simulator.ProfileESP32, NoFS: true}, "", "")
		if rep, _ := reportOf(t, e); rep.Dump != "mqtt" || rep.Files != "" {
			t.Errorf("report %+v, want dump mqtt and no files", rep)
		}
	})
	t.Run("MQTT dump, the MAC check refuses the host: the files were asked and not stored", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvWith(t, "aa:bb:cc:dd:ee:ff", "")
		if rep, _ := reportOf(t, e); rep.Dump != "mqtt" || rep.Files != "" {
			t.Errorf("report %+v, want dump mqtt and no files", rep)
		}
	})
	t.Run("MQTT dump, the archive cannot be indexed: the files were read and lost", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvWith(t, "", "")
		e.m.o.Store = failFilesStore{e.st}
		if rep, _ := reportOf(t, e); rep.Dump != "mqtt" || rep.Files != "" {
			t.Errorf("report %+v, want dump mqtt and no files: a lost archive is not a stored one", rep)
		}
	})
	t.Run("HTTP dump with files: both over HTTP", func(t *testing.T) {
		e := newFilesEnv(t, esp32(map[string]simulator.FSFile{"/a.be": {Data: []byte("a")}}), nil)
		rep, via := reportOf(t, e)
		if rep.Dump != "http" || rep.Files != "http" || via != rep.Dump {
			t.Errorf("report %+v, audit via %q; want http for both", rep, via)
		}
	})
	t.Run("a backup without a Report attached works as before", func(t *testing.T) {
		e, _, _ := newMQTTFilesEnvWith(t, "", "")
		if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
			t.Fatal(err)
		}
	})
}
