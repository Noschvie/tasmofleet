package settings

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Why (#818): the log line, the sentence on the Settings tab and the reason the background refresh keeps are
// one class. A device that went offline before it answered is also a timeout (the outcome is unknown), and
// must still read as the more specific class, or the page could not tell it from a device that stayed silent.
func TestClassifyReadNamesTheClassNotTheText(t *testing.T) {
	offline := fmt.Errorf("mqtt: %w (x): %w", mqttcmd.ErrWentOffline, tasmota.ErrTimeout)
	for _, tc := range []struct {
		err  error
		want FailReason
	}{
		{fmt.Errorf("read: %w", ErrNoMAC), FailNoMAC},
		{tasmota.ErrAuth, FailAuth},
		{tasmota.ErrHTTPAPIRefused, FailAPIRefused},
		{tasmota.ErrUnreachable, FailUnreachable},
		{offline, FailWentOffline},
		{tasmota.ErrTimeout, FailTimeout},
		{context.DeadlineExceeded, FailTimeout},
		{tasmota.ErrBadResponse, FailBadResponse},
		{errors.New("policy values not read"), FailOther},
	} {
		if got := ClassifyRead(tc.err); got != tc.want {
			t.Errorf("ClassifyRead(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
