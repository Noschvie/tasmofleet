package transport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/store"
)

// MQTTPasswordTester is what the command channel offers for the password test (mqttcmd.Channel.TestPassword).
// The channel is asserted to it, so a fake MQTTDoer without it simply has no test.
type MQTTPasswordTester interface {
	TestPassword(ctx context.Context, d model.Device, password string) error
}

// PasswordOKMarker records a password the device accepted in a test (store.MarkMQTTPasswordOK). Options.Marker
// is asserted to it; without it an accepted test is not recorded.
type PasswordOKMarker interface {
	MarkMQTTPasswordOK(ctx context.Context, id model.DeviceID, tested, at time.Time) error
}

// PasswordTestBlockedError is a password test that sent nothing: Reason is a sentence for the person, written
// by this package and free of any secret.
type PasswordTestBlockedError struct{ Reason string }

func (e *PasswordTestBlockedError) Error() string {
	return "transport: the MQTT password test is not possible: " + e.Reason
}

// TestMQTTPassword asks the device whether it accepts the MqttPassword TasmoFleet holds for it (#609), over MQTT
// only: HTTP cannot check it (Status 6 and the web UI mask it). The test starts a FileDownload, so it needs the
// strict rule of transfers; it ignores the refusal mark on purpose, since checking a refused password again is
// what it is for. A refusal ("Error 1") sets the mark, an acceptance records the test and clears the mark.
// nil: accepted; mqttcmd.ErrPasswordRefused: refused; *PasswordTestBlockedError: nothing was sent; anything else
// says nothing about the password. Nothing returned or logged holds the password.
func (r *Router) TestMQTTPassword(ctx context.Context, id model.DeviceID) error {
	m := r.mqttDoer()
	pt, ok := m.(MQTTPasswordTester)
	if m == nil || !ok {
		return &PasswordTestBlockedError{Reason: "TasmoFleet is not connected to its MQTT broker"}
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return err
	}
	if why := passwordTestBlocker(f); why != "" {
		return &PasswordTestBlockedError{Reason: why}
	}
	pw, ok := model.FilePassword(d, r.brokerPassword())
	if !ok {
		return &PasswordTestBlockedError{Reason: "the device has no usable MQTT password"}
	}
	err = pt.TestPassword(ctx, d, pw)
	wctx := context.WithoutCancel(ctx) // the caller may be cancelled by now; the result must still be written
	switch {
	case err == nil:
		if okm, isOK := r.o.Marker.(PasswordOKMarker); isOK {
			if merr := okm.MarkMQTTPasswordOK(wctx, d.ID, d.MQTTPasswordSetAt, time.Now()); merr != nil && !errors.Is(merr, store.ErrNotFound) {
				r.o.Log.Warn("transport: could not record the accepted MQTT password", "device", int64(d.ID), "err", merr)
			}
		}
	case errors.Is(err, mqttcmd.ErrPasswordRefused):
		r.markPasswordRefused(wctx, d)
	case errors.Is(err, mqttcmd.ErrRefused): // nothing was sent: say why, in our own words
		return &PasswordTestBlockedError{Reason: fmt.Sprintf("the test was refused: %s", mqttcmd.Reason(err))}
	}
	return err
}
