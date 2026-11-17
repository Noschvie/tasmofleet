package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

func withType8(mode type8Mode) func(*mqttDevice) { return func(m *mqttDevice) { m.type8 = mode } }

// askedOverMQTT counts the commands of e's device on its topic that start with prefix.
func askedOverMQTT(e *filesEnv, prefix string) int {
	n := 0
	for _, c := range *e.overMQTT {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func auditDetail(t *testing.T, e *filesEnv) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(e.audit.ev[len(e.audit.ev)-1].Detail), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func filesArchive(t *testing.T, e *filesEnv, got []model.Backup) *FilesArchive {
	t.Helper()
	d, _ := kindOf(got, model.BackupConfigDump)
	if _, ok := kindOf(got, model.BackupFiles); !ok {
		t.Fatalf("no files archive; dump note %q", d.Note)
	}
	a, err := e.m.OpenFilesArchive(t.Context(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Why (#924): a remote device reached only over MQTT lost its files on every backup: they were read over HTTP from
// its address, which a remote site does not offer, and the run said "the device took too long to answer". A firmware
// built with USE_UFILESYS_MQTT sends them on its topic, so after a dump over MQTT the listing and every file come over
// MQTT, with no HTTP request at all: no MAC check (the topic is the identity, as for the dump), no web password, no
// file call. The archive, the audit and the result say the files came over MQTT.
func TestABackupOverMQTTOfAType8DeviceStoresItsFilesWithoutAnyHTTPRequest(t *testing.T) {
	e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "stored-web-password", withType8(type8Built))
	ctx, rep := WithReport(t.Context())
	got, err := e.m.Backup(ctx, "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := filesArchive(t, e, got)
	if len(a.Manifest.Files) != 3 || len(a.Manifest.Skipped) != 0 {
		t.Fatalf("manifest %+v", a.Manifest)
	}
	if b, err := a.Read("/lib/helper.be"); err != nil || string(b) != "# helper\n" {
		t.Fatalf("/lib/helper.be %q, %v", b, err)
	}
	if a.Manifest.Via != "mqtt" {
		t.Errorf("manifest via %q, want mqtt", a.Manifest.Via)
	}
	if r := seen.requests(); len(r) != 0 {
		t.Errorf("HTTP requests %v: the files of a Type 8 device need none", r)
	}
	if n := askedOverMQTT(e, "FileDownload8"); n != 3 {
		t.Errorf("%d files over MQTT, want 3: %v", n, *e.overMQTT)
	}
	d := auditDetail(t, e)
	if d["via"] != "mqtt" {
		t.Errorf("audit via %v", d["via"])
	}
	if side, ok := d["http_side"]; ok {
		t.Errorf("http_side = %v: the files over MQTT are no HTTP call beside the dump", side)
	}
	if rep.Dump != "mqtt" || rep.Files != "mqtt" {
		t.Errorf("report %+v, want mqtt for both", *rep)
	}
}

// Why (#924): a stock firmware answers Type 8 with Error 3 before it sends anything of the file, so the files may still
// come over HTTP as before (a local device keeps its files), and that costs a stock device one listing and one Error 3
// on its topic per backup, never a listing of every folder. The archive then says it was read over HTTP.
func TestStockFirmwareFallsBackToHTTPAfterOneType8Error3(t *testing.T) {
	e, _, _ := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8Stock))
	ctx, rep := WithReport(t.Context())
	got, err := e.m.Backup(ctx, "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := filesArchive(t, e, got)
	if len(a.Manifest.Files) != 3 || a.Manifest.Via != "http" {
		t.Fatalf("manifest %+v", a.Manifest)
	}
	if l, f := askedOverMQTT(e, "UfsList"), askedOverMQTT(e, "FileDownload8"); l != 1 || f != 1 {
		t.Errorf("over MQTT %d listings and %d Type 8 starts, want 1 and 1: %v", l, f, *e.overMQTT)
	}
	if got, want := fmt.Sprint(auditDetail(t, e)["http_side"]), "[mac check files]"; got != want {
		t.Errorf("http_side = %v, want %v", got, want)
	}
	if rep.Dump != "mqtt" || rep.Files != "http" {
		t.Errorf("report %+v", *rep)
	}
}

// status5Times is an HTTP client whose device's address does not answer the MAC check in time (a remote site).
type status5Times struct{ tasmota.Client }

func (s status5Times) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	if cmnd == "Status 5" {
		return nil, fmt.Errorf("%w: no answer", tasmota.ErrTimeout)
	}
	return s.Client.Command(ctx, t, cmnd)
}

// Why (#924): the reporter's remote devices said only "the device took too long to answer" about their files, which
// reads like a slow device and points at nothing to fix. When the firmware sends no files over MQTT and HTTP cannot
// be used, the run says both: the files need HTTP because of the firmware, and why HTTP did not carry them.
func TestStockFirmwareWithUnreachableHTTPSaysTheFilesNeedHTTP(t *testing.T) {
	const prefix = "they need HTTP (this firmware sends no files over MQTT), and HTTP could not be used: "
	t.Run("HTTP does not answer", func(t *testing.T) {
		e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8Stock))
		seen.Client = status5Times{seen.Client}
		got, err := e.m.Backup(t.Context(), "alice", 7)
		if err != nil {
			t.Fatal(err)
		}
		if n := FilesNoteOf(dumpNote(t, got)); n != prefix+"the device took too long to answer" {
			t.Errorf("files note %q", n)
		}
	})
	t.Run("HTTP known to fail", func(t *testing.T) {
		e, mem, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8Stock))
		mem.Fail(7, transport.HTTPFailUnreachable)
		got, err := e.m.Backup(t.Context(), "alice", 7)
		if err != nil {
			t.Fatal(err)
		}
		if n := FilesNoteOf(dumpNote(t, got)); n != prefix+"HTTP has not answered recently" {
			t.Errorf("files note %q", n)
		}
		if r := seen.requests(); len(r) != 0 {
			t.Errorf("HTTP requests %v while HTTP is known to fail", r)
		}
	})
}

// Why (#924): Type 8 reads the device's main file system (ufsp) and UfsList lists the selected one (dfsp); with an SD
// card mounted both are the SD card, not the flash TasmoFleet backs up (xdrv_50_filesystem.ino). So only a device
// without an SD card sends its files over MQTT; one with a card keeps the HTTP walk, which selects the flash
// (/ufsd?dir=2), and the run says why the files needed HTTP when that fails.
func TestOnlyADeviceWithoutAnSDCardSendsItsFilesOverMQTT(t *testing.T) {
	for _, sd := range []bool{false, true} {
		t.Run(fmt.Sprintf("SD card %v", sd), func(t *testing.T) {
			e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8Built), func(m *mqttDevice) { m.sd = sd })
			got, err := e.m.Backup(t.Context(), "alice", 7)
			if err != nil {
				t.Fatal(err)
			}
			a := filesArchive(t, e, got)
			mqttCalls := askedOverMQTT(e, "UfsList") + askedOverMQTT(e, "FileDownload8")
			switch {
			case !sd && (a.Manifest.Via != "mqtt" || len(seen.requests()) != 0):
				t.Errorf("no SD card: via %q, HTTP %v", a.Manifest.Via, seen.requests())
			case sd && (a.Manifest.Via != "http" || mqttCalls != 0):
				t.Errorf("SD card: via %q, file calls over MQTT %v", a.Manifest.Via, *e.overMQTT)
			}
		})
	}
	e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8Built), func(m *mqttDevice) { m.sd = true })
	seen.Client = status5Times{seen.Client}
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	if n := FilesNoteOf(dumpNote(t, got)); n != "they need HTTP (an SD card is mounted), and HTTP could not be used: the device took too long to answer" {
		t.Errorf("files note %q", n)
	}
}

// Why (D6): Error 1 means the device's MqttPassword is not the one TasmoFleet holds. The walk must stop at once (each
// further file would publish the wrong password again), say why for every file left, and not move to HTTP: the
// firmware can send files over MQTT, the password is what to fix.
func TestARefusedPasswordStopsTheFilesOverMQTT(t *testing.T) {
	e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8Refused))
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := filesArchive(t, e, got)
	if n := askedOverMQTT(e, "FileDownload8"); n != 1 {
		t.Errorf("%d Type 8 starts, want 1: %v", n, *e.overMQTT)
	}
	for p, why := range skippedReasons(a.Manifest) {
		// the file that got Error 1 says what happened to it, the ones left that they were not read
		if strings.TrimPrefix(why, "not read: ") != "the device refused its MQTT password" {
			t.Errorf("%s skipped: %q", p, why)
		}
	}
	if len(a.Manifest.Files) != 0 || len(a.Manifest.Skipped) != 3 || len(seen.requests()) != 0 {
		t.Errorf("manifest %+v, HTTP %v", a.Manifest, seen.requests())
	}
}

// Why (T2/T3): only an Error 3 to the first file proves the firmware sends nothing over MQTT. Once a file came over
// MQTT, a later failure is that file's: the walk never moves to HTTP, which would mix two sources (the topic and the
// address) in one archive.
func TestAFailureAfterAFileCameOverMQTTNeverMovesTheWalkToHTTP(t *testing.T) {
	e, _, seen := newMQTTFilesEnvSpec(t, esp32(nil), "", "", withType8(type8ThenError3))
	got, err := e.m.Backup(t.Context(), "alice", 7)
	if err != nil {
		t.Fatal(err)
	}
	a := filesArchive(t, e, got)
	if a.Manifest.Via != "mqtt" || len(a.Manifest.Files) != 1 || len(seen.requests()) != 0 {
		t.Errorf("via %q, %d files, HTTP %v", a.Manifest.Via, len(a.Manifest.Files), seen.requests())
	}
	for p, why := range skippedReasons(a.Manifest) {
		if why != "could not be read" {
			t.Errorf("%s skipped: %q", p, why)
		}
	}
}

// stockType8Files is a stock firmware's file system as the MQTT walk sees it: the listings answer, and every Type 8
// start that is published gets Error 3. A file whose path the channel cannot send is refused before anything is
// published (mqttcmd.Channel.DownloadFile), and refuse lets the router refuse the named path (ErrFilesNotOverMQTT).
type stockType8Files struct {
	lists  map[string][]tasmota.FileEntry
	refuse string
	starts int
}

func (s *stockType8Files) ListFiles(_ context.Context, _ tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	return s.lists[dir], nil
}

func (s *stockType8Files) DownloadFile(_ context.Context, _ tasmota.Target, p string, _ int64) ([]byte, error) {
	if err := tasmota.CheckFSPath(p); err != nil {
		return nil, fmt.Errorf("%w: the file path cannot be sent in a file transfer command: %w", mqttcmd.ErrRefused, err)
	}
	if p == s.refuse {
		return nil, fmt.Errorf("%w: MQTT is not connected", transport.ErrFilesNotOverMQTT)
	}
	s.starts++
	return nil, fmt.Errorf("%w: %w: %w", mqttcmd.ErrPublished, mqttcmd.ErrTransfer, mqttcmd.ErrNoFileTransfer)
}

// Why (#924, T2/T3): the fall-back to HTTP after a stock firmware's Error 3 must depend on the first Type 8 start that
// reached the device, not on the first file the walk counted. When the first counted file is refused before anything
// is published (a nested path over 49 bytes, a router refusal), every later file would send the MQTT password over
// the broker only to get Error 3, and the run would store an empty archive "via mqtt" with every file "could not be
// read", while HTTP, which can read them, is never asked. One published Error 3 is the proof; a second start is not
// needed.
func TestTheFallBackAfterError3CountsOnlyStartsThatWerePublished(t *testing.T) {
	const deep = "/aaaaaaaaaaaaaaaaaaaa"
	for _, c := range []struct {
		name, refuse string
		files        []tasmota.FileEntry // of deep, in the order the walk reaches them
	}{
		{"a path the channel cannot send", "",
			[]tasmota.FileEntry{{Name: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.be", Size: 1}, {Name: "c.be", Size: 1}, {Name: "d.be", Size: 1}}},
		{"a router refusal of one file", deep + "/c.be",
			[]tasmota.FileEntry{{Name: "c.be", Size: 1}, {Name: "d.be", Size: 1}, {Name: "e.be", Size: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lists := map[string][]tasmota.FileEntry{"/": {{Name: deep[1:], Dir: true}}, deep: c.files}
			fs := &stockType8Files{lists: lists, refuse: c.refuse}
			w := newWalker(t.Context(), t.Context(), fs, tasmota.Target{}, tasmota.FSInfo{})
			w.overMQTT = true
			if err := w.dir("/", 0); !errors.Is(err, errNoType8) {
				t.Errorf("walk ended with %v, want errNoType8 (files %d, skipped %+v)", err, len(w.man.Files), w.man.Skipped)
			}
			if fs.starts != 1 {
				t.Errorf("%d Type 8 starts published with the MQTT password, want 1", fs.starts)
			}
		})
	}
}
