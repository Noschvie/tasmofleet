package web

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// Why (#1005): a device listens on all four of its group topics, so the person must see all of them where the list
// shows one today: the first in the Group topic cell with "+N" for the others (which stays visible when a long first
// one is cut off) and all of them in the title, all of them on the Identity card, and the list's filter finds a device
// by any of them, to answer "which devices does a message to this group reach".
func TestGroupTopics2To4AreShownAndFound(t *testing.T) {
	e := setup(t)
	d := e.inv.devs[1]
	at := time.Date(2026, 11, 19, 8, 15, 0, 0, time.UTC)
	d.GroupTopic, d.GroupTopicReadAt = "tasmotas", at
	d.MoreGroupTopics, d.MoreGroupTopicsReadAt = []string{"upstairs", "ground-floor"}, at
	e.inv.devs[1] = d

	_, body := e.do(t, "GET", "/devices", "operator", nil)
	var cell string
	for _, part := range strings.Split(body, `data-label="Group topic"`)[1:] {
		if c, _, _ := strings.Cut(part, "</td>"); strings.Contains(c, "upstairs") {
			cell = c
		}
	}
	if !strings.Contains(cell, `title="tasmotas, upstairs, ground-floor"`) || !strings.Contains(cell, `<span class="dv-trunc">tasmotas</span>`) ||
		!strings.Contains(cell, `<span class="dv-gtmore">+2</span>`) {
		t.Errorf("Group topic cell, want the first, +2 and all three in the title:\n%s", cell)
	}

	for _, q := range []string{"ground-floor", "UPSTAIRS"} {
		_, body = e.do(t, "GET", "/devices?q="+url.QueryEscape(q), "operator", nil)
		if !strings.Contains(body, `/devices/1"`) {
			t.Errorf("filter %q does not find the device that listens on it", q)
		}
	}
	_, body = e.do(t, "GET", "/devices?q=cellar", "operator", nil)
	if strings.Contains(body, `/devices/1"`) {
		t.Error("filter cellar finds a device that has no such group topic")
	}

	_, body = e.do(t, "GET", "/devices/1", "operator", nil)
	card := body[strings.Index(body, `id="identity"`):]
	card = strings.ReplaceAll(card[:strings.Index(card, "</section>")], "<wbr>", "")
	if !strings.Contains(card, "<dt>Group topics") || !strings.Contains(card, "tasmotas, upstairs, ground-floor") {
		t.Errorf("Identity card lacks the group topics:\n%s", shortDump(card))
	}
}
