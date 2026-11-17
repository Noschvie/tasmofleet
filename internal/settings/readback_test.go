package settings

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/testutil/redactguard"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// afterWrite is the part of what the device got after the writes: the read-back.
func afterWrite(sent []string, writes int) []string {
	if len(sent) < writes {
		return nil
	}
	return sent[writes:]
}

// Why (#947): "Done" only says the device accepted the command. The person needs to know the device now holds the
// value: the tab reads it back and says so per setting.
func TestAWrittenSettingIsReadBackAndVerified(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"FriendlyName1": `{"FriendlyName1":"Lamp"}`}}
	svc, _ := newSvc(c)
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"FriendlyName1": "Lamp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := afterWrite(c.sent, 1); !slices.Equal(got, []string{"FriendlyName1"}) {
		t.Fatalf("after the write the device got %q, want one bare query of the written setting", got)
	}
	if res[0].ReadBack.State != CheckVerified {
		t.Fatalf("read-back %+v, want verified", res[0].ReadBack)
	}
}

// Why (#947): a device that keeps another value (it clamps, refuses, or something changes it right back) must not
// look done: the tab shows its own answer, and the write itself stays a success (the coordinator's decision: a
// mismatch is shown, it does not fail). Status 3 is how TelePeriod is read (a bare TelePeriod publishes telemetry).
func TestADifferentAnswerIsShownWithTheDevicesValue(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"Status 3": `{"StatusLOG":{"TelePeriod":10}}`}}
	svc, _ := newSvc(c)
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"TelePeriod": "60"})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].OK() {
		t.Fatalf("the write failed: %+v", res[0])
	}
	if rb := res[0].ReadBack; rb.State != CheckDiffers || rb.Have != "10" {
		t.Fatalf("read-back %+v, want differs with the device's 10", rb)
	}
	for _, s := range afterWrite(c.sent, 1) {
		if s != "Status 3" {
			t.Fatalf("the read-back sent %q, want only Status 3 (never Status 0, never a bare TelePeriod)", s)
		}
	}
}

// restartingDevice is a device that restarts after Hostname: it does not answer the first reads after it.
type restartingDevice struct {
	fakeClient
	down int // reads that still fail
}

func (d *restartingDevice) Command(ctx context.Context, tg tasmota.Target, cmnd string) (json.RawMessage, error) {
	if !strings.Contains(cmnd, " ") && d.down > 0 {
		d.mu.Lock()
		d.down--
		d.sent = append(d.sent, cmnd)
		d.mu.Unlock()
		return nil, tasmota.ErrUnreachable
	}
	return d.fakeClient.Command(ctx, tg, cmnd)
}

// Why (#947): Hostname restarts the device. Reading it at once would meet the reboot and call a working change
// "not read back"; the read-back waits for the restart first and reads again while the device is still away.
func TestARestartingSettingIsReadBackOnceTheDeviceAnswersAgain(t *testing.T) {
	d := &restartingDevice{fakeClient: fakeClient{replies: map[string]string{"Hostname": `{"Hostname":"plug-2"}`}}, down: 2}
	var waits []time.Duration
	svc := New(Options{Devices: fakeDevices{}, Client: d, Wait: func(_ context.Context, w time.Duration) error {
		waits = append(waits, w)
		return nil
	}})
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"Hostname": "plug-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].ReadBack.State != CheckVerified {
		t.Fatalf("read-back %+v, want verified once the device answers again", res[0].ReadBack)
	}
	if len(waits) != 3 || waits[0] != restartWait {
		t.Fatalf("waits %v, want the restart's wait first, then one per read the device did not answer", waits)
	}
}

// Why (#947, #809, safety rule 4): a secret is written but never read back. Its read would put the secret on the
// broker and into the page; the tab says it was set and cannot be read back, and neither the result nor the audit
// holds its value. The guard plays the router and fails on a bare read of a masked command.
func TestASecretWriteIsNeverReadBack(t *testing.T) {
	const pw = "distinct-fake-mqtt-pw-947"
	c := &fakeClient{replies: map[string]string{"Status 6": `{"StatusMQT":{"MqttUser":"u"}}`}}
	aud := &fakeAudit{}
	svc := New(Options{Devices: fakeDevices{}, Client: redactguard.Wrap(t, c), Audit: aud, Wait: noWait})
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"MqttPassword": pw, "MqttUser": "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got := afterWrite(c.sent, 2); len(got) != 0 {
		t.Fatalf("after writing secrets the device got %q, want nothing: no secret is read back", got)
	}
	for _, r := range res {
		if r.ReadBack.State != CheckNotReadBack || r.ReadBack.Why != WhySecret || r.ReadBack.Have != "" {
			t.Errorf("%s: read-back %+v, want not read back because it is a secret", r.Key, r.ReadBack)
		}
	}
	blob, _ := json.Marshal(res)
	for _, e := range aud.events {
		blob = append(blob, e.Detail+e.Result...)
	}
	if strings.Contains(string(blob), pw) {
		t.Fatalf("the secret reached the result or the audit: %s", blob)
	}
}

// Why (#947, #662): after a restart the device can come back at another address, and another device answering
// there must not make the write look verified. The identity check runs over the read-back's reads; when it refuses,
// nothing counts as read back.
func TestAReadBackAnotherDeviceAnsweredIsNotVerified(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"FriendlyName1": `{"FriendlyName1":"Lamp"}`}}
	var traced []string
	svc := New(Options{Devices: fakeDevices{}, Client: c, Wait: noWait, Verify: func(_ context.Context, _ model.Device, read *transport.Trace) error {
		traced = append(traced, read.Via())
		return errors.New("another device answers at the stored address")
	}})
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"FriendlyName1": "Lamp"})
	if err != nil {
		t.Fatal(err)
	}
	rb := res[0].ReadBack
	if rb.State != CheckNotReadBack || !strings.Contains(rb.Why, "could not be confirmed") || rb.Have != "" {
		t.Fatalf("read-back %+v, want not read back: the device could not be confirmed", rb)
	}
	if len(traced) == 0 {
		t.Fatal("the identity check was not asked")
	}
}

// Why (#947): the firmware answers some values in a form of its own. Without knowing it the tab would show a
// correct write as "differs" and Make compliant would resend it forever: Timezone 1 reads back as "+01:00"
// (CmndTimezone), ON/OFF as 1/0, coordinates with four decimals. A real difference must still differ.
func TestMatchesKnowsTheFirmwaresOwnForms(t *testing.T) {
	for _, c := range []struct {
		key, have, want string
		equal           bool
	}{
		{"Timezone", "+01:00", "1", true},
		{"Timezone", "-05:00", "-5", true},
		{"Timezone", "+14:00", "+14", true},
		{"Timezone", "+05:30", "+05:30", true},
		{"Timezone", "+00:00", "-00:00", true},
		{"Timezone", "99", "99", true},
		{"Timezone", "+01:00", "2", false},
		{"Timezone", "99", "1", false},
		{"SetOption3", "ON", "1", true},
		{"SetOption3", "OFF", "1", false},
		{"Latitude", "52.5200", "52.52", true},
		{"Latitude", "52.5201", "52.52", false},
		{"FriendlyName1", " Lamp ", "Lamp", true},
		{"FriendlyName1", "lamp", "Lamp", false},
		{"TelePeriod", "10", "60", false},
	} {
		if got := Matches(c.key, c.have, c.want); got != c.equal {
			t.Errorf("Matches(%s, %q, %q) = %v, want %v", c.key, c.have, c.want, got, c.equal)
		}
	}
}

// Why (#947): a written Timezone must read back as verified on the tab, not as a difference in the firmware's
// format.
func TestAWrittenTimezoneIsVerifiedInTheFirmwaresForm(t *testing.T) {
	c := &fakeClient{replies: map[string]string{"Timezone": `{"Timezone":"+01:00"}`}}
	svc, _ := newSvc(c)
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"Timezone": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].ReadBack.State != CheckVerified {
		t.Fatalf("read-back %+v, want verified", res[0].ReadBack)
	}
}

// Why (#947): a write that failed has nothing to read back; reading it anyway would show the old value as if it
// were the outcome of the change.
func TestAFailedWriteIsNotReadBack(t *testing.T) {
	c := &fakeClient{errs: map[string]error{"FriendlyName1 Lamp": tasmota.ErrUnknownCommand}}
	svc, _ := newSvc(c)
	res, err := svc.Apply(t.Context(), "alice", 1, map[string]string{"FriendlyName1": "Lamp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 1 || res[0].ReadBack.State != "" {
		t.Fatalf("sent %q, read-back %+v: a failed write is not read back", c.sent, res[0].ReadBack)
	}
}
