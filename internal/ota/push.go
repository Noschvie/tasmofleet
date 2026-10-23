package ota

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Push delivery (backlog F-9): instead of OtaUrl and Upgrade 1 the engine
// uploads the image to the device's /u2 endpoint, like the web UI's "Upgrade
// by file upload". The state machine is unchanged: the set_ota_url step only
// runs Guard (and the second relay reading), and upgrade_issued uploads.
// Every rule of Upgrade 1 holds: Guard runs right before the upload, a lost
// reply is never answered with a second upload (the engine probes), and the
// device's OtaUrl is never touched, so there is nothing to restore.

// FirmwareUploader is implemented by a DeviceAPI that can push an image. It
// is optional so existing fakes stay valid; without it push jobs are refused.
type FirmwareUploader interface {
	// UploadFirmware sends size bytes of body as the file filename. A nil error
	// means the device accepted the image. Every other error leaves it open
	// whether the device flashes, except a refusal by the device (nothing
	// flashed); the caller treats all of them as "probe first".
	UploadFirmware(ctx context.Context, d model.Device, filename string, size int64, body io.Reader) error
}

// FirmwareFiles is implemented by the firmware cache next to Firmware: the
// bytes of a cached artifact, verified against its SHA-256.
type FirmwareFiles interface {
	Open(ctx context.Context, a model.Artifact) (io.ReadCloser, error)
}

// uploadTimeoutFactor scales the command timeout for the upload action: the
// device writes flash while it receives the image. It stays below the
// upgrade_issued deadline, so the wait for the reboot starts after the upload.
const uploadTimeoutFactor = 4

// Reasons for a refused push. ESP8266 stays refused until the upload was
// tested on real hardware (backlog F-9).
const (
	errPushESP8266  = "push upload is not available for ESP8266 yet; use the download delivery"
	errPushChip     = "push upload needs a device with a known ESP32 chip; use the download delivery"
	errPushSafeboot = "push upload needs a device running a normal firmware, not safeboot or minimal; use the download delivery"
	errPushStaged   = "push upload cannot do a two-step update; use the download delivery"
)

// pushDeviceRefusal says why a device cannot take a push upload, or nil. It
// is decided on the detected identity, before any image is fetched.
func pushDeviceRefusal(id model.Identity) error {
	switch {
	case id.Family == model.ChipESP8266:
		return errors.New(errPushESP8266)
	case !id.Family.IsESP32():
		return errors.New(errPushChip)
	case isIntermediate(id.Variant):
		return errors.New(errPushSafeboot)
	}
	return nil
}

// pushPlanRefusal: a staged plan means a minimal hop, which the upload does not drive.
func pushPlanRefusal(p model.Plan) error {
	if p != model.PlanDirect {
		return errors.New(errPushStaged)
	}
	return nil
}

// pushCheck verifies, before the job commits to the upload, that the image can
// be read and still matches the cache: a damaged file is found while the
// job can still fail cleanly.
func (e *Engine) pushCheck(ctx context.Context, a model.Artifact) error {
	rc, err := e.openImage(ctx, a)
	if err != nil {
		return err
	}
	return rc.Close()
}

func (e *Engine) openImage(ctx context.Context, a model.Artifact) (io.ReadCloser, error) {
	files, ok := e.fw.(FirmwareFiles)
	if !ok {
		return nil, refused(errors.New("push upload is not available: the firmware cache cannot hand out files"))
	}
	if _, ok := e.api.(FirmwareUploader); !ok {
		return nil, refused(errors.New("push upload is not available: the device API cannot upload"))
	}
	rc, err := files.Open(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("firmware %s: %w", a.Filename, err)
	}
	return rc, nil
}

// push uploads the artifact. Guard ran in stageArtifact right before.
func (e *Engine) push(ctx context.Context, d model.Device, a model.Artifact) Event {
	rc, err := e.openImage(ctx, a)
	if err != nil {
		return failEvent(err)
	}
	defer rc.Close() //nolint:errcheck // read-only file
	if err := e.api.(FirmwareUploader).UploadFirmware(ctx, d, a.Filename, a.Size, rc); err != nil {
		return Event{Kind: EvCommandFailed, Err: err.Error()}
	}
	return Event{Kind: EvCommandOK}
}
