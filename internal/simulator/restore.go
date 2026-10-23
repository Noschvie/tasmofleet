package simulator

import (
	"io"
	"net/http"
	neturl "net/url"
	"strings"
)

// configDumpSize is the size of a Tasmota settings dump on ESP8266/ESP32
// (decode-config accepts 4096 bytes); anything else is rejected like the
// real firmware rejects a bad file.
const configDumpSize = 4096

// webGetConfigLocked implements "WebGetConfig <url>": the device downloads a
// settings dump from the URL, replaces its settings with it and restarts.
// The download runs in the background, like the real command, so the reply
// is only "accepted"; success shows as a reboot and a changed /dl. The
// device sends no credentials and follows no redirects. It is a stand-in for
// the one-time restore download (M2-5) and models what the real command is
// known for: it GETs the URL from the device's own address.
func (d *simDevice) webGetConfigLocked(data string) []byte {
	u, err := neturl.Parse(strings.TrimSpace(data))
	if data == "" || err != nil || u.Scheme != "http" || u.Host == "" {
		return errorReply("WEBGETCONFIG " + data)
	}
	raw := u.String()
	d.spawn(func() {
		d.fetchConfig(raw)
	})
	return []byte(`{"WebGetConfig":"Done"}`)
}

func (d *simDevice) fetchConfig(rawURL string) {
	req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, rawURL, nil) //nolint:gosec // G704: simulator device; its client dials loopback and AllowDownloadHosts only
	if err != nil {
		return
	}
	resp, err := d.f.client.Do(req) //nolint:gosec // G704: see above, loopback-only client
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }() // read-only response
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, configDumpSize+1))
	if err != nil || len(body) != configDumpSize {
		return
	}
	d.mu.Lock()
	d.configDump = body
	d.mu.Unlock()
	if !d.sleep(secRestartCmd) {
		return
	}
	d.reboot(false)
}
