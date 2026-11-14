package app

import "testing"

// Why (#826): the console's sentence about a secret crossing the broker is worded by the broker URL; a TLS broker
// must not read as unencrypted, and a URL that cannot be read takes the stricter (plain) wording.
func TestBrokerUsesTLSReadsTheScheme(t *testing.T) {
	for raw, want := range map[string]bool{
		"tcp://broker.example:1883": false, "mqtt://broker.example": false,
		"ssl://broker.example:8883": true, "tls://broker.example": true, "mqtts://broker.example": true,
		"": false, "ws://broker.example": false, "mqtts://user:pw@broker.example": false,
	} {
		if got := brokerUsesTLS(raw); got != want {
			t.Errorf("brokerUsesTLS(%q) = %v, want %v", raw, got, want)
		}
	}
}
