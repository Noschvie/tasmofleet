package web

import (
	"context"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// PasswordGap says why a device has no MqttPassword from its backups (#609). It is a reason code and nothing
// else: the type cannot carry a password, so the card that lists the devices without one never holds a value.
// The learner (package app) decides it; the card only words it.
type PasswordGap string

// The reasons. GapNone is "no reason known": a device the learner has nothing to say about.
const (
	GapNone       PasswordGap = ""
	GapOff        PasswordGap = "off"        // taking passwords from backups is switched off
	GapNoBackup   PasswordGap = "nobackup"   // the device has no backup of its own yet
	GapDmpOnly    PasswordGap = "dmponly"    // its newest backup is a .dmp without a decoded copy
	GapNotProven  PasswordGap = "notproven"  // its newest backup's snapshot does not show one of its MACs
	GapUnreadable PasswordGap = "unreadable" // the newest decoded copy could not be read
	GapNoField    PasswordGap = "nofield"    // the newest decoded copy holds no MqttPassword
	GapPending    PasswordGap = "pending"    // a usable backup exists that is not applied yet
)

// Text is the sentence under the device's name on the card.
func (g PasswordGap) Text() string {
	switch g {
	case GapOff:
		return "Taking passwords from backups is off."
	case GapNoBackup:
		return "No backup yet, so there is no password to take. It is read after the next backup."
	case GapDmpOnly:
		return "Its newest backup is kept as a .dmp file only, without a decoded copy to read the password from."
	case GapNotProven:
		return "Its newest backup is not proven to be this device's (its snapshot shows no MAC of the device), so its password is not taken."
	case GapUnreadable:
		return "Its newest decoded backup could not be read."
	case GapNoField:
		return "Its newest decoded backup holds no MQTT password."
	case GapPending:
		return "A backup with its password exists; it is taken in a moment."
	case GapNone:
	}
	return "TasmoFleet has no password for it."
}

// gapText is Text with the one fact the learner does not know: a device that TasmoFleet reaches only over MQTT
// (its HTTP fails) never gets a backup of its own before it has a password, so "read after the next backup" would
// promise what cannot happen. The device page's row and the card share it.
func gapText(g PasswordGap, f transport.Facts) string {
	if g == GapNoBackup && f.HTTP.State == transport.HTTPFailing {
		return "TasmoFleet reaches it only over MQTT, so no backup can bring its password: enter it, or use the broker password."
	}
	return g.Text()
}

// MQTTPasswordFleet is the fleet side of the device MQTT passwords (#609; package app implements it): why devices
// have none from a backup, the bulk "use the broker password" and the tests that follow it. Optional: without it
// the card shows the switch only.
type MQTTPasswordFleet interface {
	// Gaps says why each of ids has no password from a backup. It reads the stored backups, never a device.
	Gaps(ctx context.Context, ids []model.DeviceID) map[model.DeviceID]PasswordGap
	// UseBroker sets broker mode on those of ids that are still not set (the store checks it per device, in one
	// transaction) and returns the ids it wrote. With test it then queues a password test for each of them; a run
	// that is busy takes them in its queue, so the set is never applied without its tests. actor is the admin who
	// confirmed it.
	UseBroker(ctx context.Context, actor string, ids []model.DeviceID, test bool) (applied []model.DeviceID, err error)
	// Testing is the number of devices whose test is queued or running (0: idle).
	Testing() int
}

// PasswordTester runs the password test of one device (transport.Router.TestMQTTPassword): synchronous, over
// MQTT only, nothing returned or logged holds the password.
type PasswordTester interface {
	TestMQTTPassword(ctx context.Context, id model.DeviceID) error
}
