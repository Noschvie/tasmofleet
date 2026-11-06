package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// #645: every action on a device whose HTTP API is off (and which MQTT cannot serve) refuses up front with the
// same reason and fix, instead of trying and failing. What MQTT can serve is not refused.

// readsOnlyFacts: HTTP API off, on our broker and live there, but another device's GroupTopic is unread, which
// forbids MQTT writes and not reads: Commands go over MQTT, Writes have no path.
func readsOnlyFacts() transport.Facts {
	f := mqttFacts(time.Now(), model.MQTTPasswordOwn)
	f.HTTP = transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAPIRefused, Since: time.Now()}
	f.MQTT.OthersGroupUnread = 1
	return f
}

func refusedHealth(id model.DeviceID) model.Health {
	h := apiOffHealth()
	h.DeviceID = id
	h.Power = map[string]string{"POWER": "ON"}
	return h
}

func apiOffActionsEnv(t *testing.T, mutate func(*Deps)) (*env, *fakeCmd) {
	t.Helper()
	cmd := &fakeCmd{reply: `{}`, state: "ON"}
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd = cmd
		d.Settings = &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpOnlyFacts(), 2: readsOnlyFacts()}}
		if mutate != nil {
			mutate(d)
		}
	})
	e.health[1] = refusedHealth(1)
	e.health[2] = refusedHealth(2)
	return e, cmd
}

func mustRefuse(t *testing.T, what string, status int, body string) {
	t.Helper()
	if status != 409 || !strings.Contains(body, "HTTP API off (SetOption128 0)") || !strings.Contains(body, "SetOption128 1") {
		t.Errorf("%s: %d, want 409 with the reason and the fix in %q", what, status, body)
	}
}

// Why: a relay switch or Restart on such a device would only fail with an error after the click. Both refuse up
// front and send nothing; the page disables them with the reason; the same click works for a device MQTT serves.
func TestRelaySwitchAndRestartRefuseADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	e, cmd := apiOffActionsEnv(t, nil)
	r, body := e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"csrf": {"tok"}, "action": {"TOGGLE"}, "back": {"device"}})
	mustRefuse(t, "relay switch", r.StatusCode, body)
	r, body = e.do(t, "POST", "/devices/1/restart", "admin", url.Values{"csrf": {"tok"}})
	mustRefuse(t, "restart", r.StatusCode, body)
	if len(cmd.powers) != 0 || len(cmd.runs) != 0 {
		t.Fatalf("a command was sent: powers %v, runs %v", cmd.powers, cmd.runs)
	}
	_, page := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{"Switching is disabled: HTTP API off (SetOption128 0)", `id="dev-relays-why"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the device page lacks %q", want)
		}
	}
	if sw := sliceBetween(page, `class="devrelay-sw"`, "</button>"); !strings.Contains(sw, "disabled") || !strings.Contains(sw, "HTTP API off") {
		t.Errorf("the relay switch is not disabled with the reason: %s", sw)
	}
	// the header's Restart: the tag of the button that ends in ">Restart</button>"
	if i := strings.Index(page, ">Restart</button>"); i < 0 {
		t.Error("the header has no Restart button")
	} else if rb := page[strings.LastIndex(page[:i], "<button"):i]; !strings.Contains(rb, "disabled") || !strings.Contains(rb, "HTTP API off") {
		t.Errorf("Restart is not disabled with the reason: %s", rb)
	}
	// device 2 has reads over MQTT but no write path: also refused for a change
	r, body = e.do(t, "POST", "/devices/2/power/1", "operator", url.Values{"csrf": {"tok"}})
	mustRefuse(t, "relay switch without a write path", r.StatusCode, body)
	// the API is back: the same click goes through
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now(), Power: map[string]string{"POWER": "ON"}}
	e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"csrf": {"tok"}})
	if len(cmd.powers) != 1 {
		t.Fatalf("the switch did not work once the API was back: %v", cmd.powers)
	}
}

func sliceBetween(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i:]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return s
}

// Why: Refresh identity reads the device, so it is refused up front when nothing reaches it; the card disables
// the button with the reason. A device MQTT can read is refreshed as before.
func TestRefreshIdentityRefusesADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	e, _ := apiOffActionsEnv(t, nil)
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}})
	if !strings.Contains(body, "HTTP API off (SetOption128 0)") || len(e.pr.refreshs) != 0 {
		t.Fatalf("refresh: refreshs %v, body %q", e.pr.refreshs, body)
	}
	_, page := e.do(t, "GET", "/devices/1", "admin", nil)
	if btn := sliceBetween(page, "hx-post=\"/tf/devices/1/probe\"", ">Refresh identity<"); !strings.Contains(btn, "disabled") || !strings.Contains(btn, "data-refused") {
		t.Errorf("Refresh identity is not disabled with the reason: %s", btn)
	}
	e.do(t, "POST", "/devices/2/probe", "admin", url.Values{"csrf": {"tok"}})
	if len(e.pr.refreshs) != 1 || e.pr.refreshs[0] != 2 {
		t.Fatalf("a device MQTT can read was not refreshed: %v", e.pr.refreshs)
	}
}

// Why: a direct POST to Console or Settings (the form is only disabled in the page) must not reach the device
// either, and the console applies the write rule only to commands that write: reading Status over MQTT works
// where changing a setting does not.
func TestConsoleAndSettingsPostsRefuseADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	e, cmd := apiOffActionsEnv(t, nil)
	r, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Status 2"))
	mustRefuse(t, "console read without any path", r.StatusCode, body)
	r, body = e.do(t, "POST", "/devices/2/console", "admin", consoleForm("SetOption1 1"))
	mustRefuse(t, "console write without a write path", r.StatusCode, body)
	if len(cmd.runs) != 0 {
		t.Fatalf("a command was sent: %v", cmd.runs)
	}
	if r, _ = e.do(t, "POST", "/devices/2/console", "admin", consoleForm("Status 2")); r.StatusCode != 200 || len(cmd.runs) != 1 {
		t.Fatalf("a read that MQTT carries was refused: %d, runs %v", r.StatusCode, cmd.runs)
	}
	fs := &fakeSettings{values: map[string]string{"TelePeriod": "300"}}
	e, _ = apiOffActionsEnv(t, func(d *Deps) { d.Settings = fs })
	r, body = e.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "60"))
	mustRefuse(t, "settings", r.StatusCode, body)
	// settings change the device: a way to read is not enough
	r, body = e.do(t, "POST", "/devices/2/settings", "admin", baseForm("TelePeriod", "60"))
	mustRefuse(t, "settings without a write path", r.StatusCode, body)
	if len(fs.applyIn) != 0 {
		t.Fatalf("settings were applied: %v", fs.applyIn)
	}
}

// Why: bulk "Send command" applies the write rule to a command that writes only. A read goes to a device that has
// a way to read, a write does not; with no command typed yet the scope page does not judge by the write rule.
func TestBulkSendCommandAppliesTheWriteRuleOnlyToWrites(t *testing.T) {
	fb := &fakeBatch{}
	e, _ := apiOffActionsEnv(t, func(d *Deps) { d.CommandBatch = fb })
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "2", "command", "Status 2"))
	if !strings.Contains(body, "Send to 1 device") {
		t.Errorf("a read was refused for a device that can read: %s", body)
	}
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "2", "command", "SetOption1 1"))
	if strings.Contains(body, "Send to 1 device") || !strings.Contains(body, "HTTP API off") {
		t.Errorf("a write was offered for a device with no write path: %s", body)
	}
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "2"))
	if strings.Contains(body, "None of the devices is online") {
		t.Errorf("the scope page without a command used the write rule: %s", body)
	}
}

// Why: a bulk backup needs a way to back up, not merely a way to write. This device is written over MQTT but has
// no MQTT password to carry a backup; the bulk backup skips it, a bulk restart does not.
func TestBulkBackupNeedsAPathToBackUp(t *testing.T) {
	fbk := &fakeBackupBatch{}
	f := mqttFacts(time.Now(), model.MQTTPasswordNone)
	f.HTTP = transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAPIRefused, Since: time.Now()}
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd = &fakeCmd{}
		d.CommandBatch = &fakeBatch{}
		d.BackupBatch = fbk
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}
	})
	e.health[1] = refusedHealth(1)
	if p := transport.Choose(f); p.Writes == transport.None || p.Backup != transport.None {
		t.Fatalf("fixture: writes %v, backup %v, want a write path and no backup path", p.Writes, p.Backup)
	}
	e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "opts", "1", "run", "1"))
	if len(fbk.ids) != 0 {
		t.Fatalf("backed up %v, device 1 has no way to back up", fbk.ids)
	}
	_, body := e.do(t, "POST", "/devices/bulk/restart", "admin", bulkForm("id", "1"))
	if strings.Contains(body, "HTTP API off") {
		t.Errorf("the restart (a write) was refused for a device that is written over MQTT: %s", body)
	}
}

// Why: a result line says why a device got nothing. An online device with the API off is "HTTP API off ... Not
// attempted" with the fix; an offline one stays "Offline", the API off finding being stale then.
func TestSkippedReasonNamesTheHTTPAPIOff(t *testing.T) {
	got := skippedReason(apiOffHealth())
	if !strings.Contains(got, "HTTP API off (SetOption128 0). Not attempted.") || !strings.Contains(got, "SetOption128 1") {
		t.Errorf("online, API off: %q", got)
	}
	off := apiOffHealth()
	off.State, off.Since = model.HealthOffline, time.Now().Add(time.Minute) // went offline after the refusal
	if got := skippedReason(off); got != "Offline. Not attempted." {
		t.Errorf("offline: %q", got)
	}
	if got := skippedReason(model.Health{State: model.HealthOnline}); strings.Contains(got, "HTTP API off") {
		t.Errorf("an online device without the finding: %q", got)
	}
}

// Why: retrying a backup or command transaction would send to the device again; it is refused as a whole up
// front, with the reason, when a device to retry has no way. A read command to a device that can read goes on.
func TestRetryOfABackupOrCommandTransactionRefusesADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	for name, job := range map[string]model.Job{
		"backup":  {ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobFailed},
		"command": {ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "SetOption1 1", State: model.JobFailed},
		"policy":  {ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindPolicy, Commands: "SetOption1 1", State: model.JobFailed},
	} {
		job.CreatedAt = time.Now()
		// without any way, and with only a way to read: a backup and a write need their own way
		for facts, f := range map[string]transport.Facts{"no path": httpOnlyFacts(), "reads only": readsOnlyFacts()} {
			e, o := retryEnvWith(t, []model.Job{job}, map[model.DeviceID]transport.Facts{1: f})
			r, body := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{"csrf": {"tok"}})
			mustRefuse(t, name+" retry, "+facts, r.StatusCode, body)
			if len(o.retryCalls) != 0 {
				t.Fatalf("%s, %s: the retry was started", name, facts)
			}
			_, form := e.do(t, "GET", "/transactions/7/retry", "admin", nil)
			if !strings.Contains(form, "HTTP API off (SetOption128 0)") {
				t.Errorf("%s, %s: the retry page does not say why: %s", name, facts, form)
			}
		}
	}
	// a read command, to a device that can read
	e, o := retryEnvWith(t, []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Status 2", State: model.JobFailed, CreatedAt: time.Now()}},
		map[model.DeviceID]transport.Facts{1: readsOnlyFacts()})
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{"csrf": {"tok"}}); r.StatusCode != 303 || len(o.retryCalls) != 1 {
		t.Fatalf("a read retry was refused: %d %v", r.StatusCode, o.retryCalls)
	}
}

func facts1(f map[model.DeviceID]transport.Facts) func(*Deps) {
	return func(d *Deps) { d.Transport = fakeFacts{f: f} }
}

// retryEnvWith is retryEnv with a transport service.
func retryEnvWith(t *testing.T, jobs []model.Job, f map[model.DeviceID]transport.Facts) (*env, *fakeOTA) {
	t.Helper()
	o := &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}, retryRes: ota.RetryResult{Batch: 10, Devices: []model.DeviceID{1}}}
	j := &fakeJobs{jobs: jobs}
	e := setupWith(t, func(d *Deps) {
		d.OTA, d.Jobs, d.Batches, d.DeviceCmd = o, j, fakeBatches{}, &fakeCmd{reply: `{}`}
		facts1(f)(d)
	})
	e.health[1] = refusedHealth(1)
	return e, o
}

// Why: Make compliant works on cached values and would send the commands blind. The plan shows the device as
// blocked with the reason, and Apply sends nothing for it.
func TestMakeCompliantLeavesADeviceWhoseHTTPAPIIsOffOut(t *testing.T) {
	// without any way, and with only a way to read: Make compliant writes
	for facts, f := range map[string]transport.Facts{"no path": httpOnlyFacts(), "reads only": readsOnlyFacts()} {
		fix := &fakeFix{}
		e := setupWith(t, func(d *Deps) {
			d.Policy = &fakePolicy{rep: fixReport()}
			d.PolicyFix = fix
			facts1(map[model.DeviceID]transport.Facts{1: f})(d)
		})
		e.health[1] = refusedHealth(1)
		body := previewFor(t, e, url.Values{"only": {"1"}})
		if !strings.Contains(body, "HTTP API off (SetOption128 0)") {
			t.Fatalf("%s: the plan does not block the device: %s", facts, body)
		}
		r, out := e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {digestRe.FindStringSubmatch(body)[1]}, "device": {"1"}})
		mustRefuse(t, "Make compliant apply, "+facts, r.StatusCode, out)
		if len(fix.reqs) != 0 {
			t.Fatalf("%s: commands were submitted: %+v", facts, fix.reqs)
		}
	}
}

// Why: applying a GPIO template reads the reference, then backs each chosen device up and writes to it: refused as a
// whole, before any device is read or sent anything, when a chosen device has no way to be backed up and written, or
// the reference no way to be read. A reference MQTT reads is fine.
func TestGPIOTemplateApplyRefusesADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	noBackup := mqttFacts(time.Now(), model.MQTTPasswordNone) // written over MQTT, but no MQTT password to back it up first
	noBackup.HTTP = transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAPIRefused, Since: time.Now()}
	for _, c := range []struct {
		name    string
		id      model.DeviceID // the device whose HTTP API is off: 10 is the reference, 12 the chosen device
		facts   transport.Facts
		refused bool
	}{
		{"a chosen device without any way", 12, httpOnlyFacts(), true},
		{"a chosen device that cannot be backed up first", 12, noBackup, true},
		{"a reference that cannot be read", 10, httpOnlyFacts(), true},
		{"a reference read over MQTT", 10, readsOnlyFacts(), false},
	} {
		f := gtFixture()
		e := setupWith(t, func(d *Deps) {
			d.Policy = &fakePolicy{rep: fleetReport()}
			d.Templates = f
			facts1(map[model.DeviceID]transport.Facts{c.id: c.facts})(d)
		})
		e.health[c.id] = refusedHealth(c.id)
		form := url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "how": {"most-common"}, "pick": {"12"}, "digest": {"abc123"}, "confirm": {"1"}}
		r, body := e.do(t, "POST", "/policy/templates/apply", "admin", form)
		if !c.refused {
			if f.gotApply == nil {
				t.Errorf("%s: not applied (%d)", c.name, r.StatusCode)
			}
			continue
		}
		mustRefuse(t, c.name, r.StatusCode, body)
		if f.gotApply != nil {
			t.Fatalf("%s: the template was applied", c.name)
		}
		if r, _ := e.do(t, "POST", "/policy/templates/preview", "admin", form); r.StatusCode != 409 || f.gotPrev != nil {
			t.Fatalf("%s: preview %d, read %v", c.name, r.StatusCode, f.gotPrev)
		}
	}
}

// Why (#645): one wording. The four texts that said "likely refuses HTTP API commands" now say what the tile,
// card, banner and gates say, with the fix; and the fix does not read as if it meant MQTT.
func TestEveryRefusalSaysTheSameThing(t *testing.T) {
	err := tasmota.ErrHTTPAPIRefused
	_, power := deviceErrorText(err)
	_, ok := deviceProblem(err)
	h := &handler{}
	_, probe, _ := h.probeProblem(nil, err)
	msgs := map[string]string{"power": power, "settings": readErrorText(err), "humanerr": func() string { m, _ := deviceProblem(err); return m }(), "deviceedit": probe}
	_ = ok
	for name, m := range msgs {
		if !strings.Contains(m, apiOffName) || !strings.Contains(m, apiOffFix) || strings.Contains(m, "likely refuses") {
			t.Errorf("%s says %q", name, m)
		}
	}
	if strings.Contains(apiOffFix, "Switch it on") || !strings.Contains(apiOffFix, "HTTP API") {
		t.Errorf("the fix must name the HTTP API: %q", apiOffFix)
	}
}

// Why (#645 review): the update gate asks what the OTA engine will ask. The upload over MQTT is not offered (#262 was
// not shipped: no update request allows it, and the engine refuses that path), so an ESP32 on our broker that cannot
// reach the firmware server has no way to be updated while its HTTP API is off, whatever delivery is asked for. The
// gate refuses it up front with the one reason; letting it through would only get the engine's refusal, which tells
// the person to allow an upload no page offers. A device that can download over MQTT is not refused, pull or push.
func TestUpdateGateRefusesADeviceOnlyTheUnofferedUploadOverMQTTCouldUpdate(t *testing.T) {
	now := time.Now()
	noFW := mqttFacts(now, model.MQTTPasswordOwn)
	noFW.HTTP = transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAPIRefused, Since: now}
	noFW.FWReach = model.FWReach{State: model.FWReachNo, At: now.Add(-time.Hour)}
	if p := transport.Choose(noFW); p.Update != transport.None {
		t.Fatalf("fixture: the device has update path %v", p.Update)
	}
	upload := noFW
	upload.Request = transport.Request{Delivery: model.DeliveryPush, AllowMQTTUpload: true, MQTTUploadFit: transport.MQTTFit{OK: true}}
	if p := transport.Choose(upload); p.Update != transport.PushMQTT {
		t.Fatalf("fixture: only the upload over MQTT could update it, got %v", p.Update)
	}
	download := mqttFacts(now, model.MQTTPasswordOwn) // reaches the firmware server as far as anyone knows
	download.HTTP = noFW.HTTP
	o := &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}}
	e := setupWith(t, func(d *Deps) {
		d.OTA, d.Jobs, d.Firmware, d.Batches, d.BuildMemory = o, &fakeJobs{}, &fakeFW{}, fakeBatches{}, nil
		facts1(map[model.DeviceID]transport.Facts{1: download, 2: noFW})(d)
	})
	e.health[1], e.health[2] = refusedHealth(1), refusedHealth(2)
	_, picker := e.do(t, "GET", "/updates/new", "operator", nil)
	row := func(id string) string { // the picker's label of one device
		i := strings.Index(picker, `name="id" value="`+id+`"`)
		if i < 0 {
			return ""
		}
		return picker[strings.LastIndex(picker[:i], "<label"):i] + sliceBetween(picker[i:], "", "</label>")
	}
	if r := row("2"); !strings.Contains(r, "HTTP API off (SetOption128 0)") || !strings.Contains(r, "disabled") {
		t.Errorf("the picker does not grey out device 2 with the reason: %s", r)
	}
	if r := row("1"); r == "" || strings.Contains(r, "HTTP API off") {
		t.Errorf("the picker greys out device 1, which downloads over MQTT: %s", r)
	}
	for _, delivery := range []string{"", "pull", "push"} {
		form := url.Values{"csrf": {"tok"}, "id": {"2"}, "release": {"16.1.0"}, "policy": {"only-newer"}, "skip_backup": {"1"}, "delivery": {delivery}}
		if r, body := e.do(t, "POST", "/updates/preview", "operator", form); r.StatusCode != 422 || !strings.Contains(body, "SetOption128 1") {
			t.Errorf("preview, delivery %q: %d, want 422 with the reason and the fix", delivery, r.StatusCode)
		}
		if r, body := e.do(t, "POST", "/updates", "operator", form); r.StatusCode != 409 || !strings.Contains(body, "HTTP API off (SetOption128 0)") {
			t.Errorf("submit, delivery %q: %d, want 409 with the reason", delivery, r.StatusCode)
		}
		form.Set("id", "1")
		if _, body := e.do(t, "POST", "/updates", "operator", form); strings.Contains(body, "HTTP API off") {
			t.Errorf("delivery %q: a device that downloads over MQTT was refused by the gate", delivery)
		}
	}
	for _, q := range o.submitReqs {
		if q.DeviceID == 2 {
			t.Fatalf("device 2 was submitted: %+v", q)
		}
	}
}

// Why (#645 review): the Drift tab compares the newest backup with a live read. A device whose HTTP API is off and
// which nothing else reads is not asked: the read would only fail, with the client's own wording; the tab says why
// instead. A device MQTT reads is compared as before.
func TestDriftDoesNotReadADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	for _, c := range []struct {
		name  string
		facts transport.Facts
		read  bool
	}{{"no path", httpOnlyFacts(), false}, {"read over MQTT", readsOnlyFacts(), true}} {
		bk := &diffBackups{files: map[model.BackupID][]byte{2: []byte(`{"Status1":{"StatusPRM":{"Uptime":"1T00:00:00"}}}`)},
			live: []byte(`{"Status1":{"StatusPRM":{"Uptime":"9T00:00:00"}}}`)}
		bk.list = []model.Backup{{ID: 2, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: time.Now().Add(-time.Hour), Version: "16.0.0"}}
		e := setupWith(t, func(d *Deps) {
			d.Backups = bk
			facts1(map[model.DeviceID]transport.Facts{1: c.facts})(d)
		})
		e.health[1] = refusedHealth(1)
		r, body := e.do(t, "GET", "/devices/1/drift", "operator", nil)
		if c.read {
			if r.StatusCode != 200 || bk.liveN != 1 {
				t.Errorf("%s: %d, %d reads, want the comparison", c.name, r.StatusCode, bk.liveN)
			}
			continue
		}
		mustRefuse(t, "drift, "+c.name, r.StatusCode, body)
		if bk.liveN != 0 || strings.Contains(body, "likely refuses") {
			t.Errorf("%s: the device was read (%d)", c.name, bk.liveN)
		}
	}
}

// Why (#645 review): Copy from a device reads one setting live and falls back to the newest backup. When the live
// read meets a device whose HTTP API is off and no backup has the value, the page says so in the one wording with the
// fix, not with the client's "likely refuses HTTP API commands".
func TestCopyFromADeviceNamesTheHTTPAPIOffInTheOneWording(t *testing.T) {
	rp := defaultRP()
	rp.readErr = fmt.Errorf("the device did not answer: %w", fmt.Errorf("%w: set SetOption128 1 on kitchen", tasmota.ErrHTTPAPIRefused))
	e, _ := ruleEnvWith(t, rp)
	r, body := e.do(t, "POST", "/policy/rules/new/copy", "admin", url.Values{"csrf": {"tok"}, "device": {"1"}, "command": {"Timezone"}})
	got := sliceBetween(body, "Could not read", "</")
	if r.StatusCode != 422 || !strings.Contains(got, apiOffName) || !strings.Contains(got, "SetOption128 1") || strings.Contains(body, "likely refuses") {
		t.Errorf("%d: %q", r.StatusCode, got)
	}
}
