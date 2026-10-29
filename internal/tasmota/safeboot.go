package tasmota

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Reading the upload layout and switching the boot partition are the two
// device actions behind "push into safeboot" (#30). Both are documented in
// docs/research/tasmota-minimal-and-ota.md ("Telling the layouts apart").

// UpLayout says what the /up page of a device offers for a file upload.
type UpLayout string

// Layouts.
const (
	// UpLayoutDirect: the plain upload form (HTTP_FORM_RST_UPG). The device
	// takes an image into its running partition: an ESP8266, an ESP32 with two
	// app partitions, or an ESP32 that already runs safeboot.
	UpLayoutDirect UpLayout = "direct"
	// UpLayoutSafeboot: the form whose button calls upl(this)
	// (HTTP_FORM_RST_UPG_FCT). The normal firmware runs on a single-OTA
	// (safeboot) layout; an upload needs the restart into safeboot first.
	UpLayoutSafeboot UpLayout = "safeboot"
)

// uplCall is the one place the safeboot layout shows: the upload button's
// onclick. The script that defines the function says "upl(t)", and the plain
// form never calls it (Tasmota v16.0.0, xdrv_01_9_webserver.ino:391).
const uplCall = "upl(this)"

// ParseUpLayout reads the layout from the /up page. A page without the upload
// form at all is ErrUploadUnsupported.
func ParseUpLayout(page []byte) (UpLayout, error) {
	if !upUploadForm.Match(page) {
		return "", &deviceError{ErrUploadUnsupported, "the upgrade page has no file upload form"}
	}
	if strings.Contains(string(page), uplCall) {
		return UpLayoutSafeboot, nil
	}
	return UpLayoutDirect, nil
}

// BootTarget is the partition /u4 switches to.
type BootTarget string

// Boot targets.
const (
	BootSafeboot BootTarget = "fct" // restart into the safeboot (factory) partition
	BootApp      BootTarget = "ota" // restart back into the app partition
)

// BootReply is the plain-text answer of /u4 in api mode.
type BootReply string

// Replies.
const (
	// BootRestarting ("false"): the device is not on the target and restarts
	// into it now. The caller polls; it never sends the switch again.
	BootRestarting BootReply = "false"
	// BootAlready ("true"): the device is on a single-OTA layout and nothing
	// was done (fct while in safeboot, ota while in the app).
	BootAlready BootReply = "true"
	// BootNone ("none"): the device has no safeboot partition; nothing was done.
	BootNone BootReply = "none"
)

// ErrSwitchNotDone: the device answered, but with something other than the
// switch: nothing was changed (an error status, or a firmware without /u4).
// Every other error of SwitchBoot leaves open whether the device restarts.
var ErrSwitchNotDone = errors.New("tasmota: boot partition not switched")

// BootSwitcher is implemented by the production client next to Client, like
// Uploader.
type BootSwitcher interface {
	// ReadUpLayout is a GET of /up: read-only, it changes nothing.
	ReadUpLayout(ctx context.Context, t Target) (UpLayout, error)
	// SwitchBoot sends GET /u4?u4=<to>&api= exactly once. BootRestarting
	// means the device restarts. A transport error (timeout, lost reply) leaves
	// it open whether it does: the caller probes, never sends it again.
	// ErrSwitchNotDone and ErrAuth are certain: nothing changed.
	SwitchBoot(ctx context.Context, t Target, to BootTarget) (BootReply, error)
}

var _ BootSwitcher = (*httpClient)(nil)

func (c *httpClient) ReadUpLayout(ctx context.Context, t Target) (UpLayout, error) {
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return "", err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(rootTimeout))
	defer cancel()
	page, err := c.fetch(rctx, ctx, t, http.MethodGet, "/up", "", nil, "", 0)
	if err != nil {
		return "", err
	}
	return ParseUpLayout(page)
}

func (c *httpClient) SwitchBoot(ctx context.Context, t Target, to BootTarget) (BootReply, error) {
	if to != BootSafeboot && to != BootApp {
		return "", &deviceError{ErrBadResponse, "unknown boot target"}
	}
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return "", err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(rootTimeout))
	defer cancel()
	reply, err := c.fetch(rctx, ctx, t, http.MethodGet, "/u4", "u4="+url.QueryEscape(string(to))+"&api=", nil, "", 0)
	if err != nil {
		// An HTTP status other than 200 came from a device that did not act on
		// the request (fetch words those "HTTP <code>").
		var de *deviceError
		if errors.As(err, &de) && errors.Is(de.kind, ErrBadResponse) && strings.HasPrefix(de.msg, "HTTP ") {
			return "", &deviceError{ErrSwitchNotDone, de.msg}
		}
		return "", err
	}
	switch r := BootReply(strings.TrimSpace(string(reply))); r {
	case BootRestarting, BootAlready, BootNone:
		return r, nil
	}
	// Neither of the three words: the request may still have been acted on.
	return "", &deviceError{ErrBadResponse, "unexpected answer to the boot partition switch"}
}
