package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var deviceMoreMenu = regexp.MustCompile(`(?s)<div class="devhd-more" id="devhd-more-1" hx-preserve>.*?</details>`)

// deviceHeaderOf is the device header of a page or of a swap, for a failure message (not the whole page).
func deviceHeaderOf(body string) string {
	i := strings.Index(body, `id="devhd"`)
	if i < 0 {
		return "(no header)"
	}
	j := strings.Index(body[i:], `id="devhd-slot"`)
	if j < 0 {
		j = len(body) - i
	}
	return body[i : i+j]
}

// Why (#1079, A2-012): at 375 px the header shows Restart, Back up, Edit and Delete in one row, so the destructive
// Delete sits next to Edit and the three rare admin actions compete with the title on every tab. A phone gets
// Restart and Back up, and one "⋯" menu with Edit, Replace device, a rule and Delete (danger) last. The menu exists
// for admins only, because only they have Edit, Replace and Delete at all, and its entries are plain links to the
// pages that enforce their own role check.
func TestDeviceHeaderMenuHoldsTheRareAdminActionsWithDeleteLast(t *testing.T) {
	e, _, _ := newAdminEnv(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	menu := deviceMoreMenu.FindString(body)
	if menu == "" {
		t.Fatalf("the admin header has no more-actions menu:\n%s", deviceHeaderOf(body))
	}
	edit := strings.Index(menu, `href="/tf/devices/1/edit"`)
	repl := strings.Index(menu, `href="/tf/devices/1/replace"`)
	rule := strings.Index(menu, "<hr>")
	del := strings.Index(menu, `href="/tf/devices/1/delete"`)
	if edit < 0 || repl < 0 || rule < 0 || del < 0 || edit >= repl || repl >= rule || rule >= del {
		t.Errorf("want Edit, Replace device, a rule, then Delete in that order (%d %d %d %d):\n%s", edit, repl, rule, del, menu)
	}
	if del >= 0 && !strings.Contains(menu[max(0, del-40):del], "danger") {
		t.Errorf("Delete is not marked as the dangerous entry:\n%s", menu)
	}
	if strings.Contains(menu, "Restart") || strings.Contains(menu, "Back up") {
		t.Errorf("Restart and Back up stay visible in the header, they do not belong in the menu:\n%s", menu)
	}
	// The wide row is unchanged: Edit and Delete as buttons, in a wrapper that lays out as if it were not there.
	if !strings.Contains(body, `<div class="devhd-wide">`) {
		t.Errorf("the desktop buttons lost their wrapper:\n%s", deviceHeaderOf(body))
	}
}

// Why (#1079): the header comes back out of band (a refresh of the identity, the live refresh of the overview), and
// swapping it would close a menu the person has just opened under their finger. hx-preserve with an id of the
// device keeps that one node, and only that one: the rest of the header still refreshes.
func TestHeaderSwapKeepsTheOpenMoreMenu(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	if !strings.Contains(body, `id="devhd" hx-swap-oob="true"`) || deviceMoreMenu.FindString(body) == "" {
		t.Errorf("the swapped header lacks the preserved menu:\n%s", deviceHeaderOf(body))
	}
}

// Why (#1079): an operator has no Edit, Replace or Delete, so the header has no menu to open and no empty "⋯".
func TestOperatorHeaderHasNoMoreMenu(t *testing.T) {
	e, _, _ := newAdminEnv(t)
	_, body := e.do(t, "GET", "/devices/1", "operator", nil)
	if strings.Contains(body, "devhd-more") || strings.Contains(body, "devhd-wide") {
		t.Errorf("the operator's header carries the admin menu:\n%s", deviceHeaderOf(body))
	}
}
