//go:build integration

package ota

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// policyIT is the production pieces of "Make compliant" over the simulator:
// the real client, settings reader, Fixer, engine and store.
type policyIT struct {
	*itEnv
	client tasmota.Client
	rd     *settings.Manager
	eng    *Engine
}

func newPolicyIT(t *testing.T) *policyIT {
	t.Helper()
	e := newItEnv(t)
	client := tasmota.NewHTTPClient(tasmota.Options{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Timeout:         3 * time.Second,
	})
	p := &policyIT{itEnv: e, client: client, rd: settings.New(settings.Options{Devices: e.st, Client: client})}
	fixer := &policy.Fixer{Client: client, Reader: p.rd, Wait: func(context.Context, time.Duration) error { return nil }}
	p.eng = NewEngine(Options{Machine: e.machine, MaxParallel: 2, Audit: e.audits, Policy: itApplier{fixer}}, e.st, e.st, nil, nil, nil, e.clk, e.log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = p.eng.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return p
}

type itApplier struct{ f *policy.Fixer }

func (a itApplier) Apply(ctx context.Context, d model.Device, l []string) error {
	return a.f.Apply(ctx, d, l)
}
func (a itApplier) Verify(ctx context.Context, d model.Device, l []string) error {
	_, err := a.f.Verify(ctx, d, l)
	return err
}

// plan reads the device live and builds the "Make compliant" plan the web layer would.
func (p *policyIT) plan(rules []policy.Rule, id model.DeviceID) policy.FixPlan {
	p.t.Helper()
	d, err := p.st.GetDevice(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	var keys []string
	for _, r := range rules {
		keys = append(keys, r.Command)
	}
	vals, err := p.rd.ReadKeys(context.Background(), id, keys)
	if err != nil {
		p.t.Fatal(err)
	}
	fs := policy.Check(rules, []model.Device{d}, map[model.DeviceID]map[string]string{id: vals})
	plans := policy.PlanFix(fs, func(model.DeviceID, string) bool { return true })
	if len(plans) == 0 {
		return policy.FixPlan{DeviceID: id}
	}
	return plans[0]
}

func (p *policyIT) send(id model.DeviceID, cmd string) {
	p.t.Helper()
	d, _ := p.st.GetDevice(context.Background(), id)
	if _, err := p.client.Command(context.Background(), tasmota.TargetFor(d), cmd); err != nil {
		p.t.Fatalf("%s: %v", cmd, err)
	}
}

// powerCycle reboots without a save and waits until the device is back.
func (p *policyIT) powerCycle(dev model.DeviceID) {
	p.t.Helper()
	n := p.ctrl[dev].Boots()
	p.ctrl[dev].PowerCycle()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.ctrl[dev].WaitFor(ctx, func(c simulator.Controller) bool { return c.Boots() > n && c.Phase() == simulator.PhaseRun }); err != nil {
		p.t.Fatal(err)
	}
}

func count(cmds []string, prefix string) int {
	n := 0
	for _, c := range cmds {
		if strings.HasPrefix(strings.ToLower(c), strings.ToLower(prefix)) {
			n++
		}
	}
	return n
}

var itRules = []policy.Rule{
	{Command: "PowerOnState", Want: "0"},
	{Command: "SetOption4", Want: "0"},
	{Command: "TelePeriod", Want: "300"},
}

// Why: the whole point of the feature on real firmware behaviour: the drifted
// settings end up compliant AND in flash (they survive a power cut, which drops
// RAM-only values), written by exactly one SaveData, without a restart.
func TestITMakeCompliantSavesOnceAndKeepsTheDeviceRunning(t *testing.T) {
	p := newPolicyIT(t)
	dev := p.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	p.send(dev, "PowerOnState 3")
	p.send(dev, "SetOption4 1")
	p.send(dev, "TelePeriod 60")
	p.send(dev, "SaveData 0")
	p.powerCycle(dev) // start from a state that is in flash
	boots := p.ctrl[dev].Boots()
	before := len(p.ctrl[dev].Commands())

	plan := p.plan(itRules, dev)
	if got := strings.Join(plan.Lines(), "|"); got != "PowerOnState 0|SetOption4 0|TelePeriod 300" {
		t.Fatalf("plan %q", got)
	}
	id, skipped, err := p.eng.SubmitPolicy(context.Background(), "it", []PolicyRequest{{DeviceID: dev, Commands: plan.Lines()}})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%v %v", err, skipped)
	}
	j := p.waitTerminal(dev)
	if j.State != model.JobSucceeded || j.BatchID != id {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}

	sent := p.ctrl[dev].Commands()[before:]
	if count(sent, "Backlog") != 1 || count(sent, "SaveData") != 1 {
		t.Fatalf("want one Backlog and exactly one SaveData, device saw %q", sent)
	}
	if p.ctrl[dev].Boots() != boots {
		t.Fatal("no setting here needs a restart; the device must not reboot")
	}
	p.powerCycle(dev) // RAM-only values are lost here
	if rest := p.plan(itRules, dev); len(rest.Commands) != 0 {
		t.Fatalf("after a power cycle the device deviates again, so the values were never saved: %+v", rest.Commands)
	}
	if len(p.audits.evs) < 2 {
		t.Fatalf("audit %+v", p.audits.evs)
	}
}

// Why: a failed device must show failed with the reason, must be retryable
// under a new transaction without touching the devices that worked, and the
// retry must end compliant. A second plan after the fix finds nothing to do,
// which is what the policy page shows as "compliant".
func TestITMakeCompliantFailureThenRetry(t *testing.T) {
	p := newPolicyIT(t)
	good := p.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	bad := p.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{{After: "Backlog", Kind: simulator.FaultAuth}}})
	for _, d := range []model.DeviceID{good, bad} {
		p.send(d, "SetOption4 1")
	}
	gl := p.plan(itRules, good).Lines()

	reqs := []PolicyRequest{
		{DeviceID: good, Commands: gl},
		{DeviceID: bad, Commands: []string{"SetOption4 0"}},
	}
	first, _, err := p.eng.SubmitPolicy(context.Background(), "it", reqs)
	if err != nil {
		t.Fatal(err)
	}
	p.waitTerminal(good)
	jb := p.waitTerminal(bad)
	if jb.State != model.JobFailed || !strings.Contains(jb.LastError, "authentication") {
		t.Fatalf("bad device: %s %q", jb.State, jb.LastError)
	}
	goodCmds := len(p.ctrl[good].Commands())

	p.ctrl[bad].ClearFaults()
	res, err := p.eng.SubmitRetry(context.Background(), "it", first, nil)
	if err != nil || len(res.Devices) != 1 || res.Devices[0] != bad {
		t.Fatalf("retry %+v %v", res, err)
	}
	jr := p.waitJob(bad, func(j model.Job) bool { return j.BatchID == res.Batch && j.State.Terminal() }, "retry done")
	if jr.State != model.JobSucceeded {
		t.Fatalf("retry %s %q", jr.State, jr.LastError)
	}
	if len(p.ctrl[good].Commands()) != goodCmds {
		t.Fatal("the device that worked was touched by the retry")
	}
	if len(p.plan(itRules, bad).Commands) != 0 || len(p.plan(itRules, good).Commands) != 0 {
		t.Fatal("both devices must be compliant now")
	}
	if rb, _ := p.st.GetBatch(context.Background(), res.Batch); rb.RetryOf != first {
		t.Fatalf("retry batch %+v", rb)
	}
}
