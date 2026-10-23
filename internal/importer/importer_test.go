package importer

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

type fakeInv struct {
	mu      sync.Mutex
	devs    []model.Device
	upserts []model.Device
	failFor string
}

func (f *fakeInv) List(context.Context) ([]model.Device, error) { return f.devs, nil }
func (f *fakeInv) Upsert(_ context.Context, _ string, d model.Device) (model.DeviceID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.Name == f.failFor {
		return 0, false, errors.New("invalid: " + strings.Repeat("x", 500))
	}
	f.upserts = append(f.upserts, d)
	for _, e := range f.devs {
		if e.Address == d.Address {
			return e.ID, false, nil
		}
	}
	return model.DeviceID(len(f.upserts)), true, nil
}

type fakeProber struct {
	inflight, peak atomic.Int32
	fail           map[string]bool
}

func (p *fakeProber) Describe(_ context.Context, d model.Device) (model.Device, error) {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		m := p.peak.Load()
		if n <= m || p.peak.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	if p.fail[d.Address] {
		return d, errors.New("unreachable")
	}
	d.MAC = "aa:bb:cc:00:00:01"
	d.Name = "probe-name"
	return d, nil
}

type fakeAudit struct{ events []model.AuditEvent }

func (a *fakeAudit) Record(_ context.Context, e model.AuditEvent) { a.events = append(a.events, e) }

const csvHead = `1,"plug",172.31.13.5,admin,secret-pw,bulb_1,1,1,0,0,1,80,"plug",0,plug,0` + "\n"

func rd(s string) *strings.Reader { return strings.NewReader(s) }

// Why: the dry run is the admin's only look before devices are written. It
// must say what will happen per row, and must not touch the database or devices.
func TestPlanClassifiesAgainstInventoryAndFile(t *testing.T) {
	inv := &fakeInv{devs: []model.Device{{ID: 7, Name: "old", Address: "172.31.13.5", Port: 80}}}
	pr := &fakeProber{}
	im := New(Options{Inventory: inv, Prober: pr})
	csv := csvHead +
		`2,"twin",172.31.13.5,,,bulb_1,1,1,0,0,1,80,"twin",0,twin,0` + "\n" +
		`3,"new",172.31.13.6,,,bulb_1,1,1,0,0,1,80,"new",0,new,0` + "\n"
	p, err := im.Plan(context.Background(), rd(csv), nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range p.Rows {
		got = append(got, r.Action)
	}
	if fmt.Sprint(got) != "[merge skip create]" {
		t.Fatalf("actions = %v", got)
	}
	if len(inv.upserts) != 0 || pr.peak.Load() != 0 {
		t.Fatal("a dry run must neither write nor probe")
	}
	if !p.Probe {
		t.Fatal("probe request must be remembered for Apply")
	}
}

// Why: uploads are untrusted; an oversize file must fail instead of being
// parsed partially or exhausting memory.
func TestPlanRejectsOversizeInputs(t *testing.T) {
	im := New(Options{Inventory: &fakeInv{}})
	big := strings.Repeat("a", MaxFileBytes+1)
	if _, err := im.Plan(context.Background(), rd(big), nil, nil, false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("csv: %v", err)
	}
	if _, err := im.Plan(context.Background(), rd(csvHead), nil, rd(big), false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("key: %v", err)
	}
	var b strings.Builder
	for i := 0; i <= MaxRows; i++ {
		fmt.Fprintf(&b, "%d,\"d%d\",10.%d.%d.1,,,bulb_1,1,1,0,0,1,80,\"d%d\",0,t%d,0\n", i, i, i/250, i%250, i, i)
	}
	if _, err := im.Plan(context.Background(), rd(b.String()), nil, nil, false); !errors.Is(err, ErrTooMany) {
		t.Fatalf("rows: %v", err)
	}
}

func encCell(t *testing.T, key []byte, plain string) string {
	t.Helper()
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	s := g.Seal(nil, iv, []byte(plain), nil)
	ct, tag := s[:len(s)-16], s[len(s)-16:]
	return "enc:v1:" + base64.StdEncoding.EncodeToString(append(append(append([]byte{}, iv...), tag...), ct...))
}

// Why: the key unlocks every stored device password. Its bytes and the
// decrypted passwords must never come back in an error, a log value or the
// report, whatever goes wrong.
func TestSecretsNeverLeakIntoErrorsLogsOrReport(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	keyText := base64.StdEncoding.EncodeToString(key)
	cell := encCell(t, key, "hunter2-plain")
	csv := `1,"plug",172.31.13.5,admin,` + cell + `,bulb_1,1,1,0,0,1,80,"plug",0,plug,0` + "\n"
	inv := &fakeInv{failFor: "plug"}
	au := &fakeAudit{}
	im := New(Options{Inventory: inv, Audit: au})

	p, err := im.Plan(context.Background(), rd(csv), nil, rd(keyText+"\n"), false)
	if err != nil || p.Rows[0].Device.HTTPPassword != "hunter2-plain" {
		t.Fatalf("decrypt: %v %+v", err, p.Rows)
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("plan", "plan", p)
	rep, _ := im.Apply(context.Background(), "admin", p)

	_, badKeyErr := im.Plan(context.Background(), rd(csv), nil, rd("not-a-key-"+keyText), false)
	all := fmt.Sprint(logs.String(), rep, au.events, badKeyErr)
	for _, secret := range []string{"hunter2-plain", keyText, cell} {
		if strings.Contains(all, secret) {
			t.Fatalf("secret %q leaked: %s", secret, all)
		}
	}
	if badKeyErr == nil {
		t.Fatal("a malformed key must be an error")
	}
	if rep.Failed != 1 || rep.Lines[0].Detail == "" {
		t.Fatalf("a failure must be reported with a sentence: %+v", rep)
	}
}

// Why: probing 100 devices must not hammer the network (spec: bounded), and
// one dead device must not block the import (spec 11.2).
func TestApplyBoundsProbesAndKeepsUnreachable(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&b, "%d,\"d%d\",172.31.13.%d,,,bulb_1,1,1,0,0,1,80,\"d%d\",0,t%d,0\n", i, i, i, i, i)
	}
	inv := &fakeInv{}
	pr := &fakeProber{fail: map[string]bool{"172.31.13.3": true}}
	au := &fakeAudit{}
	im := New(Options{Inventory: inv, Prober: pr, Concurrency: 3, Audit: au})
	p, _ := im.Plan(context.Background(), rd(b.String()), nil, nil, true)
	rep, err := im.Apply(context.Background(), "admin", p)
	if err != nil {
		t.Fatal(err)
	}
	if pr.peak.Load() > 3 || pr.peak.Load() < 2 {
		t.Fatalf("peak probes in flight = %d, want 2..3", pr.peak.Load())
	}
	if rep.Created != 12 || rep.Unprobed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if inv.upserts[0].Name != "d1" || inv.upserts[0].MAC == "" {
		t.Fatalf("imported name must win and the probe's MAC be kept: %+v", inv.upserts[0])
	}
	if len(au.events) != 1 || au.events[0].Action != "import.tasmoadmin" || au.events[0].Actor != "admin" {
		t.Fatalf("one summary audit event expected: %+v", au.events)
	}
}

// Why: a name like ../../etc/passwd must stay inert text. The importer never
// builds a path from it, and skipped rows are not written.
func TestPathTraversalNameIsPlainTextAndSkippedRowsAreNotWritten(t *testing.T) {
	csv := `1,"../../etc/passwd",172.31.13.5,,,bulb_1,1,1,0,0,1,80,"../../etc/passwd",0,t,0` + "\n" +
		`2,"=cmd|' /C calc'!A0",172.31.13.5,,,bulb_1,1,1,0,0,1,80,"=cmd|' /C calc'!A0",0,t2,0` + "\n"
	inv := &fakeInv{}
	im := New(Options{Inventory: inv})
	p, err := im.Plan(context.Background(), rd(csv), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := im.Apply(context.Background(), "a", p)
	if len(inv.upserts) != 1 || inv.upserts[0].Name != "../../etc/passwd" || rep.Skipped != 1 {
		t.Fatalf("upserts=%+v rep=%+v", inv.upserts, rep)
	}
}

// Why: the MyConfig hint tells the admin to set FIRMWARE_BASE_URL; a broken
// MyConfig must warn, not abort the device import.
func TestMyConfigHintAndWarning(t *testing.T) {
	im := New(Options{Inventory: &fakeInv{}})
	p, err := im.Plan(context.Background(), rd(csvHead), rd(`{"ota_server_ip":"10.0.0.9","ota_server_port":"9999"}`), nil, false)
	if err != nil || !strings.Contains(p.Hint, "10.0.0.9:9999") {
		t.Fatalf("hint: %q %v", p.Hint, err)
	}
	p, err = im.Plan(context.Background(), rd(csvHead), rd(`{not json`), nil, false)
	if err != nil || len(p.Warnings) != 1 || len(p.Rows) != 1 {
		t.Fatalf("warning: %+v %v", p, err)
	}
}

// Why: cancelling an apply (browser closed) must stop writing and say so.
func TestApplyStopsOnCancel(t *testing.T) {
	inv := &fakeInv{}
	im := New(Options{Inventory: inv})
	p, _ := im.Plan(context.Background(), rd(csvHead), nil, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := im.Apply(ctx, "a", p)
	if !errors.Is(err, context.Canceled) || len(inv.upserts) != 0 || rep.Created != 0 {
		t.Fatalf("err=%v upserts=%d", err, len(inv.upserts))
	}
}

// Why (review F1): one 1 MiB file of junk lines must not become hundreds of
// thousands of retained errors or megabytes of HTML. Too many records abort
// the plan; a few bad rows are reported, capped, with a count of the rest.
func TestPlanBoundsRowErrors(t *testing.T) {
	im := New(Options{Inventory: &fakeInv{}})
	if _, err := im.Plan(context.Background(), rd(strings.Repeat("x\n", 500000)), nil, nil, false); !errors.Is(err, ErrTooMany) {
		t.Fatalf("junk file: %v", err)
	}
	p, err := im.Plan(context.Background(), rd(strings.Repeat("x\n", 300)+csvHead), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Errors) != MaxReportedErrors || p.ErrorsOmitted != 300-MaxReportedErrors || len(p.Rows) != 1 {
		t.Fatalf("errors=%d omitted=%d rows=%d", len(p.Errors), p.ErrorsOmitted, len(p.Rows))
	}
}

// Why (review F8): the admin approves the preview, so it must not say "merge"
// where Upsert will refuse (credentials or MAC-less records are never moved by
// an import, spec 11.1 identity rules).
func TestPlanPredictsRefusedMerges(t *testing.T) {
	inv := &fakeInv{devs: []model.Device{
		{ID: 1, Name: "cred", Address: "172.31.13.50", Port: 80, Topic: "cred", MAC: "aa:bb:cc:00:00:09", HTTPUser: "u", HTTPPassword: "pw"},
		{ID: 2, Name: "nomac", Address: "172.31.13.51", Port: 80, Topic: "nomac"},
		{ID: 3, Name: "ok", Address: "172.31.13.52", Port: 80, Topic: "ok", MAC: "aa:bb:cc:00:00:0a"},
	}}
	im := New(Options{Inventory: inv})
	row := func(id int, topic, ip string) string {
		return fmt.Sprintf("%d,\"n%d\",%s,,,bulb_1,1,1,0,0,1,80,\"n%d\",0,%s,0\n", id, id, ip, id, topic)
	}
	p, err := im.Plan(context.Background(), rd(row(1, "cred", "172.31.13.60")+row(2, "nomac", "172.31.13.61")+row(3, "ok", "172.31.13.62")+row(4, "other", "172.31.13.52")), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range p.Rows {
		got = append(got, r.Action)
	}
	if fmt.Sprint(got) != "[skip skip merge merge]" || !strings.Contains(p.Rows[0].Reason, "will be refused") {
		t.Fatalf("actions %v reasons %q", got, p.Rows[0].Reason)
	}
}

// Why: a stored web user without a password is no credential (spec 11.1), so
// an import that changes the address of such a device is not predicted as
// refused. Otherwise all 27 TasmoAdmin-imported devices would be pinned.
func TestPlanTreatsUserWithoutPasswordAsNoCredentials(t *testing.T) {
	inv := &fakeInv{devs: []model.Device{
		{ID: 1, Name: "plain", Address: "172.31.13.50", Port: 80, Topic: "plain", MAC: "aa:bb:cc:00:00:09", HTTPUser: "admin"},
	}}
	im := New(Options{Inventory: inv})
	p, err := im.Plan(context.Background(), rd("1,\"n\",172.31.13.60,,,bulb_1,1,1,0,0,1,80,\"n\",0,plain,0\n"), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != 1 || p.Rows[0].Action != ActionMerge {
		t.Fatalf("rows = %+v", p.Rows)
	}
}

type checkingInv struct{ fakeInv }

func (*checkingInv) CheckAddress(a string) error {
	if a == "8.8.8.8" {
		return inventory.ErrOutsideNetwork
	}
	return nil
}

// Why: an address the inventory refuses used to be planned as Create and then
// failed on apply. The plan must say Skip, with the reason, and Apply must not try it.
func TestPlanSkipsAddressesTheInventoryRefuses(t *testing.T) {
	inv := &checkingInv{}
	im := New(Options{Inventory: inv})
	csv := `1,"lamp",172.31.13.5,,,bulb_1,1,1,0,0,1,80,"lamp",0,lamp,0` + "\n" + `2,"far",8.8.8.8,,,bulb_1,1,1,0,0,1,80,"far",0,far,0` + "\n"
	p, err := im.Plan(context.Background(), rd(csv), nil, nil, false)
	if err != nil || len(p.Rows) != 2 || p.Rows[0].Action != ActionCreate || p.Rows[1].Action != ActionSkip || !strings.Contains(p.Rows[1].Reason, "outside the configured device networks") {
		t.Fatalf("plan: %v %+v", err, p.Rows)
	}
	rep, _ := im.Apply(context.Background(), "admin", p)
	if rep.Created != 1 || rep.Skipped != 1 || len(inv.upserts) != 1 {
		t.Fatalf("apply wrote the refused row: %+v", rep)
	}
}
