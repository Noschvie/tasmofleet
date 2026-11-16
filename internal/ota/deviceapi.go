package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
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

// firmwareInputs collects the answers Detect needs. Full firmware is named by Status 2 (and Status 4,
// which ProbeIdentity adds for the language, #686): no main page. Minimal firmware (ESP8266) and safeboot
// (ESP32) answer Status 2 with Unknown; then the bare Upgrade probe gives the version and intermediateRoot
// the image, when anything can name it. jobFamily is the chip family the update job read at its preflight
// (ChipUnknown outside a job and at the preflight itself).
func (a *deviceAPI) firmwareInputs(ctx context.Context, t tasmota.Target, jobFamily model.ChipFamily) (fwselect.Inputs, bool, error) {
	in := fwselect.Inputs{JobFamily: jobFamily}
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
	in.Status2 = raw
	if !unknown {
		return in, false, nil
	}
	if len(raw) == 0 {
		in.Status2 = unknownStatus2
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
	if in.Root, err = a.intermediateRoot(ctx, t, jobFamily); err != nil {
		return in, true, err
	}
	return in, true, nil
}

// info1Reader is the router's INFO1-only read (transport.Router.Info1Root): the image of minimal or
// safeboot firmware from the INFO1 it published since its last boot, without a request to the device.
type info1Reader interface {
	Info1Root(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error)
}

// intermediateRoot names the image of firmware that answered Status 2 with Unknown (#686).
//
// Inside an update job (jobFamily known) pinned to MQTT the device's INFO1 since its last boot names the image,
// and Detect checks that image against the job's family; the main page is not read then. A job pinned to HTTP
// never takes the INFO1: it comes from the topic, while the job's Status 2, Upgrade and upload go to the
// address, and a check counts only for the transport it was read over (#810), so another chip at the address
// (an IP swap) would pass on the INFO1 of the job's device. Such a job, and an MQTT job without a usable INFO1,
// reads the main page over its pin (D5) as the independent proof of the image (#918): Guard checks the job's
// preflight identity, not this one, so only this image can catch another chip that answers after a restart.
// Outside a job (the preview, a retry's look, the preflight of a device found on minimal) the router's
// RootPage reads INFO1 first and the main page only without one (the fallback, path 3).
func (a *deviceAPI) intermediateRoot(ctx context.Context, t tasmota.Target, jobFamily model.ChipFamily) (*fwselect.RootInfo, error) {
	var ri tasmota.RootInfo
	err := transport.ErrNoInfo
	if pin, _ := transport.PathFrom(ctx); pin == transport.MQTT && jobFamily != model.ChipUnknown {
		if r, ok := a.c.(info1Reader); ok {
			ri, err = r.Info1Root(ctx, t)
		}
	}
	if errors.Is(err, transport.ErrNoInfo) || errors.Is(err, tasmota.ErrUnreachable) {
		ri, err = a.c.RootPage(ctx, t)
	}
	if err != nil {
		return nil, fmt.Errorf("root page: %w", err)
	}
	return &fwselect.RootInfo{Version: ri.Version, Image: ri.Image, Lang: ri.Lang, LangKnown: ri.LangKnown, Minimal: ri.Minimal}, nil
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

// ProbeIdentity runs Status 2 (Upgrade probe and the image source on minimal, see firmwareInputs),
// Status 4 and Status 11 (normal firmware only) and OtaUrl.
func (a *deviceAPI) ProbeIdentity(ctx context.Context, d model.Device, jobFamily model.ChipFamily) (model.Identity, string, int64, error) {
	t := tasmota.TargetFor(d)
	in, minimal, err := a.firmwareInputs(ctx, t, jobFamily)
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
func (a *deviceAPI) Probe(ctx context.Context, d model.Device, jobFamily model.ChipFamily) (model.Identity, int64, error) {
	t := tasmota.TargetFor(d)
	in, minimal, err := a.firmwareInputs(ctx, t, jobFamily)
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
