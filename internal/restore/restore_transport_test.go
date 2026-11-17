package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/tasmotacmd"
	"github.com/hellwand/tasmofleet/internal/testutil/redactguard"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#607, the product owner's rule of 2026-11-14): a restore over MQTT first whose backup changes the device's MQTT
// connection cannot lock TasmoFleet out while the device's HTTP API answers (HTTP stays the way back), so it asks no
// lockout question then. HTTP must answer right now (Status 5 with the device's MAC), not on record. After the upload
// the device is marked, so its next commands go over HTTP while it may not be back on the broker (as after a
// connection change by a write, T10).
func TestARestoreThatChangesTheMQTTConnectionAsksNothingWhileHTTPAnswers(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	as, err := m.Assess(t.Context(), a.ID)
	if err != nil || as.Lockout || !as.ViaMQTT {
		t.Fatalf("assessment %+v, %v: no lockout question while HTTP answers", as, err)
	}
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatalf("start without the lockout confirmation: %v", err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !bytes.Equal(e.deviceDump(t), e.dumpA) {
		t.Fatalf("job: %+v", done)
	}
	if got := e.marks(); !slices.Equal(got, []model.DeviceID{e.dev}) {
		t.Errorf("marks %v: the device whose MQTT connection the upload changed must be marked once", got)
	}
}

// Why (#607): the same restore while HTTP does not answer can cut TasmoFleet off, so the lockout question comes back
// and says that HTTP does not answer; nothing is uploaded without it.
func TestARestoreThatChangesTheMQTTConnectionAsksWhenHTTPDoesNotAnswer(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.httpDown.Store(true)
	u := &fakeUploader{e: e, apply: true}
	m := e.mqttManager(t, u, transport.MQTT)
	_, err := m.Start(t.Context(), "alice", a.ID)
	var need *OverrideError
	if !errors.As(err, &need) || !need.Lockout || !strings.Contains(need.Detail, "HTTP API does not answer") {
		t.Fatalf("want the lockout question naming HTTP, got %v", err)
	}
	if u.calls != 0 {
		t.Errorf("uploaded %d times without the confirmation", u.calls)
	}
}

// Why (#607): "HTTP answers" is proven right before the upload too, not only at the start: the safety backup and the
// files can take minutes. HTTP that went silent in between turns the restore back into one that needs the person's
// confirmation; the job fails at the upload step with nothing uploaded and says how to go on.
func TestARestoreWhoseHTTPStopsAnsweringBeforeTheUploadUploadsNothing(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.hb.hook = func(call int) {
		if call == 1 { // after the safety backup
			e.httpDown.Store(true)
		}
	}
	u := &fakeUploader{e: e, apply: true}
	job, err := e.mqttManager(t, u, transport.MQTT).Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || Step(done) != StepUpload || !strings.Contains(done.Detail, "nothing was changed") || !strings.Contains(done.Detail, "HTTP") {
		t.Fatalf("job: %+v", done)
	}
	if u.calls != 0 || !bytes.Equal(e.deviceDump(t), e.dumpB) {
		t.Errorf("uploads %d: the device must be unchanged", u.calls)
	}
	if got := e.marks(); len(got) != 0 {
		t.Errorf("marked %v without an upload", got)
	}
}

// Why (#607): after a restore over MQTT that changed the MQTT connection, the device comes back under its new topic or
// broker, so the old topic stays silent although the device restarted with the backup. The boot check uses whichever
// transport answers (here HTTP), so the job does not fail with "did not restart"; the verification then takes the
// transport that proved the restart.
func TestTheBootCheckAfterAnMQTTConnectionChangeUsesTheTransportThatAnswers(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	u := &silencingUploader{fakeUploader: &fakeUploader{e: e, apply: true}}
	m := e.mqttManager(t, u.fakeUploader, transport.MQTT)
	m.o.Uploader = u
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified {
		t.Fatalf("job: %+v: the device restarted with the backup", done)
	}
}

// silencingUploader uploads, then makes the old topic silent: the device left it with the restored settings.
type silencingUploader struct{ *fakeUploader }

func (u *silencingUploader) UploadConfig(ctx context.Context, t tasmota.Target, dump []byte, bind transport.UploadBinding) error {
	err := u.fakeUploader.UploadConfig(ctx, t, dump, bind)
	u.e.mqttDown.Store(true)
	return err
}

// Why (#607, the files of #324): a restore over MQTT first must not lose the files a restore over HTTP wrote: Tasmota
// has no file transfer over MQTT, so the files go over HTTP while HTTP answers, before the settings, after the identity
// was checked over HTTP. The confirmation page lists them as for a restore over HTTP.
func TestRestoreOverMQTTWritesTheFilesOverHTTP(t *testing.T) {
	e, a := filesFlow(t)
	u := &fakeUploader{e: e, apply: true}
	m := e.mqttManager(t, u, transport.MQTT)
	as, err := m.Assess(t.Context(), a.ID)
	if err != nil || !as.Files.Restores() || len(as.Files.Files) != 3 {
		t.Fatalf("assessment %+v, %v", as.Files, err)
	}
	job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified || !bytes.Equal(e.deviceDump(t), e.dumpA) {
		t.Fatalf("job: %+v", done)
	}
	for p, want := range backupFiles {
		if got := e.devFile(t, p); got != want {
			t.Errorf("%s on the device: %q, want the backup's content", p, got)
		}
	}
	log := e.cmdLog()
	if i := slices.Index(log, "UPLOAD"); i < 0 || len(e.cl.uploads()) != 3 {
		t.Errorf("log %v, file uploads %v", log, e.cl.uploads())
	}
}

// Why (#607): files of an MQTT restore go to the host at the device's address, which may not be this device any more,
// also when it still was at the plan: the address is checked again right before the writes. When that host reports
// another MAC the files are left out and the job says so; the settings still go over MQTT,
// which reaches the device by its topic, and nothing is written to the other host.
func TestFilesOfAnMQTTRestoreAreLeftOutWhenHTTPFindsAnotherDevice(t *testing.T) {
	e, a := filesFlow(t)
	u := &fakeUploader{e: e, apply: true}
	m := e.mqttManager(t, u, transport.MQTT)
	// The plan's check still finds the device; the address changes hands before the check right before the writes.
	m.o.Client = macSwitch{idGuard: idGuard{e.cl, e}, n: new(atomic.Int32)}
	job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !bytes.Equal(e.deviceDump(t), e.dumpA) {
		t.Fatalf("job: %+v", done)
	}
	if up := e.cl.uploads(); len(up) != 0 {
		t.Errorf("files went to the other host: %v", up)
	}
	if files, _ := SplitDetail(done.Detail); !strings.Contains(files, "Files not restored") || !strings.Contains(files, "not this device") {
		t.Errorf("the job must say why the files were not restored: %q", done.Detail)
	}
}

// macSwitch is idGuard whose host at the address reports another MAC from the second HTTP identity question on.
type macSwitch struct {
	idGuard
	n *atomic.Int32
}

func (s macSwitch) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if pin, _ := transport.PathFrom(ctx); pin == transport.HTTP && c == "Status 5" && s.n.Add(1) == 2 {
		s.e.httpMAC.Store("aa:bb:cc:99:99:99")
	}
	return s.idGuard.Command(ctx, t, c)
}

// Why (#607, R14): the files and the HTTP check of a restore over MQTT go to the device's address resolved once; a name
// that resolves to more than one IP, or no address at all (a device added from the broker), cannot be asked over HTTP.
// The settings still go over MQTT, the files are left out with that reason, and nothing is sent to any address. A backup
// that changes the MQTT connection then asks the lockout question, since HTTP cannot be shown to answer.
func TestARestoreOverMQTTWithoutOneHTTPAddressLeavesTheFilesOutAndAsks(t *testing.T) {
	for name, set := range map[string]func(*flowEnv){
		"name with two IPs": func(e *flowEnv) {
			e.m.o.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}, nil
			}
		},
		"no address": func(e *flowEnv) {
			dev, _ := e.st.GetDevice(t.Context(), e.dev)
			dev.Address = ""
			if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, a := filesFlow(t)
			e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
			set(e)
			e.httpDown.Store(true) // any HTTP call would be a mistake here; this makes one fail loudly below
			as, err := e.m.Assess(t.Context(), a.ID)
			if err != nil || as.Files.Restores() || as.Files.Why != whyFilesNoAddress {
				t.Fatalf("assessment %+v, %v", as.Files, err)
			}
		})
	}
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	m.o.Resolve = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	_, err := m.Start(t.Context(), "alice", a.ID)
	var need *OverrideError
	if !errors.As(err, &need) || !need.Lockout {
		t.Fatalf("without an HTTP address the lockout question must come: %v", err)
	}
}

// Why: a restore cancelled while it waits for the restart (TasmoFleet shuts down) ends with the context's error, not
// with "did not restart", which would claim something about the device.
func TestTheWaitForTheRestartEndsWithTheCancellation(t *testing.T) {
	e := newFlow(t, "15.6.0")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.m.waitBoot(ctx, nil, e.target(), 0, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// Why (H1): a decoded backup that is not JSON cannot be compared, so the restore over MQTT counts it as a change of the
// MQTT connection (the question comes when HTTP does not answer).
func TestAnUnreadableDecodedBackupCountsAsAChangeOfTheMQTTConnection(t *testing.T) {
	e := newFlowWith(t, "15.6.0", rawDecoder("not json"))
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
	dev, _ := e.st.GetDevice(t.Context(), e.dev)
	if why := m.mqttLockout(t.Context(), dev, a); !strings.Contains(why, "cannot be read") {
		t.Fatalf("%q", why)
	}
}

type rawDecoder string

func (rawDecoder) Version(context.Context) (string, error)          { return "16.0.0.0", nil }
func (d rawDecoder) Decode(context.Context, []byte) ([]byte, error) { return []byte(d), nil }

// Why (#607): the replace page says per backup what it holds of the device's files. Since backups go over MQTT first and
// read their files over HTTP, "they need HTTP" means HTTP did not answer during that run, not that it went over MQTT.
func TestTheReplacePageSaysWhyABackupHasNoFiles(t *testing.T) {
	m := &Manager{o: Options{Files: noteFiles{"they need HTTP"}}}
	got := m.dumpFiles(t.Context(), []model.Backup{{ID: 3}})
	if got[3] != "no files (HTTP did not answer)" {
		t.Fatalf("%q", got[3])
	}
}

type noteFiles struct{ note string }

func (noteFiles) OpenFilesArchive(context.Context, model.BackupID) (*backup.FilesArchive, error) {
	return nil, backup.ErrNoFilesArchive
}
func (n noteFiles) RunFiles(context.Context, model.BackupID) (backup.RunFiles, error) {
	return backup.RunFiles{Note: n.note}, nil
}

// connDecoder decodes a dump the way topicDecoder does, without a topic and with extra decoded fields (a JSON fragment
// that starts with a comma): a backup that differs from the device in those fields only.
type connDecoder struct{ extra string }

func (connDecoder) Version(context.Context) (string, error) { return "16.0.0.0", nil }
func (d connDecoder) Decode(_ context.Context, dmp []byte) ([]byte, error) {
	return []byte(fmt.Sprintf(`{"header":{"data":{"hardware":"ESP82xx"}},"cfg_crc":%d,"tele_period":%d%s}`, dmp[0], dmp[1], d.extra)), nil
}

// fingerprintJSON is a decoded MqttFingerprint as decode-config writes it: 20 bytes as "0x.." strings.
func fingerprintJSON(b byte) string {
	parts := make([]string, 20)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"0x%02x"`, b)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// The backups that move the device away from where TasmoFleet reaches it on the broker without touching the seven
// settings compared before #607's fix round: MQTT off, other prefixes, TLS on (the review's three cases).
var brokerMoves = map[string]string{
	"MQTT off":       `,"flag":{"mqtt_enabled":0}`,
	"other prefixes": `,"mqtt_prefix":["cmd","stat","tele"]`,
	"TLS on":         `,"flag4":{"mqtt_tls":1}`,
}

// Why (#607, R28; the 2026-11-14 16:05 decision names the whole list): a restore writes every MQTT setting of its backup
// at once, so it changes the device's MQTT connection whenever any setting differs that a write would count as cutting
// MQTT off (cmdpolicy.CutsMQTTOff). Each one must be compared with the device; a setting left out lets a restore over
// MQTT switch MQTT off or move the device to other topics or TLS without the lockout question while HTTP is silent. The
// device's values are read over MQTT; equal values (the defaults the backup writes as "") must not count, or every
// restore would ask.
func TestEachMQTTConnectionSettingOfABackupIsCompared(t *testing.T) {
	type tc struct {
		extra string
		name  string              // the name the sentence must give
		dev   func(*model.Device) // what the inventory knows
		sim   []string            // commands that put the simulated device into the state of the case
		boot  map[string]string   // settings the simulated device starts with (a write of MqttHost would restart it)
	}
	tlsFP := `,"flag4":{"mqtt_tls":1},"flag5":{"tls_use_fingerprint":1}`
	cases := map[string]tc{
		"topic":           {extra: `,"mqtt_topic":"some-other-topic"`, name: "Topic"},
		"fulltopic":       {extra: `,"mqtt_fulltopic":"%topic%/%prefix%/"`, name: "FullTopic"},
		"grouptopic":      {extra: `,"mqtt_grptopic":"others"`, name: "GroupTopic", dev: func(d *model.Device) { d.GroupTopic = "tasmotas" }},
		"mqtthost":        {extra: `,"mqtt_host":"elsewhere.invalid"`, name: "MqttHost", dev: func(d *model.Device) { d.MQTTHost = "broker.invalid" }},
		"mqttport":        {extra: `,"mqtt_port":8883`, name: "MqttPort", dev: func(d *model.Device) { d.MQTTPort = 1883 }},
		"mqttuser":        {extra: `,"mqtt_user":"someone-else"`, name: "MqttUser"},
		"mqttpassword":    {extra: `,"mqtt_pwd":"other"`, name: "MqttPassword", dev: func(d *model.Device) { d.MQTTPasswordMode, d.MQTTPassword = model.MQTTPasswordOwn, "pw" }},
		"mqttclient":      {extra: `,"mqtt_client":"other_%06X"`, name: "MqttClient"},
		"prefix":          {extra: brokerMoves["other prefixes"], name: "Prefix"},
		"mqttfingerprint": {extra: tlsFP + `,"mqtt_fingerprint1":` + fingerprintJSON(0) + `,"mqtt_fingerprint2":` + fingerprintJSON(1), name: "MqttFingerprint2", sim: []string{"SetOption103 1", "SetOption132 1"}},
		"setoption3":      {extra: brokerMoves["MQTT off"], name: "SetOption3"},
		"setoption103":    {extra: brokerMoves["TLS on"], name: "SetOption103"},
		"setoption132":    {extra: `,"flag5":{"tls_use_fingerprint":1}`, name: "SetOption132"},

		// A device TasmoFleet knows only from the broker (app.onMQTTSeen) may have no broker on record: the device's own
		// MqttHost and MqttPort are read then, or a backup that moves it to another broker would go through unasked.
		"mqtthost, not on record": {extra: `,"mqtt_host":"elsewhere.invalid"`, name: "MqttHost", boot: map[string]string{"mqtthost": "broker.invalid"}},
		"mqttport, not on record": {extra: `,"mqtt_port":8883`, name: "MqttPort"},
	}
	for key, field := range connCutOffs {
		if _, ok := cases[key]; !ok && field != "" {
			t.Errorf("no case shows that mqttLockout compares %s (%s)", key, field)
		}
	}
	// The same settings as the device: nothing changes. The second also holds TLS with fingerprints equal to Tasmota's
	// defaults (MQTT_FINGERPRINT1/2), so the fingerprints are compared, not only read.
	same := map[string]tc{
		"defaults": {extra: `,"mqtt_client":"DVES_%06X","mqtt_prefix":["","",""],"flag":{"mqtt_enabled":1},"flag4":{"mqtt_tls":0},"flag5":{"tls_use_fingerprint":0}`},
		"TLS with the device's fingerprints": {extra: tlsFP + `,"mqtt_fingerprint1":` + fingerprintJSON(0) +
			`,"mqtt_fingerprint2":["0xda","0x39","0xa3","0xee","0x5e","0x6b","0x4b","0x0d","0x32","0x55","0xbf","0xef","0x95","0x60","0x18","0x90","0xaf","0xd8","0x07","0x09"]`,
			sim: []string{"SetOption103 1", "SetOption132 1"}},
		// The broker not on record but the device's own: equal (host names without regard to case), nothing to ask.
		"the device's broker, not on record": {extra: `,"mqtt_host":"Broker.invalid","mqtt_port":1883`, boot: map[string]string{"mqtthost": "broker.invalid"}},
	}
	lockout := func(t *testing.T, c tc) string {
		e := newFlowSpec(t, "15.6.0", connDecoder{extra: c.extra}, simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Settings: c.boot})
		for _, cmd := range c.sim {
			if _, err := e.cl.Command(t.Context(), e.target(), cmd); err != nil {
				t.Fatal(err)
			}
		}
		a := e.dumpBackup(t, e.dumpA)
		m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
		dev, err := e.st.GetDevice(t.Context(), e.dev)
		if err != nil {
			t.Fatal(err)
		}
		if c.dev != nil {
			c.dev(&dev)
		}
		return m.mqttLockout(transport.WithPath(t.Context(), transport.MQTT), dev, a)
	}
	for key, c := range cases {
		t.Run(key, func(t *testing.T) {
			got := lockout(t, c)
			if !strings.Contains(got, "this backup changes "+c.name) || strings.Contains(got, "did not report") {
				t.Fatalf("%q: the backup changes %s", got, c.name)
			}
		})
	}
	for name, c := range same {
		t.Run(name, func(t *testing.T) {
			if got := lockout(t, c); got != "" {
				t.Fatalf("%q: the backup holds the device's own MQTT settings", got)
			}
		})
	}
}

// Why: a setting the device does not report cannot be shown to be unchanged, so it counts as a possible change of the
// MQTT connection (fail closed), named as one that could not be compared.
func TestAnMQTTSettingTheDeviceDoesNotReportCountsAsAChange(t *testing.T) {
	for cmd, extra := range map[string]string{
		"MqttClient": `,"mqtt_client":"DVES_%06X"`,
		// Not on record (a device known only from the broker): read from the device, so a silent one is not equal.
		"MqttHost": `,"mqtt_host":"broker.invalid"`,
		"MqttPort": `,"mqtt_port":1883`,
	} {
		t.Run(cmd, func(t *testing.T) {
			e := newFlowWith(t, "15.6.0", connDecoder{extra: extra})
			a := e.dumpBackup(t, e.dumpA)
			m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
			e.cl.failCmd = func(cmnd string) error {
				if cmnd == cmd {
					return tasmota.ErrTimeout
				}
				return nil
			}
			dev, _ := e.st.GetDevice(t.Context(), e.dev)
			if got := m.mqttLockout(transport.WithPath(t.Context(), transport.MQTT), dev, a); !strings.Contains(got, "did not report its "+cmd) {
				t.Fatalf("%q", got)
			}
		})
	}
}

// Why (#809): MqttUser is secret by the one rule, so the router masks the reply of a bare MqttUser ("***"). The lockout
// check compares the backup's login with the device's: read bare, a backup with the device's own login would count as
// changing it (every restore over MQTT would ask), and one with another login could never be told apart. The login is
// read from Status 6, which the router does not mask; the guard plays the router's part. A device that does not
// answer Status 6 has not reported its login: never equal.
func TestTheLockoutReadsTheMQTTUserFromStatus6(t *testing.T) {
	lockout := func(t *testing.T, user func(device string) string, fail bool) (string, []string) {
		e := newFlow(t, "15.6.0")
		raw, err := e.cl.Command(t.Context(), e.target(), "Status 6")
		var s6 struct{ StatusMQT struct{ MqttUser string } }
		if err != nil || json.Unmarshal(raw, &s6) != nil || s6.StatusMQT.MqttUser == "" {
			t.Fatalf("simulator Status 6: %s %v", raw, err)
		}
		e = newFlowWith(t, "15.6.0", connDecoder{extra: `,"mqtt_user":` + strconv.Quote(user(s6.StatusMQT.MqttUser))})
		a := e.dumpBackup(t, e.dumpA)
		m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
		m.o.Client = redactguard.Wrap(t, m.o.Client)
		if fail {
			e.cl.failCmd = func(cmnd string) error {
				if cmnd == "Status 6" {
					return tasmota.ErrTimeout
				}
				return nil
			}
		}
		dev, _ := e.st.GetDevice(t.Context(), e.dev)
		before := len(e.cmdLog())
		got := m.mqttLockout(transport.WithPath(t.Context(), transport.MQTT), dev, a)
		return got, e.cmdLog()[before:]
	}
	got, sent := lockout(t, func(d string) string { return d }, false)
	if got != "" {
		t.Errorf("the backup holds the device's own login: %q (sent %v)", got, sent)
	}
	if slices.Contains(sent, "MqttUser") {
		t.Errorf("the bare MqttUser was sent, its reply is masked: %v", sent)
	}
	if got, _ := lockout(t, func(string) string { return "someone-else" }, false); !strings.Contains(got, "this backup changes MqttUser") || strings.Contains(got, "someone-else") {
		t.Errorf("another login: %q", got)
	}
	if got, _ := lockout(t, func(d string) string { return d }, true); !strings.Contains(got, "did not report its MqttUser") {
		t.Errorf("Status 6 unanswered: %q", got)
	}
}

// Why (#607, R28): connCutOffs is the restore's copy of the write rule's list of settings that change the MQTT
// connection (cmdpolicy.CutsMQTTOff). Every command of the pinned Tasmota catalogue that the rule counts must be in it,
// and nothing else, so a setting added to the write rule cannot be forgotten by the restore (the two lists drifted
// once: the restore compared seven settings while the rule counted fourteen).
func TestEveryCutOffCommandIsComparedByTheRestore(t *testing.T) {
	cat, err := tasmotacmd.Default()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	probe := func(cmd string) {
		if !cmdpolicy.CutsMQTTOff(cmd) {
			return
		}
		name, _, _ := strings.Cut(cmd, " ")
		n, ok := cmdpolicy.ResolveName(name)
		if !ok {
			t.Fatalf("%s: cut off but not resolvable", cmd)
		}
		key := n.Base
		if n.Base == "setoption" {
			key += strconv.Itoa(n.Index)
		}
		seen[key] = true
		if _, ok := connCutOffs[key]; !ok {
			t.Errorf("%q changes the MQTT connection (cmdpolicy) but the restore does not compare %s", cmd, key)
		}
	}
	for _, c := range cat.Commands {
		for _, arg := range []string{"0", "1", "x"} {
			probe(c.Command + " " + arg)
			probe(c.Command + "1 " + arg)
		}
	}
	for i := range 256 {
		probe("SetOption" + strconv.Itoa(i) + " 0")
		probe("SetOption" + strconv.Itoa(i) + " 1")
	}
	for key := range connCutOffs {
		if !seen[key] {
			t.Errorf("connCutOffs holds %s, which cmdpolicy does not count as changing the MQTT connection", key)
		}
	}
}

// Why (#607, R28; the review's first finding): a backup that switches MQTT off, moves the device to other prefixes or
// turns TLS on cuts TasmoFleet off from a device it reaches only over MQTT, as a topic change does. While HTTP does not
// answer the lockout question must come, and nothing is uploaded without it.
func TestABackupThatMovesTheDeviceOnTheBrokerAsksWhenHTTPDoesNotAnswer(t *testing.T) {
	for name, extra := range brokerMoves {
		t.Run(name, func(t *testing.T) {
			e := newFlowWith(t, "15.6.0", connDecoder{extra: extra})
			a := e.dumpBackup(t, e.dumpA)
			e.httpDown.Store(true)
			u := &fakeUploader{e: e, apply: true}
			_, err := e.mqttManager(t, u, transport.MQTT).Start(t.Context(), "alice", a.ID)
			var need *OverrideError
			if !errors.As(err, &need) || !need.Lockout {
				t.Fatalf("want the lockout question, got %v", err)
			}
			if u.calls != 0 {
				t.Errorf("uploaded %d times without the confirmation", u.calls)
			}
		})
	}
}

// Why (#607, no false "did not restart"): after such a restore the device answers on other topics, TLS or not at all
// on the broker, so the old topic stays silent. While HTTP answers, the boot check asks HTTP and the job ends verified;
// the device is marked so that its next commands go over HTTP, not into the old topic.
func TestTheBootCheckAfterABackupThatMovesTheDeviceOnTheBrokerUsesHTTP(t *testing.T) {
	for name, extra := range brokerMoves {
		t.Run(name, func(t *testing.T) {
			e := newFlowWith(t, "15.6.0", connDecoder{extra: extra})
			a := e.dumpBackup(t, e.dumpA)
			e.sim.SetConfigDump(e.dumpB)
			u := &silencingUploader{fakeUploader: &fakeUploader{e: e, apply: true}}
			m := e.mqttManager(t, u.fakeUploader, transport.MQTT)
			m.o.Uploader = u
			job, err := m.Start(t.Context(), "alice", a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if done := e.finish(t, job.ID); done.State != store.RestoreDone || !done.Verified {
				t.Fatalf("job: %+v: the device restarted with the backup", done)
			}
			if got := e.marks(); !slices.Equal(got, []model.DeviceID{e.dev}) {
				t.Errorf("marks %v: the device whose MQTT connection the upload changed must be marked once", got)
			}
		})
	}
}

// Why (#607, T16): the admin accepted the lockout because HTTP was silent at the start; when HTTP answers again by the
// upload, the restore still changes the MQTT connection while HTTP works, so the device is marked like any other: its
// next commands go over HTTP instead of waiting for timeouts on the old topic.
func TestAnAcceptedLockoutStillMarksTheDeviceWhenHTTPAnswersAtTheUpload(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowMQTTLockout: true})
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
	if got := e.marks(); !slices.Equal(got, []model.DeviceID{e.dev}) {
		t.Errorf("marks %v: HTTP answered at the upload, so the device must be marked once", got)
	}
}

// Why (#607, T16, R13): the admin who accepts the lockout accepts losing the device, not handing its web password to
// whatever holds its address. When the host there reports another MAC at the upload, HTTP was not proven for this
// device, so the wait for the restart must not poll that host with the stored credentials (Status 1 and Status 11 carry
// them unasked): the HTTP way back opens only after an HTTP proof at the upload. The job then ends "did not restart",
// the outcome of an accepted lockout.
func TestAnAcceptedLockoutNeverSendsTheWebPasswordToTheHostAtTheAddress(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	u := &silencingUploader{fakeUploader: &fakeUploader{e: e, apply: true}}
	m := e.mqttManager(t, u.fakeUploader, transport.MQTT)
	m.o.Uploader = u
	w := watchCreds(t, e, m)
	e.httpMAC.Store("aa:bb:cc:99:99:99")
	w.on.Store(true)
	job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowMQTTLockout: true})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if got := w.calls(); len(got) != 0 {
		t.Errorf("the host that reported another MAC received the stored web password with %v", got)
	}
	if done.State == store.RestoreDone {
		t.Errorf("job: %+v: no transport proved that this device restarted", done)
	}
	if got := e.marks(); len(got) != 0 {
		t.Errorf("marks %v: HTTP was not proven for this device, so its next commands must not be sent there", got)
	}
}

// credsWatch is the Router of a restore over MQTT (idGuard) that records, while on, every HTTP request, and separately
// every one that would carry the device's stored web credentials to the host at its address.
type credsWatch struct {
	idGuard
	on   atomic.Bool
	mu   sync.Mutex
	all  []string // every HTTP request while on
	seen []string // those that carry the credentials
}

func (w *credsWatch) note(t tasmota.Target, what string) {
	if !w.on.Load() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all = append(w.all, what)
	if t.HasCredentials() {
		w.seen = append(w.seen, what)
	}
}

func (w *credsWatch) requests() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.all...)
}

func (w *credsWatch) calls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

func (w *credsWatch) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if pin, _ := transport.PathFrom(ctx); pin == transport.HTTP {
		w.note(t, c)
	}
	return w.idGuard.Command(ctx, t, c)
}

func (w *credsWatch) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	w.note(t, "FSInfo")
	return w.idGuard.FSInfo(ctx, t)
}

func (w *credsWatch) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	w.note(t, "ListFiles")
	return w.idGuard.ListFiles(ctx, t, dir)
}

func (w *credsWatch) DownloadFile(ctx context.Context, t tasmota.Target, p string, max int64) ([]byte, error) {
	w.note(t, "DownloadFile")
	return w.idGuard.DownloadFile(ctx, t, p, max)
}

func (w *credsWatch) UploadFile(ctx context.Context, t tasmota.Target, p string, data []byte) error {
	w.note(t, "UploadFile")
	return w.idGuard.UploadFile(ctx, t, p, data)
}

// watchCreds gives the device a stored web password and makes the restore service's client a credsWatch.
func watchCreds(t *testing.T, e *flowEnv, m *Manager) *credsWatch {
	t.Helper()
	dev, err := e.st.GetDevice(t.Context(), e.dev)
	if err != nil {
		t.Fatal(err)
	}
	dev.HTTPPassword = "stored-web-password"
	if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	w := &credsWatch{idGuard: idGuard{e.cl, e}}
	m.o.Client = w
	return w
}

// Why (#607, the review's second finding): the files of a restore over MQTT go to the host at the device's address,
// which may be another device by now. The HTTP identity check asks it without the stored web password first, so it must
// come before the first file call, which carries the password: a host that is not the device must never see it. Over
// HTTP the restore has always worked this way (liveMAC); the confirmation page (Assess) and the run must too.
func TestTheFilesOfAnMQTTRestoreNeverSendTheWebPasswordToAnotherHost(t *testing.T) {
	t.Run("confirmation page", func(t *testing.T) {
		e, a := filesFlow(t)
		m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
		w := watchCreds(t, e, m)
		e.httpMAC.Store("aa:bb:cc:99:99:99")
		w.on.Store(true)
		as, err := m.Assess(t.Context(), a.ID)
		if got := w.calls(); len(got) != 0 {
			t.Errorf("the other host received the stored web password with %v", got)
		}
		if err != nil || as.Files.Restores() || as.Files.Why != whyFilesOtherHost {
			t.Fatalf("assessment %+v, %v", as.Files, err)
		}
	})
	t.Run("run", func(t *testing.T) {
		e, a := filesFlow(t)
		m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
		w := watchCreds(t, e, m)
		e.hb.hook = func(call int) {
			if call == 1 { // after the safety backup, right before the files
				e.httpMAC.Store("aa:bb:cc:99:99:99")
				w.on.Store(true)
			}
		}
		job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
		if err != nil {
			t.Fatal(err)
		}
		done := e.finish(t, job.ID)
		if files, _ := SplitDetail(done.Detail); done.State != store.RestoreDone || !strings.Contains(files, "not this device") {
			t.Fatalf("job: %+v", done)
		}
		if got := w.calls(); len(got) != 0 {
			t.Errorf("the other host received the stored web password with %v", got)
		}
	})
}

// replyClient answers each command with a canned reply ("" and a missing command: no answer).
type replyClient struct {
	nopClient
	replies map[string]string
}

func (c replyClient) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	r, ok := c.replies[cmnd]
	if !ok || r == "" {
		return nil, tasmota.ErrTimeout
	}
	return json.RawMessage(r), nil
}

// Why (R28, fail closed): the restore reads the device's MQTT settings the way Tasmota answers them (an empty prefix as
// the default, fingerprints as 20 hex bytes; SetOptions: TestSetOptionsAreReadAsNumbersWhateverTheDevicesStateText) and
// counts every answer it cannot read as "not reported", never as equal: a garbled answer must not let a connection
// change through without the question.
func TestTheDevicesMQTTSettingsAreReadAsTasmotaAnswersThem(t *testing.T) {
	fp := func(b string) string { return strings.TrimSpace(strings.Repeat(b+" ", 20)) }
	for name, tc := range map[string]struct {
		replies map[string]string
		run     func(*connCompare)
		diff    []string
		unread  []string
	}{
		"prefix not JSON": {replies: map[string]string{"Prefix": `not json`}, run: func(c *connCompare) { c.prefixes(nil) }, unread: []string{"Prefix"}},
		"prefix incomplete": {replies: map[string]string{"Prefix": `{"Prefix1":"cmnd","Prefix2":"stat"}`},
			run: func(c *connCompare) { c.prefixes(nil) }, unread: []string{"Prefix"}},
		"empty prefixes are the defaults": {replies: map[string]string{"Prefix": `{"Prefix1":"","Prefix2":"stat","Prefix3":""}`},
			run: func(c *connCompare) { c.prefixes([]string{"cmnd"}) }},
		"no answer": {run: func(c *connCompare) { c.text("MqttClient", "MqttClient", "x") }, unread: []string{"MqttClient"}},
		"text not a string": {replies: map[string]string{"MqttClient": `{"MqttClient":5}`},
			run: func(c *connCompare) { c.text("MqttClient", "MqttClient", "x") }, unread: []string{"MqttClient"}},
		"port as a number": {replies: map[string]string{"MqttPort": `{"MqttPort":1883}`},
			run: func(c *connCompare) { n, ok := c.liveNumber("MqttPort"); c.known("MqttPort", ok, n != 8883) }, diff: []string{"MqttPort"}},
		"port not a number": {replies: map[string]string{"MqttPort": `{"MqttPort":"1883"}`},
			run: func(c *connCompare) { n, ok := c.liveNumber("MqttPort"); c.known("MqttPort", ok, n != 1883) }, unread: []string{"MqttPort"}},
		"fingerprint of the backup not 20 bytes": {replies: map[string]string{"MqttFingerprint1": `{"MqttFingerprint1":"` + fp("00") + `"}`},
			run: func(c *connCompare) { c.fingerprint(1, json.RawMessage(`["0x00"]`)) }, unread: []string{"MqttFingerprint1"}},
		"fingerprint of the backup not a list": {run: func(c *connCompare) { c.fingerprint(1, json.RawMessage(`"00"`)) }, unread: []string{"MqttFingerprint1"}},
		"fingerprint not answered":             {run: func(c *connCompare) { c.fingerprint(1, json.RawMessage(fingerprintJSON(0))) }, unread: []string{"MqttFingerprint1"}},
		"fingerprint garbled": {replies: map[string]string{"MqttFingerprint1": `{"MqttFingerprint1":"` + fp("zz") + `"}`},
			run: func(c *connCompare) { c.fingerprint(1, json.RawMessage(fingerprintJSON(0))) }, unread: []string{"MqttFingerprint1"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := &connCompare{m: &Manager{o: Options{Client: replyClient{replies: tc.replies}}}, ctx: t.Context(), dev: model.Device{ID: 1}}
			tc.run(c)
			if !slices.Equal(c.diff, tc.diff) || !slices.Equal(c.unread, tc.unread) {
				t.Fatalf("changes %v, not reported %v; want %v, %v", c.diff, c.unread, tc.diff, tc.unread)
			}
		})
	}
}

// gatedWatch is a credsWatch whose router knows the device's HTTP to fail (transport.Router.FilesUsable).
type gatedWatch struct{ *credsWatch }

func (gatedWatch) FilesUsable(tasmota.Target) error { return transport.ErrFilesNeedHTTP }

// Why (#607, T15): while the device's HTTP is known to fail, the files of a restore over MQTT are left out without one
// HTTP request: the identity question before the files would only wait for the same timeout as the file calls.
func TestTheFilesOfAnMQTTRestoreAskNothingWhileHTTPIsKnownToFail(t *testing.T) {
	e, a := filesFlow(t)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	w := watchCreds(t, e, m)
	m.o.Client = gatedWatch{w}
	w.on.Store(true)
	as, err := m.Assess(t.Context(), a.ID)
	if err != nil || as.Files.Restores() || as.Files.Why != whyFilesNoHTTP {
		t.Fatalf("assessment %+v, %v", as.Files, err)
	}
	if got := w.requests(); len(got) != 0 {
		t.Errorf("HTTP was asked %v", got)
	}
}

// observedHTTP is idGuard whose calls pinned to HTTP pass the router's HTTP observer, which remembers failing HTTP calls
// (transport.Memory) as in production. While the device restarts (restarting, set by restartUploader), its HTTP API
// does not answer the wait's Status 1; at the second such poll the device is back on both transports (MQTT answers the
// next poll first, as on the live test of 0.37.0).
type observedHTTP struct {
	idGuard
	obs        tasmota.Client
	restarting *atomic.Bool
	polls      *atomic.Int32 // the wait's HTTP polls left unanswered
}

func (o observedHTTP) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if pin, _ := transport.PathFrom(ctx); pin != transport.HTTP {
		return o.idGuard.Command(ctx, t, c)
	}
	raw, err := o.obs.Command(ctx, t, c)
	if o.restarting.Load() && c == "Status 1" && o.polls.Add(1) == 2 {
		o.restarting.Store(false)
		o.e.mqttDown.Store(false)
		o.e.httpDown.Store(false)
	}
	return raw, err
}

// restartUploader uploads, then makes the device silent on both transports while it restarts.
type restartUploader struct {
	*fakeUploader
	restarting *atomic.Bool
}

func (u restartUploader) UploadConfig(ctx context.Context, t tasmota.Target, dump []byte, bind transport.UploadBinding) error {
	err := u.fakeUploader.UploadConfig(ctx, t, dump, bind)
	u.e.mqttDown.Store(true)
	u.e.httpDown.Store(true)
	u.restarting.Store(true)
	return err
}

// Why (#854, the live test of 0.37.0): after a restore over MQTT that changed the MQTT connection while HTTP answered,
// the wait for the restart also asks HTTP, and the restarting device leaves those polls unanswered. Remembered as HTTP
// failures, they kept the device's HTTP "failing" for ten minutes: the check backup right after the restore and every
// manual backup in that window left the files out ("files not backed up: they need HTTP"), and the restore could not
// compare the files it had written. A device that restarts says nothing about its HTTP API, so those polls must not
// mark it; the router's real observer and memory decide here.
func TestTheWaitForTheRestartDoesNotMarkHTTPFailing(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	var restarting atomic.Bool
	var polls atomic.Int32
	u := restartUploader{fakeUploader: &fakeUploader{e: e, apply: true}, restarting: &restarting}
	m := e.mqttManager(t, u.fakeUploader, transport.MQTT)
	m.o.Uploader = u
	g := idGuard{e.cl, e}
	router := transport.NewRouter(transport.Options{HTTP: g, Devices: e.st})
	m.o.Client = observedHTTP{idGuard: g, obs: router.HTTPOnly(), restarting: &restarting, polls: &polls}
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone || !done.Verified {
		t.Fatalf("job: %+v: the device restarted with the backup", done)
	}
	if n := polls.Load(); n < 2 {
		t.Fatalf("%d HTTP polls went unanswered during the restart; the case needs two", n)
	}
	if kind, since, failing := router.Memory().HTTPFailure(e.dev); failing {
		t.Errorf("HTTP is remembered as failing (%s since %v) after polls the restarting device left unanswered", kind, since)
	}
}

// status3 is the Status 3 answer of kitchen-spotlight (testdata/fixtures/devices, Tasmota 16.0.0) with the SetOption
// words flag4 and flag5 given: SetOption103 is bit 21 of flag4, SetOption132 bit 18 of flag5.
func status3(flag4, flag5 string) string {
	return `{"StatusLOG":{"SerialLog":2,"WebLog":2,"MqttLog":0,"SysLog":0,"LogHost":"","LogPort":514,"SSId":["IoT","HOME"],` +
		`"TelePeriod":300,"Resolution":"558180C0","SetOption":["40208008","2805C80001000600003C5A0A192800000000","000000A8","` +
		flag4 + `","` + flag5 + `","00000000"]}}`
}

// Why (#845, R28): Tasmota answers SetOption<n> with the device's own StateText1 or StateText2 (GetStateText), so a device
// with German texts says "AUS" and "AN". Read from that answer, SetOption103 and SetOption132 of such a device were never
// reported, and every restore over MQTT asked the lockout question while HTTP was silent, or marked the device while it
// answered, for a backup with the device's own TLS settings. The restore reads them as numbers from Status 3 instead,
// which no StateText changes; an answer without the bit is still "not reported", never equal (fail closed).
func TestSetOptionsAreReadAsNumbersWhateverTheDevicesStateText(t *testing.T) {
	german := func(tls, fp string, status string) map[string]string {
		return map[string]string{"SetOption103": `{"SetOption103":"` + tls + `"}`, "SetOption132": `{"SetOption132":"` + fp + `"}`,
			"Status 3": status}
	}
	both := func(c *connCompare, tls, fp int) {
		c.option("SetOption103 (MQTT TLS)", 103, tls)
		c.option("SetOption132 (TLS fingerprint)", 132, fp)
	}
	for name, tc := range map[string]struct {
		replies map[string]string
		run     func(*connCompare)
		diff    []string
		unread  []string
	}{
		"AUS, the backup's own": {replies: german("AUS", "AUS", status3("00006000", "00004000")),
			run: func(c *connCompare) { both(c, 0, 0) }},
		"AN, the backup's own": {replies: german("AN", "AN", status3("00206000", "00044000")),
			run: func(c *connCompare) { both(c, 1, 1) }},
		"AUS, the backup turns them on": {replies: german("AUS", "AUS", status3("00006000", "00004000")),
			run: func(c *connCompare) { both(c, 1, 1) }, diff: []string{"SetOption103 (MQTT TLS)", "SetOption132 (TLS fingerprint)"}},
		"AN, the backup turns TLS off": {replies: german("AN", "AUS", status3("00206000", "00004000")),
			run: func(c *connCompare) { both(c, 0, 0) }, diff: []string{"SetOption103 (MQTT TLS)"}},
		"Status 3 not answered": {replies: german("AUS", "AUS", ""),
			run: func(c *connCompare) { both(c, 0, 0) }, unread: []string{"SetOption103 (MQTT TLS)", "SetOption132 (TLS fingerprint)"}},
		"Status 3 without the SetOption words": {replies: german("AUS", "AUS", `{"StatusLOG":{"TelePeriod":300}}`),
			run: func(c *connCompare) { both(c, 0, 0) }, unread: []string{"SetOption103 (MQTT TLS)", "SetOption132 (TLS fingerprint)"}},
		"Status 3 with a garbled word": {replies: german("AUS", "AUS", status3("0000600G", "00004000")),
			run: func(c *connCompare) { both(c, 0, 0) }, unread: []string{"SetOption103 (MQTT TLS)"}},
		"Status 3 not JSON": {replies: german("AUS", "AUS", `not json`),
			run: func(c *connCompare) { both(c, 0, 0) }, unread: []string{"SetOption103 (MQTT TLS)", "SetOption132 (TLS fingerprint)"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := &connCompare{m: &Manager{o: Options{Client: replyClient{replies: tc.replies}}}, ctx: t.Context(), dev: model.Device{ID: 1}}
			tc.run(c)
			if !slices.Equal(c.diff, tc.diff) || !slices.Equal(c.unread, tc.unread) {
				t.Fatalf("changes %v, not reported %v; want %v, %v", c.diff, c.unread, tc.diff, tc.unread)
			}
		})
	}
}

// Why (#845): the SetOption words of Status 3 place each bit option as the firmware's SetoptionDecode does; a wrong word
// or bit compares another option with the backup. kitchen-spotlight's real answer has SetOption3, 15, 21, 30, 53, 55,
// 57, 95, 96 and 128 on; SetOption32-49 are numbers, not bits, and there is no SetOption178.
func TestEachSetOptionIsReadFromItsWordOfStatus3(t *testing.T) {
	on := map[int]bool{3: true, 15: true, 21: true, 30: true, 53: true, 55: true, 57: true, 95: true, 96: true, 128: true}
	for n := range 180 {
		c := &connCompare{m: &Manager{o: Options{Client: replyClient{replies: map[string]string{"Status 3": status3("00006000", "00004000")}}}},
			ctx: t.Context(), dev: model.Device{ID: 1}}
		c.option("x", n, 0) // the backup has it off: a change exactly when the device has it on
		switch {
		case n >= 32 && n <= 49 || n > 177:
			if len(c.unread) != 1 {
				t.Errorf("SetOption%d read as a bit: changes %v", n, c.diff)
			}
		case len(c.unread) != 0 || (len(c.diff) == 1) != on[n]:
			t.Errorf("SetOption%d: changes %v, not reported %v; want on %v", n, c.diff, c.unread, on[n])
		}
	}
}

// answering is the device on its topic with its own answers to some commands (a reply "" leaves it unanswered); every
// other call goes to the idGuard.
type answering struct {
	idGuard
	replies map[string]string
}

func (a answering) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if r, ok := a.replies[c]; ok {
		if r == "" {
			return nil, fmt.Errorf("%w: test: the device left %s unanswered", tasmota.ErrTimeout, c)
		}
		return json.RawMessage(r), nil
	}
	return a.idGuard.Command(ctx, t, c)
}

// Why (#844, R28): a decoded backup holds the Topic setting with its placeholders ("tasmota_%06X", Tasmota's default),
// the inventory the topic the device runs under ("tasmota_32D74B"). Compared with each other, every restore over MQTT
// of a device that keeps a placeholder topic counted as a change of the topic: a false lockout question while HTTP was
// silent, a mark while it answered. The device answers Topic with its setting, so the backup's setting is compared with
// the device's, raw against raw like MqttClient, and the same setting on the same device is the same topic. Only
// %hostname% depends on another setting, the Hostname, which a restore writes too: with it in the topic both Hostnames
// are compared, every value with a '%' as the one the firmware puts in its place at boot. A side that cannot be read is
// "not reported", never equal (fail closed).
func TestAPlaceholderTopicIsComparedRawWithTheDevicesTopic(t *testing.T) {
	const (
		changesTopic    = "this backup changes Topic"
		changesHostname = "this backup changes Hostname (in the Topic)"
		tail            = ", so the backup's MQTT settings cannot be compared with the device's"
	)
	for name, tc := range map[string]struct {
		topic, hostname string            // the backup's Topic and Hostname settings (hostname "": none)
		device          map[string]string // the device's answers
		want            string            // the whole sentence
	}{
		"the same placeholder": {topic: "tasmota_%06X", hostname: "%s-%04d",
			device: map[string]string{"Topic": `{"Topic":"tasmota_%06X"}`}},
		"another placeholder": {topic: "plug_%06X",
			device: map[string]string{"Topic": `{"Topic":"tasmota_%06X"}`}, want: changesTopic},
		"no placeholder, the device's own": {topic: "kitchen-spotlight",
			device: map[string]string{"Topic": `{"Topic":"kitchen-spotlight"}`}},
		"%hostname% with the same Hostname": {topic: "%hostname%", hostname: "kitchen-spotlight",
			device: map[string]string{"Topic": `{"Topic":"%hostname%"}`, "Hostname": `{"Hostname":"kitchen-spotlight"}`}},
		"%hostname% with another Hostname": {topic: "%hostname%", hostname: "hall-spotlight",
			device: map[string]string{"Topic": `{"Topic":"%hostname%"}`, "Hostname": `{"Hostname":"kitchen-spotlight"}`}, want: changesHostname},
		"%hostname%, both Hostnames with a %": {topic: "dev_%hostname%", hostname: "lamp-%06X",
			device: map[string]string{"Topic": `{"Topic":"dev_%hostname%"}`, "Hostname": `{"Hostname":"%s-%04d"}`}},
		"%hostname%, a % only in the backup's Hostname": {topic: "%hostname%", hostname: "%s-%04d",
			device: map[string]string{"Topic": `{"Topic":"%hostname%"}`, "Hostname": `{"Hostname":"kitchen-spotlight"}`}, want: changesHostname},
		"%hostname% without Hostname in the backup": {topic: "%hostname%",
			device: map[string]string{"Topic": `{"Topic":"%hostname%"}`, "Hostname": `{"Hostname":"kitchen-spotlight"}`},
			want:   "the device did not report its Hostname (in the Topic)" + tail},
		"%hostname%, Hostname not answered": {topic: "%hostname%", hostname: "kitchen-spotlight",
			device: map[string]string{"Topic": `{"Topic":"%hostname%"}`, "Hostname": ""},
			want:   "the device did not report its Hostname (in the Topic)" + tail},
		"Topic not answered": {topic: "tasmota_%06X",
			device: map[string]string{"Topic": ""}, want: "the device did not report its Topic" + tail},
	} {
		t.Run(name, func(t *testing.T) {
			extra := fmt.Sprintf(`,"mqtt_topic":%q`, tc.topic)
			if tc.hostname != "" {
				extra += fmt.Sprintf(`,"hostname":%q`, tc.hostname)
			}
			e := newFlowWith(t, "15.6.0", connDecoder{extra: extra})
			a := e.dumpBackup(t, e.dumpA)
			m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
			m.o.Client = answering{idGuard: idGuard{e.cl, e}, replies: tc.device}
			dev, err := e.st.GetDevice(t.Context(), e.dev)
			if err != nil {
				t.Fatal(err)
			}
			dev.Topic = "tasmota_32D74B" // the resolved topic, as the inventory holds it
			if got := m.mqttLockout(transport.WithPath(t.Context(), transport.MQTT), dev, a); got != tc.want {
				t.Fatalf("%q, want %q", got, tc.want)
			}
		})
	}
}

// Why (#844): the whole restore of a device whose backup keeps Tasmota's default topic, while HTTP is silent: the
// device answers Topic with the same setting, so its own topic is no change of the MQTT connection, no lockout question
// comes and the device is not marked.
func TestARestoreOfADeviceWithAPlaceholderTopicAsksNothing(t *testing.T) {
	e := newFlowWith(t, "15.6.0", connDecoder{extra: `,"mqtt_topic":"tasmota_%06X"`})
	dev, err := e.st.GetDevice(t.Context(), e.dev)
	if err != nil {
		t.Fatal(err)
	}
	mac := strings.ToUpper(transport.NormMAC(dev.MAC))
	dev.Topic = "tasmota_" + mac[6:]
	if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.httpDown.Store(true)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	m.o.Client = answering{idGuard: idGuard{e.cl, e}, replies: map[string]string{"Topic": `{"Topic":"tasmota_%06X"}`}}
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatalf("the device's own topic asked: %v", err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
	if got := e.marks(); len(got) != 0 {
		t.Errorf("marked %v for a restore that keeps the device's topic", got)
	}
}
