package web

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/restore"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Why (#840, #1078): since 0.37.0 MQTT is the first transport for everything, so the broker hands every command,
// password and configuration dump to every client allowed to subscribe to the device's topics. An access control list
// on the broker is the only thing that keeps those clients out, and TasmoFleet cannot see whether there is one. The
// warning therefore stands where a secret leaves through the broker by the person's own action and where the broker is
// set up, and nowhere else (a notice everywhere is a notice nobody reads). Since #1078 (UI audit A1-032, A2-036, A3-041,
// A6-002) it is one short warn banner, the same on every page: a 210 px red wall of bold text read as an incident and
// pushed the page's own content away, and each page's own long paragraph made the warning differ from page to page.
// The long explanation lives in the guide.
//
// aclSentence is the notice's one sentence (ui.BrokerACLText), spelled out so a page that words it otherwise is caught.
const aclSentence = "Any client on it can read device passwords and backups."

// aclNoticeHTML is the notice as every page must render it: ui.Banner in tone warn (never danger), the shared lead in
// bold, the one sentence in regular weight, and exactly one action, the guide in a new tab without the opener.
func aclNoticeHTML() string {
	return `<div class="banner tone-warn" role="status"><span class="banner-dot" aria-hidden="true"></span><div class="banner-text"><b>` + ui.BrokerACLLead + `</b> ` +
		aclSentence + `</div><a class="banner-btn" href="` + ui.BrokerACLDocURL + `" target="_blank" rel="noopener noreferrer">Set up the ACL ` +
		`<span class="sr-only">(opens in a new tab)</span></a></div>`
}

// aclCount is how many broker ACL notices a page has: each one carries the shared lead once, nothing else does.
func aclCount(page string) int { return strings.Count(page, ui.BrokerACLLead) }

// countACL is aclCount, and it checks that each notice is the one short warn banner.
func countACL(t *testing.T, page string) int {
	t.Helper()
	n := aclCount(page)
	if got := strings.Count(page, aclNoticeHTML()); got != n {
		t.Errorf("%d ACL notices, %d of them the one short warn banner (tone warn, the lead, one sentence, one action to the guide)", n, got)
	}
	return n
}

func TestSystemMQTTTabCarriesTheBrokerACLWarning(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if n := countACL(t, page); n != 1 {
		t.Fatalf("the MQTT tab has %d ACL notices, want 1", n)
	}
	// it belongs to the MQTT tab: after its opening tag and before the broker fields
	start := strings.Index(page, `data-sys-tab="mqtt"`)
	notice := strings.Index(page, ui.BrokerACLLead)
	fields := strings.Index(page, `name="mqtt.host"`)
	if start < 0 || notice < start || notice > fields {
		t.Errorf("the notice is not at the top of the MQTT tab: tab %d, notice %d, broker field %d", start, notice, fields)
	}
}

// aclBrokerService is the settings service with a broker configured (tcp://broker.lan:1883, user "tf") and nothing
// hidden: the state in which the notice shows wherever a path goes over MQTT (#880).
func aclBrokerService(t *testing.T) *sysconfig.Service {
	t.Helper()
	mem := &sysMem{rows: map[string]store.SystemSetting{
		"MQTT_URL":  {Key: "MQTT_URL", Value: "tcp://broker.lan:1883"},
		"MQTT_USER": {Key: "MQTT_USER", Value: "tf"},
	}}
	cfg, err := config.Load([]string{"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := sysconfig.New(context.Background(), sysconfig.Options{Base: cfg, Store: mem, Audit: &fakeAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// brokerWithOwnDevice is an env with a configured broker and the device that has its own MQTT password.
func brokerWithOwnDevice(t *testing.T) (*env, *sysconfig.Service) {
	t.Helper()
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) {
		inv := d.Inventory.(*fakeInv)
		inv.devs[7] = ownDevice()
		d.MQTTPasswords = &fakeMQTTPW{inv: inv}
	})
	return e, svc
}

// aclHelpLine is the plain help line on the broker ACL (#1078) where the notice does not stand: its link is the guide.
const aclHelpLink = `<a href="` + ui.BrokerACLDocURL + `" target="_blank" rel="noopener noreferrer">Set up the ACL<span class="sr-only"> (opens in a new tab)</span></a>`

// Why (#1078, A2-036, A6-002, A3-041): the notice is about the broker, not about the page the person is on. On the
// device's Edit form it was the largest element, a red block between the web password and the MQTT password, while the
// person fixed an address; on the setup's last step it was a 20-line wall above an empty broker host, where nobody can
// act on it yet; on the restore page it repeated what the page's own transport sentence says. Those three places carry
// no notice: the edit form and the setup a plain help line with the guide, the restore page its sentence. All three
// asked with a broker configured and the warning not hidden, the state in which the notice shows everywhere else.
func TestBrokerACLNoticeLeftSetupEditFormAndRestore(t *testing.T) {
	se, _, _ := setupHandlerEnv(t)
	_, setup := se.do(t, "GET", "/setup", "admin", nil)
	if n := aclCount(setup); n != 0 {
		t.Errorf("setup step 3 has %d ACL notices, want none", n)
	}
	host, help := strings.Index(setup, `name="mqtt.host"`), strings.Index(setup, aclHelpLink)
	if host < 0 || help < host {
		t.Errorf("the setup does not link the guide after the broker fields (broker field %d, guide link %d)", host, help)
	}

	ee, _ := brokerWithOwnDevice(t)
	_, edit := ee.do(t, "GET", "/devices/7/edit", "admin", nil)
	if n := aclCount(edit); n != 0 {
		t.Errorf("the device Edit form has %d ACL notices, want none", n)
	}
	mode, line := strings.Index(edit, `name="mqtt_password_mode"`), strings.Index(edit, aclHelpLink)
	if mode < 0 || line < mode || strings.Count(edit, aclHelpLink) != 1 {
		t.Errorf("the Edit form has no single help line with the guide under the MQTT password source (source %d, line %d)", mode, line)
	}
	if !strings.Contains(edit, `<small class="ufield-help acl-help">Without a broker ACL, any client on the broker can read this password and the backups.`) {
		t.Error("the Edit form's help line does not say what the broker exposes")
	}

	re, _, rs, _ := newDiffEnvWith(t, aclBrokerService(t))
	rs.assess = restore.Assessment{ViaMQTT: true, PathNote: "over MQTT first (FileUpload)"}
	_, rp := re.do(t, "GET", "/backups/1/restore", "admin", nil)
	if n := aclCount(rp); n != 0 {
		t.Errorf("the restore page over MQTT has %d ACL notices, want none", n)
	}
	// the risk is still stated where the transfer starts (T5, T18): by the restore's own transport sentence
	if !strings.Contains(rp, "without a broker ACL that is every client of the broker") {
		t.Error("the restore page over MQTT no longer says who can read the configuration")
	}
}

// Why (#1078, A6-001, A6-002): with no broker there is nothing a broker could hand out, so a warning that "every client
// of your broker" can read passwords is false and opens the page in alarm. System, MQTT says that MQTT is off instead.
// System, MQTT (A6-001: it asked only whether the warning was hidden) and the setup (A6-002: no condition at all) fail
// without the fix; the Edit form already asked brokerACLOpen and guards that its help line keeps doing so.
func TestNoBrokerMeansNoACLNotice(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, tab := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if n := aclCount(tab); n != 0 {
		t.Errorf("System, MQTT without a broker has %d ACL notices", n)
	}
	if !strings.Contains(tab, "MQTT is off. Enter a broker host to turn it on.") {
		t.Error("System, MQTT without a broker does not say that MQTT is off")
	}

	se, ssvc, _ := setupHandlerEnv(t)
	if _, err := ssvc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, setup := se.do(t, "GET", "/setup", "admin", nil)
	if n := aclCount(setup); n != 0 {
		t.Errorf("setup step 3 without a broker has %d ACL notices", n)
	}

	ee, esvc := brokerWithOwnDevice(t)
	if _, err := esvc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, edit := ee.do(t, "GET", "/devices/7/edit", "admin", nil)
	if !strings.Contains(edit, `name="mqtt_password_mode"`) {
		t.Fatal("the MQTT password section vanished with the broker: it must stay (the password can be stored first)")
	}
	if n := aclCount(edit); n != 0 || strings.Contains(edit, aclHelpLink) {
		t.Errorf("the Edit form without a broker has %d ACL notices or the help line", n)
	}
}

// Why (#826, #840): the sentence the console puts before a hidden command that goes over MQTT says plainly that,
// without an ACL, that is every client of the broker, and the confirmation shows the notice; over HTTP there is
// nothing to warn about.
func TestConsoleConfirmationOverMQTTWarnsAboutTheBrokerACL(t *testing.T) {
	for _, tc := range []struct {
		name string
		mqtt bool
		want int
	}{{"over MQTT", true, 1}, {"over HTTP", false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &routeStub{}
			if tc.mqtt {
				rs.set(false, 1)
			} else {
				rs.set(false)
			}
			e, _ := noticeEnv(t, rs)
			_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("WebPassword hunter2-840"))
			if !strings.Contains(body, "Confirm sending") {
				t.Fatalf("no confirmation: %s", body)
			}
			if n := countACL(t, body); n != tc.want {
				t.Fatalf("%d ACL notices on the confirmation, want %d", n, tc.want)
			}
			if tc.mqtt && !strings.Contains(body, "without a broker ACL that is every client of the broker") {
				t.Errorf("the route sentence does not say who can read it: %s", body)
			}
		})
	}
}

func TestBulkCommandPreviewOverMQTTWarnsAboutTheBrokerACL(t *testing.T) {
	rs := &routeStub{}
	rs.set(false, 1, 2)
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd, d.CommandBatch, d.TypedRoutes, d.SysConfig = &fakeCmd{}, &fakeBatch{}, rs.routes, aclBrokerService(t)
	})
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "PowerDelta 10"))
	if n := countACL(t, body); n != 1 {
		t.Fatalf("the preview has %d ACL notices, want 1: %s", n, body)
	}
	// the same preview with every device over HTTP has none
	rs.set(false)
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "PowerDelta 10"))
	if n := aclCount(body); n != 0 {
		t.Errorf("a preview over HTTP only has %d ACL notices", n)
	}
}

// Why (#607, #840): a backup over MQTT puts the device's whole settings dump, with every secret, on the broker. The
// bulk backup page is where a person starts several at once, so it warns when a broker is configured; without a
// broker there is nothing to warn about.
func TestBulkBackupScopePageWarnsAboutTheBrokerACLWhenABrokerIsConfigured(t *testing.T) {
	withFacts := func(d *Deps) {
		d.BackupBatch = &fakeBackupBatch{}
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(time.Now(), model.MQTTPasswordOwn)}}
	}
	e, _, _, _ := sysEnvFull(t, true, withFacts)
	_, body := e.do(t, "POST", "/devices/bulk/backup", "admin", url.Values{"csrf": {"tok"}, "id": {"1"}})
	if n := countACL(t, body); n != 1 {
		t.Fatalf("the bulk backup page has %d ACL notices, want 1: %s", n, body)
	}
	e, svc, _, _ := sysEnvFull(t, true, withFacts)
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, body = e.do(t, "POST", "/devices/bulk/backup", "admin", url.Values{"csrf": {"tok"}, "id": {"1"}})
	if n := aclCount(body); n != 0 {
		t.Errorf("without a broker the bulk backup page has %d ACL notices", n)
	}
}

// Why (#853, follow-up of #840): a backup over MQTT carries every secret of the device through the broker (#607), so
// the places that start one say so, not only the restore: the device's Back up button, "Back up all now" and the
// schedule that backs up by itself. The one notice is the one component of #840 with its one text (#1078); per-user
// dismissal was decided against, so there is nothing to close it.
//
// devHeadOf is the header block of a device page: the Back up button and what stands by it.
func devHeadOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="devhd"`)
	if i < 0 {
		t.Fatal("no device header in the page")
	}
	rest := page[i:]
	if j := strings.Index(rest, `<nav class="utabs"`); j >= 0 { // the device tabs follow the header
		rest = rest[:j]
	}
	return rest
}

// httpOnlyBackupFacts is a device a backup reaches over HTTP only: it is not on the broker.
func httpOnlyBackupFacts() transport.Facts {
	return transport.Facts{Now: time.Now(), HTTP: transport.HTTPState{State: transport.HTTPOK}, FirmwareBaseURL: true,
		Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota32", FreeKB: 1700}}
}

func TestDeviceBackUpButtonCarriesTheBrokerACLWarningOnlyOverMQTT(t *testing.T) {
	now := time.Now()
	facts := withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{
		1: mqttFacts(now, model.MQTTPasswordOwn), 2: httpOnlyBackupFacts()}})
	e := setupWith(t, func(d *Deps) { facts(d); d.SysConfig = aclBrokerService(t) })
	if p := transport.Choose(mqttFacts(now, model.MQTTPasswordOwn)).Backup; p != transport.MQTT {
		t.Fatalf("the fixture's backup path is %v, not MQTT", p)
	}
	if p := transport.Choose(httpOnlyBackupFacts()).Backup; p != transport.HTTP {
		t.Fatalf("the HTTP fixture's backup path is %v, not HTTP", p)
	}
	for _, tab := range []string{"/devices/1", "/devices/1/backups"} {
		_, page := e.do(t, "GET", tab, "admin", nil)
		if n := countACL(t, page); n != 1 {
			t.Fatalf("%s has %d ACL notices, want 1", tab, n)
		}
		head := devHeadOf(t, page)
		if !strings.Contains(head, ui.BrokerACLLead) {
			t.Errorf("%s: the notice is not in the header, by the Back up button", tab)
		}
		if strings.Index(head, ui.BrokerACLLead) < strings.Index(head, ">Back up</button>") {
			t.Errorf("%s: the notice comes before the Back up button", tab)
		}
	}
	_, page := e.do(t, "GET", "/devices/2", "admin", nil)
	if n := aclCount(page); n != 0 {
		t.Errorf("a device that is backed up over HTTP has %d ACL notices", n)
	}
}

// Why (#853): the notice says "the warning stays visible until the broker has an ACL". It belongs to the device's
// backup path, not to whether Back up can be pressed right now: while a job holds the device the button is disabled,
// but the running backup is the very one that sends every password through the broker, and the live overview swaps
// the header out of band, so a notice tied to the button would vanish and come back and shift the page.
func TestDeviceBackUpNoticeStaysWhileAJobHoldsTheDevice(t *testing.T) {
	now := time.Now()
	jobs := &fakeJobs{jobs: []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobApplying, CreatedAt: now}}}
	e := setupWith(t, func(d *Deps) {
		withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}})(d)
		d.Jobs = jobs
		d.SysConfig = aclBrokerService(t)
	})
	for _, tab := range []string{"/devices/1", "/devices/1/backups"} {
		_, page := e.do(t, "GET", tab, "admin", nil)
		if !blockedButton(buttonTag(page, "Back up")) || !strings.Contains(elementText(page, "devhd-why", "</p>"), "Unavailable while a backup of the device is running.") {
			t.Fatalf("%s: the fixture's Back up button is not held by the running backup", tab)
		}
		if n := aclCount(devHeadOf(t, page)); n != 1 {
			t.Errorf("%s: %d ACL notices in the header while a backup runs, want 1", tab, n)
		}
	}
}

// Why (#684, #853): the notice says that a backup travels over the broker. It belongs where a backup is started
// and stands once: Back up all now moved from the Devices card to the page header (the table is last on a
// phone), and the notice moved with it, right below the header, above the cards. It is not repeated in a card.
func TestFleetBackupsPageCarriesTheBrokerACLWarningOnceByBackUpAll(t *testing.T) {
	for _, role := range []string{"admin", "operator"} {
		t.Run(role, func(t *testing.T) {
			e := fleetEnv(t, fleetFixture(time.Now()))
			_, body := e.do(t, "GET", "/devices/backups", role, nil)
			if n := countACL(t, body); n != 1 {
				t.Fatalf("the Backups page has %d ACL notices, want 1 (by Back up all now; the same words twice on one page read as noise)", n)
			}
			for _, card := range []string{"schedule", "devices", "lastrun"} {
				if n := aclCount(cardOf(t, body, card)); n != 0 {
					t.Errorf("the %s card holds %d ACL notices, the notice stands once below the page header", card, n)
				}
			}
			head, notice, wrap := strings.Index(body, `class="pghead"`), strings.Index(body, ui.BrokerACLLead), strings.Index(body, `class="syswrap"`)
			if head < 0 || notice < head || notice > wrap {
				t.Fatalf("the notice (%d) is not between the page header (%d) and the card stacks (%d)", notice, head, wrap)
			}
			if role == "admin" {
				btn := strings.Index(body, "Back up all now")
				if btn < head || btn > notice || !strings.Contains(body[btn:notice], "</div>") {
					t.Errorf("the notice (%d) does not follow the page header that holds Back up all now (%d), outside its action row", notice, btn)
				}
			}
		})
	}
	// without a broker nothing is sent through one: no notice
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if n := aclCount(body); n != 0 {
		t.Errorf("without a broker the Backups page has %d ACL notices", n)
	}
}

// Why (#924, #1078): on a firmware built with USE_UFILESYS_MQTT a backup over MQTT sends every file through the broker
// too: Berry scripts with their tokens and the TLS private key, each file after a start command that carries the
// device's MQTT password. The short notice must not narrow what crosses the broker to the configuration (that told a
// person on such a build that the scripts and keys stay off it): it says "backups", whole, and the guide it leads to
// names the files.
func TestTheACLNoticesOfABackupNameTheFilesItSendsOverMQTT(t *testing.T) {
	e := fleetEnv(t, fleetFixture(time.Now()))
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if countACL(t, body) != 1 || !strings.Contains(body, aclSentence) {
		t.Fatal("the Backups page has no ACL notice that says the backups can be read")
	}
	if strings.Contains(body, "whole configuration") {
		t.Error("the Backups page's notice narrows what a backup sends to the configuration")
	}
	guide, err := os.ReadFile("../../docs/mqtt-broker-acl.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"**every file**", "Berry scripts with their tokens", "the TLS private key"} {
		if !strings.Contains(string(guide), w) {
			t.Errorf("the broker ACL guide the notice leads to does not say %q", w)
		}
	}
}

// Why (#853, #1078): the help line under the device's MQTT password source says that, without a broker ACL, the
// broker hands out the password and the backups. Without a configured broker neither goes anywhere, and once the
// admin hid the warning for this broker the line goes with it, like the notice everywhere else.
func TestDevicePasswordACLHelpFollowsTheBrokerACLCondition(t *testing.T) {
	e, svc := brokerWithOwnDevice(t)
	edit := func() string { _, page := e.do(t, "GET", "/devices/7/edit", "admin", nil); return page }
	if !strings.Contains(edit(), aclHelpLink) {
		t.Fatal("with a broker the Edit form has no help line on the ACL")
	}
	aclHideNow(t, svc)
	if strings.Contains(edit(), aclHelpLink) {
		t.Error("the help line stays after the admin hid the warning for this broker")
	}
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD", "MQTT_ACL_WARNING_HIDDEN"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(edit(), aclHelpLink) {
		t.Error("without a broker the Edit form has the help line")
	}
}

// aclShown counts the notices of the Backups page (Back up all now) and the System MQTT tab, the pages whose notice
// hiding hides. The console and bulk command confirmations and the device's Back up button read the same condition
// (brokerACLOpen); their tests above and below run with it open.
func aclShown(t *testing.T, e *env) (backups, mqttTab int) {
	t.Helper()
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	backups = countACL(t, body)
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	mqttTab = countACL(t, body)
	return backups, mqttTab
}

// aclSave posts the MQTT tab as the browser does: the broker as host, port and TLS, the user, and the hide box.
func aclSave(t *testing.T, e *env, host, port, user string, confirm bool) {
	t.Helper()
	form := url.Values{"mqtt.host": {host}, "mqtt.port": {port}, "v.MQTT_USER": {user}, "reuse.MQTT_PASSWORD": {"1"}, "mqtt.aclhide.seen": {"1"}}
	if confirm {
		form.Set("mqtt.aclhide", "1")
	}
	if r, body := e.do(t, "POST", "/settings/system?tab=mqtt", "admin", form); r.StatusCode >= 400 {
		t.Fatalf("saving the MQTT tab: %d %s", r.StatusCode, body)
	}
}

// aclApply changes system settings directly (not through the form), keeping the stored broker password.
func aclApply(t *testing.T, svc *sysconfig.Service, values map[string]string) {
	t.Helper()
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "admin", Values: values, Reuse: []string{"MQTT_PASSWORD"},
		StepUp: sysconfig.StepUp{Done: true, Possible: true}}); err != nil {
		t.Fatal(err)
	}
}

// aclHideNow is the admin's confirmation for the broker the service holds now.
func aclHideNow(t *testing.T, svc *sysconfig.Service) {
	t.Helper()
	var c config.Config
	for _, f := range svc.State() {
		switch f.Key {
		case "MQTT_URL":
			c.MQTTURL = f.Value
		case "MQTT_USER":
			c.MQTTUser = f.Value
		}
	}
	aclApply(t, svc, map[string]string{"MQTT_ACL_WARNING_HIDDEN": aclBinding(c)})
}

// aclCycle is what every place of the notice must do (#880): shown with a broker and nothing hidden (want notices),
// gone after hiding, and back when the broker is changed behind the hiding's back (by the settings
// service, not by the form that would drop it).
func aclCycle(t *testing.T, svc *sysconfig.Service, want int, count func() int) {
	t.Helper()
	if n := count(); n != want {
		t.Fatalf("before hiding: %d notices, want %d", n, want)
	}
	aclHideNow(t, svc)
	if n := count(); n != 0 {
		t.Fatalf("after hiding: %d notices, want none", n)
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://elsewhere.lan:1883"})
	if n := count(); n != want {
		t.Fatalf("after the broker changed: %d notices, want %d (the hidden warning is for the old broker)", n, want)
	}
}

// Why (#880): TasmoFleet cannot see an ACL, so a user whose broker has one would see a wrong red warning on every MQTT
// path for ever. The admin hides it, but only for the broker it was given for: another host, port or
// user is another broker, and the notice must be back without anyone remembering to remove the hiding.
func TestBrokerACLHiddenWarningHidesTheNoticeOnlyForTheBrokerItWasGivenFor(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Fatalf("before hiding the Backups page has %d and the MQTT tab %d notices, want 1 and 1", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "tf", true)
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Fatalf("hiding given in the save that also changed the user hid the notice (%d, %d): it is for the broker as saved before", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "tf", true)
	if b, m := aclShown(t, e); b != 0 || m != 0 {
		t.Fatalf("after hiding the Backups page has %d and the MQTT tab %d notices, want 0 and 0", b, m)
	}
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `name="mqtt.aclhide" value="1" checked`) {
		t.Error("the hide box is not ticked after hiding")
	}
	for _, tc := range []struct{ name, host, port, user string }{
		{"another host", "broker2.lan", "1883", "tf"},
		{"another port", "broker.lan", "1884", "tf"},
		{"another user", "broker.lan", "1883", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
			aclSave(t, e, "broker.lan", "1883", "tf", false)
			aclSave(t, e, "broker.lan", "1883", "tf", true)
			if b, _ := aclShown(t, e); b != 0 {
				t.Fatalf("hiding did not hide the notice (%d)", b)
			}
			aclSave(t, e, tc.host, tc.port, tc.user, true)
			if b, m := aclShown(t, e); b != 1 || m != 1 {
				t.Errorf("%s: the Backups page has %d and the MQTT tab %d notices after the change, want 1 and 1", tc.name, b, m)
			}
			// the old broker entered again later (not through the form) must not bring the hiding back: it
			// was dropped with the change, not kept for the old broker
			aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883", "MQTT_USER": "tf"})
			if b, m := aclShown(t, e); b != 1 || m != 1 {
				t.Errorf("%s: the old hiding came back with the old broker (%d, %d notices)", tc.name, b, m)
			}
		})
	}
}

// Why (#880): hiding the warning is a decision about security, so it is audited like every system setting, and only an
// admin may make it (the settings page is admin only).
func TestBrokerACLHiddenWarningIsAuditedAndAdminOnly(t *testing.T) {
	var aud *fakeAudit
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()); aud = d.Recorder.(*fakeAudit) })
	form := url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "mqtt.aclhide": {"1"}, "mqtt.aclhide.seen": {"1"}, "reuse.MQTT_PASSWORD": {"1"}}
	if r, _ := e.do(t, "POST", "/settings/system?tab=mqtt", "operator", form); r.StatusCode != 403 {
		t.Fatalf("an operator saved the hiding: %d", r.StatusCode)
	}
	if b, _ := aclShown(t, e); b != 1 {
		t.Fatal("an operator's refused request hid the notice")
	}
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, _ := aclShown(t, e); b != 0 {
		t.Fatalf("the notice is still shown after the admin hid it (%d)", b)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for _, ev := range aud.events {
		if ev.Action == "sysconfig.update" && strings.Contains(ev.Detail, "MQTT_ACL_WARNING_HIDDEN") && ev.Result == "ok" {
			return
		}
	}
	t.Errorf("no audit entry names MQTT_ACL_WARNING_HIDDEN: %+v", aud.events)
}

// aclPath is one place the notice stands on: how to open it (each with its own settings service, as the cycle changes
// the broker), what asking for it as a role returns, and how many notices the page carries while it is open.
type aclPath struct {
	name string
	want int
	open func(t *testing.T) (*sysconfig.Service, func(role string) string)
}

// aclPaths are all places of the notice except the System MQTT tab and the setup, which are the settings themselves.
func aclPaths(now time.Time) []aclPath {
	routed := func(extra func(d *Deps)) func(t *testing.T) (*sysconfig.Service, *env) {
		return func(t *testing.T) (*sysconfig.Service, *env) {
			svc := aclBrokerService(t)
			rs := &routeStub{}
			rs.set(false, 1, 2)
			e := setupWith(t, func(d *Deps) {
				withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}})(d)
				d.DeviceCmd, d.TypedRoutes, d.SysConfig = &fakeCmd{reply: `{"Done":"yes"}`}, rs.routes, svc
				extra(d)
			})
			e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: now}
			return svc, e
		}
	}
	page := func(mk func(t *testing.T) (*sysconfig.Service, *env), method, path string, form func() url.Values) func(t *testing.T) (*sysconfig.Service, func(string) string) {
		return func(t *testing.T) (*sysconfig.Service, func(string) string) {
			svc, e := mk(t)
			return svc, func(role string) string {
				var f url.Values
				if form != nil {
					f = form()
				}
				_, body := e.do(t, method, path, role, f)
				return body
			}
		}
	}
	return []aclPath{
		{"device Back up", 1, page(routed(func(*Deps) {}), "GET", "/devices/1", nil)},
		{"bulk command", 1, page(routed(func(d *Deps) { d.CommandBatch = &fakeBatch{} }), "POST", "/devices/bulk/command",
			func() url.Values { return bulkForm("id", "1", "id", "2", "command", "PowerDelta 10") })},
		// the console page of device 1 carries its own notice and the header's one by Back up (its backup path is MQTT too)
		{"console", 2, page(routed(func(*Deps) {}), "POST", "/devices/1/console",
			func() url.Values { return consoleForm("WebPassword hunter2-880") })},
		{"bulk backup page", 1, func(t *testing.T) (*sysconfig.Service, func(string) string) {
			e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) {
				d.BackupBatch = &fakeBackupBatch{}
				d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}}
			})
			return svc, func(role string) string {
				_, body := e.do(t, "POST", "/devices/bulk/backup", role, url.Values{"csrf": {"tok"}, "id": {"1"}})
				return body
			}
		}},
		{"fleet backups", 1, func(t *testing.T) (*sysconfig.Service, func(string) string) {
			e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(now) })
			return svc, func(role string) string { _, body := e.do(t, "GET", "/devices/backups", role, nil); return body }
		}},
	}
}

// Why (#880): the notice is one condition everywhere, so hiding it hides it on the confirmations of console and
// bulk command, by the device's Back up button, on the bulk backup page and on the Backups page too, not only on the
// page it was made on, and a changed broker brings it back on each of them.
func TestBrokerACLHiddenWarningHidesTheNoticeOnEveryMQTTPath(t *testing.T) {
	for _, p := range aclPaths(time.Now()) {
		t.Run(p.name, func(t *testing.T) {
			svc, get := p.open(t)
			aclCycle(t, svc, p.want, func() int { return aclCount(get("admin")) })
		})
	}
}

// Why (#1078, A1-032, A3-041): the notice used to differ from page to page (each page added its own paragraph), was
// red and bold, and carried a second line, "Hide it". Every place that keeps it (T18) renders the same short warn
// banner: the lead, one sentence and exactly one action, the guide; hiding is the admin's option on System, MQTT,
// never a second action, and an operator gets the same banner as an admin.
func TestBrokerACLNoticeIsTheSameWarnBannerEverywhere(t *testing.T) {
	type place struct {
		name string
		want int
		get  func(t *testing.T, role string) string
	}
	places := []place{
		{"System, MQTT", 1, func(t *testing.T, _ string) string {
			e, _, _, _ := sysEnv(t, true)
			_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
			return body
		}},
		{"device Test button", 1, func(t *testing.T, role string) string {
			e, _ := pwEnv(t, &fakePWTester{}, &fakePWFleet{gaps: map[model.DeviceID]PasswordGap{}})
			_, body := e.do(t, "GET", "/devices/16", role, nil)
			return body
		}},
		{"bulk broker password page", 1, func(t *testing.T, _ string) string {
			e, _ := pwEnv(t, &fakePWTester{}, &fakePWFleet{})
			_, body := e.do(t, "GET", "/settings/system/mqtt-passwords/broker", "admin", nil)
			return body
		}},
	}
	for _, p := range aclPaths(time.Now()) {
		places = append(places, place{p.name, p.want, func(t *testing.T, role string) string { _, get := p.open(t); return get(role) }})
	}
	operator := 0 // the notices an operator saw: the operator's variant must really have been rendered
	for _, p := range places {
		t.Run(p.name, func(t *testing.T) {
			for _, role := range []string{"admin", "operator"} {
				body := p.get(t, role)
				n := countACL(t, body) // checks the shape of every notice on the page
				if role == "admin" && n != p.want {
					t.Errorf("admin: %d ACL notices, want %d", n, p.want)
				}
				if role == "operator" {
					operator += n // a page only an admin may open shows an operator none
				}
				if strings.Contains(body, "Hide it") || strings.Contains(body, "#"+aclHideID) {
					t.Errorf("%s: the notice has a second action to hide it", role)
				}
			}
		})
	}
	if operator == 0 {
		t.Error("an operator saw no notice on any page: the operator's banner was not checked")
	}
}

// Why (#962, the maintainer: "No sentence that says or implies the broker has an ACL, anywhere"): TasmoFleet cannot see
// an ACL, and the switch exists for a broker that has none as much as for one that has. So neither the option, its
// hint, the notice's link nor the audit entry of the save may claim one, and the label and hint are the agreed words.
func TestBrokerACLHideOptionNeverClaimsAnACL(t *testing.T) {
	const label = "Hide the broker ACL warning"
	const hint = "Every client of this broker can read device backups, passwords included. The warning comes back when you change the broker."
	banned := []string{"has an ACL", "have an ACL", "Confirm", "confirm", "CONFIRMED", "Clients read only", "own topics"}
	check := func(what, text string) {
		t.Helper()
		for _, b := range banned {
			if strings.Contains(text, b) {
				t.Errorf("%s says %q", what, b)
			}
		}
	}
	f, ok := sysconfig.Lookup("MQTT_ACL_WARNING_HIDDEN")
	if !ok || f.Label != label || f.Help != hint {
		t.Errorf("the field is %q / %q, want the agreed label and hint", f.Label, f.Help)
	}
	check("the field", f.Label+" "+f.Help)
	var aud *fakeAudit
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()); aud = d.Recorder.(*fakeAudit) })
	tab := func() string { _, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil); return body }
	for _, step := range []string{"unticked", "ticked", "no valid broker"} {
		switch step {
		case "ticked":
			aclSave(t, e, "broker.lan", "1883", "", true)
		case "no valid broker":
			aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883/path"})
		}
		body := tab()
		at := strings.Index(body, `id="mqtt-acl-hide"`)
		end := strings.Index(body[max(at, 0):], "</small>")
		if at < 0 || end < 0 {
			t.Fatalf("the MQTT tab (%s) has no option to look at", step)
		}
		check("the option on the MQTT tab ("+step+")", body[at:at+end])
		if step != "no valid broker" && (!strings.Contains(body, ">"+label+"<") || !strings.Contains(body, hint)) {
			t.Errorf("the MQTT tab (%s) does not show the label and the hint of the option", step)
		}
	}
	// The notice no longer has a line about hiding it (#1078); its own words, on every place it stands, must not claim
	// an ACL either.
	notice := regexp.MustCompile(`<div class="banner-text"><b>` + regexp.QuoteMeta(ui.BrokerACLLead) + `</b>(.*?)</div>`)
	for _, p := range aclPaths(time.Now()) {
		_, get := p.open(t)
		m := notice.FindAllStringSubmatch(get("admin"), -1)
		if len(m) != p.want {
			t.Errorf("%s: %d notices, want %d", p.name, len(m), p.want)
		}
		for _, l := range m {
			check("the notice on "+p.name, ui.BrokerACLLead+l[1])
		}
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	n := 0
	for _, ev := range aud.events {
		if ev.Action == "sysconfig.update" {
			check("the audit entry of a save", ev.Detail)
			n++
		}
	}
	if n == 0 {
		t.Error("no audit entry of a save to check")
	}
}

// Why (#880): unticking the box and saving is how an admin takes the hiding back, for instance after the ACL was
// removed from the broker. The notice must be back at once, on the page that shows it and on the MQTT tab.
func TestBrokerACLHiddenWarningIsTakenBackByUntickingTheBox(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, m := aclShown(t, e); b != 0 || m != 0 {
		t.Fatalf("hiding did not hide the notice (%d, %d)", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "", false) // mqtt.aclhide.seen is posted, mqtt.aclhide is not: the box is unticked
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Errorf("after unticking the box the Backups page has %d and the MQTT tab %d notices, want 1 and 1", b, m)
	}
}

// Why (#880): removing the broker (a reset of MQTT_URL) ends the hiding with it. Entering the very same broker
// again later is a new decision: the notice shows until the admin hides it again.
func TestBrokerACLHiddenWarningDoesNotSurviveRemovingTheBroker(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, _ := aclShown(t, e); b != 0 {
		t.Fatalf("hiding did not hide the notice (%d)", b)
	}
	// the browser posts the broker fields and the ticked box next to the reset box
	form := url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "mqtt.aclhide": {"1"}, "mqtt.aclhide.seen": {"1"}, "clear.MQTT_URL": {"1"}, "reuse.MQTT_PASSWORD": {"1"}}
	if r, body := e.do(t, "POST", "/settings/system?tab=mqtt", "admin", form); r.StatusCode >= 400 {
		t.Fatalf("resetting the broker: %d %s", r.StatusCode, body)
	}
	for _, f := range svc.State() {
		if f.Key == "MQTT_URL" && f.Value != "" {
			t.Fatalf("the reset did not remove the broker: %q", f.Value)
		}
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883"}) // the same broker, entered again
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Errorf("the old hiding came back with the same broker (%d, %d notices)", b, m)
	}
}

// Why (#880): a stored broker URL that does not split into host and port (a path, user information) cannot carry the
// confirmation. A box that saves nothing would be a lie, so it is not offered, and the page says what to do.
func TestBrokerACLBoxIsNotOfferedWithoutAValidBroker(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `name="mqtt.aclhide"`) {
		t.Fatal("the box is not offered with a valid broker")
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883/path"})
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if strings.Contains(body, `name="mqtt.aclhide"`) || !strings.Contains(body, "Save a valid broker first") {
		t.Error("with a broker URL that cannot be split the page still offers the box, or does not say to save a valid broker")
	}
}
