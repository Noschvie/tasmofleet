package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeScript creates an executable stand-in for decode-config. The scripts
// are local shell programs; nothing here touches the network.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "decode-config")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // test helper must create an executable
		t.Fatal(err)
	}
	return p
}

const fakeDecodeConfig = `
case "$1" in
--version) echo "decode-config v16.0.0.0 [abc123]"; exit 0;;
--source) src="$2";;
*) echo "bad args: $*" >&2; exit 2;;
esac
[ "$3" = "--output-format" ] && [ "$4" = "json" ] || { echo "bad format" >&2; exit 2; }
perm=$(stat -c %a "$src")
size=$(wc -c < "$src")
echo "{\"perm\":\"$perm\",\"size\":$size,\"home\":\"${HOME-unset}\",\"leak\":\"${TASMOFLEET_TEST_SECRET-unset}\",\"tmpdir\":\"$TMPDIR\"}"
`

func TestExecDecoderVersion(t *testing.T) {
	d := NewExecDecoder(writeScript(t, fakeDecodeConfig), t.TempDir())
	v, err := d.Version(context.Background())
	if err != nil || v != "16.0.0.0" {
		t.Fatalf("version %q, %v", v, err)
	}
}

// The dump holds Wi-Fi and MQTT passwords: the temp copy must be private,
// vanish afterwards, and the child must not inherit the server's environment
// (OIDC secrets, metrics token, ...).
func TestExecDecoderDecodeIsolation(t *testing.T) {
	t.Setenv("TASMOFLEET_TEST_SECRET", "must-not-leak")
	tmp := t.TempDir()
	d := NewExecDecoder(writeScript(t, fakeDecodeConfig), tmp)
	out, err := d.Decode(context.Background(), []byte("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output: %s", out)
	}
	if got["perm"] != "600" || got["size"] != float64(10) {
		t.Errorf("temp file: %v", got)
	}
	if got["leak"] != "unset" || got["home"] != "unset" {
		t.Errorf("environment leaked into the decoder: %v", got)
	}
	if got["tmpdir"] != tmp {
		t.Errorf("TMPDIR = %v, want %s", got["tmpdir"], tmp)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("temp dump left behind: %v", entries)
	}
}

func TestExecDecoderFailures(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"exit code":  `echo "unsupported version 99" >&2; exit 1`,
		"not json":   `echo "this is not json"`,
		"no version": `echo hello`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			d := NewExecDecoder(writeScript(t, body), t.TempDir())
			if _, err := d.Decode(ctx, []byte("x")); err == nil {
				t.Error("Decode: want error")
			}
		})
	}
	d := NewExecDecoder(writeScript(t, `echo hello`), t.TempDir())
	if _, err := d.Version(ctx); err == nil {
		t.Error("unparsable version must be an error")
	}
	if _, err := NewExecDecoder("/nonexistent/decode-config", t.TempDir()).Decode(ctx, []byte("x")); err == nil {
		t.Error("missing binary must be an error")
	}
}

func TestExecDecoderStderrInErrorIsShort(t *testing.T) {
	d := NewExecDecoder(writeScript(t, `printf 'line one\nSECOND-LINE-SHOULD-NOT-APPEAR\n' >&2; exit 1`), t.TempDir())
	_, err := d.Decode(context.Background(), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "line one") || strings.Contains(err.Error(), "SECOND-LINE") {
		t.Errorf("err: %v", err)
	}
}

// A hung decoder must not hang the backup run, and its children must die too
// (PyInstaller one-file builds re-exec themselves).
func TestExecDecoderTimeoutKillsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	d := NewExecDecoder(writeScript(t, `(sleep 1; touch `+marker+`) & sleep 30`), t.TempDir())
	d.Timeout = 150 * time.Millisecond
	start := time.Now()
	if _, err := d.Decode(context.Background(), []byte("x")); err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("child process survived the timeout")
	}
}

func TestExecDecoderOutputLimit(t *testing.T) {
	d := NewExecDecoder(writeScript(t, `yes '{"a":1}' | head -c 9000000`), t.TempDir())
	if _, err := d.Decode(context.Background(), []byte("x")); err == nil {
		t.Error("oversized output must fail")
	}
}
