package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#502): the edit form changes name, address, port, credentials, tags and notes, nothing
// else. The record it saves is built from the one read when the request started; in the meantime
// background writers may have recorded what the form does not show: onMQTTSeen sets the MQTT
// capability (once per device and run, so a lost flag stays lost until a restart), discovery learns
// the topic. Saving the old copy of those fields would put them back, so the save is a
// compare-and-set and, when the record changed, applies the form to the fresh record.
func TestEditKeepsWhatABackgroundWriterRecordedAfterTheFormWasRead(t *testing.T) {
	e := setup(t)
	e.inv.afterGet = func() { // the request has read the device; now the background writes
		d := e.inv.devs[1]
		d.MQTT, d.Topic = model.MQTTOurs, "kitchen-light"
		e.inv.devs[1] = d
	}
	resp, body := e.do(t, "POST", "/devices/1", "admin", url.Values{"csrf": {"tok"}, "name": {"kitchen"}, "address": {"10.0.0.1"}, "notes": {"typed by the admin"}})
	if resp.StatusCode != 303 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	d := e.inv.devs[1]
	if d.Notes != "typed by the admin" {
		t.Fatalf("the admin's edit was not saved: %+v", d)
	}
	if d.MQTT != model.MQTTOurs || d.Topic != "kitchen-light" {
		t.Fatalf("the edit put back what a background writer recorded meanwhile: MQTT %q, topic %q", d.MQTT, d.Topic)
	}
	if d.HTTPPassword != "s3cret-pw" {
		t.Fatalf("the stored password did not survive: %q", d.HTTPPassword)
	}
}

// The form is applied again onto the fresh record, so what the admin typed wins over a concurrent
// change of the same field, and what they did not touch comes from the fresh record.
func TestEditAppliesTheFormToTheFreshRecordAfterAConflict(t *testing.T) {
	e := setup(t)
	e.inv.afterGet = func() {
		d := e.inv.devs[1]
		d.Notes, d.Tags = "changed elsewhere", []string{"hall"}
		e.inv.devs[1] = d
	}
	resp, body := e.do(t, "POST", "/devices/1", "admin", url.Values{"csrf": {"tok"}, "name": {"kitchen"}, "address": {"10.0.0.1"}, "notes": {"typed by the admin"}, "tags": {"kitchen, light"}})
	if resp.StatusCode != 303 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if d := e.inv.devs[1]; d.Notes != "typed by the admin" || strings.Join(d.Tags, ",") != "kitchen,light" {
		t.Fatalf("what the admin typed must win: %+v", d)
	}
}

// A device that keeps changing under the save ends in an answer, not in a loop or a silent
// overwrite: nothing is written and the person is told to save again.
func TestEditGivesUpWhenTheDeviceKeepsChanging(t *testing.T) {
	e := setup(t)
	n := 0
	e.inv.beforeUpdateIf = func() {
		n++
		d := e.inv.devs[1]
		d.Notes = fmt.Sprintf("background write %d", n)
		e.inv.devs[1] = d
	}
	resp, body := e.do(t, "POST", "/devices/1", "admin", url.Values{"csrf": {"tok"}, "name": {"kitchen"}, "address": {"10.0.0.1"}, "notes": {"typed by the admin"}})
	if resp.StatusCode != 422 || !strings.Contains(body, "changed while you were saving") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "inventory:") {
		t.Fatalf("the Go error reached the page: %s", body)
	}
	if n != editTries || e.inv.devs[1].Notes == "typed by the admin" {
		t.Fatalf("%d tries (want %d), stored notes %q", n, editTries, e.inv.devs[1].Notes)
	}
}
