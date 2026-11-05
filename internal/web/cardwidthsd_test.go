package web

import (
	"os"
	"strings"
	"testing"
)

// #419-D: the device pages sit in the app's column stacks (approved mockup card-widths-d-mockup),
// so no card carries a width of its own and none is left half empty.

// sliceFrom returns the part of s from the first occurrence of from up to the next occurrence of to.
func sliceFrom(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", from, s)
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}

// Why: Activity is one card that fills the page; its entries flow through as many columns as fit
// (2 at 1280, 3 at 1920) instead of one thin column in a wide card. The overview's Activity box
// keeps its single column (it is in a narrow stack), so the columns are tied to the history card.
func TestDeviceActivityPageIsOneCardWithColumns(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.History = fakeHistory{stateChanges(30)} })
	_, page := e.do(t, "GET", "/devices/1/history", "operator", nil)
	if !strings.Contains(page, `<section class="devcard cw-act" aria-labelledby="dc-hist">`) {
		t.Errorf("the Activity page lost its columned card:\n%s", page)
	}
	_, ov := e.do(t, "GET", "/devices/1", "operator", nil)
	if strings.Contains(ov, "cw-act") {
		t.Error("the overview's Activity box must stay a single column")
	}
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".devcard.cw-act .dev-act {") || !strings.Contains(string(css), "column-width:") {
		t.Error("no CSS lets the Activity entries flow into columns")
	}
}

// Why: Replace device used to be one 820 px card on a 1576 px page. The form is the wide stack; what
// comes along and what happens sit in the narrow stack. The four always-on checks are information,
// not form fields (disabled boxes are never submitted; the restore always writes the whole dump), so
// they left the form without changing what the replacement does.
func TestReplaceTabIsAWideFormAndANarrowStack(t *testing.T) {
	e, _, _ := newAdminEnv(t)
	_, body := e.do(t, "GET", "/devices/1/replace", "admin", nil)
	wide := sliceFrom(t, body, `<div class="cstack cstack-w2">`, `<div class="cstack">`)
	narrow := body[strings.Index(body, `<div class="cstack">`):]
	if !strings.Contains(wide, `id="rp-pick"`) || strings.Contains(wide, "rp-checks") {
		t.Errorf("the wide stack must hold the form and no checks:\n%s", wide)
	}
	form := sliceFrom(t, wide, `<form id="rp-pick"`, `</form>`)
	if strings.Contains(form, `type="checkbox"`) {
		t.Errorf("the form still carries checkboxes:\n%s", form)
	}
	for _, want := range []string{"What comes along", "Copy name, topic and MQTT settings", "Copy rules, timers and PowerOnState",
		"Copy Wi-Fi credentials (the passwords come along, not shown)", "Keep the entry: its name, tags, notes and history stand for the new device",
		"What happens", "The old device must stay switched off from now on"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("the narrow stack lacks %q", want)
		}
	}
	if strings.Contains(wide, "What comes along") {
		t.Error("the checks belong to the narrow stack")
	}
}

// Why: on the preview the checks are omitted (the Copied box lists what moves, and the Start card
// holds the one acknowledgement); the preview keeps form + Copied wide, Not copied + Start narrow.
// The acknowledgement and the hidden fields stay in the start form: only layout changed.
func TestReplacePreviewKeepsFormCopiedWideAndStartNarrow(t *testing.T) {
	e, _, _ := newAdminEnv(t)
	_, body := e.do(t, "GET", "/devices/1/replace/preview?address=10.0.0.9", "admin", nil)
	if strings.Contains(body, "What comes along") || strings.Contains(body, `type="checkbox" checked disabled`) {
		t.Error("the always-on checks are omitted on the preview")
	}
	wide := sliceFrom(t, body, `<div class="cstack cstack-w2">`, `<div class="cstack">`)
	narrow := body[strings.Index(body, `<div class="cstack">`):]
	for _, want := range []string{`id="rp-pick"`, "Copied to the new device"} {
		if !strings.Contains(wide, want) {
			t.Errorf("the wide stack lacks %q", want)
		}
	}
	if strings.Contains(wide, "Not copied") || strings.Contains(wide, `name="confirm"`) {
		t.Error("Not copied and the start form belong to the narrow stack")
	}
	for _, want := range []string{"Not copied", `class="rp-start"`, `name="confirm" value="1"`, `name="csrf"`, `name="backup"`, "Start replacement…"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("the narrow stack lacks %q", want)
		}
	}
	// the acknowledgement stays required: a post without it is refused (TestReplaceWizardRefusalsAndOneConfirmation)
	if strings.Count(narrow, `<form method="post"`) != 1 {
		t.Error("exactly one form posts the replacement")
	}
}
