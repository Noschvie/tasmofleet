package ui

import (
	"strings"
	"testing"
)

// Why: settings text comes from sysconfig and the environment (keys, values). The new components
// must show hostile text as text like every other component.
func TestSettingsComponentsEscapeText(t *testing.T) {
	h := hostile
	for name, out := range map[string]string{
		"Info":         comp(t, Info("info-x", h, h)),
		"SettingField": comp(t, SettingField(SettingFieldData{Key: "K", Label: h, Width: "half", Help: h, Info: h, Err: ""})),
		"FieldError":   comp(t, SettingField(SettingFieldData{Key: "K", Label: h, Width: "half", Err: h})),
		"Card":         comp(t, SettingsCard(SettingsCardData{ID: "c", Title: h, Note: h, Badges: []SettingsBadge{{Tone: ToneWarn, Label: h}}})),
	} {
		if strings.Contains(out, "<script>") || strings.Contains(out, `" onmouseover="`) || strings.Contains(out, "style=") {
			t.Errorf("%s: hostile text became markup: %s", name, out)
		}
		if !strings.Contains(out, "&lt;") {
			t.Errorf("%s: the hostile text is not shown as text", name)
		}
	}
}

// Why: the "Reset to default" control sat in its own grid cell and staggered the grid. It is
// part of the field it resets, in the label row; a locked field cannot be reset.
func TestSettingFieldKeepsTheResetInItsLabelRow(t *testing.T) {
	got := comp(t, SettingField(SettingFieldData{Key: "BACKUP_KEEP", Label: "Kept", Width: "xs", CanReset: true}))
	lrow := got[strings.Index(got, `<div class="lrow">`):strings.Index(got, "</div>")]
	if !strings.Contains(lrow, `name="clear.BACKUP_KEEP"`) {
		t.Errorf("the reset control is not in the label row: %s", got)
	}
	locked := comp(t, SettingField(SettingFieldData{Key: "K", Label: "L", Width: "half", CanReset: true, Locked: true}))
	if strings.Contains(locked, "clear.K") || !strings.Contains(locked, "Environment") {
		t.Errorf("a locked field offers a reset or lacks its badge: %s", locked)
	}
}

// Why: a field with an error says so for assistive technology under the id the control points to,
// and the error replaces the help line.
func TestSettingFieldErrorReplacesHelp(t *testing.T) {
	got := comp(t, SettingField(SettingFieldData{Key: "K", Label: "L", Width: "half", Help: "the help", Err: "bad value"}))
	if !strings.Contains(got, `id="err-v.K" role="alert">bad value`) || strings.Contains(got, "the help") {
		t.Errorf("error/help: %s", got)
	}
}
