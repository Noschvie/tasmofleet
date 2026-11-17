package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
)

func (h *handler) registerPasswordTest(route routeFunc, op func(http.Handler) http.Handler) {
	if h.d.PasswordTest != nil {
		route("POST /devices/{id}"+pwTestPath, op, h.passwordTest)
	}
}

// pwViewOf is what the device page's password row needs besides the facts: the switch, the reason a device has
// none from a backup (asked only for a device that has none), and which buttons the viewer gets.
func (h *handler) pwViewOf(ctx context.Context, v view, d model.Device, f transport.Facts, paths transport.Paths) pwView {
	pw := pwView{LearnOff: !h.learnPasswordsOn(), CanTest: h.d.PasswordTest != nil, CanChange: v.IsAdmin && h.d.MQTTPasswords != nil,
		CanBroker: v.IsAdmin && h.d.PasswordFleet != nil && h.brokerHasPassword()}
	if h.brokerACLOpen() && paths.Backup != transport.MQTT {
		pw.TestACL = aclTextPasswordTest // a backup over MQTT is covered by the header's notice (T18)
	}
	if h.d.PasswordFleet != nil && mqttPasswordState(pwInputOf(d, f), !pw.LearnOff).Kind == pwNotKnown {
		pw.Gap = h.d.PasswordFleet.Gaps(ctx, []model.DeviceID{d.ID})[d.ID]
	}
	return pw
}

// passwordTest runs the password test of one device (#609): FileDownload with the stored password, over MQTT only,
// stopped at once (transport.Router.TestMQTTPassword). It runs inside the request, bounded: a test queued behind
// a transfer that holds the device's topic gives up after transport.PasswordTestBound, and nothing was sent then.
// Operators may press it, like Back up: it changes nothing on the device. The result is an audit entry (accepted,
// refused, blocked or error, never the password) and a flash; a test that could not run says why on its own page.
func (h *handler) passwordTest(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	ctx, cancel := context.WithTimeout(r.Context(), transport.PasswordTestBound)
	defer cancel()
	err := h.d.PasswordTest.TestMQTTPassword(ctx, d.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	outcome := transport.PasswordTestOutcome(err)
	if h.d.Recorder != nil {
		detail, jerr := json.Marshal(map[string]string{"result": outcome, "run": "device page"})
		if jerr != nil {
			detail = []byte("{}")
		}
		res := "ok"
		if err != nil {
			res = "failed"
		}
		h.d.Recorder.Record(r.Context(), model.AuditEvent{At: h.d.Clock.Now(), Actor: actor(r), Action: "device.mqtt_password_test",
			Target: "device:" + strconv.FormatInt(int64(d.ID), 10), Detail: string(detail), Result: res, RemoteAddr: httpx.ClientIP(r.Context()).String()})
	}
	switch outcome {
	case "accepted":
		redirect(w, r, devPath(d.ID, "")+"?ok=pw-accepted")
	case "refused":
		redirect(w, r, devPath(d.ID, "")+"?ok=pw-refused")
	default:
		text := "The password test did not run: " + passwordTestProblem(err)
		if outcome == "error" {
			h.log.WarnContext(r.Context(), "password test did not get an answer", "device", int64(d.ID)) // no err: the channel's text is not ours
		}
		h.message(w, r, http.StatusOK, h.view(r, "Password not tested", "devices", messageData{Text: text, LinkText: "Back to " + d.Name, Link: devPath(d.ID, "")}))
	}
}

// passwordTestProblem words a test that could not say anything about the password. The channel's own error texts
// are never shown: only the reason a blocked test carries (our own sentence), or a fixed line.
func passwordTestProblem(err error) string {
	var blocked *transport.PasswordTestBlockedError
	switch {
	case errors.As(err, &blocked):
		return sentence(blocked.Reason)
	case errors.Is(err, context.DeadlineExceeded):
		return "the device is busy with another transfer. Try again in a moment."
	}
	return "the device did not answer. Nothing says the password is wrong; try again when it is online."
}

// pwTestFlash is the line after a password test that said something about the password (the row says the rest).
func pwTestFlash(ok string) string {
	if ok == "pw-accepted" {
		return "The device accepted its password."
	}
	return "The device refused its password. See the Device MQTT password row."
}
