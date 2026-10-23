package ota

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// The power restore rows of the transition table (spec 9.3). The state is
// read three times (preflight, right before the first Upgrade, first thing
// after the final boot) and switched back on only if the device lost it and
// nobody switched anything off in between; the rules exist so that a load
// the user turned off is never turned on by an update.

const relays13 = model.RelaySet(1<<0 | 1<<2)

func withPower(j model.Job, on model.RelaySet) model.Job {
	j.PowerOn, j.PowerReadAt = on, t0
	return j
}

func TestStepPreflightRecordsRelayState(t *testing.T) {
	runRows(t, []row{
		{
			name: "preflight/identity records the relays that are ON and when",
			in:   job(model.JobPreflight, model.StageFinal), ev: Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: origURL, Uptime: 5000, Power: relays13, PowerRead: true},
			state: model.JobPreflight, actions: []ActionKind{ActPlan},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.PowerOn != relays13 || !j.PowerReadAt.Equal(t0) {
					t.Fatalf("PowerOn = %b at %v", j.PowerOn, j.PowerReadAt)
				}
			},
		},
		{
			name: "preflight/identity leaves PulseTime, Interlock and shutter relays out and says so",
			in:   job(model.JobPreflight, model.StageFinal), ev: Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: origURL, Power: relays13, PowerExcluded: 1 << 2, PowerRead: true},
			state: model.JobPreflight, actions: []ActionKind{ActPlan},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.PowerOn != 1 || !strings.Contains(j.PowerNote, "relays 3 ") {
					t.Fatalf("PowerOn = %b note %q", j.PowerOn, j.PowerNote)
				}
			},
		},
	})
}

// Direct plans (ESP32, 4 MB ESP8266) have only a final stage, which is also
// the first stage on full firmware: it gets the second reading too.
func TestStepSecondReadingOnDirectPlans(t *testing.T) {
	in := withPower(job(model.JobSetOtaURL, model.StageFinal), relays13)
	in.Plan = model.PlanDirect
	if !firstFullStage(in) {
		t.Fatal("the only stage of a direct plan is the first full-firmware stage")
	}
	got, _ := Step(in, Event{Kind: EvCommandOK, PowerRead: true, Power: 1}, t0.Add(time.Minute), testCfg())
	if got.PowerOn != 1 || !strings.Contains(got.PowerNote, "relays 3 were not ON right before the update") {
		t.Fatalf("PowerOn %b note %q", got.PowerOn, got.PowerNote)
	}
	// the final stage of a staged plan runs behind minimal: no reading there
	staged := withPower(job(model.JobSetOtaURL, model.StageFinal), relays13)
	if firstFullStage(staged) {
		t.Fatal("the final stage of a staged plan is not the first full-firmware stage")
	}
}

func TestStepSecondReadingBeforeUpgrade(t *testing.T) {
	in := withPower(job(model.JobSetOtaURL, model.StageMinimal), relays13)
	later := t0.Add(time.Minute)
	got, _ := Step(in, Event{Kind: EvCommandOK, PowerRead: true, Power: 1}, later, testCfg())
	if got.State != model.JobUpgradeIssued || got.PowerOn != 1 || !got.PowerReadAt.Equal(later) {
		t.Fatalf("state %s PowerOn %b at %v: only relays ON in both readings stay candidates", got.State, got.PowerOn, got.PowerReadAt)
	}
	if !strings.Contains(got.PowerNote, "relays 3 were not ON right before the update") {
		t.Fatalf("note %q", got.PowerNote)
	}
	got, _ = Step(in, Event{Kind: EvCommandOK, PowerUnreadable: true}, later, testCfg())
	if got.PowerOn != 0 || !strings.Contains(got.PowerNote, ErrTextPowerUnreadable) {
		t.Fatalf("an unreadable second reading must drop the restore: %b %q", got.PowerOn, got.PowerNote)
	}
	got, _ = Step(in, Event{Kind: EvCommandOK}, later, testCfg())
	if got.PowerOn != 0 || !strings.Contains(got.PowerNote, "not read right before the update") {
		t.Fatalf("the missing reading of the first stage must drop the restore: %b %q", got.PowerOn, got.PowerNote)
	}
	later2 := withPower(job(model.JobSetOtaURL, model.StageFinal), relays13) // staged: the final stage runs behind minimal
	got, _ = Step(later2, Event{Kind: EvCommandOK}, later, testCfg())
	if got.PowerOn != relays13 {
		t.Fatal("a set_ota_url of a later stage has no reading and must not change the candidates")
	}
}

func TestStepPowerReport(t *testing.T) {
	// withCutoff: the device went offline at t0+10s; the state was read at t0
	at := func(d time.Duration) Event { return Event{Kind: EvPowerReport, Power: 1, At: t0.Add(d)} }
	for _, st := range []model.JobState{model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued} {
		in := withPower(job(st, model.StageMinimal), relays13)
		got, as := Step(in, at(time.Second), t0, testCfg())
		if got.PowerOn != 1<<2 || len(as) != 0 || !strings.Contains(got.PowerNote, "relays 1 were switched off during the update") {
			t.Errorf("%s: PowerOn %b note %q actions %v", st, got.PowerOn, got.PowerNote, kinds(as))
		}
		other := at(time.Second)
		other.Power = 1 << 1
		if got, _ = Step(in, other, t0, testCfg()); got.PowerOn != relays13 || got.PowerNote != "" {
			t.Errorf("%s: a report about a relay that was not a candidate changed the job", st)
		}
	}
	// A report received before the state was read is old news.
	in := withPower(job(model.JobBackup, model.StageMinimal), relays13)
	if got, _ := Step(in, at(-time.Second), t0, testCfg()); got.PowerOn != relays13 {
		t.Error("a report from before the reading was taken for a user action")
	}
	// The time of reception decides, not the state the job is in when the
	// report is processed: one received before the device went offline counts
	// in await_reboot too, one received after it is the boot announcement.
	off := withPower(job(model.JobAwaitReboot, model.StageMinimal), relays13)
	off.PowerCutoff = t0.Add(10 * time.Second)
	if got, _ := Step(off, at(5*time.Second), t0.Add(11*time.Second), testCfg()); got.PowerOn != 1<<2 {
		t.Error("a report received before the cutoff was dropped because it was processed late")
	}
	if got, _ := Step(off, at(15*time.Second), t0.Add(16*time.Second), testCfg()); got.PowerOn != relays13 {
		t.Error("a boot announcement after the cutoff was taken for a user action")
	}
	for _, st := range []model.JobState{model.JobQueued, model.JobSucceeded, model.JobFailed, model.JobCancelled} {
		in := withPower(job(st, model.StageFinal), relays13)
		if got, _ := Step(in, at(time.Second), t0, testCfg()); got.PowerOn != relays13 {
			t.Errorf("%s: report changed the job", st)
		}
	}
}

// The cutoff is when the device first went offline, set once.
func TestStepPowerCutoffIsSetOnce(t *testing.T) {
	in := withPower(job(model.JobUpgradeIssued, model.StageMinimal), relays13)
	got, _ := Step(in, Event{Kind: EvProbeOffline}, t0.Add(time.Minute), testCfg())
	if !got.PowerCutoff.Equal(t0.Add(time.Minute)) {
		t.Fatalf("cutoff %v", got.PowerCutoff)
	}
	again := got
	again.State = model.JobUpgradeIssued
	got2, _ := Step(again, Event{Kind: EvProbeOffline}, t0.Add(time.Hour), testCfg())
	if !got2.PowerCutoff.Equal(t0.Add(time.Minute)) {
		t.Fatalf("cutoff moved to %v", got2.PowerCutoff)
	}
}

func TestStepPowerRestoreRows(t *testing.T) {
	final := func(power model.RelaySet, read bool) Event {
		return Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: origURL, Uptime: 3, Power: power, PowerRead: read}
	}
	verify := job(model.JobVerify, model.StageFinal)
	restoreURL := job(model.JobRestoreOtaURL, model.StageFinal)
	restorePwr := withPower(job(model.JobRestorePower, model.StageFinal), relays13)
	runRows(t, []row{
		{
			name: "verify/identity final, every relay OFF after boot (state lost) -> restore_power",
			in:   withPower(verify, relays13), ev: final(0, true),
			state: model.JobRestorePower, actions: []ActionKind{ActRestorePower},
			check: func(t *testing.T, j model.Job, _ []Action) { wantDeadline(t, j, 30e9) },
		},
		{
			name: "verify/identity final, a relay that was ON is ON after boot (device kept its state) -> succeeded, nothing touched",
			in:   withPower(verify, relays13), ev: final(1, true),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.PowerOn != 0 || j.LastError != "" {
					t.Fatalf("PowerOn %b LastError %q", j.PowerOn, j.LastError)
				}
			},
		},
		{
			name: "verify/identity final, reading after boot failed -> succeeded with a note, nothing switched",
			in:   withPower(verify, relays13), ev: Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: origURL, PowerUnreadable: true},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextPowerUnreadable) },
		},
		{
			name: "verify/identity final, no reading at all -> never switch blind",
			in:   withPower(verify, relays13), ev: final(0, false),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "not read") },
		},
		{
			name: "verify/identity final, nothing was ON -> succeeded, no restore",
			in:   verify, ev: final(0, true),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity final, restore turned off -> succeeded",
			in:   func() model.Job { j := withPower(verify, relays13); j.SkipPowerRestore = true; return j }(), ev: final(0, true),
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			name: "verify/identity final with cancel requested still restores power",
			in:   func() model.Job { j := withPower(verify, relays13); j.CancelRequested = true; return j }(), ev: final(0, true),
			state: model.JobRestorePower, actions: []ActionKind{ActRestorePower},
		},
		{
			name: "verify/identity final, OtaUrl differs -> restore_ota_url first",
			in:   withPower(verify, relays13), ev: Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: minimalURL, Uptime: 3, PowerRead: true},
			state: model.JobRestoreOtaURL, actions: []ActionKind{ActRestoreOtaURL},
		},
		{
			name: "restore_ota_url/command_ok with candidates -> restore_power",
			in:   withPower(restoreURL, relays13), ev: Event{Kind: EvCommandOK},
			state: model.JobRestorePower, actions: []ActionKind{ActRestorePower},
		},
		{
			name: "restore_ota_url/command_ok without candidates -> succeeded",
			in:   restoreURL, ev: Event{Kind: EvCommandOK},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
		},
		{
			name: "restore_ota_url/command_failed keeps the note and still restores power",
			in:   withPower(restoreURL, relays13), ev: Event{Kind: EvCommandFailed, Err: "timeout"},
			state: model.JobRestorePower, actions: []ActionKind{ActRestorePower},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextRestoreFailed) },
		},
		{
			name: "restore_power/command_ok -> succeeded, collected notes become LastError",
			in:   func() model.Job { j := restorePwr; j.PowerNote = "relays 2 skipped"; return j }(), ev: Event{Kind: EvCommandOK},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.LastError != "relays 2 skipped" || j.PowerNote != "" {
					t.Fatalf("LastError %q PowerNote %q", j.LastError, j.PowerNote)
				}
			},
		},
		{
			name: "restore_power/command_failed -> succeeded with the reason",
			in:   restorePwr, ev: Event{Kind: EvCommandFailed, Err: "relays 3 still OFF"},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, ErrTextPowerFailed)
				wantErrContains(t, j, "relays 3 still OFF")
			},
		},
		{
			name: "restore_power/timeout -> succeeded with the reason",
			in:   restorePwr, ev: Event{Kind: EvTimeout},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, ErrTextPowerFailed) },
		},
		{
			name: "restore_power/command_failed after a failed OtaUrl restore keeps both notes",
			in:   func() model.Job { j := restorePwr; j.LastError = ErrTextRestoreFailed; return j }(), ev: Event{Kind: EvCommandFailed},
			state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, ErrTextRestoreFailed)
				wantErrContains(t, j, ErrTextPowerFailed)
			},
		},
	})
}

// Nobody expects a load to come back on after a long gap (a crashed host
// that resumes hours later), and the user may have switched it off since.
func TestStepPowerStateTooOld(t *testing.T) {
	cfg := testCfg()
	old := t0.Add(cfg.PowerRestoreMaxAge + time.Minute)
	verify := withPower(job(model.JobVerify, model.StageFinal), relays13)
	got, as := Step(verify, Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: origURL, PowerRead: true}, old, cfg)
	if got.State != model.JobSucceeded || got.PowerOn != 0 || len(as) != 1 || as[0].Kind != ActNotify {
		t.Fatalf("state %s PowerOn %b actions %v", got.State, got.PowerOn, kinds(as))
	}
	wantErrContains(t, got, "too old")
	// the same through resume in restore_power
	got, as = Step(withPower(job(model.JobRestorePower, model.StageFinal), relays13), Event{Kind: EvResume}, old, cfg)
	if got.State != model.JobSucceeded || len(as) != 1 || as[0].Kind != ActNotify {
		t.Fatalf("resume of a stale restore: %s %v", got.State, kinds(as))
	}
	wantErrContains(t, got, "too old")
	// at exactly the limit it still runs
	got, _ = Step(withPower(job(model.JobRestorePower, model.StageFinal), relays13), Event{Kind: EvResume}, t0.Add(cfg.PowerRestoreMaxAge), cfg)
	if got.State != model.JobRestorePower {
		t.Fatalf("restore at the age limit was refused: %s", got.State)
	}
}

// A job that fails or is cancelled after the device was told to flash says
// which relays were ON, so the operator knows what to check.
func TestStepFailureNamesRelaysThatWereOn(t *testing.T) {
	in := withPower(job(model.JobAwaitReboot, model.StageFinal), relays13)
	in.SeenOffline = true
	got, _ := Step(in, Event{Kind: EvTimeout}, t0, testCfg())
	if got.State != model.JobFailed {
		t.Fatalf("state %s", got.State)
	}
	wantErrContains(t, got, "relays 1, 3 were ON before the update")
	// before anything was sent to the device there is nothing to say
	pre := withPower(job(model.JobSetOtaURL, model.StageMinimal), relays13)
	pre.Attempt = 0
	got, _ = Step(pre, Event{Kind: EvCommandFailed, Err: "x"}, t0, testCfg())
	if strings.Contains(got.LastError, "were ON") {
		t.Fatalf("LastError %q mentions relays although the device was not touched", got.LastError)
	}
	// cancelled at the minimal hop
	in = withPower(job(model.JobVerify, model.StageMinimal), relays13)
	in.CancelRequested = true
	got, _ = Step(in, Event{Kind: EvIdentity, Identity: minimalIdent("16.0.0"), OtaURL: minimalURL, Uptime: 4}, t0, testCfg())
	if got.State != model.JobCancelled {
		t.Fatalf("state %s", got.State)
	}
	wantErrContains(t, got, "were ON before the update")
}

// Uncertain readings after the boot fail closed.
func TestStepPowerAfterBootUncertain(t *testing.T) {
	verify := withPower(job(model.JobVerify, model.StageFinal), relays13)
	id := func(e Event) Event {
		e.Kind, e.Identity, e.OtaURL, e.Uptime = EvIdentity, finalIdent(), origURL, 3
		return e
	}
	got, _ := Step(verify, id(Event{PowerRead: true, Power: 0, PowerDeny: "the device now reports another MAC address than the inventory"}), t0, testCfg())
	if got.State != model.JobSucceeded || got.PowerOn != 0 {
		t.Fatalf("a device that cannot be confirmed must not be switched: %s %b", got.State, got.PowerOn)
	}
	wantErrContains(t, got, "another MAC")
	got, as := Step(verify, id(Event{PowerRead: true, PowerUnknown: 1}), t0, testCfg())
	if got.State != model.JobRestorePower || got.PowerOn != 1<<2 || len(as) != 1 {
		t.Fatalf("relay 1 unclear: %s PowerOn %b", got.State, got.PowerOn)
	}
	wantErrContains(t, withPowerNote(got), "relays 1 answered neither ON nor OFF")
	got, _ = Step(verify, id(Event{PowerRead: true, PowerUnknown: relays13}), t0, testCfg())
	if got.State != model.JobSucceeded || got.PowerOn != 0 {
		t.Fatalf("every relay unclear: %s %b", got.State, got.PowerOn)
	}
}

// The minimal hop is not the end: nothing is switched there (minimal has no
// Power command).
func TestStepNoPowerRestoreAfterMinimalHop(t *testing.T) {
	in := withPower(job(model.JobVerify, model.StageMinimal), relays13)
	got, as := Step(in, Event{Kind: EvIdentity, Identity: minimalIdent("16.0.0"), OtaURL: minimalURL, Uptime: 4}, t0, testCfg())
	if got.State != model.JobSetOtaURL || len(as) != 1 || as[0].Kind != ActSetOtaURL {
		t.Fatalf("state %s actions %v", got.State, kinds(as))
	}
}

// RelaySet is what makes Job comparable; its edges decide which relays are
// switched, so they are pinned down.
func TestRelaySet(t *testing.T) {
	var s model.RelaySet
	s = s.Add(1).Add(3).Add(32).Add(0).Add(33).Add(-1)
	if got := s.List(); len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 32 {
		t.Fatalf("List = %v", got)
	}
	if s.Has(2) || !s.Has(3) || s.Has(0) || s.Has(33) {
		t.Fatalf("Has wrong for %b", s)
	}
	if got := relays13.String(); got != "1, 3" {
		t.Fatalf("String = %q", got)
	}
}

// Fail closed: every zero, missing or unknown input on the way to
// restore_power ends in "no restore" (a job note where something was lost),
// never in a switch command. Each row drives the machine through the step
// that could start the restore and asserts it did not.
func TestStepNoRestoreOnZeroOrMissingInput(t *testing.T) {
	final := func(power model.RelaySet, mod func(*Event)) Event {
		ev := Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: origURL, Uptime: 3, Power: power, PowerRead: true}
		if mod != nil {
			mod(&ev)
		}
		return ev
	}
	verify := withPower(job(model.JobVerify, model.StageFinal), relays13)
	cases := []struct {
		name string
		in   model.Job
		ev   Event
		cfg  func(*Config)
	}{
		{"no candidate relays", job(model.JobVerify, model.StageFinal), final(0, nil), nil},
		{"restore turned off", func() model.Job { j := verify; j.SkipPowerRestore = true; return j }(), final(0, nil), nil},
		{"reading time never recorded", func() model.Job { j := verify; j.PowerReadAt = time.Time{}; return j }(), final(0, nil), nil},
		{"no maximum age configured", verify, final(0, nil), func(c *Config) { c.PowerRestoreMaxAge = 0 }},
		{"negative maximum age", verify, final(0, nil), func(c *Config) { c.PowerRestoreMaxAge = -time.Minute }},
		{"reading after the boot missing (zero event)", verify, Event{Kind: EvIdentity, Identity: finalIdent(), OtaURL: origURL}, nil},
		{"reading after the boot flagged unreadable", verify, final(0, func(e *Event) { e.PowerRead, e.PowerUnreadable = false, true }), nil},
		{"reading after the boot not flagged as read", verify, final(0, func(e *Event) { e.PowerRead = false }), nil},
		{"device not confirmed after the boot", verify, final(0, func(e *Event) { e.PowerDeny = "x" }), nil},
		{"every candidate unclear after the boot", verify, final(0, func(e *Event) { e.PowerUnknown = relays13 }), nil},
		{"candidate kept by the device", verify, final(1, nil), nil},
		{"restore_ota_url ok, no candidates", job(model.JobRestoreOtaURL, model.StageFinal), Event{Kind: EvCommandOK}, nil},
		{"restore_ota_url ok, restore off", func() model.Job {
			j := withPower(job(model.JobRestoreOtaURL, model.StageFinal), relays13)
			j.SkipPowerRestore = true
			return j
		}(), Event{Kind: EvCommandOK}, nil},
		{"restore_ota_url ok, state never timestamped", func() model.Job {
			j := withPower(job(model.JobRestoreOtaURL, model.StageFinal), relays13)
			j.PowerReadAt = time.Time{}
			return j
		}(), Event{Kind: EvCommandOK}, nil},
		{"restore_ota_url failed, state too old", withPower(job(model.JobRestoreOtaURL, model.StageFinal), relays13), Event{Kind: EvCommandFailed}, func(c *Config) { c.PowerRestoreMaxAge = time.Nanosecond }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			now := t0
			if tc.name == "restore_ota_url failed, state too old" {
				now = t0.Add(time.Hour)
			}
			got, as := Step(tc.in, tc.ev, now, cfg)
			if got.State == model.JobRestorePower {
				t.Fatalf("entered restore_power from %+v", tc.ev)
			}
			for _, a := range as {
				if a.Kind == ActRestorePower {
					t.Fatalf("requested ActRestorePower")
				}
			}
			if got.State != model.JobSucceeded {
				t.Fatalf("state %s, want the job to end succeeded", got.State)
			}
		})
	}

	// the same through resume: nothing to restore, too old, no limit, no timestamp
	for name, mod := range map[string]func(*model.Job, *Config){
		"nothing to restore": func(j *model.Job, _ *Config) { j.PowerOn = 0 },
		"restore off":        func(j *model.Job, _ *Config) { j.SkipPowerRestore = true },
		"never timestamped":  func(j *model.Job, _ *Config) { j.PowerReadAt = time.Time{} },
		"no maximum age":     func(_ *model.Job, c *Config) { c.PowerRestoreMaxAge = 0 },
	} {
		t.Run("resume/"+name, func(t *testing.T) {
			j, cfg := withPower(job(model.JobRestorePower, model.StageFinal), relays13), testCfg()
			mod(&j, &cfg)
			got, as := Step(j, Event{Kind: EvResume}, t0, cfg)
			for _, a := range as {
				if a.Kind == ActRestorePower {
					t.Fatal("resume requested ActRestorePower")
				}
			}
			if got.State != model.JobSucceeded {
				t.Fatalf("state %s", got.State)
			}
		})
	}

	// Pre-flight and pre-Upgrade: a reading that was not taken restores nothing.
	pre := job(model.JobPreflight, model.StageFinal)
	got, _ := Step(pre, Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: origURL, Power: relays13}, t0, testCfg())
	if got.PowerOn != 0 {
		t.Errorf("preflight identity without PowerRead set candidates %b", got.PowerOn)
	}
	got, _ = Step(pre, Event{Kind: EvIdentity, Identity: plug1M(), OtaURL: origURL}, t0, testCfg())
	if got.PowerOn != 0 {
		t.Errorf("zero preflight event set candidates %b", got.PowerOn)
	}
	first := withPower(job(model.JobSetOtaURL, model.StageMinimal), relays13)
	if got, _ = Step(first, Event{Kind: EvCommandOK}, t0, testCfg()); got.PowerOn != 0 {
		t.Errorf("missing pre-Upgrade reading kept candidates %b", got.PowerOn)
	}
	direct := first
	direct.Plan, direct.Stage = model.PlanDirect, model.StageFinal
	if got, _ = Step(direct, Event{Kind: EvCommandOK}, t0, testCfg()); got.PowerOn != 0 {
		t.Errorf("missing pre-Upgrade reading on a direct plan kept candidates %b", got.PowerOn)
	}

	// No event, known or not, starts the restore from any state except the
	// documented ones; unknown event kinds change nothing.
	for _, st := range allStates {
		for _, k := range append(allEvents, EventKind("something_new"), EventKind("")) {
			in := withPower(job(st, model.StageFinal), relays13)
			in.PowerReadAt = time.Time{} // the one input that must make every path refuse
			got, as := Step(in, Event{Kind: k, Power: relays13, PowerRead: true}, t0, testCfg())
			if st != model.JobRestorePower && got.State == model.JobRestorePower {
				t.Errorf("%s/%s entered restore_power with an untimestamped state", st, k)
			}
			for _, a := range as {
				if a.Kind == ActRestorePower {
					t.Errorf("%s/%s requested ActRestorePower with an untimestamped state", st, k)
				}
			}
		}
	}
}
