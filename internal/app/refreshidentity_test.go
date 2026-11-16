package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// "Refresh identity" brings every identity field up to date (#91) and refuses a device that is
// not the stored one (#65). The probe over MQTT first is tested in topic_identity_test.go (#682).

// refusal is how the web layer recognises a refused refresh (devices.go refreshProblem).
type refusal interface{ Refusal() string }

func isRefusal(err error) bool {
	var r refusal
	return errors.As(err, &r) && r.Refusal() != ""
}

func (a *idAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.ev {
		out = append(out, e.Action)
	}
	return out
}

func (a *idAudit) find(action string) (model.AuditEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.ev {
		if e.Action == action {
			return e, true
		}
	}
	return model.AuditEvent{}, false
}

// Why (#91): after "Refresh identity" the device page must show what the device reports now:
// device name, hardware, build, chip, firmware, hostname, MAC and MQTT topic. Before, the name
// was never written, the hostname only arrived later in the background (the card showed
// "Unknown" right after the refresh) and the topic was only repaired when unresolved.
func TestRefreshUpdatesEveryIdentityField(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	// Stored as an older version left it: named after the hostname, no topic, no identity.
	d.Name, d.Topic = "plug-1", ""
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status": status16}}
	p.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDevice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ field, got, want string }{
		{"device name", got.Name, "Tasmota TV Wand"},
		{"hardware", got.Identity.Hardware, "ESP8285N08"},
		{"build", got.Identity.Variant, "tasmota"},
		{"chip", string(got.Identity.Family), string(model.ChipESP8266)},
		{"hostname", got.Net.Hostname, "plug-1"},
		{"IP", got.Net.IP, "192.0.2.10"},
		{"MAC", got.MAC, "aa:bb:cc:00:00:01"},
		{"MQTT topic", got.Topic, "tasmota_2BE970"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if got.Identity.Version == "" || got.Identity.DetectedAt.IsZero() || got.Identity.FlashSizeKB == 0 {
		t.Errorf("firmware/flash not refreshed: %+v", got.Identity)
	}
	if got.Net.ReadAt.IsZero() {
		t.Error("the network facts of Status 5 must be stored with the refresh, not only later")
	}
}

// Why (#52 decision, kept by #91): a name the user chose in tasmofleet is never overwritten by a
// refresh. The topic is not a user choice but a mirror of the device's (#111): it follows.
func TestRefreshKeepsAUserChosenNameButTopicFollows(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	d.Name, d.Topic = "Kitchen lamp", "my-topic"
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status": status16}}
	for range 2 {
		if err := p.Refresh(context.Background(), id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Name != "Kitchen lamp" || got.Topic != "tasmota_2BE970" {
		t.Fatalf("name %q topic %q, want the user's name and the device's topic", got.Name, got.Topic)
	}
}

// Why (#91): a name that is the device's own DeviceName follows it when it changes on the
// device; the Tasmota default DeviceName ("Tasmota") never replaces a hostname name, it would
// turn many distinct devices into "Tasmota".
func TestRefreshNameFollowsTheDeviceNameButNotTheDefault(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	status := func(name, topic string) string {
		b, _ := json.Marshal(map[string]any{"Status": map[string]string{"DeviceName": name, "Topic": topic}})
		return string(b)
	}
	d.Name = "plug-1" // the hostname, as older versions named devices
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	base := p.Client
	refresh := func(name, topic string) model.Device {
		t.Helper()
		p.Client = override{Client: base, reply: map[string]string{"Status 5": status5Net, "Status": status(name, topic)}}
		if err := p.Refresh(context.Background(), id, "admin"); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetDevice(context.Background(), id)
		return got
	}
	if got := refresh("Tasmota", "plug_a"); got.Name != "plug-1" {
		t.Fatalf("Name = %q, the default DeviceName replaced the hostname name", got.Name)
	}
	if got := refresh("Desk lamp", "plug_a"); got.Name != "Desk lamp" {
		t.Fatalf("Name = %q, want the DeviceName", got.Name)
	}
	if got := refresh("Desk lamp left", "plug_b"); got.Name != "Desk lamp left" || got.Topic != "plug_b" {
		t.Fatalf("name %q topic %q, want both to follow the device", got.Name, got.Topic)
	}
}

// Why (#65): if the stored address now belongs to another device (DHCP reshuffle, a spoofed
// claim), a refresh must not fill the stored device with the other device's identity, and the
// stored password must not go to that host more than the one command that read its MAC.
// The refusal is distinct (the device page explains it) and audited.
func TestRefreshRefusesADeviceWithAnotherMAC(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	audit := &idAudit{}
	p.Audit = audit
	var sent []string
	p.Client = recorder{override{Client: p.Client, reply: map[string]string{
		"Status 5": `{"StatusNET":{"Hostname":"intruder","IPAddress":"192.0.2.10","Mac":"DE:AD:BE:EF:00:01"}}`,
	}}, &sent}
	err := p.Refresh(context.Background(), id, "admin")
	if !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if strings.Join(sent, ",") != "Status 5" {
		t.Fatalf("sent %v after the MAC mismatch, want only Status 5", sent)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Identity.Version != "" || got.Net.Hostname != "" || got.MAC != d.MAC || got.Name != d.Name {
		t.Fatalf("a refused refresh wrote %+v", got)
	}
	ev, ok := audit.find("device.refresh_refused")
	if !ok || ev.Actor != "admin" || ev.Result != "refused" || !strings.Contains(ev.Detail, "de:ad:be:ef:00:01") {
		t.Fatalf("audit %+v (all: %v)", ev, audit.actions())
	}
}

// Why (#65): a wired board answers with its Wi-Fi MAC and lists the Ethernet MAC too; either
// one in the stored MAC set is the same device.
func TestRefreshAcceptsTheEthernetMACOfTheStoredSet(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	cur, _ := st.GetDevice(context.Background(), id)
	// A device stored by its Ethernet MAC only.
	cur.MAC = "aa:bb:cc:00:00:e1"
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{
		"Status 5": `{"StatusNET":{"Hostname":"eth","IPAddress":"0.0.0.0","Mac":"AA:BB:CC:00:00:01","Ethernet":{"Hostname":"eth","IPAddress":"192.0.2.10","Gateway":"192.0.2.1","Mac":"AA:BB:CC:00:00:E1"}}}`,
	}}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatalf("the Ethernet MAC is the stored device: %v", err)
	}
}

// Why (#65, D8): a device stored without a MAC (import) has nothing to compare with. An admin's
// refresh binds the MAC it read, audited, so every later probe can tell another device at the
// address. An automatic refresh (discovery's reprobe, triggered by an MQTT message anyone can
// publish) never binds: binding stays an admin action.
func TestRefreshBindsTheMACOnlyForAnAdmin(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	audit := &idAudit{}
	p.Audit = audit
	cur, _ := st.GetDevice(context.Background(), id)
	cur.MAC = ""
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" {
		t.Fatalf("an automatic refresh bound MAC %q", got.MAC)
	}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.MAC != d.MAC {
		t.Fatalf("MAC = %q, want the one the device reported, %q", got.MAC, d.MAC)
	}
	ev, ok := audit.find("device.mac_bound")
	if !ok || ev.Actor != "admin" || !strings.Contains(ev.Detail, got.MAC) {
		t.Fatalf("audit %+v (all: %v)", ev, audit.actions())
	}
}

// Why (#65): binding a MAC that another stored device holds would give two records one device;
// the address answers as that other device, so the refresh is refused like a mismatch.
func TestRefreshRefusesToBindAMACOfAnotherDevice(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	p.Audit = &idAudit{}
	cur, _ := st.GetDevice(context.Background(), id)
	cur.MAC = ""
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDevice(context.Background(), model.Device{Name: "owner", Address: "192.0.2.77", MAC: "aa:bb:cc:00:00:01"}); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net}}
	if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" || got.Identity.Version != "" {
		t.Fatalf("a refused refresh wrote %+v", got)
	}
}

// unreachable is a device client for a device HTTP cannot reach.
type unreachable struct{ tasmota.Client }

func (unreachable) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	return nil, tasmota.ErrUnreachable
}

// credLog records every command and whether its Target carried the stored credentials.
type credLog struct {
	tasmota.Client
	mu    sync.Mutex
	sent  []string // "Status 5" or "Status 5 +cred"
	auth  bool     // the device has a web password: commands without credentials get ErrAuth
	reply map[string]string
}

func (c *credLog) Command(ctx context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	c.mu.Lock()
	entry := cmd
	if t.HasCredentials() {
		entry += " +cred"
	}
	c.sent = append(c.sent, entry)
	c.mu.Unlock()
	if c.auth && !t.HasCredentials() {
		return nil, tasmota.ErrAuth
	}
	if r, ok := c.reply[cmd]; ok {
		return json.RawMessage(r), nil
	}
	return c.Client.Command(ctx, t, cmd)
}

func (c *credLog) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	if c.auth && !t.HasCredentials() {
		return tasmota.RootInfo{}, tasmota.ErrAuth
	}
	return c.Client.RootPage(ctx, t)
}

func (c *credLog) withCredentials() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if strings.HasSuffix(s, " +cred") {
			out = append(out, s)
		}
	}
	return out
}

// Why (#96 review): the MAC is checked before the stored password goes anywhere. Status 5 is
// asked without credentials first; a device without a web password answers it, and then the
// whole probe needs no credentials, so a device with another MAC never sees the password.
// Only a device that asks for a password (401) gets Status 5 again with the credentials.
func TestRefreshAsksTheMACWithoutCredentialsFirst(t *testing.T) {
	const otherMAC = `{"StatusNET":{"Hostname":"x","IPAddress":"192.0.2.10","Mac":"DE:AD:BE:EF:00:01"}}`
	setup := func(t *testing.T) (*Prober, *store.SQLite, model.DeviceID) {
		p, st, id, _, _ := raceSetup(t)
		cur, _ := st.GetDevice(context.Background(), id)
		cur.HTTPUser, cur.HTTPPassword = "admin", "stored-pw"
		if err := st.UpdateDevice(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		return p, st, id
	}
	t.Run("no web password, another MAC", func(t *testing.T) {
		p, _, id := setup(t)
		c := &credLog{Client: p.Client, reply: map[string]string{"Status 5": otherMAC}}
		p.Client = c
		if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if got := c.withCredentials(); len(got) != 0 {
			t.Fatalf("credentials went to another device: %v", got)
		}
	})
	t.Run("no web password, the stored device", func(t *testing.T) {
		p, _, id := setup(t)
		c := &credLog{Client: p.Client}
		p.Client = c
		if err := p.Refresh(context.Background(), id, "admin"); err != nil {
			t.Fatal(err)
		}
		if got := c.withCredentials(); len(got) != 0 {
			t.Fatalf("a device without a web password got the credentials: %v", got)
		}
	})
	t.Run("web password, another MAC", func(t *testing.T) {
		p, _, id := setup(t)
		c := &credLog{Client: p.Client, auth: true, reply: map[string]string{"Status 5": otherMAC}}
		p.Client = c
		if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if strings.Join(c.sent, ",") != "Status 5,Status 5 +cred" {
			t.Fatalf("sent %v, want Status 5 without, then with credentials, then nothing", c.sent)
		}
	})
}

// storedMinimal stores a simulated device that runs minimal firmware, with mac and an identity
// an earlier HTTP probe stored.
func storedMinimal(t *testing.T, mac string) (*Prober, *store.SQLite, model.DeviceID, model.Identity) {
	t.Helper()
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "minimal-plug", Profile: simulator.ProfileESP8266Min})
	st := newTestStore(t)
	d.Name, d.MAC = "plug", mac
	id, err := st.CreateDevice(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	before := model.Identity{Family: model.ChipESP8266, Hardware: "ESP8285N08", Version: "15.0.0", Image: "tasmota", Variant: "tasmota",
		DetectedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if err := st.UpdateIdentity(context.Background(), id, before); err != nil {
		t.Fatal(err)
	}
	p.Devices = st
	return p, st, id, before
}

// Why (#96 review L2): minimal firmware answers no Status 5, so the MAC of a device that has
// one stored cannot be checked. The refresh must not then take the answers as the stored
// device's: the stored identity and its time stay, and the page says why.
func TestRefreshOfMinimalFirmwareWithAStoredMACKeepsTheIdentity(t *testing.T) {
	p, st, id, before := storedMinimal(t, "aa:bb:cc:00:00:01")
	var sent []string
	p.Client = recorder{override{Client: p.Client}, &sent}
	err := p.Refresh(context.Background(), id, "admin")
	if !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal that explains the unchecked MAC", err)
	}
	if strings.Join(sent, ",") != "Status 5,Status 2" {
		t.Fatalf("sent %v, want nothing after Status 2 decided minimal", sent)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Identity.Version != before.Version || got.Identity.Variant != before.Variant || !got.Identity.DetectedAt.Equal(before.DetectedAt) {
		t.Fatalf("identity %+v, want the stored %+v", got.Identity, before)
	}
	// Without a stored MAC there is nothing to check: the minimal identity is stored.
	p2, st2, id2, _ := storedMinimal(t, "")
	if err := p2.Refresh(context.Background(), id2, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st2.GetDevice(context.Background(), id2); got.Identity.Variant != "minimal" {
		t.Fatalf("variant %q, want minimal", got.Identity.Variant)
	}
}

// Why (#96 review L4): only a real MAC is bound. An all-zero or malformed MAC in the answer would
// bind a value every broken device shares, and the next refresh would compare with it.
func TestRefreshBindsOnlyAValidMAC(t *testing.T) {
	for _, mac := range []string{"00:00:00:00:00:00", "not-a-mac", ""} {
		t.Run(mac, func(t *testing.T) {
			p, st, id, _, _ := raceSetup(t)
			audit := &idAudit{}
			p.Audit = audit
			cur, _ := st.GetDevice(context.Background(), id)
			cur.MAC = ""
			if err := st.UpdateDevice(context.Background(), cur); err != nil {
				t.Fatal(err)
			}
			p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": `{"StatusNET":{"Hostname":"h","IPAddress":"192.0.2.10","Mac":"` + mac + `"}}`}}
			if err := p.Refresh(context.Background(), id, "admin"); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" {
				t.Fatalf("bound %q", got.MAC)
			}
			if _, ok := audit.find("device.mac_bound"); ok {
				t.Fatal("a binding was audited")
			}
		})
	}
}

func newTestStore(t *testing.T) *store.SQLite {
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
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	return st
}

// Why (#104): a device added while it still had Tasmota's default DeviceName is stored under
// the name "Tasmota", which tasmofleet filled in itself. If it is then renamed on the device and
// refreshed before any probe recorded the DeviceName, the stored name must follow it, else the
// header and the list keep the stale "Tasmota". A name the user typed is still kept.
func TestRefreshReplacesTheDefaultNameTasmofleetFilledIn(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	d.Name = "Tasmota"
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	status := `{"Status":{"DeviceName":"Irrigation Led Ring","Topic":"plug_a"}}`
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status": status}}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Name != "Irrigation Led Ring" {
		t.Fatalf("Name = %q, want the DeviceName the device reports now", got.Name)
	}
}
