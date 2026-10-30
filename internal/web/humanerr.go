package web

import (
	"errors"
	"unicode"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// deviceProblem is the sentence for a failed call to a device. Go error chains
// ("backup: download config: tasmota: ... HTTP 404") are for the log, never for
// the browser. ok is false when the error is not a known device problem; the
// caller then shows its own generic sentence and logs err.
func deviceProblem(err error) (msg string, ok bool) {
	var ur interface{ UserReason() string } // a reason the transport wrote (a backup over MQTT that was not possible)
	switch {
	case errors.As(err, &ur):
		return capitalise(ur.UserReason()) + ".", true
	case errors.Is(err, tasmota.ErrAuth):
		return "The device asked for a web password. Check the web user and password on the Edit page.", true
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		return "The device refuses HTTP API commands from this address. Set SetOption128 1 on the device, or use its own IP address or hostname.", true
	case errors.Is(err, tasmota.ErrUnreachable):
		return "The device did not answer. Check that it is on and on the network.", true
	case errors.Is(err, tasmota.ErrTimeout):
		return "The device took too long to answer. Try again in a moment.", true
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return "The address of this device is outside the configured device networks.", true
	case errors.Is(err, tasmota.ErrBadResponse):
		return "The device sent an answer that could not be read.", true
	case errors.Is(err, backup.ErrDecoderOutdated):
		return "The backup was stored, but the decoder is older than the device firmware, so it could not be decoded.", true
	}
	return "", false
}

// backupProblem is the sentence under a failed manual backup.
func (h *handler) backupProblem(err error) string {
	if msg, ok := deviceProblem(err); ok {
		return "Backup failed. " + msg
	}
	h.log.Warn("manual backup failed", "err", err)
	return "Backup failed. The device did not hand over its configuration. The server log has the details."
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
