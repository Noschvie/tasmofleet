package ota

import (
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Push into safeboot (#30). An ESP32 with the safeboot layout has one app partition, so a full
// image can only be uploaded while the device runs its safeboot (factory) partition:
//
//	set_ota_url (Guard only) -> safeboot_switch -> upgrade_issued (the upload) -> await_reboot -> verify
//
// The rules that keep this from leaving a device in safeboot or flashing twice:
//   - safeboot_switch sends GET /u4?u4=fct exactly once (the engine does it when the state is
//     entered). A lost reply or a restart of tasmofleet is answered by probing, never by sending
//     it again.
//   - The upload is sent once, to a device that just answered as safeboot, after Guard ran again.
//   - A job that ends with the device in (or possibly in) safeboot goes through safeboot_recover:
//     one GET /u4?u4=ota, then probing. That is the only recovery; if it does not bring the old
//     firmware back, the job fails and tells the user where to finish by hand.

// toSafebootRecover enters the one recovery attempt. reason is why the push failed and ends in
// LastError. The device was restarted on purpose by now, so the relay note of fail() applies.
func toSafebootRecover(j model.Job, now time.Time, cfg Config, reason string) (model.Job, []Action) {
	if j.Attempt == 0 {
		j.Attempt = 1
	}
	j = to(j, model.JobSafebootRecover, now)
	j.LastError = reason
	j.DeadlineAt = now.Add(cfg.SafebootWindow)
	return j, []Action{{Kind: ActSafebootBack}}
}

// afterSwitchSent notes that the device may be restarting by now: MQTT "switched off" reports
// from here on are the device announcing its boot state.
func afterSwitchSent(j model.Job, now time.Time) model.Job {
	if j.PowerCutoff.IsZero() {
		j.PowerCutoff = now
	}
	j.UpdatedAt = now
	return j
}

func stepSafebootSwitch(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandOK:
		return afterSwitchSent(j, now), probeLater(cfg)
	case EvCommandFailed:
		if ev.NotFlashed {
			// The device answered that it did not switch (no safeboot partition, error status,
			// wrong password), or the user cancelled before it was sent: it still runs the app,
			// there is nothing to recover.
			if j.CancelRequested {
				return cancelled(j, now, withErr("cancelled, the device was not touched", ev.Err))
			}
			return fail(j, now, withErr("could not restart into safeboot, the device was not touched", ev.Err))
		}
		// A lost reply: the restart may have started. Look, never send it again.
		return afterSwitchSent(j, now), probeLater(cfg)
	case EvGuardRefused:
		return fail(j, now, withErr("guard refused", ev.Err))
	case EvProbeOffline:
		j.SeenOffline = true
		return afterSwitchSent(j, now), probeLater(cfg)
	case EvProbeOnline:
		if ev.Identity.Variant != variantSafeboot {
			return j, probeLater(cfg) // still the app, or an answer that says nothing
		}
		if j.CancelRequested {
			return toSafebootRecover(j, now, cfg, "cancelled while the device switched into safeboot")
		}
		// The device answers as safeboot: upload now (the engine runs Guard again first).
		j = to(j, model.JobUpgradeIssued, now)
		j.Attempt++
		j.SeenOffline = false
		j.UptimeBefore = ev.Uptime // safeboot's own clock: the reboot proof after the upload is relative to it
		j.DeadlineAt = now.Add(cfg.UpgradeAccept)
		return j, []Action{{Kind: ActUpgrade}}
	case EvTimeout:
		// Never seen as safeboot: it may still be switching, or never did. The switch back is
		// harmless on a device that runs the app.
		return toSafebootRecover(j, now, cfg, "the device did not answer as safeboot in time")
	case EvStart, EvResume, EvIdentity, EvPlanned, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

// atAddr is " at <address>" when the engine knows how to reach the device.
func atAddr(addr string) string {
	if addr == "" {
		return ""
	}
	return " at " + addr
}

// manualSafeboot is the failure text of a device that could not be brought back.
func manualSafeboot(reason, addr string) string {
	return joinNotes("Device is still in safeboot"+atAddr(addr)+": open its web page and upload the firmware, or use Upgrade by download", reason)
}

func stepSafebootRecover(j model.Job, ev Event, now time.Time, cfg Config) (model.Job, []Action) {
	switch ev.Kind {
	case EvCommandOK:
		return afterSwitchSent(j, now), probeLater(cfg)
	case EvCommandFailed:
		if ev.NotFlashed {
			// Certain that the switch back was not done: waiting will not help.
			return endRecovery(fail(j, now, manualSafeboot(j.LastError, ev.Addr)))
		}
		return afterSwitchSent(j, now), probeLater(cfg) // lost reply: probe, no second attempt
	case EvProbeOffline:
		return j, probeLater(cfg)
	case EvProbeOnline:
		id := ev.Identity
		if id.Variant == "" || isIntermediate(id.Variant) {
			return j, probeLater(cfg) // still in safeboot, or an answer that says nothing
		}
		// An app answers. No reboot proof is asked for: the recovery starts long after the switch
		// into safeboot was sent (the switch window, or the upload), so a device that answers as
		// the app is the one that came back.
		if id.Version != j.IdentityBefore.Version {
			// The upload was accepted after all and the device runs another firmware: the
			// normal verification decides what it is.
			j = to(j, model.JobVerify, now)
			j.LastError = ""
			j.DeadlineAt = now.Add(cfg.RebootWindow)
			return j, []Action{{Kind: ActProbeIdentity}}
		}
		reason := j.LastError
		j.LastError = ErrTextSafebootFailed + "; the device was switched back to " + j.IdentityBefore.Version
		if reason != "" {
			j.LastError += " (" + reason + ")"
		}
		return endRecovery(failKeep(j, now))
	case EvTimeout:
		return endRecovery(fail(j, now, manualSafeboot(j.LastError, ev.Addr)))
	case EvStart, EvResume, EvIdentity, EvPlanned, EvGuardRefused, EvBackupDone, EvBackupFailed, EvUpgradeReport, EvPowerReport, EvCancel:
		// not in the transition table: the job stays unchanged
	}
	return j, nil
}

// failKeep ends the job with the LastError it already has; a cancel request makes it cancelled.
func failKeep(j model.Job, now time.Time) (model.Job, []Action) {
	if j.CancelRequested {
		return cancelled(j, now, j.LastError)
	}
	return fail(j, now, j.LastError)
}

// endRecovery turns the end of a recovery into "cancelled" when the user asked for that. A job
// that fails keeps failing: the device state is the news.
func endRecovery(j model.Job, a []Action) (model.Job, []Action) {
	if j.CancelRequested && j.State == model.JobFailed {
		j.State = model.JobCancelled
	}
	return j, a
}
