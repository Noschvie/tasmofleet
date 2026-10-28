package web

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the server zone to UTC: the pages show times in the server's zone (as the
// fallback of the browser-side timestamps), so the expected texts must not depend on the TZ of
// the machine that runs the tests.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
