package settings

import (
	"context"
	"errors"

	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// FailReason is why a read of a device's data failed, as one of a few fixed classes (#818). It is never the
// error text: that text can hold what the device answered (safety rule 4). The page words the class, the log
// prints it, and the background refresh keeps it: one mapping from error to class, so the three cannot drift.
type FailReason string

const (
	FailNoMAC       FailReason = "no-mac"           // minimal firmware: the device answers without a MAC
	FailAuth        FailReason = "auth"             // the stored credentials are refused
	FailAPIRefused  FailReason = "http-api-refused" // the HTTP API is off
	FailUnreachable FailReason = "unreachable"
	FailWentOffline FailReason = "went-offline" // LWT Offline came before the answer
	FailTimeout     FailReason = "timeout"      // no answer in time
	FailBadResponse FailReason = "bad-response"
	FailOtherDevice FailReason = "other-device" // another device answers at the address or on the topic (the caller decides this: it owns that error)
	FailOther       FailReason = "other"
)

// ClassifyRead maps the error of a read to its class. FailOtherDevice is not found here: the refusal of an
// identity check is an error of the package that makes the check, which maps it itself.
func ClassifyRead(err error) FailReason {
	switch {
	case errors.Is(err, ErrNoMAC):
		return FailNoMAC
	case errors.Is(err, tasmota.ErrAuth):
		return FailAuth
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		return FailAPIRefused
	case errors.Is(err, tasmota.ErrUnreachable):
		return FailUnreachable
	case errors.Is(err, mqttcmd.ErrWentOffline): // before ErrTimeout: it wraps it
		return FailWentOffline
	case errors.Is(err, tasmota.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return FailTimeout
	case errors.Is(err, tasmota.ErrBadResponse):
		return FailBadResponse
	}
	return FailOther
}
