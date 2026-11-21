package web

import (
	"net/http"
	"strings"
	"testing"
)

func resetDrift() {
	driftMu.Lock()
	clear(driftLast)
	driftMu.Unlock()
}

// driftBody reads the Changes page of device 1 as an admin from a fresh live read.
func driftBody(t *testing.T, e *env, query string) (int, string) {
	t.Helper()
	resetDrift()
	resp, body := e.do(t, "GET", "/devices/1/drift"+query, "admin", nil)
	return resp.StatusCode, body
}

const (
	status3 = `"Status3":{"StatusLOG":{"SerialLog":0}}`
	// Status 9 exists only on devices with energy monitoring: the others answer {"Command":"Error"}. The
	// backup stores that as an _error marker; over MQTT the live read can hold the raw answer instead.
	storedStatus9Error = `"Status9":{"_error":"tasmota: unparsable response: device answered Command Error"}`
	liveStatus9Raw     = `"Status9":{"Command":"Error","Input":"STATUS 9"}`
)

// Why (#1083): a section the device could not be read for is not a setting that changed. The page
// told a person with a healthy plug that three settings differed, and showed the Go error text.
func TestAFailedStatusReadIsNotAChangedSetting(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[5] = []byte(`{` + status3 + `,` + storedStatus9Error + `}`)
	bk.live = []byte(`{` + status3 + `,` + liveStatus9Raw + `}`)
	status, body := driftBody(t, e, "")
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	if n := strings.Count(body, `devdrift-row is-changed`); n != 0 {
		t.Errorf("%d rows count as changed, the unread section is not a change:\n%s", n, body)
	}
	for _, want := range []string{"Part of the device could not be read (Status 9). Those settings are not compared.", `role="status"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"_error", "unparsable response", "Command Error", "STATUS 9", "(absent)"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page shows %q, a read error is not shown as a setting:\n%s", bad, body)
		}
	}
	if strings.Contains(body, "Everything matches the last backup") {
		t.Errorf("a page that could not compare a part must not claim that everything matches:\n%s", body)
	}
	_, all := driftBody(t, e, "?all=1")
	if strings.Contains(all, "Status9") || !strings.Contains(all, "Show changed only") || !strings.Contains(all, "SerialLog") {
		t.Errorf("Show all lists the compared settings only (Status 3 here), none of the unread Status 9:\n%s", all)
	}
	if rows := strings.Count(all, `class="devdrift-row"`); rows != 1 {
		t.Errorf("Show all lists %d unchanged rows, want 1:\n%s", rows, all)
	}
}

// Why: a read that failed live while the backup holds the section is no deletion of those settings
// either; it is the unread part that is named.
func TestASectionTheLiveReadFailedOnIsNamedNotShownAsAbsent(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[5] = []byte(`{` + status3 + `,"Status5":{"StatusNET":{"Hostname":"lamp"}}}`)
	bk.live = []byte(`{` + status3 + `,"Status5":{"_error":"tasmota: timeout"}}`)
	_, body := driftBody(t, e, "")
	if !strings.Contains(body, "Part of the device could not be read (Status 5).") || strings.Contains(body, "Hostname") || strings.Contains(body, "tasmota: timeout") {
		t.Errorf("the unread Status 5 is named and none of its settings or the error is shown:\n%s", body)
	}
	if strings.Contains(body, `devdrift-row is-changed`) {
		t.Errorf("no row changed:\n%s", body)
	}
}

// Why: Status 9 answers Command Error on every device without energy monitoring, by design. Both reads
// say so; nothing is missing, and a banner on every such plug would teach people to ignore it. The two
// sides may word the error differently (HTTP and MQTT wrap it differently), so it is the structure that
// decides, never the text.
func TestAnErrorBothSidesHoldIsNotReportedWhateverItsText(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[5] = []byte(`{` + status3 + `,` + storedStatus9Error + `}`)
	bk.live = []byte(`{` + status3 + `,"Status9":{"_error":"tasmota: command error"}}`)
	_, body := driftBody(t, e, "")
	if strings.Contains(body, "could not be read") || strings.Contains(body, "_error") || strings.Contains(body, `devdrift-row is-changed`) {
		t.Errorf("a section both sides could not read is neither a banner nor a change:\n%s", body)
	}
	if !strings.Contains(body, "Everything matches the last backup") {
		t.Errorf("the compared settings match:\n%s", body)
	}
	_, all := driftBody(t, e, "?all=1")
	if strings.Contains(all, "Status9") || strings.Contains(all, "_error") || !strings.Contains(all, "Show changed only") {
		t.Errorf("Show all does not list the unread section:\n%s", all)
	}
}

// Why: when the device answered none of the sections the page has nothing to compare; it must say
// so and offer the retry, not list every stored setting as removed.
func TestAPageWhoseLiveReadAnsweredNothingSaysSoAndOffersTryAgain(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[5] = []byte(`{` + status3 + `,"Status5":{"StatusNET":{"Hostname":"lamp"}}}`)
	bk.live = []byte(`{"Status3":{"_error":"tasmota: timeout: dial tcp 10.0.0.9:80"},"Status5":{"_error":"tasmota: timeout: dial tcp 10.0.0.9:80"}}`)
	status, body := driftBody(t, e, "")
	if status != http.StatusBadGateway || !strings.Contains(body, "The device could not be read.") || !strings.Contains(body, ">Try again<") {
		t.Fatalf("status %d, want 502 with the read banner and Try again:\n%s", status, body)
	}
	for _, bad := range []string{"devdrift-tbl", "10.0.0.9", "timeout", "Hostname", "Everything matches"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page shows %q:\n%s", bad, body)
		}
	}
	// A failed read is not kept like a good one: a reload inside the interval is throttled, it does not
	// show the failure again as if it were fresh data.
	resp, retry := e.do(t, "GET", "/devices/1/drift", "admin", nil)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(retry, "queried a moment ago") {
		t.Errorf("a reload of a failed read: %d\n%s", resp.StatusCode, retry)
	}
}

// Why: a failed whole read used to print the Go error chain ("dial tcp ...") on the page; the page keeps
// the class sentence only (safety rule 4: nothing of the device's answer or of its address in the page).
func TestAFailedWholeReadShowsTheClassSentenceNotTheErrorChain(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.liveErr = errString("dial tcp 10.9.9.9:80: connect: connection refused")
	resetDrift()
	resp, body := e.do(t, "GET", "/devices/1/drift", "admin", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "The device could not be read.") || strings.Contains(body, "10.9.9.9") || strings.Contains(body, "dial tcp") {
		t.Errorf("status %d, the error chain must not reach the page:\n%s", resp.StatusCode, body)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Why: nothing differs is an answer; an empty table under a count of zero read like a page that failed.
func TestDriftWithNoDifferenceSaysEverythingMatches(t *testing.T) {
	e, bk, _ := newDiffEnv(t)
	bk.files[5] = []byte(`{` + status3 + `}`)
	bk.live = []byte(`{` + status3 + `}`)
	_, body := driftBody(t, e, "")
	if !strings.Contains(body, "Everything matches the last backup") || strings.Contains(body, "devdrift-tbl") || strings.Contains(body, "No setting changed") {
		t.Errorf("no difference: the sentence, no table:\n%s", body)
	}
}

// Why: the tab's job is "what differs from the last backup"; "Drift" is a developer's word. The
// route stays, so bookmarks keep working.
func TestTheDeviceTabIsCalledChanges(t *testing.T) {
	e, _, _ := newDiffEnv(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `/devices/1/drift"`) || !strings.Contains(body, ">Changes</a>") || strings.Contains(body, ">Drift</a>") {
		t.Errorf("the tab reads Changes and keeps its route:\n%s", body)
	}
}
