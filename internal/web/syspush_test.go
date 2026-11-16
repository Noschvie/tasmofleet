package web

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// pushSettingsEnv is the Settings page with the real push channel on a temporary database and a fake
// dispatcher for the test push (its own tests are in package notify). The audit is the returned one.
func pushSettingsEnv(t *testing.T, f *fakeNotify) (*env, *store.SQLite, *notify.Push, *fakeAudit) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // cleanup of a temp DB
	key, err := notify.LoadVAPIDKey(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := notify.NewPush(st, key, func(context.Context, string, string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	aud := &fakeAudit{}
	e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) {
		d.Push, d.Recorder = p, aud
		if f != nil {
			d.Notify = f
		}
	})
	return e, st, p, aud
}

func addPush(t *testing.T, st *store.SQLite, endpoint, method, subject, label string) string {
	t.Helper()
	if err := st.SavePushSubscription(t.Context(), store.PushSubscription{Endpoint: endpoint, P256dh: "p", Auth: "a", OwnerMethod: method, OwnerSubject: subject, Label: label}); err != nil {
		t.Fatal(err)
	}
	return notify.SubscriptionID(endpoint)
}

const (
	endpointA = "https://fcm.googleapis.com/fcm/send/secret-capability-AAA"
	endpointB = "https://updates.push.services.mozilla.com/wpush/v2/secret-capability-BBB"
)

// Why (W10, #611): the list must tell the admin which browser it is, who owns it, where it pushes to and
// whether the last push failed (the only sign that a subscription is dead). The endpoint's path is the
// browser's capability to receive pushes and must never reach the HTML; a hash names the row instead.
func TestPushListShowsOwnerHostAndLastErrorButNoEndpoint(t *testing.T) {
	e, st, p, _ := pushSettingsEnv(t, &fakeNotify{})
	idA := addPush(t, st, endpointA, "local", "test:admin", "Chrome on Android")
	idB := addPush(t, st, endpointB, "oidc", "sso-olivia", "Firefox on Windows")
	if err := st.RecordPushResult(t.Context(), endpointB, time.Now(), "HTTP 500"); err != nil {
		t.Fatal(err)
	}
	r, body := e.do(t, "GET", "/settings/system?tab=notifications", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, want := range []string{"Subscribed browsers (2)", "Chrome on Android", "Firefox on Windows", "fcm.googleapis.com", "updates.push.services.mozilla.com",
		"Last delivery failed", "HTTP 500", `data-push-id="` + idA + `"`, `data-push-id="` + idB + `"`, `data-push-key="` + p.PublicKey() + `"`,
		`hx-post="/tf/settings/system/push/test/` + idA + `"`, `data-dlg-action="/tf/settings/system/push/remove/` + idB + `"`, `dlg-bkdel`, "Web Push"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, leak := range []string{"secret-capability", "/fcm/send", "/wpush/v2", "https://fcm", "https://updates.push"} {
		if strings.Contains(body, leak) {
			t.Errorf("the page carries the endpoint (%q)", leak)
		}
	}
	if strings.Index(body, "Chrome on Android") > strings.Index(body, "Firefox on Windows") {
		t.Error("the signed-in admin's own browser must come first")
	}
}

// Why: with no subscription the card says so (and offers nothing to test or remove); without a push channel
// (no VAPID key) there is no card at all, because nothing behind it could work.
func TestPushCardIsEmptyWithoutSubscriptionsAndAbsentWithoutPush(t *testing.T) {
	e, _, _, _ := pushSettingsEnv(t, &fakeNotify{})
	_, body := e.do(t, "GET", "/settings/system?tab=notifications", "admin", nil)
	if !strings.Contains(body, "No browser is subscribed yet.") || strings.Contains(body, "data-push-row-test") || !strings.Contains(body, "data-push-subscribe") {
		t.Error("an empty card must say so and keep only the subscribe control")
	}
	e2, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Notify = &fakeNotify{} })
	_, body = e2.do(t, "GET", "/settings/system?tab=notifications", "admin", nil)
	if strings.Contains(body, "card-notify-push") || strings.Contains(body, "data-push") {
		t.Error("the Web Push card is drawn without a push channel")
	}
}

// Why: removing is how an admin stops the pushes of a colleague (an SSO admin removed at the identity provider
// is not detected, W6); testing is how they check a phone from the desktop. Both change what reaches a browser,
// so operators and anonymous requests get nothing, and the row stays. (The CSRF check is the guard's, as for
// every unsafe route.)
func TestPushRemoveAndTestRoutesAreAdminOnly(t *testing.T) {
	f := &fakeNotify{pushResult: notify.Attempt{OK: true, Detail: "the push service accepted it"}}
	e, st, _, _ := pushSettingsEnv(t, f)
	id := addPush(t, st, endpointA, "local", "test:admin", "Chrome on Android")
	for role, want := range map[string]int{"operator": http.StatusForbidden, "": http.StatusUnauthorized} {
		for _, c := range []struct{ method, path string }{
			{"GET", "/settings/system/push/remove/" + id}, {"POST", "/settings/system/push/remove/" + id}, {"POST", "/settings/system/push/test/" + id},
		} {
			if r, _ := e.do(t, c.method, c.path, role, url.Values{}); r.StatusCode != want {
				t.Errorf("%s %s as %q: %d, want %d", c.method, c.path, role, r.StatusCode, want)
			}
		}
	}
	if subs, _ := st.ListPushSubscriptions(t.Context()); len(subs) != 1 {
		t.Fatalf("a non-admin removed a subscription: %+v", subs)
	}
	if len(f.pushTested) != 0 {
		t.Fatalf("a non-admin sent a test push: %v", f.pushTested)
	}
}

// Why (W6 remedy): any admin can remove any subscription, also one that belongs to another admin. The audit
// says who did it and which push service, never the endpoint.
func TestPushRemoveAnyAdminsSubscription(t *testing.T) {
	e, st, _, _ := pushSettingsEnv(t, &fakeNotify{})
	idB := addPush(t, st, endpointB, "oidc", "sso-olivia", "Firefox on Windows")
	idA := addPush(t, st, endpointA, "local", "test:admin", "Chrome on Android")
	r, _ := e.do(t, "POST", "/settings/system/push/remove/"+idB, "admin", url.Values{})
	if r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "tab=notifications") {
		t.Fatalf("remove: %d %q, want a redirect to the tab", r.StatusCode, r.Header.Get("Location"))
	}
	subs, _ := st.ListPushSubscriptions(t.Context())
	if len(subs) != 1 || subs[0].Endpoint != endpointA {
		t.Fatalf("after removing another admin's browser: %+v", subs)
	}
	if r, _ := e.do(t, "POST", "/settings/system/push/remove/"+idB, "admin", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("second remove: %d, want 404", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/settings/system/push/remove/"+idA, "admin", url.Values{}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("own remove: %d", r.StatusCode)
	}
	_, page := e.do(t, "GET", r.Header.Get("Location")[len("/tf"):], "admin", nil)
	if !strings.Contains(page, "Browser removed") {
		t.Error("the page after removing does not say so")
	}
}

// Why: the audit log is read by people and shipped elsewhere; the endpoint's path is the browser's capability
// and must not be in it, but the push service and the action must.
func TestPushRemoveAndTestAuditTheHostOnly(t *testing.T) {
	f := &fakeNotify{pushResult: notify.Attempt{OK: true, Detail: "the push service accepted it"}}
	e, st, _, aud := pushSettingsEnv(t, f)
	idA := addPush(t, st, endpointA, "local", "test:admin", "Chrome on Android")
	e.do(t, "POST", "/settings/system/push/test/"+idA, "admin", url.Values{})
	e.do(t, "POST", "/settings/system/push/remove/"+idA, "admin", url.Values{})
	aud.mu.Lock()
	defer aud.mu.Unlock()
	var actions []string
	for _, ev := range aud.events {
		actions = append(actions, ev.Action)
		if strings.Contains(ev.Target+ev.Detail+ev.Result, "secret-capability") || strings.Contains(ev.Target+ev.Detail+ev.Result, idA) {
			t.Errorf("audit carries the endpoint or the id: %+v", ev)
		}
		if ev.Target != "push:fcm.googleapis.com" || ev.Actor != "test:admin" {
			t.Errorf("audit %+v", ev)
		}
	}
	if strings.Join(actions, ",") != "push.test,push.remove" {
		t.Errorf("audit actions %v", actions)
	}
}

// Why: without scripts "Remove" is a link, so the confirmation must be a page: it names what is removed, posts
// to the remove route, and opening it changes nothing.
func TestPushRemoveConfirmPage(t *testing.T) {
	e, st, _, _ := pushSettingsEnv(t, &fakeNotify{})
	id := addPush(t, st, endpointB, "oidc", "sso-olivia", "Firefox on Windows")
	r, body := e.do(t, "GET", "/settings/system/push/remove/"+id, "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "Remove Firefox on Windows?") || !strings.Contains(body, `action="/tf/settings/system/push/remove/`+id+`"`) {
		t.Fatalf("confirm page %d: %s", r.StatusCode, body)
	}
	if strings.Contains(body, "secret-capability") {
		t.Error("the confirmation page carries the endpoint")
	}
	if subs, _ := st.ListPushSubscriptions(t.Context()); len(subs) != 1 {
		t.Fatal("opening the confirmation page removed the subscription")
	}
	if r, _ := e.do(t, "GET", "/settings/system/push/remove/nope", "admin", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", r.StatusCode)
	}
}

// Why: the person must see what happened to the test: that the push service took it (and that this does not
// prove the system showed it), or why it failed, or that the rows's browser is gone. With scripts the row is
// swapped; without them the page comes back with the same words.
func TestPushTestShowsTheOutcomeWithAndWithoutScripts(t *testing.T) {
	f := &fakeNotify{pushResult: notify.Attempt{OK: true, Detail: "the push service accepted it"}}
	e, st, _, _ := pushSettingsEnv(t, f)
	id := addPush(t, st, endpointA, "local", "test:admin", "Chrome on Android")
	r, row := e.do(t, "POST", "/settings/system/push/test/"+id, "admin", url.Values{}, "HX-Request", "true")
	if r.StatusCode != 200 || len(f.pushTested) != 1 || f.pushTested[0] != id {
		t.Fatalf("test: %d, tested %v", r.StatusCode, f.pushTested)
	}
	for _, want := range []string{"Push test: sent, the push service accepted it", "check the system&#39;s notification settings", `data-push-id="` + id + `"`} {
		if !strings.Contains(row, want) {
			t.Errorf("the swapped row lacks %q: %s", want, row)
		}
	}
	if strings.Contains(row, "<html") || strings.Contains(row, "secret-capability") {
		t.Error("the swap must be the row alone, without the endpoint")
	}
	_, page := e.do(t, "POST", "/settings/system/push/test/"+id, "admin", url.Values{})
	if !strings.Contains(page, "Push test: sent, the push service accepted it") || !strings.Contains(page, "Subscribed browsers (1)") {
		t.Error("without scripts the page must come back with the result")
	}

	f.pushResult = notify.Attempt{Detail: "HTTP 400"}
	_, row = e.do(t, "POST", "/settings/system/push/test/"+id, "admin", url.Values{}, "HX-Request", "true")
	if !strings.Contains(row, "Push test: failed: HTTP 400") || !strings.Contains(row, `role="alert"`) {
		t.Errorf("a refusal must show as a failure: %s", row)
	}
	f.pushErr = notify.ErrTooSoon
	_, row = e.do(t, "POST", "/settings/system/push/test/"+id, "admin", url.Values{}, "HX-Request", "true")
	if !strings.Contains(row, "wait a few seconds between tests") {
		t.Errorf("rate limit not worded: %s", row)
	}
}

// Why: a test can remove the row (the push service no longer knows the browser, or its admin is no longer an
// admin). The answer must then say why instead of leaving the old row on screen, and a row that was removed
// meanwhile is not tested at all.
func TestPushTestOfAGoneRow(t *testing.T) {
	f := &fakeNotify{pushErr: notify.ErrPushOwnerGone}
	e, st, _, aud := pushSettingsEnv(t, f)
	id := addPush(t, st, endpointA, "oidc", "sso-former", "Chrome on Android")
	// a row removed meanwhile (another admin pressed Remove) is not tested and not audited
	if _, err := st.DeletePushSubscription(t.Context(), endpointA); err != nil {
		t.Fatal(err)
	}
	_, row := e.do(t, "POST", "/settings/system/push/test/"+id, "admin", url.Values{}, "HX-Request", "true")
	if !strings.Contains(row, "no such subscription") || len(f.pushTested) != 0 {
		t.Fatalf("a row removed meanwhile: %s, tested %v", row, f.pushTested)
	}
	aud.mu.Lock()
	n := len(aud.events)
	aud.mu.Unlock()
	if n != 0 {
		t.Errorf("a test of a missing row was audited (%d events)", n)
	}
	// the dispatcher removes the row of an owner who is no longer an admin and says so
	id = addPush(t, st, endpointA, "oidc", "sso-former", "Chrome on Android")
	f.pushDo = func(string) { _, _ = st.DeletePushSubscription(t.Context(), endpointA) }
	_, row = e.do(t, "POST", "/settings/system/push/test/"+id, "admin", url.Values{}, "HX-Request", "true")
	if !strings.Contains(row, "no longer an admin") || strings.Contains(row, "Send test") {
		t.Errorf("the owner-gone answer must replace the row with the reason: %s", row)
	}
}

// Why: the label is shown on a page and chosen by the client's User-Agent, so it must come from a short fixed
// table and never be the header itself (markup, a long string, a tracking value).
func TestPushLabelFromUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36":                                   "Chrome on Linux",
		"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36":                         "Chrome on Android",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0":                                                        "Firefox on Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1": "Safari on iPhone",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15":                   "Safari on macOS",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0":           "Edge on Windows",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/25.0 Chrome/121.0.0.0 Mobile Safari/537.36":        "Samsung Internet on Android",
		"": "Browser", "curl/8.0": "Browser", "Mozilla/5.0 (Unknown OS)": "Browser",
		"<script>alert(1)</script>": "Browser", "Windows only": "Browser on Windows",
	} {
		if got := pushLabel(ua); got != want {
			t.Errorf("pushLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}

// Why: whatever the client sends as User-Agent, the stored label and the page show only a word of the table.
func TestPushSubscribeStoresALabelNeverTheUserAgent(t *testing.T) {
	e, st, _, _ := pushSettingsEnv(t, &fakeNotify{})
	ua := `<img src=x onerror=alert(1)> Firefox/132.0 (Windows NT 10.0) tracking-9f8e7d`
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.srv.URL+"/tf/settings/system/push/subscription", strings.NewReader(subscriptionJSON(t, endpointA)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Role", "admin")
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("subscribe: %d", resp.StatusCode)
	}
	subs, _ := st.ListPushSubscriptions(t.Context())
	if len(subs) != 1 || subs[0].Label != "Firefox on Windows" {
		t.Fatalf("stored %+v, want the label of the table", subs)
	}
	_, body := e.do(t, "GET", "/settings/system?tab=notifications", "admin", nil)
	if strings.Contains(body, "tracking-9f8e7d") || strings.Contains(body, "onerror") {
		t.Error("the User-Agent reached the page")
	}
}

// Why (#611): a worker under /static/ would have the scope /static/ and so could not cover the installed
// app (start_url is the base path); a header to widen it would be a change of the security headers. So the
// worker is served at the base path. It must be revalidated on every use (a browser must see a new worker at
// once, never an immutable copy), and the notify.js it imports must be the plain URL for the same reason: the
// ?v= URLs are cached for a year, which would leave an upgraded TasmoFleet with the old mapping in the worker.
func TestServiceWorkerIsServedAtTheBaseRootAndImportsNotifyUnversioned(t *testing.T) {
	e, _, _, _ := pushSettingsEnv(t, &fakeNotify{})
	r, body := e.do(t, "GET", "/sw.js", "", nil)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("/sw.js: %d %q", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if cc := r.Header.Get("Cache-Control"); cc != "no-cache" || r.Header.Get("ETag") == "" {
		t.Errorf("Cache-Control %q, ETag %q: the worker must be revalidated", cc, r.Header.Get("ETag"))
	}
	if !strings.Contains(body, `importScripts("static/notify.js")`) {
		t.Fatalf("the worker does not import notify.js by its plain URL:\n%s", body)
	}
	r, _ = e.do(t, "GET", "/static/notify.js", "", nil)
	if r.Header.Get("Cache-Control") != "no-cache" || strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("notify.js without ?v= is %q, want revalidated", r.Header.Get("Cache-Control"))
	}
	e2, _, _, _ := sysEnvWithDeps(t, nil)
	if r, _ := e2.do(t, "GET", "/sw.js", "admin", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("/sw.js without a push channel: %d, want 404", r.StatusCode)
	}
}
