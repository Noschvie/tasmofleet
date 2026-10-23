package simulator

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func dumpServer(t *testing.T, body []byte) (*httptest.Server, chan struct{}) {
	t.Helper()
	hit := make(chan struct{}, 4)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		_, _ = w.Write(body) //nolint:errcheck // test server
	}))
	t.Cleanup(s.Close)
	return s, hit
}

// Why: the restore flow (M2-5) is only testable if a simulated device behaves
// like the real WebGetConfig: it fetches the URL itself, replaces its settings
// and reboots, so the next /dl returns the restored dump.
func TestWebGetConfigRestoresDumpAndReboots(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_1M, Version: "16.0.0"})
	want := bytes.Repeat([]byte{0x5A}, configDumpSize)
	srv, hit := dumpServer(t, want)

	if got := cm(t, d, "WebGetConfig "+srv.URL+"/restore/tok"); !strings.Contains(got, "WebGetConfig") {
		t.Fatalf("reply: %s", got)
	}
	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatal("device never fetched the URL")
	}
	settled(t, d, 1)
	body, code, err := cmWithPath(d, "/dl", "")
	if err != nil || code != 200 || body != string(want) {
		t.Fatalf("dl after restore: code %d err %v equal=%v", code, err, body == string(want))
	}
}

// Why: a real device refuses a file that is not a settings dump; a wrong
// size must neither change the config nor reboot.
func TestWebGetConfigRejectsWrongSizeAndBadURL(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP8266_1M, Version: "16.0.0"})
	before, _, _ := cmWithPath(d, "/dl", "")
	srv, hit := dumpServer(t, []byte("short"))
	cm(t, d, "WebGetConfig "+srv.URL+"/x")
	<-hit
	time.Sleep(300 * time.Millisecond)
	after, _, _ := cmWithPath(d, "/dl", "")
	if after != before || d.Boots() != 0 {
		t.Errorf("config changed or rebooted (boots %d)", d.Boots())
	}
	if got := cm(t, d, "WebGetConfig ftp://x/y"); !strings.Contains(got, `"Command":"Error"`) {
		t.Errorf("bad URL: %s", got)
	}
}
