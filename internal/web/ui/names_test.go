package ui

import (
	"strings"
	"testing"
)

// Why (#1074): the username of the sign-in form was capitalised and spell-checked on a phone. FieldData can say
// both, and says nothing by default (a name or a note keeps the browser's help).
func TestFieldCanTurnOffAutocapitalizeAndSpellcheck(t *testing.T) {
	got := comp(t, Field(FieldData{Label: "Username", Name: "username", NoAutocapitalize: true, NoSpellcheck: true}))
	if !strings.Contains(got, `autocapitalize="off"`) || !strings.Contains(got, `spellcheck="false"`) {
		t.Errorf("username field: %s", got)
	}
	got = comp(t, Field(FieldData{Label: "Name", Name: "display_name"}))
	if strings.Contains(got, "autocapitalize") || strings.Contains(got, "spellcheck") {
		t.Errorf("a plain field must leave both to the browser: %s", got)
	}
}

// Why (#1074): a row menu is a disclosure, not a menu widget: no menu roles (no arrow-key model behind them), a
// trigger named by the caller's label, and a refused entry that stays a control Tab reaches, says why in its
// own text and carries no action.
func TestRowMenuIsAPlainDisclosure(t *testing.T) {
	got := comp(t, RowMenu("Actions for Maren", []MenuItem{
		{Label: "Edit", Href: "/e"},
		{Label: "Delete", Disabled: true, Why: "Not your own account"},
	}, false))
	if strings.Contains(got, "role=") {
		t.Errorf("a row menu claims a role: %s", got)
	}
	if !strings.Contains(got, `<summary class="btn btn-ghost btn-icon icon" aria-label="Actions for Maren"`) {
		t.Errorf("trigger: %s", got)
	}
	if !strings.Contains(got, `<button type="button" class="menu-item" aria-disabled="true"><span>Delete</span> <span class="menu-why">Not your own account</span></button>`) {
		t.Errorf("a refused entry is a button with aria-disabled and its reason, no href: %s", got)
	}
}

// Why (#1074): five proposal rows with two buttons each, named "Add", "Ignore", "Apply": the buttons say what
// they do to which device, and still start with the visible word (label in name).
func TestAttentionProposalButtonsNameTheirDevice(t *testing.T) {
	got := comp(t, AttentionList(AttentionData{CSRF: "tok", Props: []AttProposal{{Name: "Attic Plug", Text: "New device", Primary: "Add", Secondary: "Ignore", PrimaryAction: "/a", SecondaryAction: "/b"}}}))
	for _, want := range []string{`aria-label="Add Attic Plug"`, `aria-label="Ignore Attic Plug"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}
