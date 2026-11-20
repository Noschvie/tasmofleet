package cmdpolicy

import "testing"

// Why (#1003): a console write the device accepted for a value TasmoFleet keeps with the identity (the OTA URL, the
// group topics, the topic, the names, the hostname) makes the stored value stale, so TasmoFleet reads the identity
// again. Only a command with a value writes; the bare command only reads, and a line the policy refuses was never sent.
// Every part of a Backlog counts.
func TestSetsIdentityValue(t *testing.T) {
	for cmd, want := range map[string]bool{
		"OtaUrl http://ota.example/tasmota.bin.gz": true,
		"otaurl 1":            true, // back to the build's default URL
		"GroupTopic2 kitchen": true,
		"GroupTopic kitchen":  true,
		"Topic plug2":         true,
		"DeviceName Lamp":     true,
		"FriendlyName1 Lamp":  true,
		"Hostname lamp":       true,
		"Backlog Power1 ON; OtaUrl http://x.example": true,
		"Backlog0 Hostname lamp":                     true,
		"OtaUrl":                                     false, // the bare read
		"GroupTopic2":                                false,
		"DeviceName":                                 false,
		"Backlog OtaUrl; Status 2":                   false,
		"MqttHost broker.example":                    false, // the broker read follows it (#598)
		"Power1 ON":                                  false,
		"Status 2":                                   false,
		"":                                           false,
		"OtaUrl http://x\x00":                        false, // refused by the policy: never sent
	} {
		if got := SetsIdentityValue(cmd); got != want {
			t.Errorf("SetsIdentityValue(%q) = %v, want %v", cmd, got, want)
		}
	}
}
