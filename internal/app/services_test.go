package app

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

// Why (review F3): the About page lists the configuration; an MQTT URL with
// credentials must never be readable there.
func TestDescribeConfigHidesURLCredentialsAndSecrets(t *testing.T) {
	cfg := config.Config{MQTTURL: "tcp://tasmofleet:hunter2@mqtt.example:1883", OIDCClientSecret: "s3cr3t", MetricsToken: "tok"}
	for _, kv := range describeConfig(cfg) {
		if strings.Contains(kv.Value, "hunter2") || strings.Contains(kv.Value, "s3cr3t") || kv.Value == "tok" {
			t.Errorf("%s leaks: %q", kv.Key, kv.Value)
		}
	}
}
