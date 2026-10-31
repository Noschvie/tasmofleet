package app

import (
	"os"
	"strconv"
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

// Why (#33): Docker creates a missing bind-mount source as root, so the non-root
// image user cannot write to /data. That must stop the start with a message that
// names the directory, the uid and the README, not the opaque secretbox error.
func TestCheckDataDirWritableExplainsBindMountPermissions(t *testing.T) {
	if os.Getuid() == 0 {
		t.Fatal("run as a non-root user: root ignores directory permissions, so this check cannot be exercised")
	}
	dir := t.TempDir()
	if err := checkDataDirWritable(dir); err != nil {
		t.Fatalf("writable dir refused: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let TempDir remove it
	err := checkDataDirWritable(dir)
	if err == nil {
		t.Fatal("read-only data dir accepted")
	}
	for _, want := range []string{"data dir " + dir + " is not writable by uid " + strconv.Itoa(os.Getuid()), "README (bind mounts)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// Why (security review): the About page lists the effective configuration. A webhook address can
// carry its credential in the path (Home Assistant) or the query (Gotify), so it shows scheme,
// host and port only, like a secret shows no value.
func TestDescribeConfigRedactsTheWebhookAddressAndMasksTheNotificationSecrets(t *testing.T) {
	cfg := config.Config{NotifyWebhookURL: "http://hass.lan:8123/api/webhook/ha-credential-77?token=gotify-88", NotifyWebhookSecret: "hdr-secret-1", NotifySMTPPassword: "mail-pass-2"}
	var url string
	for _, kv := range describeConfig(cfg) {
		for _, leak := range []string{"ha-credential-77", "gotify-88", "hdr-secret-1", "mail-pass-2", "/api/webhook"} {
			if strings.Contains(kv.Value, leak) {
				t.Errorf("%s shows %q: %q", kv.Key, leak, kv.Value)
			}
		}
		if kv.Key == "TASMOFLEET_NOTIFY_WEBHOOK_URL" {
			url = kv.Value
		}
	}
	if url != "http://hass.lan:8123/***" {
		t.Errorf("webhook address on About = %q, want host and port with the rest hidden", url)
	}
}
