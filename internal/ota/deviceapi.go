package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// commandGap is the pause between two commands to the same device. ESP8266
// web servers with little heap corrupt replies under bursts (the reason
// Status 0 is never sent).
const commandGap = 200 * time.Millisecond

// NewDeviceAPI returns the production DeviceAPI over a tasmota.Client. It
// sends individual Status N queries one at a time, never Status 0.
func NewDeviceAPI(c tasmota.Client) DeviceAPI {
	return &deviceAPI{c: c, gap: commandGap, detect: fwselect.Detect}
}

type deviceAPI struct {
	c      tasmota.Client
	gap    time.Duration
	detect func(fwselect.Inputs) (model.Identity, error)
}

// cmd sends one command after the inter-command gap.
func (a *deviceAPI) cmd(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if a.gap > 0 {
		timer := time.NewTimer(a.gap)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return a.c.Command(ctx, t, c)
}

var unknownStatus2 = json.RawMessage(`{"Command":"Unknown","Input":"STATUS 2"}`)

// firmwareInputs collects the answers Detect needs. Minimal firmware
// (ESP8266) and safeboot (ESP32) answer Status 2 with Unknown; then the bare
// Upgrade probe gives the version and the root page the image and
// language. withRoot fetches the root page for normal firmware too (the
// language build is only visible there).
func (a *deviceAPI) firmwareInputs(ctx context.Context, t tasmota.Target, withRoot bool) (fwselect.Inputs, bool, error) {
	var in fwselect.Inputs
	raw, err := a.cmd(ctx, t, "Status 2")
	unknown := errors.Is(err, tasmota.ErrUnknownCommand)
	if err != nil && !unknown {
		return in, false, fmt.Errorf("status 2: %w", err)
	}
	if err == nil {
		if _, perr := tasmota.ParseStatus2(raw); errors.Is(perr, tasmota.ErrUnknownCommand) {
			unknown = true
		} else if perr != nil {
			return in, false, fmt.Errorf("status 2: %w", perr)
		}
	}
	if unknown {
		if len(raw) == 0 {
			raw = unknownStatus2
		}
		up, err := a.cmd(ctx, t, "Upgrade")
		if err != nil {
			return in, true, fmt.Errorf("upgrade probe: %w", err)
		}
		p, err := tasmota.ParseUpgradeProbe(up)
		if err != nil {
			return in, true, fmt.Errorf("upgrade probe: %w", err)
		}
		in.Upgrade = &fwselect.UpgradeProbe{Version: p.Version}
	}
	in.Status2 = raw
	if unknown || withRoot {
		ri, err := a.c.RootPage(ctx, t)
		if err != nil && !unknown && errors.Is(err, transport.ErrNoInfo) {
			// Over MQTT the page is built from INFO1, which a device sends once per boot. Firmware that
			// answered Status 2 needs the page only for its language, which stays unknown (D10); minimal
			// firmware needs it for its image, so there the missing INFO1 stops the probe.
			return in, unknown, nil
		}
		if err != nil {
			return in, unknown, fmt.Errorf("root page: %w", err)
		}
		in.Root = &fwselect.RootInfo{Version: ri.Version, Image: ri.Image, Lang: ri.Lang, LangKnown: ri.LangKnown, Minimal: ri.Minimal}
	}
	return in, unknown, nil
}

func (a *deviceAPI) uptime(ctx context.Context, t tasmota.Target) (int64, error) {
	raw, err := a.cmd(ctx, t, "Status 11")
	if err != nil {
		return 0, fmt.Errorf("status 11: %w", err)
	}
	sts, err := tasmota.ParseStatus11(raw)
	if err != nil {
		return 0, fmt.Errorf("status 11: %w", err)
	}
	return sts.UptimeSec, nil
}

// ProbeIdentity runs Status 2 (Upgrade probe + root page on minimal),
// Status 4 and Status 11 (normal firmware only), the root page and OtaUrl.
func (a *deviceAPI) ProbeIdentity(ctx context.Context, d model.Device) (model.Identity, string, int64, error) {
	t := tasmota.TargetFor(d)
	in, minimal, err := a.firmwareInputs(ctx, t, true)
	if err != nil {
		return model.Identity{}, "", 0, err
	}
	var up int64
	if !minimal {
		raw, err := a.cmd(ctx, t, "Status 4")
		if err != nil {
			return model.Identity{}, "", 0, fmt.Errorf("status 4: %w", err)
		}
		in.Status4 = raw
		if up, err = a.uptime(ctx, t); err != nil {
			return model.Identity{}, "", 0, err
		}
	}
	raw, err := a.cmd(ctx, t, "OtaUrl")
	if err != nil {
		return model.Identity{}, "", 0, fmt.Errorf("otaurl: %w", err)
	}
	otaURL, err := tasmota.ParseOtaURL(raw)
	if err != nil {
		return model.Identity{}, "", 0, fmt.Errorf("otaurl: %w", err)
	}
	id, err := a.detect(in)
	if err != nil {
		return model.Identity{}, "", 0, fmt.Errorf("detect: %w", err)
	}
	if minimal {
		// Status 4 does not exist on minimal: keep the last known flash data.
		id.FlashSizeKB, id.ProgramFlashSizeKB, id.FreeKB = d.Identity.FlashSizeKB, d.Identity.ProgramFlashSizeKB, d.Identity.FreeKB
	}
	return id, otaURL, up, nil
}

// Probe is the liveness check while waiting: Status 2 (with the minimal
// fallback) and Status 11. Any answer means online; an identity Detect
// cannot build is returned empty, which the machine never reads as change.
func (a *deviceAPI) Probe(ctx context.Context, d model.Device) (model.Identity, int64, error) {
	t := tasmota.TargetFor(d)
	in, minimal, err := a.firmwareInputs(ctx, t, false)
	if err != nil {
		return model.Identity{}, 0, err
	}
	var up int64
	if !minimal {
		if up, err = a.uptime(ctx, t); err != nil {
			return model.Identity{}, 0, err
		}
	}
	id, err := a.detect(in)
	if err != nil {
		return model.Identity{}, up, nil //nolint:nilerr // online is what counts; identity unknown
	}
	return id, up, nil
}

// SetOtaURL sets OtaUrl and checks that the device stored exactly that URL.
func (a *deviceAPI) SetOtaURL(ctx context.Context, d model.Device, u string) error {
	raw, err := a.cmd(ctx, tasmota.TargetFor(d), "OtaUrl "+u)
	if err != nil {
		return fmt.Errorf("otaurl: %w", err)
	}
	got, err := tasmota.ParseOtaURL(raw)
	if err != nil {
		return fmt.Errorf("otaurl: %w", err)
	}
	if got != u {
		return fmt.Errorf("otaurl: device stored %q, sent %q", got, u)
	}
	return nil
}

var _ FirmwareUploader = (*deviceAPI)(nil)

// UploadFirmware pushes an image to the device's /u2 (see push.go). The
// device client must be the production tasmota client.
func (a *deviceAPI) UploadFirmware(ctx context.Context, d model.Device, filename string, size int64, body io.Reader) error {
	u, ok := a.c.(tasmota.Uploader)
	if !ok {
		return errors.New("upload: this device client cannot upload")
	}
	if err := u.UploadFirmware(ctx, tasmota.TargetFor(d), filename, size, body); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

var upgradeEcho = regexp.MustCompile(`^Version \S+ from (\S+)$`)

// Upgrade sends Upgrade 1 and returns the URL the device says it will
// download. A reply without "Version X from <url>" means it did not start.
func (a *deviceAPI) Upgrade(ctx context.Context, d model.Device) (string, error) {
	raw, err := a.cmd(ctx, tasmota.TargetFor(d), "Upgrade 1")
	if err != nil {
		return "", fmt.Errorf("upgrade: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("upgrade: %w", tasmota.ErrBadResponse)
	}
	s, _ := m["Upgrade"].(string)
	g := upgradeEcho.FindStringSubmatch(s)
	if g == nil {
		if len(s) > 80 {
			s = s[:80]
		}
		return "", fmt.Errorf("upgrade not started, device replied %q", s)
	}
	return g[1], nil
}
