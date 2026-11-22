package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/stability"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The tests of #1082: a device page says one true thing about a device that is offline or never answered. Device 1
// ("kitchen") has a stored identity (it answered once), device 2 ("attic") has none: it never answered.

// truthEnv is a handler with the services the overview reads for this: restart, the stability store and a policy
// whose one rule has not been read on the attic (its values are empty).
func truthEnv(t *testing.T, more ...func(*Deps)) *env {
	t.Helper()
	devs := []model.Device{{ID: 1, Name: "kitchen"}, {ID: 2, Name: "attic"}}
	rep := policyReportOf([]policy.Rule{{Command: "TelePeriod", Want: "300"}}, devs, map[model.DeviceID]map[string]string{1: {"TelePeriod": "300"}, 2: {}})
	return setupWith(t, func(d *Deps) {
		d.DeviceCmd = &fakeCmd{}
		d.Stability = fakeStab{}
		d.Policy = &fakePolicy{rep: rep}
		for _, m := range more {
			m(d)
		}
	})
}

// bannerCount is how many inline banners the device header and what follows it carry, up to the tile row (the shell's
// own connection banners are not the device's).
func bannerCount(page string) int {
	return strings.Count(between(page, `id="devhd"`, `id="devlive"`), `class="banner `)
}

// refsResolve fails the test for every header button whose aria-describedby names an element that is not on the page:
// a screen reader then reads no reason at all, and the sighted person sees none either.
func refsResolve(t *testing.T, page string, labels ...string) {
	t.Helper()
	for _, label := range labels {
		bt := buttonTag(page, label)
		if !strings.Contains(bt, "aria-disabled") {
			continue // a button that can act needs no reason
		}
		id := describedBy(bt)
		if id == "" || strings.Count(page, `id="`+id+`"`) != 1 {
			t.Errorf("%s names %q as its reason, which is on the page %d times: %s", label, id, strings.Count(page, `id="`+id+`"`), bt)
		}
	}
}

// Why (#1082, A2-004): "Offline since <the time TasmoFleet started>" says a device went away that was never there. A
// device that never answered says Unknown in the badge and in Reachability, has the one "has not answered yet"
// banner, no stale "Stable" verdict, no relay or sensor box and no policy count that reads as compliance. The health
// tracker calls such a device Offline once its polls fail, so this is the page's mapping (pageHealth), as the list's.
func TestNeverAnsweredDeviceSaysUnknownAndNothingStale(t *testing.T) {
	e := truthEnv(t)
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, Since: time.Now()} // the tracker's Offline, no answer ever
	_, body := e.do(t, "GET", "/devices/2", "admin", nil)

	if !strings.Contains(body, `<span class="devhd-state t-neutral"><span class="devhd-dot" aria-hidden="true"></span>Unknown</span>`) {
		t.Errorf("the badge is not a neutral Unknown:\n%s", between(body, `class="devhd-name"`, `</div>`))
	}
	if reach := between(body, `data-tile="reach"`, "</div></div>"); !strings.Contains(reach, ">Unknown<") || strings.Contains(reach, "Offline") {
		t.Errorf("Reachability: %s", reach)
	}
	if strings.Contains(body, "Offline since") || strings.Contains(body, `id="devhd-off"`) {
		t.Error("a device that never answered did not go offline")
	}
	if n := strings.Count(body, `id="devhd-notyet"`); n != 1 || bannerCount(body) != 1 {
		t.Errorf("one banner, the not-answered one: %d with that id, %d banners", n, bannerCount(body))
	}
	card := between(body, `<section id="stability"`, `</section>`)
	if strings.Contains(card, ">Stable<") || !strings.Contains(card, ">No data<") || strings.Contains(card, "0 in 24 h") {
		t.Errorf("no verdict without data:\n%s", card)
	}
	if strings.Contains(body, `id="dc-relays"`) || strings.Contains(body, `id="dc-sensors"`) || strings.Contains(body, "no relay") {
		t.Error("nothing was read from the device: no Relays or Sensors card")
	}
	if pol := between(body, `data-tile="policy"`, "</div></div>"); !strings.Contains(pol, "Not read") || strings.Contains(pol, "1 / 1") {
		t.Errorf("Policy tile: %s", pol)
	}
	if strings.Contains(body, `id="devlive" class="devlive" data-off`) {
		t.Error("the live region is not dimmed as 'Last reported': nothing was ever reported")
	}
	for _, label := range []string{"Restart", "Back up"} {
		if bt := buttonTag(body, label); !blockedButton(bt) || describedBy(bt) != "devhd-notyet" {
			t.Errorf("%s is not blocked with the banner as its reason: %s", label, bt)
		}
	}
	if strings.Contains(body, `id="devhd-why"`) {
		t.Error("the banner says it: no second line")
	}
	refsResolve(t, body, "Restart", "Back up")
}

// Why (#1082): a device whose tracker state is Unknown (not yet polled) and that never answered cannot be restarted
// either: it has nothing to talk to. Restart was enabled in that window and failed after the click.
func TestRestartIsBlockedForADeviceThatNeverAnsweredWhateverTheTracker(t *testing.T) {
	e := truthEnv(t)
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthUnknown}
	_, body := e.do(t, "GET", "/devices/2", "admin", nil)
	if bt := buttonTag(body, "Restart"); !blockedButton(bt) || describedBy(bt) != "devhd-notyet" {
		t.Errorf("Restart: %s", bt)
	}
	// a device that answered once keeps its Restart: only the never-answered rule is new
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	_, body = e.do(t, "GET", "/devices/2", "admin", nil)
	if bt := buttonTag(body, "Restart"); strings.Contains(bt, "aria-disabled") {
		t.Errorf("an online device's Restart is blocked: %s", bt)
	}
}

// Why (#1082, A2-004): an identity read in an earlier run (DetectedAt is stored) means the device did answer; it is
// offline then, not unknown, whatever this run's tracker has seen. The one predicate is LastSeen and DetectedAt.
func TestADeviceThatAnsweredInAnEarlierRunIsOfflineNotUnknown(t *testing.T) {
	e := truthEnv(t)
	e.inv.devs[2] = model.Device{ID: 2, Name: "attic", Address: "10.0.0.2", Identity: model.Identity{Version: "15.0.0", DetectedAt: time.Now().Add(-48 * time.Hour)}}
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, Since: time.Now()}
	_, body := e.do(t, "GET", "/devices/2", "admin", nil)
	if !strings.Contains(body, `id="devhd-off"`) || strings.Contains(body, `id="devhd-notyet"`) || !strings.Contains(body, ">Offline<") {
		t.Error("a device that answered once is offline, with the offline banner")
	}
}

// Why (#1082, A2-002): the same fact was said three times (a paragraph in the header, the banner, the Paths card), and
// the banner listed the disabled buttons. An offline device now has exactly one banner, a lead and one sentence, and the
// buttons that cannot act point at it. This holds when the router also has a backup sentence of its own (MQTT only,
// the group topics unread): that one is not a second line while the device is offline.
func TestOfflineDeviceHasOneBannerAndNoSecondLine(t *testing.T) {
	now := time.Now()
	// no MQTT: every path is HTTP, which an offline device blocks, so the card's rows carry the offline reason
	httpFacts := transport.Facts{Now: now, FirmwareBaseURL: true, HTTP: transport.HTTPState{State: transport.HTTPOK, Since: now.Add(-6 * time.Hour)},
		Identity: model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", FreeKB: 700}}
	for name, mutate := range map[string]func(*Deps){
		"plain":                  nil,
		"http paths":             withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpFacts}}),
		"mqtt only, no backup":   withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordNone)}}),
		"mqtt only, with backup": withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordBroker)}}),
	} {
		t.Run(name, func(t *testing.T) {
			e := truthEnv(t, func(d *Deps) {
				if mutate != nil {
					mutate(d)
				}
			})
			e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: now.Add(-5 * time.Hour), Since: now.Add(-5 * time.Hour)}
			_, body := e.do(t, "GET", "/devices/1", "admin", nil)
			if bannerCount(body) != 1 || strings.Count(body, `id="devhd-off"`) != 1 {
				t.Errorf("one banner wanted, %d on the page", bannerCount(body))
			}
			if strings.Contains(body, `id="devhd-why"`) {
				t.Error("the header paragraph is gone while the device is offline")
			}
			banner := elementText(body, "devhd-off", "</div>")
			if !strings.HasPrefix(banner, "Offline since ") || !strings.HasSuffix(banner, ". Controls are off until it is back.") {
				t.Errorf("a lead and one sentence: %q", banner)
			}
			if !strings.Contains(between(body, `id="devhd-off"`, "</div></div>"), "<time") {
				t.Error("the time is a <time> stamp, shown in the browser's zone")
			}
			if bt := buttonTag(body, "Restart"); !blockedButton(bt) || describedBy(bt) != "devhd-off" {
				t.Errorf("Restart does not point at the banner: %s", bt)
			}
			if bt := buttonTag(body, "Back up"); strings.Contains(bt, "aria-disabled") && describedBy(bt) != "devhd-off" {
				t.Errorf("Back up is blocked but does not point at the banner: %s", bt)
			}
			refsResolve(t, body, "Restart", "Back up")
			card := between(body, `id="devpaths-actions"`, "</details>")
			if name == "http paths" && strings.Count(card, "Not available") == 0 {
				t.Errorf("the card still says which actions have no path:\n%s", card)
			}
			if strings.Contains(card, reasonOffline) {
				t.Errorf("the Paths card says a third time that the device is offline:\n%s", card)
			}
		})
	}
}

// Why (#447, #1082): "until it is back" must not sit under a Reachability tile that says the device's web page
// answers. When HTTP showed something after the device went offline, that is the banner's one sentence.
func TestOfflineBannerSaysWhatHTTPShowedInsteadOfUntilItIsBack(t *testing.T) {
	now := time.Now()
	e := truthEnv(t)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: now.Add(-2 * time.Hour), Since: now.Add(-time.Hour), SeenHTTP: now, LWT: "Offline"}
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	banner := elementText(body, "devhd-off", "</div>")
	if !strings.HasSuffix(banner, offlineHTTPNote(e.health[1])) || strings.Contains(banner, "until it is back") {
		t.Errorf("the banner contradicts the tile: %q", banner)
	}
}

// Why (#1082, A2-002): the backup reason is for the case where the person can act on it: the device is online but
// Back up has no path. It is one line in a row of its own under the buttons, not a paragraph in the header's meta line
// (where it pushed the buttons down and said it a second time next to the banner).
func TestOnlineDeviceThatCannotBackUpSaysWhyInARowUnderTheButtons(t *testing.T) {
	e := truthEnv(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(time.Now(), model.MQTTPasswordNone)}}))
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	why := strings.Index(body, `id="devhd-why"`)
	if why < 0 {
		t.Fatal("no note for a Back up that has no path")
	}
	if acts, meta := strings.Index(body, `class="devhd-acts"`), strings.Index(body, `class="devhd-meta"`); meta >= acts || acts >= why {
		t.Errorf("the note belongs under the buttons: meta %d, buttons %d, note %d", meta, acts, why)
	}
	if strings.Contains(between(body, `class="devhd-t"`, `class="devhd-acts"`), "devhd-why") {
		t.Error("the note sits in the header's name block")
	}
	if line := elementText(body, "devhd-why", "</p>"); !strings.HasPrefix(line, "Back up: ") || strings.Count(line, "Back up") != 1 {
		t.Errorf("one line about Back up: %q", line)
	}
	if bannerCount(body) != 0 {
		t.Errorf("an online device has no banner: %d", bannerCount(body))
	}
	refsResolve(t, body, "Restart", "Back up")
}

// Why (#1082, A2-002): the note beside Back up is the thing the person does ("Refresh the identity of ... first"), not
// the router's 55-word sentence with a UTC stamp, which stays on the Paths card for the person who wants the reasoning.
func TestBackupNoteIsOneShortSentenceAndTheCardKeepsTheFullReason(t *testing.T) {
	f := mqttFacts(time.Now(), model.MQTTPasswordNone)
	f.MQTT.OthersGroupUnread = []string{"Basement Fan", "Kitchen Ceiling Light"}
	e := truthEnv(t, withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}))
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	line := elementText(body, "devhd-why", "</p>")
	if want := "Back up: Refresh the identity of Basement Fan and Kitchen Ceiling Light first."; strings.TrimSpace(line) != want {
		t.Errorf("note %q, want %q", line, want)
	}
	if strings.Contains(line, "UTC") || strings.Contains(line, "No path") || sentences(line) != 1 {
		t.Errorf("a plain one-sentence note without a timestamp: %q", line)
	}
	if card := between(body, `id="devpaths"`, `</section>`); !strings.Contains(card, "have not been read yet, so a message to this device") {
		t.Errorf("the Paths card keeps the full sentence:\n%s", card)
	}
}

// Why (#1082, #409): a device that refused the stored password answered, so it is not "never answered": the badge says
// "Auth failed" and Reachability says "Login refused", and a banner saying "has not answered yet" next to them is the
// disagreement this issue removes. The tracker has no LastSeen for it when the refusal came on its first poll.
func TestADeviceThatRefusedTheLoginDidAnswerAndIsNotNeverAnswered(t *testing.T) {
	e := truthEnv(t)
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthAuthFailed, Since: time.Now()}
	_, body := e.do(t, "GET", "/devices/2", "admin", nil)
	if strings.Contains(body, "has not answered yet") || strings.Contains(body, `id="devhd-notyet"`) {
		t.Error("a device that refused its login is not 'not answered yet'")
	}
	if !strings.Contains(body, ">Auth failed<") {
		t.Errorf("the badge keeps saying why:\n%s", between(body, `class="devhd-name"`, `</div>`))
	}
	if neverAnswered(model.Device{}, model.Health{State: model.HealthAuthFailed}) {
		t.Error("neverAnswered is false for a refused login")
	}
}

// Why (#1072, #1082): the buttons' aria-describedby must name something that is on the page in every state the header
// can be in. A never-answered device whose operation holds it has neither banner (the operation's banner says it), so
// its buttons point at the reason line, which must then be there.
func TestEveryHeaderReasonReferenceResolvesInEveryState(t *testing.T) {
	now := time.Now()
	hold := &fakeJobs{jobs: []model.Job{{ID: 1, BatchID: 7, DeviceID: 2, State: model.JobApplying, Kind: model.KindBackup, Commands: model.BackupMarker, CreatedAt: now}}}
	for _, tc := range []struct {
		name   string
		more   func(*Deps)
		health model.Health
		dev    model.DeviceID
		want   string // the id the buttons name
	}{
		{"offline", nil, model.Health{State: model.HealthOffline, LastSeen: now.Add(-time.Hour), Since: now.Add(-time.Hour)}, 1, "devhd-off"},
		{"never answered", nil, model.Health{State: model.HealthOffline, Since: now}, 2, "devhd-notyet"},
		{"never answered while an operation holds it", func(d *Deps) { d.Jobs = hold }, model.Health{State: model.HealthOffline, Since: now}, 2, "devhd-why"},
		{"online without a backup path", withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordNone)}}), model.Health{State: model.HealthOnline, LastSeen: now}, 1, "devhd-why"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var more []func(*Deps)
			if tc.more != nil {
				more = append(more, tc.more)
			}
			e := truthEnv(t, more...)
			tc.health.DeviceID = tc.dev
			e.health[tc.dev] = tc.health
			_, body := e.do(t, "GET", "/devices/"+itoa(int(tc.dev)), "admin", nil)
			if bt := buttonTag(body, "Back up"); describedBy(bt) != tc.want {
				t.Errorf("Back up names %q, want %q: %s", describedBy(bt), tc.want, bt)
			}
			refsResolve(t, body, "Restart", "Back up")
		})
	}
}

// Why (#126, #1082): the not-answered banner is part of the header's slot, so the live refresh that follows the first
// answer takes it out without a reload, and one that finds the device still silent puts it in. Before, it was page
// content that stayed until the person reloaded.
func TestLiveRefreshCarriesTheNotAnsweredBannerInTheSlot(t *testing.T) {
	e := truthEnv(t)
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, Since: time.Now()}
	_, body := e.do(t, "GET", "/devices/2/live", "admin", nil)
	if slot := between(body, `<div id="devhd-slot"`, "</div></div>"); !strings.Contains(slot, `hx-swap-oob="true"`) || !strings.Contains(slot, `id="devhd-notyet"`) {
		t.Errorf("the swap does not bring the banner: %s", slot)
	}
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	_, body = e.do(t, "GET", "/devices/2/live", "admin", nil)
	if !strings.Contains(body, `<div id="devhd-slot" hx-swap-oob="true"></div>`) {
		t.Errorf("after the first answer the swap empties the slot: %s", between(body, `<div id="devhd-slot"`, "</div>"))
	}
}

// Why (#1082, A2-005): an offline device's Stability numbers are what it reported before it went away; the card says
// "Last reported" like the tiles (a word, not an opacity, #1071). An online device's card does not.
func TestStabilityCardOfAnOfflineDeviceSaysLastReported(t *testing.T) {
	e := truthEnv(t)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOffline, LastSeen: time.Now().Add(-time.Hour), Since: time.Now().Add(-time.Hour), SignalDBm: -56}
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if card := between(body, `<section id="stability"`, `</section>`); !strings.Contains(card, "Last reported") {
		t.Errorf("offline card: %s", card)
	}
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SignalDBm: -56}
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if card := between(body, `<section id="stability"`, `</section>`); strings.Contains(card, "Last reported") {
		t.Errorf("online card: %s", card)
	}
}

// Why (#1082): the card's verdict for a device that never answered is "No data" in a neutral badge, and the restart
// count is a dash: "0 in 24 h" says the device was quiet, and nobody was watching.
func TestStabilityCardOfADeviceThatNeverAnsweredHasNoVerdict(t *testing.T) {
	c := stabCardOf(time.Now(), stability.Result{Level: stability.Stable}, nil, model.Health{}, "", stabState{NoData: true})
	if c.badgeLabel() != "No data" || c.badgeTone() != ui.ToneNeutral {
		t.Errorf("badge %q %q", c.badgeLabel(), c.badgeTone())
	}
	for _, r := range c.Rows {
		if r.Key == "Restarts" && (r.Main != "—" || r.Rest != "") {
			t.Errorf("restarts: %q%q", r.Main, r.Rest)
		}
	}
	c = stabCardOf(time.Now(), stability.Result{Level: stability.Stable}, nil, model.Health{}, "", stabState{})
	if c.badgeLabel() != "Stable" || c.badgeTone() != ui.ToneOK {
		t.Errorf("a device with data keeps its verdict: %q %q", c.badgeLabel(), c.badgeTone())
	}
}

// Why (#1082, A2-005): "2 / 2" next to "2 rules not read yet" reads as all compliant while nothing was read. A rule that
// was not read has not matched: it is left out of n, and a device none of whose rules was read says "Not read".
func TestPolicyTileDoesNotCountUnreadRulesAsMatched(t *testing.T) {
	for _, tc := range []struct {
		name  string
		c     policyCard
		value string
		tone  string
	}{
		{"all read and matching", policyCard{Rules: 3}, "3 / 3", "ok"},
		{"nothing read", policyCard{Rules: 2, Unknown: 2}, "Not read", "neutral"},
		{"one of three unread", policyCard{Rules: 3, Unknown: 1}, "2 / 3", "neutral"},
		{"one deviation, one unread", policyCard{Rules: 3, Unknown: 1, Deviations: []deviation{{Rule: "r", Have: "a", Want: "b"}}}, "1 / 3", "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := policyTile(&tc.c, false)
			if tl.Value != tc.value || string(tl.Tone) != tc.tone {
				t.Errorf("got %q (%s), want %q (%s)", tl.Value, tl.Tone, tc.value, tc.tone)
			}
		})
	}
}

// Why (#1082): there is one definition of "never answered". The device page, the list's counts and the dashboard read
// it, so a device cannot be "Unknown" on one and "Offline" on the other.
func TestNeverAnsweredIsOneDefinition(t *testing.T) {
	d := model.Device{}
	if !neverAnswered(d, model.Health{}) {
		t.Error("no contact and no identity: never answered")
	}
	if neverAnswered(d, model.Health{LastSeen: time.Now()}) || neverAnswered(model.Device{Identity: model.Identity{DetectedAt: time.Now()}}, model.Health{}) {
		t.Error("either one is an answer")
	}
	row := deviceRow{Device: d, Health: model.Health{State: model.HealthOffline}}
	if shownState(row) != model.HealthUnknown {
		t.Error("the dashboard maps the same device to Unknown")
	}
	if got := pageHealth(d, row.Health, nil).State; got != model.HealthUnknown {
		t.Errorf("the page maps it to %s", got)
	}
	if got := pageHealth(d, row.Health, &model.Job{}).State; got != model.HealthOffline {
		t.Errorf("an operation holds it: the tracker's state stays, %s", got)
	}
}
