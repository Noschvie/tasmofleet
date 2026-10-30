package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// MQTTPasswords stores how a device's MqttPassword is found (D6 of the 0.20.0 plan; store.SQLite).
// Optional: without it the edit form has no MQTT password section. Only FileDownload and
// FileUpload (backup and restore over MQTT) need the password.
type MQTTPasswords interface {
	// SetMQTTPassword stores the mode and, for mode own, the sealed password; it clears the
	// "device refused" mark.
	SetMQTTPassword(ctx context.Context, id model.DeviceID, mode model.MQTTPasswordMode, plain string) error
}

const (
	fieldMQTTMode     = "mqtt_password_mode"
	fieldMQTTPassword = "mqtt_password"
	maxMQTTPassword   = 128
)

// mqttChange is what the posted edit form asks for. apply is false when the form carries no mode
// field or nothing changes; the stored value is then left alone.
type mqttChange struct {
	mode  model.MQTTPasswordMode
	plain string
	apply bool
}

// mqttFormChange reads the MQTT password section of the edit form. The password is write-only:
// blank keeps the stored own password. The refused mark is cleared only by a touched section or
// the "Retry the stored password" box. errMsg is for the
// mqtt_password field; nothing is stored when it is set.
func mqttFormChange(r *http.Request, old model.Device) (c mqttChange, mode model.MQTTPasswordMode, errMsg string) {
	if _, present := r.PostForm[fieldMQTTMode]; !present {
		return mqttChange{}, old.MQTTPasswordMode, ""
	}
	mode = model.MQTTPasswordMode(r.PostFormValue(fieldMQTTMode))
	pw := r.PostFormValue(fieldMQTTPassword)
	switch mode {
	case model.MQTTPasswordNone, model.MQTTPasswordBroker:
		c = mqttChange{mode: mode}
	case model.MQTTPasswordOwn:
		switch {
		case pw != "":
			if len(pw) > maxMQTTPassword || strings.ContainsAny(pw, "\"\\") || strings.ContainsFunc(pw, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				return mqttChange{}, mode, "Use up to 128 characters, without quotes, backslashes or control characters."
			}
			c = mqttChange{mode: mode, plain: pw}
		case old.MQTTPasswordMode == model.MQTTPasswordOwn && old.MQTTPassword != "":
			c = mqttChange{mode: mode, plain: old.MQTTPassword} // blank keeps the stored one
		default:
			return mqttChange{}, mode, "Enter the device's MQTT password."
		}
	default:
		return mqttChange{}, old.MQTTPasswordMode, "Choose where the password comes from."
	}
	// Only a touched section writes (and clears the refused mark): mode or password changed, or the
	// person asked to retry the stored one. Unrelated saves leave it, and the audit log, alone.
	touched := mode != old.MQTTPasswordMode || (mode == model.MQTTPasswordOwn && pw != "") || r.PostFormValue("retry_mqtt_password") == "1"
	c.apply = touched
	return c, mode, ""
}

// saveMQTTPassword stores the change and audits it. The password never reaches the audit entry.
func (h *handler) saveMQTTPassword(r *http.Request, id model.DeviceID, old model.Device, c mqttChange) error {
	if !c.apply || h.d.MQTTPasswords == nil {
		return nil
	}
	err := h.d.MQTTPasswords.SetMQTTPassword(r.Context(), id, c.mode, c.plain)
	if h.d.Recorder != nil {
		detail, jerr := json.Marshal(map[string]any{"mode": string(c.mode), "was": string(old.MQTTPasswordMode), "cleared_refused_mark": !old.MQTTPasswordBadAt.IsZero()})
		if jerr != nil {
			detail = []byte("{}")
		}
		ev := model.AuditEvent{At: h.d.Clock.Now(), Actor: actor(r), Action: "device.mqtt_password", Target: "device:" + strconv.FormatInt(int64(id), 10), Detail: string(detail), Result: "ok", RemoteAddr: httpx.ClientIP(r.Context()).String()}
		if err != nil {
			ev.Result = "error"
		}
		h.d.Recorder.Record(r.Context(), ev)
	}
	return err
}
