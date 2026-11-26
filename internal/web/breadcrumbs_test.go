package web

import (
	"cmp"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// The guard of #1000. People read the breadcrumb trail and the lit sub-navigation tab as two
// answers to "where am I", so they must agree on every page (rule 4 of the maintainer's rules):
//
//  1. the first crumb is the top-bar group that is lit;
//  2. the second crumb is the sub-navigation tab that is lit, or, on a page that lights none
//     (a page of one device, the account page), the object the page is about: a device is named
//     by its name and hangs directly under the area ("Devices / kitchen"), never "Device";
//  3. no crumb repeats an earlier one (a start typed in twice reads "Devices / List / Devices / x");
//  4. a trail under three steps is not drawn (#1106): it would only repeat the top bar, the tab and the h1. A
//     device's page is the exception, its Devices crumb leads back to the list as the person left it (#127).
//
// A handler writes only the end of the trail; the start comes from ui.ShellData.Trail, which reads
// the same model that draws the tabs. These tables catch a page that still types its own start,
// and a page whose nav key lights another tab than its trail names.

// trailPage is one page of the guard: a request and what its trail must be.
type trailPage struct {
	method, path string
	form         url.Values
	hdr          []string
	// object is the second crumb of a page that lights no sub-navigation tab (a device's name).
	object string
	// want is the exact trail, written as the person reads it ("Devices / List / Bulk action"),
	// for the pages the maintainer's rules name. Empty: only the rules above are checked.
	want string
	// status is the expected status, 200 when 0: a row that silently checks a 404 page proves nothing.
	status int
	// fragment: the answer is an htmx out-of-band swap of the trail, not a page. The swap replaces
	// the whole trail, so it must be the full one (want), not only its end.
	fragment bool
	// none: the trail is under three steps, so the page draws none (rule 4); the lit group is still checked.
	none bool
}

type trailScene struct {
	name  string
	env   func(t *testing.T) *env
	pages func(t *testing.T, e *env) []trailPage
}

var (
	trailBarGroup = regexp.MustCompile(`<a class="tb-group" href="[^"]*" aria-current="true">([^<]*)</a>`)
	trailLitTab   = regexp.MustCompile(`<a class="sn-item" href="[^"]*" aria-current="page">\s*([^<]*?)\s*(?:<|$)`)
	trailCrumbSep = regexp.MustCompile(`<span class="crumb-sep"[^>]*>/</span>`)
	trailTag      = regexp.MustCompile(`<[^>]*>`)
)

// crumbsOf reads the labels of the trail out of a page or of an out-of-band swap.
func crumbsOf(t *testing.T, body string) []string {
	t.Helper()
	i := strings.Index(body, `id="crumbs"`)
	if i < 0 {
		t.Fatalf("no breadcrumb trail in %.300s", body)
	}
	nav := body[i : i+strings.Index(body[i:], "</nav>")]
	nav = trailCrumbSep.ReplaceAllString(nav, "\x00")
	var out []string
	for _, c := range strings.Split(trailTag.ReplaceAllString(nav[strings.Index(nav, ">")+1:], ""), "\x00") {
		out = append(out, html.UnescapeString(strings.TrimSpace(c)))
	}
	return out
}

func checkTrail(t *testing.T, e *env, p trailPage) {
	t.Helper()
	hdr := p.hdr
	res, body := e.do(t, p.method, p.path, "admin", p.form, hdr...)
	if want := cmp.Or(p.status, http.StatusOK); res.StatusCode != want {
		t.Errorf("%s %s: status %d, want %d", p.method, p.path, res.StatusCode, want)
		return
	}
	if p.none {
		if strings.Contains(body, `id="crumbs"`) {
			t.Errorf("%s %s: a trail under three steps is drawn: %q", p.method, p.path, strings.Join(crumbsOf(t, body), " / "))
		}
		group := trailBarGroup.FindStringSubmatch(body)
		pt := phoneTitle.FindStringSubmatch(body)
		if group == nil || pt == nil || html.UnescapeString(pt[1]) != group[1] {
			t.Errorf("%s %s: the lit group %v and the phone title %v must name the same area", p.method, p.path, group, pt)
		}
		return
	}
	trail := crumbsOf(t, body)
	got := strings.Join(trail, " / ")
	if p.want != "" && got != p.want {
		t.Errorf("%s %s: trail %q, want %q", p.method, p.path, got, p.want)
	}
	// The phone header names the area, the first crumb: never the page, whose h1 is the page's name (#1079).
	pt := phoneTitle.FindStringSubmatch(body)
	if pt != nil && html.UnescapeString(pt[1]) != trail[0] {
		t.Errorf("%s %s: the phone title is %q, the area is %q", p.method, p.path, html.UnescapeString(pt[1]), trail[0])
	}
	if p.fragment {
		return
	}
	if pt == nil {
		t.Errorf("%s %s: no phone title", p.method, p.path)
	}
	group := trailBarGroup.FindStringSubmatch(body)
	if group == nil {
		t.Errorf("%s %s: no lit top-bar group", p.method, p.path)
		return
	}
	if len(trail) == 1 && trail[0] == group[1] { // the group's own page (the dashboard)
		return
	}
	if len(trail) < 2 || trail[0] != group[1] {
		t.Errorf("%s %s: trail %q does not start with the lit group %q", p.method, p.path, got, group[1])
		return
	}
	if tab := trailLitTab.FindStringSubmatch(body); tab == nil && p.object == "" && strings.Contains(body, `<div class="sn sn-empty">`) {
		// the group has one page, so there is no tab row (#1106): the entry crumb is that page, the top-bar button's target
		btn := regexp.MustCompile(`<a class="tb-group" href="([^"]*)" aria-current="true">`).FindStringSubmatch(body)
		if btn == nil || !strings.Contains(body, `href="`+btn[1]+`">`+html.EscapeString(trail[1])+`</a>`) {
			t.Errorf("%s %s: trail %q: %q is not the one page of the lit group", p.method, p.path, got, trail[1])
		}
	} else if tab != nil {
		if p.object != "" {
			t.Errorf("%s %s: the page is about %q and must light no tab, but %q is lit", p.method, p.path, p.object, tab[1])
		}
		if trail[1] != tab[1] {
			t.Errorf("%s %s: trail %q names %q, the lit tab is %q", p.method, p.path, got, trail[1], tab[1])
		}
	} else if p.object == "" {
		t.Errorf("%s %s: no tab is lit, so the row must name the object the trail starts with", p.method, p.path)
	} else if trail[1] != p.object {
		t.Errorf("%s %s: trail %q, want %q as the second crumb", p.method, p.path, got, p.object)
	}
	for i, c := range trail {
		if slices.Index(trail, c) != i {
			t.Errorf("%s %s: trail %q says %q twice", p.method, p.path, got, c)
		}
	}
}

func TestEveryTrailAgreesWithTheLitTab(t *testing.T) {
	for _, sc := range trailScenes() {
		t.Run(sc.name, func(t *testing.T) {
			e := sc.env(t)
			for _, p := range sc.pages(t, e) {
				checkTrail(t, e, p)
			}
		})
	}
}

// sceneIDs holds what a scene creates and its pages then name (a browser, a rule): its env func writes, its pages func reads.
var sceneIDs = map[string]string{}

func trailScenes() []trailScene {
	return []trailScene{
		{"devices", setup, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				{method: "GET", path: "/", none: true},
				{method: "GET", path: "/devices", none: true},
				{method: "GET", path: "/devices/9999", want: "Devices / List / Device not found", status: 404},
				{method: "GET", path: "/devices/1", want: "Devices / kitchen", object: "kitchen"},
				{method: "GET", path: "/devices/1/edit", want: "Devices / kitchen / Edit", object: "kitchen"},
				{method: "GET", path: "/devices/1/delete", want: "Devices / kitchen / Delete", object: "kitchen"},
				// the rename and the identity refresh swap the trail of the page the person is on
				{method: "POST", path: "/devices/1/probe", form: url.Values{"csrf": {"tok"}}, hdr: []string{"HX-Request", "true"},
					want: "Devices / kitchen", fragment: true},
				{method: "GET", path: "/audit", none: true},
				{method: "GET", path: "/about", none: true},
				{method: "GET", path: "/whats-new", want: "System / About / What’s new"},
			}
		}},
		{"bulk", func(t *testing.T) *env { e, _ := bulkEnv(t); return e }, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				// rule 1: a page under a tab names that tab
				{method: "POST", path: "/devices/bulk/command", form: bulkForm("id", "1", "id", "2"), want: "Devices / List / Bulk action"},
				{method: "POST", path: "/devices/bulk/command", form: bulkForm("id", "1", "id", "2", "command", "Status 2"), want: "Devices / List / Bulk action"},
				{method: "GET", path: "/devices/1/console", want: "Devices / kitchen", object: "kitchen"},
				{method: "GET", path: "/devices/1/restart", want: "Devices / kitchen / Restart", object: "kitchen"},
			}
		}},
		{"bulk result", func(t *testing.T) *env {
			now := time.Now()
			return resultEnv(t, model.Job{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Status 2", State: model.JobFailed, CreatedAt: now, UpdatedAt: now})
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "GET", path: "/devices/bulk/result/7?id=1", want: "Devices / List / Bulk action"}}
		}},
		{"device backups", func(t *testing.T) *env { e, _, _ := newAdminEnv(t); return e }, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				{method: "GET", path: "/devices/1/backups", want: "Devices / kitchen", object: "kitchen"},
				{method: "GET", path: "/devices/1/backups/prune", want: "Devices / kitchen / Backups / Prune", object: "kitchen"},
				{method: "GET", path: "/devices/1/backups/delete?id=4", want: "Devices / kitchen / Backups / Delete selected", object: "kitchen"},
				// rule 3: a device is named by its name, never "Device"
				{method: "GET", path: "/backups/4/delete", want: "Devices / kitchen / Backups / Delete", object: "kitchen"},
				{method: "GET", path: "/backups/4/restore", want: "Devices / kitchen / Restore", object: "kitchen"},
				{method: "GET", path: "/devices/1/replace", want: "Devices / kitchen", object: "kitchen"},
				// the answer to an action on one device reads like the device's own pages and lights no tab (it said List)
				{method: "GET", path: "/devices/1/backups/delete", want: "Devices / kitchen / Nothing selected", object: "kitchen", status: 422},
			}
		}},
		{"device action answers", func(t *testing.T) *env {
			return setupWith(t, func(d *Deps) {
				d.DeviceCmd = &fakeCmd{runErr: errors.New("timed out"), powerErr: tasmota.ErrBadResponse}
			})
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				// the confirmation reads Devices / kitchen / Restart with no tab lit; its answer must not jump to List
				{method: "POST", path: "/devices/1/restart", form: url.Values{"csrf": {"tok"}}, want: "Devices / kitchen / Restart failed", object: "kitchen", status: http.StatusInternalServerError},
				// the page of the no-JS power switch is titled with the device name: the device is the last crumb
				{method: "POST", path: "/devices/1/power/1", form: url.Values{"csrf": {"tok"}, "action": {"TOGGLE"}}, want: "Devices / kitchen", object: "kitchen", status: http.StatusBadGateway},
			}
		}},
		{"backup of a deleted device", func(t *testing.T) *env {
			ab := &adminBackups{list: nil}
			ab.list = []model.Backup{{ID: 8, DeviceID: 0, Kind: model.BackupConfigDump, TakenAt: time.Now(), Version: "15.6.0", Size: 4096},
				// a backup whose device is gone from the inventory by the time the page reads it (deleted in between)
				{ID: 9, DeviceID: 99, Kind: model.BackupConfigDump, TakenAt: time.Now(), Version: "15.6.0", Size: 4096}}
			k := &keepBackups{adminBackups: ab}
			e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.Backups, d.BackupFleet = k, &fakeFleet{} })
			return e
		}, func(t *testing.T, e *env) []trailPage {
			// a deleted device's backup lives on the Backups tab, which is the tab that must be lit (it was nav "system")
			return []trailPage{
				{method: "GET", path: "/devices/backups", none: true},
				{method: "GET", path: "/backups/8/delete", want: "Devices / Device backups / Delete"},
				{method: "GET", path: "/backups/9/delete", want: "Devices / Device backups / Delete"},
			}
		}},
		{"fleet backups", func(t *testing.T) *env { e, _ := cleanupEnv(t); return e }, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "GET", path: "/devices/backups/prune", want: "Devices / Device backups / Prune now"}}
		}},
		{"compare", func(t *testing.T) *env { e, _, _ := newDiffEnv(t); return e }, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				{method: "GET", path: "/devices/1/backups/diff?a=3&b=6", want: "Devices / kitchen / Compare backups", object: "kitchen"},
				{method: "GET", path: "/devices/1/drift", want: "Devices / kitchen", object: "kitchen"},
			}
		}},
		{"updates", func(t *testing.T) *env {
			e, _, j, _ := setupOTA(t)
			now := time.Now()
			j.jobs = []model.Job{
				{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobAwaitReboot, TargetRelease: "16.1.0", CreatedAt: now},
				{ID: 2, BatchID: 8, DeviceID: 1, Kind: model.KindPolicy, Commands: "TelePeriod 300", State: model.JobSucceeded, CreatedBy: "adam", CreatedAt: now},
			}
			return e
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				// rule 2: a transaction lives on the Transactions tab of Updates, however it was started
				{method: "GET", path: "/devices/1/history", want: "Devices / kitchen", object: "kitchen"},
				{method: "GET", path: "/transactions", none: true},
				{method: "GET", path: "/transactions/7", want: "Updates / Activity / TX-0007"},
				{method: "GET", path: "/transactions/8", want: "Updates / Activity / TX-0008"},
				{method: "GET", path: "/transactions/7/cancel", want: "Updates / Activity / TX-0007 / Cancel"},
				{method: "GET", path: "/jobs/1/cancel", want: "Updates / Activity / TX-0007 / Cancel update"},
				{method: "GET", path: "/transactions/8/retry", want: "Updates / Activity / TX-0008 / Retry"},
				{method: "GET", path: "/firmware", none: true},
			}
		}},
		{"policy", func(t *testing.T) *env {
			e, mem, _ := rulesEnv(t, "")
			saveRule(t, e, url.Values{"change_note": {"first"}})
			sceneIDs["rule"] = ruleIDs(t, mem)[0]
			return e
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				{method: "GET", path: "/policy", none: true},
				{method: "GET", path: "/policy/io", want: "Devices / Policy / Import / export"},
				{method: "GET", path: "/policy/history", want: "Devices / Policy / History"},
				{method: "GET", path: "/policy/rules/new?command=SaveData", want: "Devices / Policy / Save data (s)"},
				{method: "GET", path: "/policy/rules/" + sceneIDs["rule"], object: "", want: "Devices / Policy / Flash wear"},
				{method: "GET", path: "/policy/rules/delete?id=" + sceneIDs["rule"], want: "Devices / Policy / Delete"},
				// the preview swaps the trail on every keystroke: it must be the whole trail
				{method: "POST", path: "/policy/rules/preview", hdr: []string{"HX-Request", "true"},
					form: ruleForm(url.Values{"id": {"new"}, "origin": {"command"}, "name": {"Power lock"}, "command": {"PowerLock"}, "want": {"0"}}),
					want: "Devices / Policy / Power lock", fragment: true},
			}
		}},
		{"system settings", func(t *testing.T) *env {
			e, _, _, _ := sysEnvFull(t, true, nil)
			return e
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				{method: "GET", path: "/settings/system", none: true},
				// rule 3: System confirmations start with System, and the tab they came from follows the entry
				{method: "GET", path: "/settings/system/reset", want: "System / Settings / Advanced / Reset"},
			}
		}},
		{"snapshots", func(t *testing.T) *env {
			s := snapSetup(t, true)
			if _, err := s.mgr.Run(t.Context(), 7); err != nil {
				t.Fatal(err)
			}
			return s.env
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "GET", path: "/settings/system/snapshots", none: true}}
		}},
		{"remove a browser", func(t *testing.T) *env {
			e, st, _, _ := pushSettingsEnv(t, &fakeNotify{})
			sceneIDs["push"] = addPush(t, st, endpointA, "local", "test:admin", "Chrome on Android")
			return e
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "GET", path: "/settings/system/push/remove/" + sceneIDs["push"], want: "System / Settings / Notifications / Remove browser"}}
		}},
		{"make compliant", func(t *testing.T) *env { return fixEnv(t, &fakeFix{}, nil) }, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "POST", path: "/policy/fix", form: url.Values{"csrf": {"tok"}, "all": {"1"}}, want: "Devices / Policy / Make compliant"}}
		}},
		{"firmware", func(t *testing.T) *env {
			return fwEnv(t, &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{{ID: 5, Release: "16.1.0", Filename: "tasmota32.bin", Size: 1024, SHA256: "def456"}}})
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{
				{method: "GET", path: "/firmware", none: true},
				{method: "GET", path: "/firmware/artifacts/5/delete", want: "Updates / Firmware / Delete"},
			}
		}},
		{"release notes", func(t *testing.T) *env { return relEnv(t, newFeed(nil)) }, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "GET", path: "/firmware/releases/16.2.0", want: "Updates / Firmware / Tasmota 16.2.0"}}
		}},
		{"broker password", func(t *testing.T) *env {
			e, _ := pwEnv(t, &fakePWTester{}, &fakePWFleet{})
			return e
		}, func(t *testing.T, e *env) []trailPage {
			return []trailPage{{method: "GET", path: "/settings/system/mqtt-passwords/broker", want: "System / Settings / MQTT / Use the broker password"}}
		}},
	}
}

// Why (#1000): a transaction started by "Make compliant" is the same page as any other: the
// Transactions tab lists it, so the trail says where it lives, not which page started it. (The
// person's way back is Cancel's job, #1002.)
func TestPolicyStartedTransactionIsListedWhereItsTrailPoints(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 2, BatchID: 8, DeviceID: 1, Kind: model.KindPolicy, Commands: "TelePeriod 300", State: model.JobSucceeded, CreatedBy: "adam", CreatedAt: time.Now()}}
	_, list := e.do(t, "GET", "/transactions", "admin", nil)
	if !strings.Contains(list, `href="/tf/transactions/8"`) {
		t.Fatalf("the Transactions tab does not list the policy transaction, so its trail would point at a page that hides it: %.500s", list)
	}
	if r, _ := e.do(t, "GET", "/transactions/8", "admin", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
}
