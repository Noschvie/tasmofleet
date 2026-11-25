package web

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// describedByIDs are the ids every aria-describedby of the page names.
func describedByIDs(page string) []string {
	var ids []string
	for rest := page; ; {
		i := strings.Index(rest, `aria-describedby="`)
		if i < 0 {
			return ids
		}
		rest = rest[i+len(`aria-describedby="`):]
		ids = append(ids, rest[:strings.Index(rest, `"`)])
	}
}

// Why (#1072): a button that cannot act says why as text a keyboard and a touch user reach, and the text must be on
// the page in every state the button is blocked in: a describedby that names nothing leaves a screen reader
// without the reason and a sighted person without the line. Checked for each way the header's buttons get blocked.
func TestHeaderBlockedButtonsAlwaysNameALineThatIsOnThePage(t *testing.T) {
	now := time.Now()
	cases := map[string]func(*env){
		"offline":            func(e *env) { e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: now} },
		"offline and held":   func(e *env) { e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: now} },
		"HTTP API off":       func(e *env) { e.health[1] = refusedHealth(1) },
		"never answered":     func(e *env) { delete(e.health, 1) },
		"an operation holds": func(e *env) {},
		"online and nothing": func(e *env) { e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: now} },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e, _ := apiOffActionsEnv(t, func(d *Deps) {
				d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpOnlyFacts()}}
				if name == "offline and held" || name == "an operation holds" {
					d.Jobs = &fakeJobs{jobs: []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobApplying, CreatedAt: now}}}
				}
			})
			setup(e)
			_, page := e.do(t, "GET", "/devices/1", "admin", nil)
			for _, id := range describedByIDs(page) {
				if !strings.Contains(page, `id="`+id+`"`) {
					t.Errorf("aria-describedby=%q names nothing", id)
				}
			}
			for _, label := range []string{"Restart", "Back up"} {
				bt := buttonTag(page, label)
				if bt == "" && strings.Contains(page, ">"+label+"</a>") {
					continue // the action can run: the header offers its confirmation link, no blocked button to check
				}
				if bt == "" {
					t.Fatalf("no %s button", label)
				}
				if strings.Contains(bt, "aria-disabled") && !blockedButton(bt) {
					t.Errorf("%s is blocked the old way (disabled or a title only): %s", label, bt)
				}
				if strings.Contains(bt, " disabled") {
					t.Errorf("%s carries the disabled attribute: %s", label, bt)
				}
			}
		})
	}
}

// Why (#1072): Restart and Back up both cannot act for one cause (the HTTP API is off), so the page says it once, in
// one line that names both buttons, instead of one line per button.
func TestRestartAndBackUpShareOneReasonLine(t *testing.T) {
	e, _ := apiOffActionsEnv(t, nil)
	_, page := e.do(t, "GET", "/devices/1", "admin", nil)
	line := elementText(page, "devhd-why", "</p>")
	if !strings.HasPrefix(line, "Restart and Back up: HTTP API off") || strings.Count(line, "HTTP API off") != 1 {
		t.Errorf("one shared line wanted: %q", line)
	}
	if describedBy(buttonTag(page, "Restart")) != "devhd-why" || describedBy(buttonTag(page, "Back up")) != "devhd-why" {
		t.Error("both buttons name the shared line")
	}
}

// Why (#1072): the offline banner already says the device is offline, so the buttons name it instead of a second line
// that says the same.
func TestOfflineButtonsNameTheBannerNotASecondLine(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now()}
	_, page := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, label := range []string{"Restart", "Back up"} {
		if bt := buttonTag(page, label); !blockedButton(bt) || describedBy(bt) != "devhd-off" {
			t.Errorf("%s does not name the offline banner: %s", label, bt)
		}
	}
	if strings.Contains(page, `id="devhd-why"`) {
		t.Error("a second line repeats what the banner says")
	}
	// #1082: the banner is one lead and one sentence. It does not list the disabled buttons (they look disabled and point
	// at it; Switching and Refresh identity have reason lines of their own), and it does not call anything disabled.
	banner := elementText(page, "devhd-off", "</div>")
	if !strings.HasPrefix(banner, "Offline since ") || !strings.HasSuffix(banner, " Controls are off until it is back.") {
		t.Errorf("the banner is a lead and the one sentence: %s", banner)
	}
	for _, listed := range []string{"Restart", "Back up", "Edit", "Delete", "disabled", "Refresh identity", "Switching"} {
		if strings.Contains(banner, listed) {
			t.Errorf("the banner lists %q again: %s", listed, banner)
		}
	}
}

// Why (#1072): Refresh identity stays a probe button (hx-post, data-needs-device) that cannot act while the device
// is offline or its HTTP API is off; the card says why as text and the live region carries the words for the
// next refresh, because the card itself is not swapped for a state change. A card rendered blocked carries
// hx-disable: htmx never binds the probe to it, so the markup alone sends nothing on a click, Enter or Space,
// whether or not the click guard of select.js runs first; live.js lifts it when the device comes back.
func TestRefreshIdentityIsBlockedWithItsReasonAsText(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now()}
	_, page := e.do(t, "GET", "/devices/1", "admin", nil)
	btn := buttonTag(page, "Refresh identity")
	if !strings.Contains(btn, `hx-post="/tf/devices/1/probe"`) || !strings.Contains(btn, `aria-disabled="true"`) || strings.Contains(btn, " disabled") || !strings.Contains(btn, "data-needs-device") ||
		!slices.Contains(strings.Fields(btn), "hx-disable") {
		t.Errorf("offline: %s", btn)
	}
	if line := elementText(page, "ident-why", "</p>"); line != reasonOffline {
		t.Errorf("offline line: %q", line)
	}
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now()}
	_, page = e.do(t, "GET", "/devices/1", "admin", nil)
	btn = sliceBetween(page, `hx-post="/tf/devices/1/probe"`, ">Refresh identity<")
	if strings.Contains(btn, "aria-disabled") || strings.Contains(btn, " disabled") || slices.Contains(strings.Fields(btn), "hx-disable") {
		t.Errorf("online: %s", btn)
	}
	if line := elementText(page, "ident-why", "</p>"); line != "" || !strings.Contains(page, `id="ident-why" data-off-why="The device is offline." hidden`) {
		t.Errorf("online: the line must be empty and hidden: %q", line)
	}
}

// Why (#1072): the live region is what changes when the API goes off; it carries the reason as the page writes it
// (the "(SetOption128 0)" kept on one line) so live.js holds no copy of the wording.
func TestLiveRegionCarriesTheReasonForTheIdentityLine(t *testing.T) {
	e, _ := apiOffActionsEnv(t, nil)
	_, page := e.do(t, "GET", "/devices/1", "admin", nil)
	tpl := sliceBetween(page, "<template data-ident-why>", "</template>")
	if !strings.Contains(tpl, `<span class="nowrap">(SetOption128 0)</span>`) || !strings.Contains(page, "data-reads-off=") {
		t.Errorf("template: %q", tpl)
	}
	e = setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{} })
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now()}
	_, page = e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(page, "data-ident-why") {
		t.Error("a device that can be read carries no reason")
	}
}

// Why (#1072): the Backups tab said "Back up now" on a disabled button in the empty state; with nothing to take it
// says why there is none instead, and with backups the bar's button is blocked with its own reason line.
func TestBackupsTabSaysWhyBackUpIsBlocked(t *testing.T) {
	e, _, _ := subPageEnv(t)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now()}
	_, page := e.do(t, "GET", "/devices/1/backups", "admin", nil)
	// the header already says it (the offline banner): the bar names that line instead of drawing a second copy
	if bt := buttonTag(page, "Back up now"); !blockedButton(bt) || describedBy(bt) != "devhd-off" || strings.Contains(bt, "form=") {
		t.Fatalf("the bar's Back up now is not the blocked variant: %s", bt)
	}
	if strings.Contains(page, "bk-now-why") || !strings.Contains(page, `id="devhd-off"`) {
		t.Errorf("the reason is drawn twice, or the line the button names is not on the page")
	}
}

// Why (#1072): "Can't back up yet" with only the bare reason leaves the person at a dead end for the two most
// common blockers; a device that does not answer gets the next step, and the link to its address for admins.
func TestEmptyBackupsTabOfADeviceThatDoesNotAnswerNamesTheNextStep(t *testing.T) {
	e := setup(t)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now()}
	for _, c := range []struct {
		user string
		edit bool
	}{{"admin", true}, {"operator", false}} {
		_, page := e.do(t, "GET", "/devices/1/backups", c.user, nil)
		empty := sliceBetween(page, `class="uempty"`, "</div></div>")
		if !strings.Contains(empty, "The device is offline. It can be backed up once it answers again. Check its power and its address.") {
			t.Errorf("%s: no next step: %s", c.user, empty)
		}
		if got := strings.Contains(empty, `href="/tf/devices/1/edit"`); got != c.edit {
			t.Errorf("%s: Edit link %v, want %v", c.user, got, c.edit)
		}
	}
}

// Why (#1072): a user page lists the actions the rules forbid; each says why in a line the page shows, one line
// per reason, so a page reads "Make operator, Deactivate and Delete: This would leave no enabled administrator."
// once (no "…" before the colon, a full stop at the end), and the buttons cannot submit anything (no link, no dialog).
func TestUserDetailBlockedActionsShareOneLinePerReason(t *testing.T) {
	last := auth.Guard{Reason: auth.MsgLastAdmin}
	ok := auth.Guard{Allowed: true}
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:other",
		User:   auth.UserRow{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin"},
		Guards: auth.UserGuards{Role: last, Disable: last, Delete: last, Reset: ok, Profile: ok}})
	var ids = map[string]bool{}
	for _, label := range []string{"Make operator", "Deactivate", "Delete"} {
		bt := buttonTag(body, label)
		if !blockedButton(bt) {
			t.Errorf("%s is not a blocked button: %s", label, bt)
			continue
		}
		if strings.Contains(bt, "data-dialog") || strings.Contains(bt, "href=") {
			t.Errorf("a blocked action still opens its dialog: %s", bt)
		}
		ids[describedBy(bt)] = true
	}
	if len(ids) != 1 {
		t.Fatalf("one reason, one line: %v", ids)
	}
	for id := range ids {
		if line := elementText(body, id, "</p>"); line != "Make operator, Deactivate and Delete: This would leave no enabled administrator." {
			t.Errorf("the line: %q", line)
		}
	}
}

// Why (#1072): the info banner of a proxy or SSO user already says why its buttons are off (managed where the user
// comes from, signed in by the proxy on every request); the buttons name the banner instead of repeating it in lines.
func TestUserDetailOfAProxyUserPointsItsButtonsAtTheInfoBanner(t *testing.T) {
	no := auth.Guard{Reason: "Managed by the authenticating proxy."}
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin",
		User:   auth.UserRow{Subject: "hdr:ops", Name: "ops", Method: auth.MethodHeader, Role: "operator"},
		Guards: auth.UserGuards{Role: no, Disable: no, Delete: no, Reset: no, Profile: no}})
	for _, label := range []string{"Make admin", "Reset password", "Deactivate", "Delete", "End all sessions"} {
		bt := buttonTag(body, label)
		if !blockedButton(bt) || describedBy(bt) != "ud-info" {
			t.Errorf("%s does not name the info banner: %s", label, bt)
		}
	}
	if line := elementText(body, "ud-info", "</div>"); !strings.Contains(line, "Signed in by the authenticating proxy on every request.") {
		t.Errorf("the banner the buttons name: %q", line)
	}
	if strings.Contains(body, `id="ud-why-0"`) || strings.Contains(body, "Managed by the authenticating proxy") {
		t.Error("the banner's reason is repeated as a line")
	}
}
