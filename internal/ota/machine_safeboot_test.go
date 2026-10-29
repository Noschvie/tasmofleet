package ota

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// The push into safeboot (#30) restarts a device on purpose. These rows pin the rules that keep a
// device from being left in safeboot or flashed twice; each says why in its name or comment.

func sbESP32(version, variant string) model.Identity {
	return model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WDQ6 v1.0", Version: version, Image: "release-tasmota32", Variant: variant, FlashSizeKB: 4096, FreeKB: 697}
}

func safebootIdent() model.Identity {
	return model.Identity{Family: model.ChipESP32, Version: "15.6.0", Image: "release-safeboot", Variant: variantSafeboot}
}

// sbJob is a push job through safeboot after preflight: 15.6.0 -> 16.0.0 on an ESP32.
func sbJob(s model.JobState) model.Job {
	j := job(s, model.StageFinal)
	j.Plan, j.Delivery, j.ViaSafeboot = model.PlanDirect, model.DeliveryPush, true
	j.IdentityBefore = sbESP32("15.6.0", "tasmota32")
	j.TargetVariant = "tasmota32"
	j.Attempt = 0
	j.UptimeBefore = 5000
	j.DeadlineAt = t0.Add(time.Minute)
	return j
}

func TestSafebootEnteringTheSwitch(t *testing.T) {
	// Why: the restart is a device action of its own; it must be written to the store (the state)
	// before it is sent, and nothing else (no upload) may be sent with it.
	runRows(t, []row{
		{name: "set_ota_url/command_ok/via safeboot", in: sbJob(model.JobSetOtaURL), ev: Event{Kind: EvCommandOK},
			state: model.JobSafebootSwitch, actions: []ActionKind{ActSafebootSwitch}, check: func(t *testing.T, j model.Job, _ []Action) {
				wantDeadline(t, j, 2*time.Minute)
				if j.Attempt != 0 {
					t.Errorf("Attempt %d: no upload was sent yet", j.Attempt)
				}
			}},
		{name: "set_ota_url/command_ok/not via safeboot", in: func() model.Job { j := sbJob(model.JobSetOtaURL); j.ViaSafeboot = false; return j }(),
			ev: Event{Kind: EvCommandOK}, state: model.JobUpgradeIssued, actions: []ActionKind{ActUpgrade}},
	})
}

func TestSafebootSwitchRows(t *testing.T) {
	sw := sbJob(model.JobSafebootSwitch)
	cancelled := sw
	cancelled.CancelRequested = true
	probe := []ActionKind{ActProbe}
	runRows(t, []row{
		// The reply "false" means the device restarts; the one /u4 is not repeated, ever.
		{name: "safeboot_switch/command_ok", in: sw, ev: Event{Kind: EvCommandOK}, state: model.JobSafebootSwitch, actions: probe,
			check: func(t *testing.T, j model.Job, as []Action) {
				wantDelay(t, as, 5*time.Second)
				if !j.PowerCutoff.Equal(t0) {
					t.Errorf("PowerCutoff %v: from here on MQTT reports are the device's boot announcements", j.PowerCutoff)
				}
			}},
		// A lost reply may hide a restart: probe, never re-send.
		{name: "safeboot_switch/command_failed/lost reply", in: sw, ev: Event{Kind: EvCommandFailed, Err: "timeout"}, state: model.JobSafebootSwitch, actions: probe},
		// The device answered that it did not switch: still on the app, nothing to recover.
		{name: "safeboot_switch/command_failed/certain", in: sw, ev: Event{Kind: EvCommandFailed, NotFlashed: true, Err: "the device has no safeboot partition"},
			state: model.JobFailed, actions: []ActionKind{ActNotify}, check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "not touched")
				wantErrContains(t, j, "no safeboot partition")
				if strings.Contains(j.LastError, "relays") {
					t.Errorf("the device never restarted but the note says relays are lost: %q", j.LastError)
				}
			}},
		{name: "safeboot_switch/command_failed/certain and cancelled", in: cancelled, ev: Event{Kind: EvCommandFailed, NotFlashed: true, Err: "cancelled by the user"},
			state: model.JobCancelled, actions: []ActionKind{ActNotify}},
		{name: "safeboot_switch/guard_refused", in: sw, ev: Event{Kind: EvGuardRefused, Err: "wrong family"}, state: model.JobFailed, actions: []ActionKind{ActNotify}},
		{name: "safeboot_switch/probe_offline", in: sw, ev: Event{Kind: EvProbeOffline}, state: model.JobSafebootSwitch, actions: probe},
		// Still the app (the restart has not happened yet): an upload now would go to the wrong partition.
		{name: "safeboot_switch/probe_online/app", in: sw, ev: Event{Kind: EvProbeOnline, Identity: sbESP32("15.6.0", "tasmota32"), Uptime: 9000}, state: model.JobSafebootSwitch, actions: probe},
		{name: "safeboot_switch/probe_online/unknown", in: sw, ev: Event{Kind: EvProbeOnline}, state: model.JobSafebootSwitch, actions: probe},
		// The only way to an upload: the device answered as safeboot.
		{name: "safeboot_switch/probe_online/safeboot", in: sw, ev: Event{Kind: EvProbeOnline, Identity: safebootIdent(), Uptime: 4}, state: model.JobUpgradeIssued,
			actions: []ActionKind{ActUpgrade}, check: func(t *testing.T, j model.Job, _ []Action) {
				wantDeadline(t, j, 3*time.Minute)
				if j.Attempt != 1 || j.UptimeBefore != 4 || j.SeenOffline {
					t.Errorf("Attempt %d UptimeBefore %d SeenOffline %v", j.Attempt, j.UptimeBefore, j.SeenOffline)
				}
			}},
		// Cancel is honoured by the recovery, not by an upload.
		{name: "safeboot_switch/probe_online/safeboot and cancelled", in: cancelled, ev: Event{Kind: EvProbeOnline, Identity: safebootIdent()}, state: model.JobSafebootRecover,
			actions: []ActionKind{ActSafebootBack}},
		{name: "safeboot_switch/timeout", in: sw, ev: Event{Kind: EvTimeout}, state: model.JobSafebootRecover, actions: []ActionKind{ActSafebootBack}, check: func(t *testing.T, j model.Job, _ []Action) {
			wantDeadline(t, j, 2*time.Minute)
			if j.Attempt != 1 {
				t.Errorf("Attempt %d: the device was restarted, the failure note must apply", j.Attempt)
			}
		}},
		{name: "safeboot_switch/cancel", in: sw, ev: Event{Kind: EvCancel}, state: model.JobSafebootSwitch, check: func(t *testing.T, j model.Job, as []Action) {
			if !j.CancelRequested || len(as) != 0 {
				t.Errorf("CancelRequested %v actions %v", j.CancelRequested, kinds(as))
			}
		}},
		// A restart of tasmofleet: the state was stored before the /u4 went out, so whether it
		// went out is unknown. Probe only.
		{name: "safeboot_switch/resume", in: sw, ev: Event{Kind: EvResume}, state: model.JobSafebootSwitch, actions: probe, check: func(t *testing.T, j model.Job, _ []Action) {
			wantDeadline(t, j, 2*time.Minute)
		}},
	})
}

func TestSafebootUploadRows(t *testing.T) {
	up := sbJob(model.JobUpgradeIssued)
	up.Attempt, up.UptimeBefore = 1, 4
	await := up
	await.State, await.SeenOffline = model.JobAwaitReboot, true
	awaitOnline := up
	awaitOnline.State = model.JobAwaitReboot
	verify := up
	verify.State = model.JobVerify
	probe := []ActionKind{ActProbe}
	recover := []ActionKind{ActSafebootBack}
	runRows(t, []row{
		// Safeboot answers until the upload starts to be written: that is not a reboot.
		{name: "upgrade_issued/probe_online/safeboot", in: up, ev: Event{Kind: EvProbeOnline, Identity: safebootIdent(), Uptime: 2}, state: model.JobUpgradeIssued, actions: probe},
		// Even an uptime smaller than the app's (5000 s) says nothing: it is safeboot's own clock.
		{name: "upgrade_issued/probe_online/safeboot, small uptime", in: up, ev: Event{Kind: EvProbeOnline, Identity: safebootIdent(), Uptime: 1}, state: model.JobUpgradeIssued, actions: probe},
		{name: "upgrade_issued/probe_online/app", in: up, ev: Event{Kind: EvProbeOnline, Identity: sbESP32("16.0.0", "tasmota32"), Uptime: 3}, state: model.JobVerify, actions: []ActionKind{ActProbeIdentity}},
		{name: "upgrade_issued/probe_offline", in: up, ev: Event{Kind: EvProbeOffline}, state: model.JobAwaitReboot, actions: probe},
		// A lost reply is answered by probing, never by a second upload.
		{name: "upgrade_issued/command_failed/lost reply", in: up, ev: Event{Kind: EvCommandFailed, Err: "timeout"}, state: model.JobUpgradeIssued, actions: probe},
		{name: "upgrade_issued/command_ok", in: up, ev: Event{Kind: EvCommandOK}, state: model.JobUpgradeIssued, actions: probe},
		// Refused upload: the device sits in safeboot.
		{name: "upgrade_issued/command_failed/refused", in: up, ev: Event{Kind: EvCommandFailed, NotFlashed: true, Err: "Not enough space"}, state: model.JobSafebootRecover, actions: recover,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "nothing was flashed")
				wantErrContains(t, j, "Not enough space")
			}},
		// Guard says no right before the upload: the upload is not sent and the device waits in safeboot.
		{name: "upgrade_issued/guard_refused", in: up, ev: Event{Kind: EvGuardRefused, Err: "family"}, state: model.JobSafebootRecover, actions: recover},
		{name: "upgrade_issued/timeout", in: up, ev: Event{Kind: EvTimeout}, state: model.JobSafebootRecover, actions: recover},
		{name: "upgrade_issued/resume", in: up, ev: Event{Kind: EvResume}, state: model.JobUpgradeIssued, actions: probe},
		{name: "await_reboot/timeout/never offline", in: awaitOnline, ev: Event{Kind: EvTimeout}, state: model.JobSafebootRecover, actions: recover},
		{name: "await_reboot/timeout/seen offline", in: await, ev: Event{Kind: EvTimeout, Addr: "172.31.13.35"}, state: model.JobFailed, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, ErrTextNotBack)
				wantErrContains(t, j, "safeboot at 172.31.13.35")
			}},
		{name: "verify/identity/back in safeboot", in: verify, ev: Event{Kind: EvIdentity, Identity: safebootIdent()}, state: model.JobSafebootRecover, actions: recover},
		// The normal outcome: the app runs the target.
		{name: "verify/identity/target", in: verify, ev: Event{Kind: EvIdentity, Identity: sbESP32("16.0.0", "tasmota32"), OtaURL: origURL}, state: model.JobSucceeded, actions: []ActionKind{ActNotify},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.LastError != "" {
					t.Errorf("LastError %q", j.LastError)
				}
			}},
	})
}

func TestSafebootRecoverRows(t *testing.T) {
	rec := sbJob(model.JobSafebootRecover)
	rec.Attempt, rec.LastError = 1, "upload refused, nothing was flashed: Not enough space"
	rec.PowerOn = 1
	rec.PowerReadAt = t0
	cancelled := rec
	cancelled.CancelRequested = true
	probe := []ActionKind{ActProbe}
	end := []ActionKind{ActNotify}
	runRows(t, []row{
		{name: "safeboot_recover/command_ok", in: rec, ev: Event{Kind: EvCommandOK}, state: model.JobSafebootRecover, actions: probe},
		// At most one recovery attempt: a lost reply is probed, never answered with a second /u4.
		{name: "safeboot_recover/command_failed/lost reply", in: rec, ev: Event{Kind: EvCommandFailed, Err: "timeout"}, state: model.JobSafebootRecover, actions: probe},
		{name: "safeboot_recover/command_failed/certain", in: rec, ev: Event{Kind: EvCommandFailed, NotFlashed: true, Err: "HTTP 404", Addr: "172.31.13.35"}, state: model.JobFailed, actions: end,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "Device is still in safeboot")
				wantErrContains(t, j, "Not enough space")
			}},
		{name: "safeboot_recover/probe_offline", in: rec, ev: Event{Kind: EvProbeOffline}, state: model.JobSafebootRecover, actions: probe},
		{name: "safeboot_recover/probe_online/still safeboot", in: rec, ev: Event{Kind: EvProbeOnline, Identity: safebootIdent()}, state: model.JobSafebootRecover, actions: probe},
		{name: "safeboot_recover/probe_online/unknown", in: rec, ev: Event{Kind: EvProbeOnline}, state: model.JobSafebootRecover, actions: probe},
		{name: "safeboot_recover/probe_online/old firmware", in: rec, ev: Event{Kind: EvProbeOnline, Identity: sbESP32("15.6.0", "tasmota32"), Uptime: 20}, state: model.JobFailed, actions: end,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "Upload into safeboot failed; the device was switched back to 15.6.0")
				wantErrContains(t, j, "Not enough space")
				wantErrContains(t, j, "relays 1 were ON") // the restart switched the relays off; the job says so
			}},
		{name: "safeboot_recover/probe_online/old firmware and cancelled", in: cancelled, ev: Event{Kind: EvProbeOnline, Identity: sbESP32("15.6.0", "tasmota32")}, state: model.JobCancelled, actions: end,
			check: func(t *testing.T, j model.Job, _ []Action) { wantErrContains(t, j, "switched back to 15.6.0") }},
		// The upload was accepted after all: the normal verification decides.
		{name: "safeboot_recover/probe_online/new firmware", in: rec, ev: Event{Kind: EvProbeOnline, Identity: sbESP32("16.0.0", "tasmota32")}, state: model.JobVerify, actions: []ActionKind{ActProbeIdentity},
			check: func(t *testing.T, j model.Job, _ []Action) {
				if j.LastError != "" {
					t.Errorf("LastError %q survives into verify", j.LastError)
				}
				wantDeadline(t, j, 8*time.Minute)
			}},
		{name: "safeboot_recover/timeout", in: rec, ev: Event{Kind: EvTimeout, Addr: "172.31.13.35"}, state: model.JobFailed, actions: end,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "Device is still in safeboot at 172.31.13.35: open its web page and upload the firmware, or use Upgrade by download")
				wantErrContains(t, j, "Not enough space")
			}},
		{name: "safeboot_recover/timeout/no address known", in: rec, ev: Event{Kind: EvTimeout}, state: model.JobFailed, actions: end,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "Device is still in safeboot: open")
			}},
		// The user cancelled, the device could not be brought back: the job is cancelled, and the text still tells where the device is.
		{name: "safeboot_recover/timeout/cancelled", in: cancelled, ev: Event{Kind: EvTimeout, Addr: "172.31.13.35"}, state: model.JobCancelled, actions: end,
			check: func(t *testing.T, j model.Job, _ []Action) {
				wantErrContains(t, j, "Device is still in safeboot at 172.31.13.35")
			}},
		// A failure that is not about the user's cancel stays failed.
		{name: "safeboot_recover/timeout/not cancelled", in: rec, ev: Event{Kind: EvTimeout}, state: model.JobFailed, actions: end},
		{name: "safeboot_recover/cancel", in: rec, ev: Event{Kind: EvCancel}, state: model.JobSafebootRecover, check: func(t *testing.T, j model.Job, as []Action) {
			if !j.CancelRequested || len(as) != 0 {
				t.Errorf("CancelRequested %v actions %v", j.CancelRequested, kinds(as))
			}
		}},
		// A restart of tasmofleet during the recovery: probe only, the one attempt is used up or lost.
		{name: "safeboot_recover/resume", in: rec, ev: Event{Kind: EvResume}, state: model.JobSafebootRecover, actions: probe, check: func(t *testing.T, j model.Job, _ []Action) {
			wantDeadline(t, j, 2*time.Minute)
		}},
	})
}

// Why: the safety rules are about what can ever be sent, not about single rows. Whatever happens, a
// job sends the restart into safeboot once (entering safeboot_switch), the upload only when it
// enters upgrade_issued from a safeboot answer, and the recovery /u4 once (entering
// safeboot_recover). No other transition may carry one of these actions.
func TestSafebootDeviceActionsHaveOneDoor(t *testing.T) {
	events := []Event{
		{Kind: EvStart}, {Kind: EvResume}, {Kind: EvCommandOK}, {Kind: EvCommandFailed}, {Kind: EvCommandFailed, NotFlashed: true},
		{Kind: EvGuardRefused}, {Kind: EvProbeOnline}, {Kind: EvProbeOnline, Identity: safebootIdent()},
		{Kind: EvProbeOnline, Identity: sbESP32("15.6.0", "tasmota32")}, {Kind: EvProbeOnline, Identity: sbESP32("16.0.0", "tasmota32")},
		{Kind: EvProbeOffline}, {Kind: EvTimeout}, {Kind: EvCancel}, {Kind: EvIdentity, Identity: safebootIdent()},
		{Kind: EvIdentity, Identity: sbESP32("16.0.0", "tasmota32")}, {Kind: EvUpgradeReport, Err: "Failed"},
	}
	states := []model.JobState{model.JobSetOtaURL, model.JobSafebootSwitch, model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobSafebootRecover}
	for _, st := range states {
		for _, cancel := range []bool{false, true} {
			for _, ev := range events {
				in := sbJob(st)
				in.Attempt, in.CancelRequested = 1, cancel
				out, as := Step(in, ev, t0, testCfg())
				for _, a := range as {
					switch a.Kind {
					case ActSafebootSwitch:
						if in.State != model.JobSetOtaURL || out.State != model.JobSafebootSwitch {
							t.Errorf("%s/%s: restart into safeboot sent on %s -> %s", st, ev.Kind, in.State, out.State)
						}
					case ActSafebootBack:
						if out.State != model.JobSafebootRecover || in.State == model.JobSafebootRecover {
							t.Errorf("%s/%s: recovery sent on %s -> %s", st, ev.Kind, in.State, out.State)
						}
					case ActProbeIdentity, ActPlan, ActBackup, ActSetOtaURL, ActProbe, ActRestoreOtaURL, ActRestorePower, ActNotify:
						// not device actions of the push into safeboot
					case ActUpgrade:
						if in.State != model.JobSafebootSwitch || ev.Kind != EvProbeOnline || ev.Identity.Variant != variantSafeboot {
							t.Errorf("%s/%s: upload sent on %s -> %s", st, ev.Kind, in.State, out.State)
						}
					}
				}
			}
		}
	}
}
