package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// cooldownEnv is the rule pages' environment with the page clock stopped at now and a rule policy whose live
// check holds the kitchen back until until.
func cooldownEnv(t *testing.T, now, until time.Time) (*env, *rulesMem, *rulePolicy) {
	t.Helper()
	rp := defaultRP()
	rp.cooldown = map[model.DeviceID]time.Time{1: until}
	e, mem, _ := rulesEnvDeps(t, func(d *Deps) { d.Policy = rp; d.Clock = noonClock{now} })
	return e, mem, rp
}

// Why: a Run now that read nothing for a device looks like a broken check ("Not judged" stays). Every tab Run now
// is drawn on says which device was left alone and from when it can be read again, as a day-relative time that
// is not mistaken for today when it is not (#422, #989).
func TestRunNowNamesTheDevicesTheCooldownHeldBack(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		form url.Values
	}{
		{"overview", time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local), url.Values{}},
		{"deviations", time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local), url.Values{"back": {"deviations"}}},
		{"history", time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local), url.Values{"back": {"history"}}},
		{"across midnight", time.Date(2026, 11, 19, 23, 58, 0, 0, time.Local), url.Values{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			until := c.now.Add(3 * time.Minute)
			e, mem, _ := cooldownEnv(t, c.now, until)
			ruleSeed(t, e, mem, nil)
			c.form.Set("csrf", "tok")
			r, body := e.do(t, "POST", "/policy/live", "admin", c.form)
			if r.StatusCode != 200 {
				t.Fatalf("status %d", r.StatusCode)
			}
			note := between(body, `data-cooldown`, "</div>")
			if !strings.Contains(note, "Read less than 5 minutes ago, not read again:") || !strings.Contains(note, "kitchen") {
				t.Errorf("the note names the cooldown and the device:\n%q", note)
			}
			want := ui.StampRelativeText(c.now, until)
			if c.name == "across midnight" && strings.HasPrefix(want, "Today") {
				t.Fatalf("a time on the next day must carry its date: %q", want)
			}
			if !strings.Contains(note, `data-ts="relative"`) || !strings.Contains(note, ">"+want+"</time>") {
				t.Errorf("the time %q is a day stamp:\n%q", want, note)
			}
		})
	}
}

// Why: without a hold the page must not claim one.
func TestRunNowSaysNothingAboutTheCooldownWhenNothingWasHeld(t *testing.T) {
	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local)
	e, mem, rp := cooldownEnv(t, now, now.Add(3*time.Minute))
	rp.cooldown = nil
	ruleSeed(t, e, mem, nil)
	_, body := e.do(t, "POST", "/policy/live", "admin", url.Values{"csrf": {"tok"}})
	if strings.Contains(body, "not read again") {
		t.Error("no device was held back")
	}
}

// Why: Check now on a rule page redirects (a reload must not post again), so the reason for a skipped read has
// to travel in the URL. It is shown for the devices this rule judges only: the hold of an unrelated device says
// nothing about this rule's result. The time in the URL changes the display and nothing else, so a forged one
// is not drawn.
func TestRuleCheckNowSaysUntilWhenTheCooldownHeldItsDevices(t *testing.T) {
	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local)
	until := now.Add(3 * time.Minute)
	e, mem, rp := cooldownEnv(t, now, until)
	id := ruleSeed(t, e, mem, nil)
	rp.judged = []policy.Outcome{{DeviceID: 1, RuleID: id, Kind: policy.OutcomeUnknown}}
	form := url.Values{"csrf": {"tok"}, "rule": {id}}

	r, _ := e.do(t, "POST", "/policy/live", "admin", form)
	loc := "/tf/policy/rules/" + id + "?ok=cooldown&until=" + strconv.FormatInt(until.Unix(), 10)
	if r.Header.Get("Location") != loc {
		t.Fatalf("location %q, want %q", r.Header.Get("Location"), loc)
	}
	_, body := e.do(t, "GET", strings.TrimPrefix(loc, "/tf"), "admin", nil)
	note := between(body, `data-cooldown`, "</div>")
	if !strings.Contains(note, "read less than 5 minutes ago were not read again") || !strings.Contains(note, ">Today 14:03</time>") {
		t.Errorf("the page says why and until when:\n%q", note)
	}
	for _, bad := range []string{"until=abc", "until=" + strconv.FormatInt(now.Add(-time.Hour).Unix(), 10), "until=" + strconv.FormatInt(now.Add(time.Hour).Unix(), 10)} {
		_, body := e.do(t, "GET", "/policy/rules/"+id+"?ok=cooldown&"+bad, "admin", nil)
		note := between(body, `data-cooldown`, "</div>")
		if !strings.Contains(note, "were not read again") || strings.Contains(note, "<time") {
			t.Errorf("%s: the reason without a time:\n%q", bad, note)
		}
	}

	rp.judged = []policy.Outcome{{DeviceID: 2, RuleID: id, Kind: policy.OutcomeUnknown}} // kitchen is held, but this rule judges attic
	r, _ = e.do(t, "POST", "/policy/live", "admin", form)
	if r.Header.Get("Location") != "/tf/policy/rules/"+id+"?ok=checked" {
		t.Errorf("a hold of a device this rule does not judge is no news for it: %q", r.Header.Get("Location"))
	}
}

// Why: when the whole fleet is held back the note must stay a note; it names the first few devices and counts
// the rest instead of listing a fleet.
func TestRunNowNamesAtMostFiveHeldDevices(t *testing.T) {
	now := time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local)
	e, mem, rp := cooldownEnv(t, now, now.Add(3*time.Minute))
	rp.more = 5 // eight devices in all
	for _, d := range rp.devices() {
		rp.cooldown[d.ID] = now.Add(3 * time.Minute)
	}
	ruleSeed(t, e, mem, nil)
	_, body := e.do(t, "POST", "/policy/live", "admin", url.Values{"csrf": {"tok"}})
	note := between(body, `data-cooldown`, "</div>")
	if strings.Contains(note, " ,") || !strings.Contains(note, "), and 3 more devices") {
		t.Errorf("the list reads as a sentence:\n%q", note)
	}
	if strings.Count(note, "(from ") != 5 || !strings.Contains(note, "and 3 more devices") {
		t.Errorf("five named, three counted:\n%q", note)
	}
}
