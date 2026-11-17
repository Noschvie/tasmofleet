package mqttcmd

import (
	"context"

	"github.com/hellwand/tasmofleet/internal/model"
)

// CheckFilePassword refuses a password that a file transfer command cannot carry verbatim (ErrRefused, without
// the password in the error); the facts source asks it before any transfer is chosen. "" can be carried.
func CheckFilePassword(password string) error { return checkFilePassword(password) }

// TestPassword asks the device whether it accepts password as its MqttPassword, without a transfer
// (#609). It sends the start of a settings download, FileDownload {"Password":…,"Type":2}: Tasmota
// checks the password before it allocates anything and answers "Error 1" when it differs; "Started"
// means it was accepted. After "Started" the device holds a copy of its settings in RAM (nothing is
// written to flash); FileDownload 0 ends that at once. Tasmota answers that abort with one more chunk
// of the dump before "Aborted" (CmndFileDownload sends a chunk while its buffer is set,
// xdrv_02_1_mqtt_file.ino 16.0.0), so an accepted test puts the file header and one data chunk on the
// broker: up to 2980 B on an ESP8266 and 4480 B on an ESP32 (docs/research/tasmota-mqtt-transport.md
// 41-44), which is most of the settings dump or all of it, with every password stored there (Wi-Fi,
// web, MqttPassword), Tasmota-obfuscated only.
//
// nil: accepted. ErrPasswordRefused: refused. Anything else says nothing about the password; it is
// marked ErrPublished once anything was published, like Download. The topic is held for the whole
// test, and it opens with FileDownload 0 like a Download when the topic's state is unknown; a test
// never marks the topic clean, so the next Download opens with the abort again. It is a transfer that
// stops after "Started" (transferSpec.probe), so it follows every rule of Download up to that point.
func (c *Channel) TestPassword(ctx context.Context, d model.Device, password string) error {
	start, err := downloadStart(password)
	if err != nil {
		return err
	}
	_, err = c.transfer(ctx, d, transferSpec{start: start, probe: true})
	return err
}
