package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Why: after an upgrade the running version must be readable from the first log
// lines, not only from the image tag (#73). The test uses a fake version so it
// cannot pass on the "dev" default.
func TestStartupLineNamesVersionGoAndDecoder(t *testing.T) {
	old := Version
	Version = "9.8.7-test"
	t.Cleanup(func() { Version = old })

	for _, tc := range []struct{ decoder, want string }{
		{"", "decoder=none"},
		{"16.0.0.0", "decoder=16.0.0.0"},
	} {
		var buf bytes.Buffer
		a := &runtime{log: slog.New(slog.NewTextHandler(&buf, nil)), decoder: tc.decoder}
		a.logStarting()
		out := buf.String()
		for _, w := range []string{"tasmofleet starting", "version=9.8.7-test", "go=go", tc.want} {
			if !strings.Contains(out, w) {
				t.Errorf("startup line %q lacks %q", out, w)
			}
		}
	}
}
