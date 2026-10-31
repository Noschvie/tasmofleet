package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

// sendTimeout bounds one attempt, connection and answer together.
const sendTimeout = 10 * time.Second

// webhookBody is the JSON the webhook gets. Home Assistant gets the same body: its webhook
// trigger hands it to the automation as trigger.json.
type webhookBody struct {
	Event  string `json:"event"`
	Result string `json:"result"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	URL    string `json:"url"`
	Time   string `json:"time"`
}

// WebhookJSON is the body of ev, indented for the preview on the settings page.
func WebhookJSON(ev Event, indent bool) string {
	b := webhookBody{Event: ev.Name, Result: ev.Result, Title: ev.Title, Text: ev.Text, URL: ev.URL, Time: ev.Time.Format(time.RFC3339)}
	var out []byte
	if indent {
		out, _ = json.MarshalIndent(b, "", "  ") // plain strings cannot fail to marshal
	} else {
		out, _ = json.Marshal(b)
	}
	return string(out)
}

// newWebhookClient makes the client of the webhook sender: no proxy from the environment (the
// address is the one the person typed), ten seconds per attempt, and a redirect only to the same
// host, so a secret header never follows a redirect to somebody else.
func newWebhookClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:   sendTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return errRedirectElsewhere
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errRedirectElsewhere // a secret header must not fall back to clear text
			}
			return nil
		},
	}
}

var errRedirectElsewhere = errors.New("redirected to another host or to plain http (refused)")

// sendWebhook POSTs ev once. It returns a short detail that is safe to show: "HTTP 200" on
// success, the reason on failure. retry says a second attempt may help (a network error or a
// server error), not a refusal such as 404.
func sendWebhook(ctx context.Context, cl *http.Client, c config.Config, ev Event) (detail string, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.NotifyWebhookURL, bytes.NewReader([]byte(WebhookJSON(ev, false))))
	if err != nil {
		return "", false, errors.New("the webhook address is not valid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TasmoFleet")
	if c.NotifyWebhookAuth == config.NotifyAuthHeader && c.NotifyWebhookSecret != "" {
		req.Header.Set(c.NotifyWebhookHeader, c.NotifyWebhookSecret)
	}
	resp, err := cl.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectElsewhere) {
			return "", false, errRedirectElsewhere
		}
		return "", true, errors.New(scrubNet(err, c.NotifyWebhookSecret))
	}
	defer func() { _ = resp.Body.Close() }() // the status is all that is read
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return fmt.Sprintf("HTTP %d", code), false, nil
	case code >= 300 && code < 400:
		return "", false, fmt.Errorf("HTTP %d (a redirect that was not followed)", code)
	default:
		return "", code >= 500 || code == http.StatusTooManyRequests, fmt.Errorf("HTTP %d", code)
	}
}

// scrubNet words a transport error without the address (a webhook address can be a secret by
// itself, like a Home Assistant webhook id) and without the secret.
func scrubNet(err error, secret string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timed out after 10 s"
	}
	s := err.Error()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}
