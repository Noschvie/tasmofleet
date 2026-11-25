package web

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#1093): a destructive action must look equally dangerous, and read the same, with and without JavaScript. The
// dialog and the page of an action used to carry their own copies of tone and title, and the copies drifted: "Cancel
// remaining" was blue in the dialog and red on its page, the rule's dialog named the rule and its page said "this
// rule", the file's page repeated its title in a card heading. Every case renders both surfaces from the real handlers
// and compares what the person sees: the title, the verb, its tone, and that the page says its title once.

// confirmSurface is what one side of a confirmation shows.
type confirmSurface struct {
	Title, Verb string
	Danger      bool
}

var (
	buttonRe  = regexp.MustCompile(`<button[^>]*>[^<]*</button>`)
	tagAttrRe = func(name string) *regexp.Regexp { return regexp.MustCompile(`(?:^|\s)` + name + `="([^"]*)"`) }
)

func dlgAttr(tag, name string) string {
	if m := tagAttrRe(name).FindStringSubmatch(tag); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

// openerSurface reads a shared dialog's words from the link that opens it (dialog.js copies them).
func openerSurface(t *testing.T, body, marker string) confirmSurface {
	t.Helper()
	tag := tagOf(t, body, marker)
	if tag == "" {
		t.Fatalf("no opener with %q in:\n%.1500s", marker, body)
	}
	return confirmSurface{Title: dlgAttr(tag, "data-dlg-title"), Verb: dlgAttr(tag, "data-dlg-verb"), Danger: strings.Contains(tag, " data-dlg-danger")}
}

// dialogSurface reads a dialog the server draws: its h2 and the submit button.
func dialogSurface(t *testing.T, body, id string) confirmSurface {
	t.Helper()
	i := strings.Index(body, `<dialog id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no dialog %q in:\n%.1500s", id, body)
	}
	block := body[i : i+strings.Index(body[i:], "</dialog>")]
	h2 := regexp.MustCompile(`<h2[^>]*>([^<]*)</h2>`).FindStringSubmatch(block)
	if h2 == nil {
		t.Fatalf("dialog %q has no title", id)
	}
	s := confirmSurface{Title: html.UnescapeString(h2[1])}
	for _, b := range buttonRe.FindAllString(block, -1) {
		if strings.Contains(b, `type="submit"`) {
			s.Verb, s.Danger = html.UnescapeString(b[strings.Index(b, ">")+1:strings.LastIndex(b, "<")]), strings.Contains(b, "btn-danger-solid")
		}
	}
	return s
}

// pageSurface reads a confirmation page: its h1, and the submit button of the confirm card. It also fails when the
// title stands twice (the card repeating the page header).
func pageSurface(t *testing.T, body string) confirmSurface {
	t.Helper()
	h1 := regexp.MustCompile(`<h1>([^<]*)</h1>`).FindStringSubmatch(body)
	if h1 == nil {
		t.Fatalf("no h1 in:\n%.1500s", body)
	}
	s := confirmSurface{Title: html.UnescapeString(h1[1])}
	if n := strings.Count(body, ">"+html.EscapeString(s.Title)+"<"); n != 1 {
		t.Errorf("the page has %d elements that say %q, want one (the page header)", n, s.Title)
	}
	card := body[strings.Index(body, `confirm-card`):]
	card = card[:strings.Index(card, "</section>")]
	for _, b := range buttonRe.FindAllString(card, -1) {
		if strings.Contains(b, `type="submit"`) {
			s.Verb, s.Danger = html.UnescapeString(b[strings.Index(b, ">")+1:strings.LastIndex(b, "<")]), strings.Contains(b, "btn-danger-solid")
		}
	}
	return s
}

func sameWords(t *testing.T, name string, dialog, page confirmSurface) {
	t.Helper()
	if dialog.Title == "" || page.Title == "" || dialog.Verb == "" {
		t.Fatalf("%s: a surface is empty: dialog %+v, page %+v", name, dialog, page)
	}
	if dialog.Title != page.Title {
		t.Errorf("%s: the dialog asks %q, the page %q", name, dialog.Title, page.Title)
	}
	if dialog.Verb != page.Verb {
		t.Errorf("%s: the dialog confirms with %q, the page with %q", name, dialog.Verb, page.Verb)
	}
	if !dialog.Danger || !page.Danger {
		t.Errorf("%s: a destructive confirmation is danger-solid on both surfaces (dialog %v, page %v)", name, dialog.Danger, page.Danger)
	}
}

func TestDestructiveConfirmationsAreOneActionInDialogAndPage(t *testing.T) {
	t.Run("cancel remaining", func(t *testing.T) {
		e, _, _, _ := setupOTA(t)
		_, page := e.do(t, "GET", "/transactions/7", "admin", nil)
		_, confirm := e.do(t, "GET", "/transactions/7/cancel", "admin", nil)
		sameWords(t, "cancel remaining", openerSurface(t, page, `data-dlg-verb="Cancel remaining"`), pageSurface(t, confirm))
	})
	t.Run("cancel schedule", func(t *testing.T) {
		e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
		jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
		_, page := e.do(t, "GET", "/transactions/10", "admin", nil)
		_, confirm := e.do(t, "GET", "/transactions/10/cancel", "admin", nil)
		sameWords(t, "cancel schedule", openerSurface(t, page, `data-dlg-verb="Cancel schedule"`), pageSurface(t, confirm))
	})
	t.Run("cancel update", func(t *testing.T) {
		e, _, jobs, _ := setupOTA(t)
		jobs.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobQueued, CreatedAt: time.Now()}}
		_, page := e.do(t, "GET", "/transactions/7", "admin", nil)
		_, confirm := e.do(t, "GET", "/jobs/1/cancel", "admin", nil)
		sameWords(t, "cancel update", openerSurface(t, page, `data-dlg-verb="Cancel update"`), pageSurface(t, confirm))
	})
	t.Run("cancel the update of a planned device", func(t *testing.T) {
		e, _, jobs, _ := setupOTAAt(t, fixedClock{fri})
		jobs.jobs = []model.Job{schedJob(20, 10, 1, model.JobScheduled, sat)}
		_, confirm := e.do(t, "GET", "/jobs/20/cancel", "admin", nil)
		if s := pageSurface(t, confirm); !s.Danger || s.Verb != "Cancel update" {
			t.Errorf("the page of a planned update's cancel: %+v", s)
		}
	})
	t.Run("restart", func(t *testing.T) {
		e := cmdEnv(t, &fakeCmd{})
		_, confirm := e.do(t, "GET", "/devices/1/restart", "admin", nil)
		page := pageSurface(t, confirm)
		_, overview := e.do(t, "GET", "/devices/1", "admin", nil)
		sameWords(t, "restart from the device header", dialogSurface(t, overview, "dlg-restart"), page)
		_, list := e.do(t, "GET", "/devices", "admin", nil)
		sameWords(t, "restart from the list", openerSurface(t, list, `data-dlg-action="/tf/devices/1/restart"`), page)
		_, console := e.do(t, "GET", "/devices/1/console", "admin", nil)
		sameWords(t, "restart from the console", dialogSurface(t, console, "dlg-restart"), page)
	})
	t.Run("delete rule", func(t *testing.T) {
		e, mem, _ := rulesEnv(t, "")
		saveRule(t, e, url.Values{"change_note": {"first"}})
		id := ruleIDs(t, mem)[0]
		_, confirm := e.do(t, "GET", "/policy/rules/delete?id="+id, "admin", nil)
		page := pageSurface(t, confirm)
		_, list := e.do(t, "GET", "/policy", "admin", nil)
		sameWords(t, "delete rule from the list", openerSurface(t, list, `data-dlg-field-id="`+id+`"`), page)
		_, rule := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
		sameWords(t, "delete rule from its page", dialogSurface(t, rule, "dlg-rule-delete"), page)
		// the page says how many devices stop being checked, as the dialog does (A5-025)
		if !strings.Contains(plain(confirm), "It currently judges 0 devices.") {
			t.Errorf("the rule's page lacks the consequence the dialog names:\n%s", plain(confirm))
		}
	})
	t.Run("delete file", func(t *testing.T) {
		fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
			{ID: 2, Release: "16.1.0", Filename: "tasmota32.bin", Family: model.ChipESP32, Variant: "tasmota", Size: 1024, SHA256: "9f2c" + strings.Repeat("b", 56) + "e77c", Source: "ota.tasmota.com", FetchedAt: time.Now()},
		}}
		e := fwEnv(t, fw)
		_, list := e.do(t, "GET", "/firmware", "admin", nil)
		_, confirm := e.do(t, "GET", "/firmware/artifacts/2/delete", "admin", nil)
		sameWords(t, "delete file", dialogSurface(t, list, "dlg-file-2"), pageSurface(t, confirm))
	})
	t.Run("delete uploaded image", func(t *testing.T) {
		fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
			{ID: 3, Release: "16.0.0", Filename: "custom.bin", Family: model.ChipESP32, Variant: "tasmota", Size: 1024, SHA256: "9f2c" + strings.Repeat("b", 56) + "e77c", Source: "custom", FetchedAt: time.Now()},
		}}
		e := fwEnv(t, fw)
		_, list := e.do(t, "GET", "/firmware", "admin", nil)
		_, confirm := e.do(t, "GET", "/firmware/artifacts/3/delete", "admin", nil)
		sameWords(t, "delete uploaded image", dialogSurface(t, list, "dlg-file-3"), pageSurface(t, confirm))
	})
	t.Run("reset settings", func(t *testing.T) {
		e, _, _, _ := sysEnv(t, true)
		_, advanced := e.do(t, "GET", "/settings/system?tab=advanced", "admin", nil)
		_, confirm := e.do(t, "GET", "/settings/system/reset", "admin", nil)
		sameWords(t, "reset settings", dialogSurface(t, advanced, "dlg-reset"), pageSurface(t, confirm))
		// the trigger is a default button in a neutral strip; red is the confirming button's
		trigger := tagOf(t, advanced, `data-dialog="dlg-reset"`)
		if strings.Contains(trigger, "danger") || !strings.Contains(advanced, "sys-danger") {
			t.Errorf("the Reset trigger is %s", trigger)
		}
	})
}

// Why (#1093): the confirming button of a user's Deactivate and Delete is danger-solid, while the triggers are a
// default button (Deactivate) and the danger button (Delete) on the page and the same two weights in the row menu: the
// same action had two weights on the two surfaces (A7-017).
func TestUserDeactivateAndDeleteTones(t *testing.T) {
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin",
		User:   auth.UserRow{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator"},
		Guards: allowed})
	for _, verb := range []string{"Deactivate", "Delete"} {
		found := false
		for _, m := range regexp.MustCompile(`<dialog id="[^"]*"[^>]*>.*?</dialog>`).FindAllString(body, -1) {
			if strings.Contains(m, `btn-danger-solid">`+verb+` user</button>`) {
				found = true
			}
		}
		if !found {
			t.Errorf("the %s dialog has no danger-solid confirming button", verb)
		}
	}
	if !regexp.MustCompile(`<a class="btn" [^>]*>Deactivate</a>`).MatchString(body) {
		t.Errorf("Deactivate is a default button on the user page:\n%s", plain(body))
	}
	if !regexp.MustCompile(`<a class="btn btn-danger" [^>]*>Delete</a>`).MatchString(body) {
		t.Errorf("Delete is the danger button on the user page")
	}
	menu, _ := userMenu(auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin"}, auth.UserRow{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator", Guards: allowed}, 0)
	for _, it := range menu {
		switch it.Label {
		case "Deactivate":
			if it.Danger {
				t.Errorf("Deactivate reads red in the row menu: a default entry, as on the user page")
			}
		case "Delete":
			if !it.Danger {
				t.Errorf("Delete is the red entry of the row menu, as on the user page")
			}
		}
	}
}

// Why (#1093): powering off or restarting devices interrupts what they do, so the scope page's run button is
// danger-solid and repeats the action with its object ("Power off 3 devices"); a backup or a command stays primary.
func TestBulkScopeRunButtonToneFollowsTheAction(t *testing.T) {
	render := func(k bulkKind) string {
		var b strings.Builder
		v := bulkScopeView{Kind: k, Base: "/tf", CSRF: "tok", Run: "/tf/devices/bulk/power", RunN: 3, Rows: []bulkRow{{ID: 1, Name: "kitchen"}}}
		if err := bulkScopePage(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for kind, want := range map[bulkKind]string{
		bulkOff:     `<button type="submit" class="btn btn-danger-solid btn-tall">Power off 3 devices</button>`,
		bulkRestart: `<button type="submit" class="btn btn-danger-solid btn-tall">Restart 3 devices</button>`,
		bulkOn:      `<button type="submit" class="btn btn-primary btn-tall">Switch on 3 devices</button>`,
		bulkBackup:  `<button type="submit" class="btn btn-primary btn-tall">Back up 3 devices</button>`,
	} {
		if body := render(kind); !strings.Contains(body, want) {
			t.Errorf("%s run button: want %s in\n%s", kind, want, between(body, `<aside`, `</aside>`))
		}
	}
}

// Why (#1093): a restore overwrites the device's settings and restarts it: its confirming button is danger-solid and
// says what it does with its object, "Restore backup", like the trigger's verb.
func TestRestorePageConfirmsWithADangerButton(t *testing.T) {
	var b strings.Builder
	c := restoreConfirmView{Sub: "kitchen", Action: "/tf/backups/4/restore", CSRF: "tok", CancelHref: "/tf/devices/1/backups", Ack: "I understand.", HasPreview: true}
	if err := restoreConfirm(c).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `<button type="submit" class="btn btn-danger-solid">Restore backup</button>`) {
		t.Errorf("the restore's confirming button:\n%s", b.String())
	}
}

// Why (#1093): the list used to draw one delete dialog per rule, each with its own copy of the words, so N rules meant N
// dialogs that could drift apart and a page that grew with the rule set. One shared dialog (dlg-rule-delete) is filled
// from the opener's data-dlg-* attributes, and each opener names its rule through data-dlg-field-id.
func TestPolicyListHasOneSharedDeleteDialog(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	for i, c := range []string{"SaveData", "SetOption1", "SetOption2"} {
		saveRule(t, e, url.Values{"command": {c}, "name": {"Rule " + c}, "version": {vstr(int64(i))}})
	}
	ids := ruleIDs(t, mem)
	if len(ids) != 3 {
		t.Fatalf("want 3 rules, got %v", ids)
	}
	_, page := e.do(t, "GET", "/policy", "admin", nil)
	if n := strings.Count(page, `id="dlg-rule-delete"`); n != 1 {
		t.Errorf("the list has %d rule delete dialogs, want one shared", n)
	}
	for _, m := range regexp.MustCompile(`id="dlg-rule-([a-z0-9]+)"`).FindAllStringSubmatch(page, -1) {
		if m[1] != "delete" {
			t.Errorf("a per-rule delete dialog %q is left on the list", m[0])
		}
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-dlg-field-id="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		seen[m[1]] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("no opener fills the shared dialog with rule %s", id)
		}
	}
}
