package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

// webhookRequest is what a webhook format puts on the wire: headers (in the order the preview
// shows them) and the body. The URL, User-Agent and the secret header are the sender's.
type webhookRequest struct {
	headers [][2]string
	body    []byte
}

// buildWebhook words ev for the format of c. An unknown format is an error, not a guess: the
// message would reach a receiver in a shape it does not read.
func buildWebhook(c config.Config, ev Event) (webhookRequest, error) {
	switch c.NotifyWebhookFormat {
	case config.NotifyFormatJSON, config.NotifyFormatHomeAssistant, "":
		return webhookRequest{headers: [][2]string{{"Content-Type", "application/json"}}, body: []byte(WebhookJSON(ev, false))}, nil
	case config.NotifyFormatNtfy:
		return ntfyRequest(ev), nil
	case config.NotifyFormatGotify:
		return gotifyRequest(ev), nil
	}
	return webhookRequest{}, fmt.Errorf("the webhook format %q is not known", c.NotifyWebhookFormat)
}

// problem says a result deserves attention (failed or partial).
func problem(ev Event) bool { return ev.Result == ResultFailed || ev.Result == ResultPartial }

// ntfyRequest is the publish request of ntfy (docs.ntfy.sh/publish): the text is the body, the
// headline, link and priority are headers. The priority goes as X-Priority, which ntfy reads first: a proxy may add
// an RFC 9218 "Priority: u=..." header of its own, which ntfy would otherwise confuse with its own. Click only with a link, which needs PUBLIC_URL.
func ntfyRequest(ev Event) webhookRequest {
	prio := "3"
	if problem(ev) {
		prio = "4"
	}
	h := [][2]string{{"Content-Type", "text/plain; charset=utf-8"}, {"Title", headerText(ev.Title)}}
	if ev.URL != "" {
		h = append(h, [2]string{"Click", ev.URL})
	}
	h = append(h, [2]string{"X-Priority", prio})
	return webhookRequest{headers: h, body: []byte(ev.Text)}
}

// headerText makes s fit a header value: one line, and RFC 2047 encoded when it is not plain
// ASCII (ntfy decodes encoded words). A line break would make the HTTP client refuse the whole
// request, so it becomes a space.
func headerText(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }), " ")
	return mime.BEncoding.Encode("UTF-8", s)
}

type gotifyBody struct {
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Priority int            `json:"priority"`
	Extras   map[string]any `json:"extras,omitempty"`
}

// gotifyRequest is the JSON of Gotify's POST /message. The click extra makes the notification
// open the TasmoFleet page (only with a link, which needs PUBLIC_URL).
func gotifyRequest(ev Event) webhookRequest {
	b := gotifyBody{Title: ev.Title, Message: ev.Text, Priority: 5}
	if problem(ev) {
		b.Priority = 8
	}
	if ev.URL != "" {
		b.Extras = map[string]any{"client::notification": map[string]any{"click": map[string]string{"url": ev.URL}}}
	}
	out, _ := json.Marshal(b) // plain strings cannot fail to marshal
	return webhookRequest{headers: [][2]string{{"Content-Type", "application/json"}}, body: out}
}

// WebhookPreview shows ev as the webhook of c sends it, for the settings page: the request line,
// the headers except the secret one, and the body.
func WebhookPreview(c config.Config, ev Event) string {
	wr, err := buildWebhook(c, ev)
	if err != nil {
		return err.Error()
	}
	var sb strings.Builder
	sb.WriteString("POST <webhook URL>\n")
	for _, h := range wr.headers {
		sb.WriteString(h[0] + ": " + h[1] + "\n")
	}
	sb.WriteString("\n")
	body := string(wr.body)
	if strings.HasPrefix(wr.headers[0][1], "application/json") {
		var ind bytes.Buffer
		if json.Indent(&ind, wr.body, "", "  ") == nil {
			body = ind.String()
		}
	}
	sb.WriteString(body)
	return sb.String()
}
