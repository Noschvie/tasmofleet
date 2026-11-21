package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#716): "Discovered device #5" says nothing once the proposal is gone. An entry for ignoring or
// restoring a discovered device names the device on the audit page (target column, which stays visible
// on a phone, name first because the cell cuts its end), and older entries with an empty detail keep rendering as before. The name is text a
// device announced on the network: it must come out escaped.
func TestAuditNamesTheIgnoredDiscoveredDevice(t *testing.T) {
	e := setup(t)
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for i, ev := range []model.AuditEvent{
		{Action: "discovery.dismiss", Target: "proposal:5", Detail: `{"kind":"new","name":"<b>Lamp</b>","mac":"aa:bb:cc:dd:ee:01","address":"172.31.13.9"}`},
		{Action: "discovery.undismiss", Target: "proposal:5", Detail: `{"kind":"new","name":"<b>Lamp</b>","mac":"aa:bb:cc:dd:ee:01","address":"172.31.13.9"}`},
		{Action: "discovery.dismiss", Target: "proposal:6", Detail: `{}`}, // written before the detail existed
		{Action: "discovery.dismiss", Target: "proposal:7", Detail: ``},
		{Action: "discovery.dismiss", Target: "proposal:8", Detail: `{"name":"` + strings.Repeat("x", 100) + `"}`},
	} {
		ev.ID, ev.At, ev.Actor, ev.Result = int64(i+1), at.Add(-time.Duration(i)*time.Minute), "admin", "ok"
		e.aud.events = append(e.aud.events, ev)
	}
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	// the Details buttons repeat the target in a screen-reader-only name (#1074); the target cell is what is counted
	visible := regexp.MustCompile(`<span class="sr-only">.*?</span>`).ReplaceAllString(body, "")
	if n := strings.Count(visible, "&lt;b&gt;Lamp&lt;/b&gt; (#5)"); n != 2 {
		t.Fatalf("the escaped name is in the target cell %d times, want 2 (dismiss and undismiss)", n)
	}
	if strings.Contains(body, "<b>Lamp</b>") {
		t.Fatal("the announced name reached the page unescaped")
	}
	// A name is up to 100 characters: it must not widen the Target column; the full text stays in the details.
	if !strings.Contains(body, strings.Repeat("x", 29)+"… (#8)") {
		t.Error("a long name is not cut to one short line in the target cell")
	}
	for _, old := range []string{"Discovered device #6", "Discovered device #7"} {
		if !strings.Contains(body, old+"</td>") && !strings.Contains(body, old+"\n") {
			t.Errorf("an entry without a name must still read %q", old)
		}
		if strings.Contains(body, old+" (") {
			t.Errorf("%q was given an empty name", old)
		}
	}
}
