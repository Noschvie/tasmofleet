package restore

import (
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#609): a restore over MQTT warns when the backup would change the device's MqttPassword, since the
// next transfer would then send a password the device no longer has. A device in the new "no password"
// mode sends ""; its backup with an empty password changes nothing, and one with a password does. The
// comparison must follow the password transfers send (model.FilePassword), not a list of its own.
func TestANoPasswordDeviceComparesWithAnEmptyBackupPassword(t *testing.T) {
	t.Parallel()
	dev := model.Device{MQTTPasswordMode: model.MQTTPasswordEmpty}
	if differsPassword("", dev, nil) {
		t.Error("an empty backup password differs from the empty mode")
	}
	if !differsPassword("devicepw", dev, func() string { return "brokerpw" }) {
		t.Error("a backup with a password does not differ from the empty mode")
	}
	if differsPassword("anything", model.Device{}, func() string { return "brokerpw" }) {
		t.Error("a device without a password has nothing to compare against")
	}
}
