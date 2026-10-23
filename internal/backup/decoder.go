package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultDecodeTimeout = 20 * time.Second
	maxDecodeOutput      = 8 << 20
	maxStderrInError     = 160
)

var versionRe = regexp.MustCompile(`v?(\d+(?:\.\d+)+)`)

// ExecDecoder runs the decode-config binary as a separate process (it is
// LGPL-3.0 software and never linked). The child gets an empty environment
// except TMPDIR and PATH, a private working directory, a hard timeout, and
// is killed together with everything it spawned (PyInstaller one-file builds
// run the payload in a child process).
type ExecDecoder struct {
	path   string
	tmpDir string
	// Timeout bounds every invocation; zero means 20 s.
	Timeout time.Duration

	mu      sync.Mutex
	version string
}

var _ Decoder = (*ExecDecoder)(nil)

// NewExecDecoder returns a decoder for the binary at path. tmpDir must exist;
// it receives the temporary .dmp files (mode 0600, removed after each call)
// and is the child's TMPDIR, so PyInstaller extracts inside the data volume.
func NewExecDecoder(path, tmpDir string) *ExecDecoder {
	return &ExecDecoder{path: path, tmpDir: tmpDir}
}

// Version runs "decode-config --version" and parses "decode-config v16.0.0.0 [...]".
// A successful result is cached.
func (d *ExecDecoder) Version(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.version != "" {
		return d.version, nil
	}
	out, err := d.run(ctx, "--version")
	if err != nil {
		return "", err
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return "", errors.New("backup: cannot parse decoder version")
	}
	d.version = string(m[1])
	return d.version, nil
}

// Decode writes dmp to a private temporary file and returns the decoded JSON.
func (d *ExecDecoder) Decode(ctx context.Context, dmp []byte) ([]byte, error) {
	f, err := os.CreateTemp(d.tmpDir, "decode-*.dmp") // mode 0600
	if err != nil {
		return nil, fmt.Errorf("backup: decoder temp file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }() // best effort; the file only ever holds the dump
	if _, err := f.Write(dmp); err != nil {
		_ = f.Close() // the write error is the one worth reporting
		return nil, fmt.Errorf("backup: decoder temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("backup: decoder temp file: %w", err)
	}
	out, err := d.run(ctx, "--source", f.Name(), "--output-format", "json")
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, errors.New("backup: decoder produced no valid JSON")
	}
	return stripHostInfo(out)
}

// stripHostInfo removes header.env from decode-config output: it describes
// the machine that ran the decoder (platform, kernel, Python version), not
// the device, and would leak host details into backups and make every diff
// noisy after a host update. All other content is kept unchanged.
func stripHostInfo(out []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		return nil, fmt.Errorf("backup: decoder output is not a JSON object: %w", err)
	}
	raw, ok := top["header"]
	if !ok {
		return out, nil
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw, &header); err != nil {
		return out, nil //nolint:nilerr // a non-object header carries no env to strip
	}
	if _, ok := header["env"]; !ok {
		return out, nil
	}
	delete(header, "env")
	h, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("backup: re-encode decoder header: %w", err)
	}
	top["header"] = h
	return json.MarshalIndent(top, "", "  ")
}

func (d *ExecDecoder) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultDecodeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxDecodeOutput, 4096
	cmd := exec.CommandContext(ctx, d.path, args...) //nolint:gosec // path is operator configuration, arguments are fixed
	cmd.Env = []string{"TMPDIR=" + d.tmpDir, "PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Dir = d.tmpDir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("backup: decoder: %w", ctx.Err())
		}
		return nil, fmt.Errorf("backup: decoder failed: %w: %s", err, firstLine(stderr.String(), maxStderrInError))
	}
	if stdout.overflow {
		return nil, errors.New("backup: decoder output too large")
	}
	return stdout.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.overflow = true
		return len(p), nil // keep draining so the child does not block
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// CompareVersions compares dotted numeric versions ("16.0.0", "16.0.0.0",
// "15.1.0.2"); missing components count as 0 and a non-numeric tail such as
// "(release-tasmota)" is ignored. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	m := versionRe.FindString(strings.TrimSpace(v))
	var out []int
	for _, p := range strings.Split(strings.TrimPrefix(m, "v"), ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// CheckDecoder returns ErrDecoderOutdated when the decoder is older than the
// device firmware: decode-config would then misread new settings fields. An
// unknown device version passes (decoding is attempted, failures are flagged).
func CheckDecoder(decoderVersion, deviceVersion string) error {
	if deviceVersion == "" || len(versionParts(deviceVersion)) == 0 {
		return nil
	}
	if CompareVersions(decoderVersion, deviceVersion) < 0 {
		return fmt.Errorf("%w: decoder %s, device %s", ErrDecoderOutdated, decoderVersion, deviceVersion)
	}
	return nil
}
