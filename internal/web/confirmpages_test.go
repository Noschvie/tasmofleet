package web

import (
	"net/url"
	"strings"
	"testing"
)

// Why: without JavaScript the dialog opener must still lead somewhere. Each
// of the four dialog-only confirmations has a GET page that changes nothing,
// names the consequence, posts to the dialog's route with the CSRF token in
// the form (never in a URL), and is reachable from a real href on the opener.

func TestBackupDeleteConfirmPage(t *testing.T) {
	e, ab, _ := newAdminEnv(t)
	if r, _ := e.do(t, "GET", "/backups/4/delete", "operator", nil); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	r, body := e.do(t, "GET", "/backups/4/delete", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, `class="ucard confirm-card"`) || !strings.Contains(body, `method="post" action="/tf/backups/4/delete"`) ||
		!strings.Contains(body, "This cannot be undone") || !strings.Contains(body, "This is the newest backup") {
		t.Fatalf("newest backup page: %d %.600s", r.StatusCode, body)
	}
	if _, older := e.do(t, "GET", "/backups/1/delete", "admin", nil); strings.Contains(older, "newest backup") {
		t.Errorf("the newest-backup warning shows on an older backup")
	}
	if r, _ := e.do(t, "GET", "/backups/99/delete", "admin", nil); r.StatusCode != 404 {
		t.Errorf("unknown backup: %d", r.StatusCode)
	}
	if len(ab.deleted) != 0 {
		t.Fatalf("opening the page deleted %v", ab.deleted)
	}
	// The dialog opener is a link to the page.
	_, list := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	if !strings.Contains(list, `href="/tf/backups/4/delete" data-dialog="dlg-bkdel"`) {
		t.Errorf("the Delete opener has no href:\n%s", list)
	}
}

func TestRuleDeleteConfirmPage(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, url.Values{"change_note": {"first"}})
	if r, _ := e.do(t, "GET", "/policy/rules/delete?idx=0", "operator", nil); r.StatusCode != 403 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	r, body := e.do(t, "GET", "/policy/rules/delete?idx=0", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, `action="/tf/policy/rules/delete"`) || !strings.Contains(body, `name="idx" value="0"`) ||
		!strings.Contains(body, `name="version" value="1"`) || !strings.Contains(body, "Flash wear") {
		t.Fatalf("%d %.600s", r.StatusCode, body)
	}
	if r, _ := e.do(t, "GET", "/policy/rules/delete?idx=7", "admin", nil); r.StatusCode != 404 {
		t.Errorf("missing rule: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/policy/rules/delete?idx=x", "admin", nil); r.StatusCode != 400 {
		t.Errorf("bad index: %d", r.StatusCode)
	}
	if len(mem.vs) != 1 {
		t.Fatalf("opening the page saved a version: %d", len(mem.vs))
	}
	_, list := e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(list, `href="/tf/policy/rules/delete?idx=0" data-dialog="dlg-rule-0"`) {
		t.Errorf("the Delete opener has no href:\n%s", list)
	}
}

// Why: without JavaScript "Cancel remaining" opens this page instead of the dialog; it must
// promise what the dialog promises (a flash finishes, the others stop after their step).
func TestCancelConfirmPages(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	r, body := e.do(t, "GET", "/transactions/7/cancel", "operator", nil)
	if r.StatusCode != 200 || !strings.Contains(body, `class="ucard confirm-card"`) || !strings.Contains(body, `action="/tf/transactions/7/cancel"`) ||
		!strings.Contains(body, cancelText(0)[:60]) || !strings.Contains(body, ">Cancel remaining<") {
		t.Fatalf("batch: %d %.600s", r.StatusCode, body)
	}
	r, body = e.do(t, "GET", "/jobs/1/cancel", "operator", nil)
	if r.StatusCode != 200 || !strings.Contains(body, `action="/tf/jobs/1/cancel"`) || !strings.Contains(body, "kitchen") {
		t.Fatalf("job: %d %.600s", r.StatusCode, body)
	}
	for _, p := range []string{"/transactions/99/cancel", "/jobs/99/cancel"} {
		if r, _ := e.do(t, "GET", p, "operator", nil); r.StatusCode != 404 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
	if len(o.cancelledBatches) != 0 || len(o.cancelled) != 0 {
		t.Fatalf("opening a page cancelled %v %v", o.cancelledBatches, o.cancelled)
	}
	_, page := e.do(t, "GET", "/transactions/7", "operator", nil)
	for _, want := range []string{`href="/tf/transactions/7/cancel"`, `href="/tf/jobs/1/cancel"`} {
		if !strings.Contains(page, want) {
			t.Errorf("transaction page lacks the opener link %s", want)
		}
	}
}

// Why: the CSRF rule (PORT.md): none of the new pages may put the token into a URL.
func TestConfirmPagesCarryNoCSRFInURLs(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	for _, p := range []string{"/transactions/7/cancel", "/jobs/1/cancel"} {
		_, body := e.do(t, "GET", p, "operator", nil)
		if getWithCSRF.MatchString(body) || strings.Contains(body, `formmethod="get"`) {
			t.Errorf("%s: token in a URL", p)
		}
	}
}
