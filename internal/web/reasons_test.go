package web

import (
	"html"
	"reflect"
	"strings"
	"testing"
)

// buttonTag is the opening tag of the <button> whose text is label, "" when the page has none.
func buttonTag(page, label string) string {
	i := strings.Index(page, ">"+label+"</button>")
	if i < 0 {
		return ""
	}
	return page[strings.LastIndex(page[:i], "<button"):i]
}

// blockedButton says whether tag is the blocked variant of ui.Btn: reachable by Tab (aria-disabled, never the
// disabled attribute), with a visible reason named by aria-describedby, and no title as the only carrier.
func blockedButton(tag string) bool {
	return strings.Contains(tag, `aria-disabled="true"`) && strings.Contains(tag, `aria-describedby="`) &&
		!strings.Contains(tag, " disabled") && !strings.Contains(tag, "title=")
}

// elementText is the text of the element with the given id up to its closing tag end ("</p>", "</span>"), tags
// dropped and whitespace collapsed; "" when the page has no such element.
func elementText(page, id, end string) string {
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	chunk := page[i:]
	chunk = chunk[strings.Index(chunk, ">")+1:]
	if j := strings.Index(chunk, end); j >= 0 {
		chunk = chunk[:j]
	}
	var out strings.Builder
	in := false
	for _, r := range chunk {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			out.WriteRune(r)
		}
	}
	return html.UnescapeString(strings.Join(strings.Fields(out.String()), " "))
}

// describedBy is the id the tag's aria-describedby names, "" when it has none.
func describedBy(tag string) string {
	i := strings.Index(tag, `aria-describedby="`)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(`aria-describedby="`):]
	return rest[:strings.Index(rest, `"`)]
}

// Why (#1072): a row of blocked buttons must say each reason once. Three buttons with the same reason as three
// lines would bury the one reason that differs, so the lines are grouped by reason, in the order the buttons stand.
func TestGroupReasonsMakesOneLinePerReason(t *testing.T) {
	got := groupReasons([]blockedAction{
		{"Make admin", "Managed by the identity provider."},
		{"Reset password", ""},
		{"Deactivate", "Managed by the identity provider."},
		{"End all sessions", "No active sessions"},
		{"Delete…", "Managed by the identity provider."},
	})
	want := []reasonGroup{
		{Labels: []string{"Make admin", "Deactivate", "Delete…"}, Why: "Managed by the identity provider."},
		{Labels: []string{"End all sessions"}, Why: "No active sessions"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups %+v, want %+v", got, want)
	}
	for _, c := range []struct {
		g    reasonGroup
		lead string
	}{
		{want[0], "Make admin, Deactivate and Delete"}, // the label's "…" must not run into the colon
		{want[1], "End all sessions"},
		{reasonGroup{Labels: []string{"Restart", "Back up"}}, "Restart and Back up"},
	} {
		if got := c.g.Lead(); got != c.lead {
			t.Errorf("lead %q, want %q", got, c.lead)
		}
	}
	if got := groupReasons([]blockedAction{{"Restart", ""}}); len(got) != 0 {
		t.Errorf("a button that can act has no line: %+v", got)
	}
}
