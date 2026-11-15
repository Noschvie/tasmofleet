package ota

import (
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#825): an identity read that sends a verifying job back to waiting ("Not on the new version yet") has two
// very different causes: the device sits on the loader image of Tasmota's own two-step update (it restarted
// once and will restart again by itself), or it still runs the firmware it had. The log used the same words
// for both and then repeated "Rebooting into ..." after one command. The engine writes what the device ran on
// the event, so the log can tell the two apart.
func TestIdentityReadThatSendsAJobBackToWaitingSaysWhatTheDeviceRuns(t *testing.T) {
	notes := func(h *harness, id model.JobID) []string {
		var out []string
		for _, ev := range eventsOf(t, h, id) {
			if ev.Event == string(EvIdentity) && ev.From == model.JobVerify && ev.To == model.JobAwaitReboot {
				out = append(out, ev.Detail)
			}
		}
		return out
	}
	t.Run("on the loader image", func(t *testing.T) {
		dev := plugDevice()
		dev.ident.FreeKB = 2000
		dev.autoContinue = 20 // the device answers on minimal for a long time before it continues
		h := newHarness(t, 2, dev)
		id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
		h.run(t, h.e)
		got := notes(h, id)
		if len(got) == 0 || got[0] != "device runs the minimal image" {
			t.Fatalf("notes %q, want the loader image first", got)
		}
	})
	t.Run("still on the old firmware", func(t *testing.T) {
		dev := plugDevice()
		dev.ident.FreeKB = 2000
		dev.ignore = true // the download failed: the device answers on its old image between attempts
		h := newHarness(t, 2, dev)
		h.api.onUpdate = func(model.DeviceID) {
			h.api.mu.Lock()
			h.api.dev(1).offline = 1
			h.api.mu.Unlock()
		}
		id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
		h.run(t, h.e)
		got := notes(h, id)
		if len(got) == 0 || !strings.HasPrefix(got[0], NoteStillOldPrefix) || strings.TrimPrefix(got[0], NoteStillOldPrefix) == "" {
			t.Fatalf("notes %q, want \"device still runs <version>\"", got)
		}
	})
}

// Why (#825): in the final stage of a staged update the device runs the minimal image while it downloads the
// final one, so a read on the minimal image is its old image, not a second loader leg. The class of the note
// comes from the machine's decision (verify's one auto-continue transition), not from the variant that was read:
// a note that says "device runs the minimal image" makes the log word one fetch and one reboot as two reboots.
func TestIdentityReadOnTheMinimalImageInTheFinalStageIsTheOldImageOfThatStage(t *testing.T) {
	dev := plugDevice() // little room: a staged plan
	h := newHarness(t, 2, dev)
	upgrades := 0
	h.api.onUpdate = func(model.DeviceID) {
		upgrades++
		if upgrades == 2 { // the final image: the device downloads, keeps answering on the minimal image
			h.api.mu.Lock()
			d := h.api.dev(1)
			d.ignore, d.offline = true, 1
			h.api.mu.Unlock()
		}
	}
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	h.run(t, h.e)
	if j := h.job(t, id); j.Plan != model.PlanStaged {
		t.Fatalf("plan %s, the test needs a staged update", j.Plan)
	}
	final := false
	var got []string
	for _, ev := range eventsOf(t, h, id) {
		if ev.Event == string(EvIdentity) && ev.To == model.JobSetOtaURL {
			final = true // the minimal image was reached: stage 2 begins
			continue
		}
		if final && ev.Event == string(EvIdentity) && ev.From == model.JobVerify && ev.To == model.JobAwaitReboot {
			got = append(got, ev.Detail)
		}
	}
	if len(got) == 0 {
		t.Fatal("no identity read of the final stage sent the job back to waiting")
	}
	for _, d := range got {
		if !strings.HasPrefix(d, NoteStillOldPrefix) || !strings.Contains(d, "minimal") {
			t.Errorf("note %q, want the still-old note naming the minimal image", d)
		}
	}
}

// Why (#825): an update to the version the device already runs, in another build, must not read as a no-op: the
// note of the old image names its variant and, when it was read, its language.
func TestRunningNoteNamesTheBuildTheDeviceStillRuns(t *testing.T) {
	got := RunningNote(model.Identity{Version: "16.0.0", Variant: "tasmota32", Language: "DE", LanguageKnown: true}, false)
	if want := NoteStillOldPrefix + "16.0.0 tasmota32 DE"; got != want {
		t.Errorf("note %q, want %q", got, want)
	}
	// the view splits the note into version, variant and language: an unread version must not shift them
	if got := RunningNote(model.Identity{Variant: "tasmota32"}, false); got != NoteStillOldPrefix+"unknown tasmota32" {
		t.Errorf("a variant without a version: %q", got)
	}
	if got := RunningNote(model.Identity{Version: "16.0.0", Variant: "minimal"}, true); got != NoteRunsLoaderPrefix+"minimal image" {
		t.Errorf("the loader leg's note is %q", got)
	}
	if got := RunningNote(model.Identity{Version: "16.0.0", Variant: "minimal"}, false); !strings.HasPrefix(got, NoteStillOldPrefix) {
		t.Errorf("a minimal read the machine did not wait on is not a loader leg: %q", got)
	}
}

// Why: the note holds what the device reported, and a device can report anything; it stays short.
func TestRunningNoteCutsALongVersion(t *testing.T) {
	got := RunningNote(model.Identity{Version: strings.Repeat("9", 100)}, false)
	if want := NoteStillOldPrefix + strings.Repeat("9", 40); got != want {
		t.Errorf("note %q, want the version cut to 40 characters", got)
	}
}
