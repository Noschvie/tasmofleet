package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// addrGetter resolves device 1 to the simulator and device 2 to a closed port
// (an offline device).
type addrGetter struct {
	host string
	port int
}

func (g addrGetter) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	if id == 2 {
		return model.Device{ID: id, Address: "127.0.0.1", Port: 1}, nil
	}
	return model.Device{ID: id, Address: g.host, Port: g.port}, nil
}

type readEnv struct {
	rep   *policyReporter
	dev   simulator.Device
	cl    tasmota.Client
	tgt   tasmota.Target
	audit *idAudit
}

func newReadEnv(t *testing.T, backups fakeBackupSvc) readEnv {
	t.Helper()
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, dev.Addr())
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	aud := &idAudit{}
	rep := &policyReporter{
		devices: fakeLister{{ID: 1, Name: "plug"}, {ID: 2, Name: "off"}}, backups: backups, audit: aud,
		reader: settings.New(settings.Options{Devices: addrGetter{host, port}, Client: cl}),
		now:    func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) },
	}
	return readEnv{rep: rep, dev: dev, cl: cl, tgt: tasmota.TargetFor(model.Device{Address: host, Port: port}), audit: aud}
}

func refused(err error) bool {
	var r *web.ValueRefusedError
	return errors.As(err, &r)
}

// Why: "Copy from a device" must never become a way to send an arbitrary
// command: a refused command (Status 0 corrupts ESP8266 replies, a secret or an
// acting command would be sent bare) is refused before any device contact, so
// the simulator's command log stays empty.
func TestReadValueNeverSendsARefusedCommand(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	for _, cmd := range []string{
		"Status0", "Status", "Status 0", "Status 3", "WifiPassword", "MqttPassword", "WebPassword", "Password",
		"Restart", "Reset", "Upgrade", "OtaUrl 1", "Power", "SaveData ", "setoption0", "poweronstate", "", "Backlog Power",
	} {
		_, _, err := env.rep.ReadValue(t.Context(), "alice", 1, cmd)
		if !refused(err) {
			t.Errorf("%q: want a refusal, got %v", cmd, err)
		}
	}
	if got := env.dev.Commands(); len(got) != 0 {
		t.Fatalf("a refused command reached the device: %v", got)
	}
	if _, ok := env.audit.find("policy.copy"); ok {
		t.Error("a refusal that never touched the device is not a copy")
	}
}

// Why: a live value is what the device runs now; it is the preferred source.
func TestReadValueLive(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	v, src, err := env.rep.ReadValue(t.Context(), "alice", 1, "Timezone")
	if err != nil || src.Kind != "live" || v == "" {
		t.Fatalf("v=%q src=%+v err=%v", v, src, err)
	}
	if !src.At.Equal(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("read time %v", src.At)
	}
	if len(env.rep.vals.get(1)) != 0 {
		t.Error("a copy must not feed the live cache (poller semantics)")
	}
	ev, ok := env.audit.find("policy.copy")
	if !ok || ev.Actor != "alice" || ev.Target != "device:1" || !strings.Contains(ev.Detail, `"command":"Timezone"`) || strings.Contains(ev.Detail, v+`"`) {
		t.Errorf("audit: %+v", ev)
	}
}

// Why: an offline device can still teach a rule when a backup knows the
// setting; a setting only a live read can tell must not be invented.
func TestReadValueBackupFallback(t *testing.T) {
	taken := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	env := newReadEnv(t, fakeBackupSvc{
		list: map[model.DeviceID][]model.Backup{2: {{ID: 10, Kind: model.BackupDecoded, TakenAt: taken}}},
		data: map[model.BackupID]string{10: `{"tele_period":300}`},
	})
	v, src, err := env.rep.ReadValue(t.Context(), "alice", 2, "TelePeriod")
	if err != nil || v != "300" || src.Kind != "backup" || !src.At.Equal(taken) {
		t.Fatalf("v=%q src=%+v err=%v", v, src, err)
	}
	if _, _, err := env.rep.ReadValue(t.Context(), "alice", 2, "Sleep"); err == nil || refused(err) {
		t.Errorf("a live-only command on an offline device must fail (not refuse): %v", err)
	}
	if _, _, err := env.rep.ReadValue(t.Context(), "alice", 2, "Hostname"); err == nil {
		t.Error("a backup that does not hold the value cannot answer")
	}
}

// Why: a URL with credentials must never become an expected value (it would be
// stored in the rule, shown and exported). The password stays out of the error
// and the audit trail.
func TestReadValueRefusesCredentials(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	if _, err := env.cl.Command(t.Context(), env.tgt, "OtaUrl http://user:hunter2@192.0.2.5/tasmota.bin.gz"); err != nil {
		t.Fatal(err)
	}
	_, _, err := env.rep.ReadValue(t.Context(), "alice", 1, "OtaUrl")
	if !refused(err) || !strings.Contains(err.Error(), "enter it by hand") {
		t.Fatalf("want the credentials refusal, got %v", err)
	}
	ev, _ := env.audit.find("policy.copy")
	if ev.Result != "refused: the value contains credentials; enter it by hand" {
		t.Errorf("a refused value is audited as refused, got %q", ev.Result)
	}
	if strings.Contains(err.Error()+ev.Detail+ev.Result, "hunter2") {
		t.Error("password leaked")
	}
}

// Why: a plain URL is a fine expected value. OtaUrl is also the one a family
// default is derived from later.
func TestReadValueAcceptsPlainOtaURL(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	if _, err := env.cl.Command(t.Context(), env.tgt, "OtaUrl http://192.0.2.5/tasmota.bin.gz"); err != nil {
		t.Fatal(err)
	}
	v, src, err := env.rep.ReadValue(t.Context(), "alice", 1, "OtaUrl")
	if err != nil || v != "http://192.0.2.5/tasmota.bin.gz" || src.Kind != "live" {
		t.Fatalf("v=%q err=%v", v, err)
	}
}

// Why: the rule page re-judges a draft on every change; if that read devices,
// typing in the editor would poll the fleet.
func TestEvaluateMakesNoDeviceCall(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	rules := []policy.Rule{{Command: "TelePeriod", Want: "300"}, {Command: "Sleep", Want: "50"}}
	env.rep.vals.put(1, map[string]string{"TelePeriod": "300"}, env.rep.clock())
	rep, err := env.rep.Evaluate(t.Context(), rules)
	if err != nil {
		t.Fatal(err)
	}
	if got := env.dev.Commands(); len(got) != 0 {
		t.Fatalf("Evaluate sent %v", got)
	}
	if len(rep.Findings) == 0 || len(rep.Outcomes) == 0 || rep.Values[1]["teleperiod"] != "300" {
		t.Errorf("report: %+v", rep)
	}
	if len(rep.Rules) != 2 || rep.Rules[1].Command != "Sleep" {
		t.Error("Evaluate must judge the given rules, not the saved ones")
	}
}

// Why: Report and Evaluate must be one evaluation: the findings Make compliant
// acts on are exactly the Deviate/Unknown outcomes the tiles count.
func TestReportFindingsAreTheOutcomes(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	env.rep.rules = []policy.Rule{{Command: "TelePeriod", Want: "300"}}
	env.rep.vals.put(1, map[string]string{"TelePeriod": "60"}, env.rep.clock())
	rep, err := env.rep.Report(t.Context(), "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	var bad int
	for _, o := range rep.Outcomes {
		if o.Kind == policy.OutcomeDeviate || o.Kind == policy.OutcomeUnknown {
			bad++
		}
	}
	if bad != len(rep.Findings) || bad != 2 { // device 1 deviates, device 2 has no value
		t.Errorf("outcomes %d findings %d", bad, len(rep.Findings))
	}
}

// Why: asking for a device that does not exist is a caller mistake, not a dead
// device; the message must say so and nothing is sent or audited.
func TestReadValueUnknownDevice(t *testing.T) {
	env := newReadEnv(t, fakeBackupSvc{})
	_, _, err := env.rep.ReadValue(t.Context(), "alice", 99, "TelePeriod")
	if !refused(err) || !strings.Contains(err.Error(), "unknown device") {
		t.Fatalf("got %v", err)
	}
	if len(env.dev.Commands()) != 0 {
		t.Error("device contacted")
	}
}
