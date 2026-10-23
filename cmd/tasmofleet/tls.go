package main

import "crypto/tls"

// insecureLoopback skips certificate verification for the container health
// probe, which talks to the local listener and only checks that the process
// answers (a self-signed certificate must not make it unhealthy).
func insecureLoopback() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // loopback liveness probe, no data is sent or trusted
}
