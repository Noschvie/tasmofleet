package web

import (
	"html"
	"regexp"
	"testing"
)

var phoneTitle = regexp.MustCompile(`<span class="ph-title" id="ph-title"[^>]*>([^<]*)</span>`)

// Why (#1079, A3-030, A7-031): the phone header is one line that cannot break inside a word, so a page's
// own title ("Living Room Ceiling Light backups", "Users" above the h1 "Users") was cut mid-word or said
// twice. CSS cannot cut at a word boundary, so the header names the area (the lit group of the top bar)
// and the h1 below carries the full name. This is the rule, on every page the harness renders.
func TestPhoneTitleIsTheAreaNotThePageTitle(t *testing.T) {
	check := func(t *testing.T, e *env, path, want string) {
		t.Helper()
		res, body := e.do(t, "GET", path, "admin", nil)
		m := phoneTitle.FindStringSubmatch(body)
		if res.StatusCode != 200 || m == nil {
			t.Errorf("%s: status %d, phone title present: %v", path, res.StatusCode, m != nil)
			return
		}
		if got := html.UnescapeString(m[1]); got != want {
			t.Errorf("%s: phone title %q, want the area %q", path, got, want)
		}
	}
	t.Run("device pages", func(t *testing.T) {
		e, _, _ := newAdminEnv(t)
		check(t, e, "/devices/1", "Devices")         // the name is the h1, not the header
		check(t, e, "/devices/1/backups", "Devices") // "kitchen backups" was the title here
	})
	t.Run("system pages", func(t *testing.T) {
		e, _, _, _ := sysEnvFull(t, true, nil)
		check(t, e, "/settings/system", "System") // "Settings" is the lit tab, "System" the area
		// "Users" above the h1 "Users" (the page is rendered by the auth pages, not by a route of this harness)
		m := phoneTitle.FindStringSubmatch(renderUsersPage(t, usersFixture()))
		if m == nil || html.UnescapeString(m[1]) != "System" {
			t.Errorf("/users: phone title %q, want the area \"System\"", m)
		}
	})
	// and never anything but the lit group, wherever the page sits (a message or a missing page is Dashboard's)
	t.Run("every page the harness renders", func(t *testing.T) {
		e := setup(t)
		for _, p := range shellPages {
			res, body := e.do(t, "GET", p, "admin", nil)
			group := trailBarGroup.FindStringSubmatch(body)
			m := phoneTitle.FindStringSubmatch(body)
			if res.StatusCode >= 500 || group == nil || m == nil {
				continue
			}
			if got := html.UnescapeString(m[1]); got != group[1] {
				t.Errorf("%s: phone title %q, the lit group is %q", p, got, group[1])
			}
		}
	})
}
