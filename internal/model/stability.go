package model

import "time"

// DeviceBoot is one restart of a device that TasmoFleet noticed (#325).
type DeviceBoot struct {
	DeviceID DeviceID
	At       time.Time
	// BootCount is Tasmota's persisted BootCount after this restart; 0 when not known. Gaps between
	// two rows reveal restarts TasmoFleet did not see.
	BootCount int
	// Reason is Tasmota's RestartReason as the device sent it ("" when unknown).
	Reason string
	// UptimeBefore is how long the device had been up before this restart; 0 when not known.
	UptimeBefore time.Duration
	// Expected: TasmoFleet itself was working on the device (an update, a restore, a command) when it
	// restarted, so it is no sign of a fault. It still shows as the last restart.
	Expected bool
}

// WifiSample is the signal strength of a device at one moment, in dBm.
type WifiSample struct {
	DeviceID DeviceID
	At       time.Time
	DBm      int
}
