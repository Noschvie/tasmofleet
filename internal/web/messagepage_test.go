package web

import (
	"context"
	"strings"
	"testing"
)

// Why: a notice page is the only answer to an action that was refused or did nothing. It must
// name every device that was left out with its reason (otherwise the user cannot tell which
// device to look at), escape device names, and always lead on: a started transaction is the
// primary way on, everything else returns to the dashboard.
func TestNoticePageNamesDevicesLeftOutAndLeadsOn(t *testing.T) {
	render := func(m messageData) string {
		var b strings.Builder
		if err := noticePage("/tf", "Nothing backed up", m).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	body := render(messageData{Text: "No backup was started.", Left: []leftItem{{Name: "<lamp>", Reason: "offline"}}})
	for _, want := range []string{"<h1>Nothing backed up</h1>", "<li><strong>&lt;lamp&gt;</strong>: offline</li>", `<a class="btn btn-primary" href="/tf/">Go to the dashboard</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	body = render(messageData{Text: "was started.", LinkLead: true, Link: "/transactions/7", LinkText: "Transaction #7"})
	if !strings.Contains(body, `<a href="/tf/transactions/7">Transaction #7</a> was started.`) ||
		!strings.Contains(body, `<a class="btn btn-primary" href="/tf/transactions/7">Open the transaction</a><a class="btn" href="/tf/">Go to the dashboard</a>`) {
		t.Errorf("a started transaction is not the primary way on: %s", body)
	}
}
