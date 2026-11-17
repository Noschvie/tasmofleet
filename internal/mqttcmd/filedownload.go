package mqttcmd

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: the Tasmota file transfer protocol mandates MD5
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// File transfer limits (D7). A settings dump is 2..16 KiB (SettingsBufferAlloc).
const (
	minDumpSize = 2
	maxDumpSize = 16 << 10
	// abortWait is how long the opening "FileDownload 0" waits for the device's "Aborted".
	abortWait = time.Second
	// cmndBufferSize is MQTT_MAX_PACKET_SIZE of the device; a command packet (5 header bytes,
	// the topic with its length byte, the payload) of this size or more closes its connection.
	cmndBufferSize = 1200
	downloadType   = 2 // UPL_SETTINGS
	fileType       = 8 // UPL_UFSFILE: a file of the file system (USE_UFILESYS_MQTT)

	suffixDownload = "FILEDOWNLOAD"
	suffixUpload   = "FILEUPLOAD"
	cmdDownload    = "FileDownload"
	cmdUpload      = "FileUpload"
)

// ErrPasswordRefused: the device answered "Error 1" to FileDownload. Its MqttPassword differs
// from the one we hold; the caller marks it bad and stops trying until it is changed.
var ErrPasswordRefused = errors.New("mqttcmd: the device refused the MQTT password (Error 1)")

// ErrNoFileTransfer: the device answered a file download (Type 8) with Error 3: its firmware was built without
// USE_UFILESYS_MQTT (the stock builds), so it sends no files over MQTT. It came in answer to the start: nothing of
// the file was sent. Always wrapped together with ErrTransfer.
var ErrNoFileTransfer = errors.New("mqttcmd: the firmware sends no files over MQTT (FileDownload Error 3)")

// ErrFileUnavailable: the device answered a file download (Type 8) with Error 2: the file is not there, is a
// folder, or the device had no memory for its 4 kB read buffer; the firmware does not say which. Always wrapped
// together with ErrTransfer.
var ErrFileUnavailable = errors.New("mqttcmd: the device could not open the file (FileDownload Error 2)")

// ErrTransfer is any other failed or irregular file transfer. Nothing it wraps ever contains a
// message payload: the password travels in it, and a minimal firmware echoes it back.
var ErrTransfer = errors.New("mqttcmd: file transfer failed")

// ErrEmptyData is a transfer that failed on a data message with an empty Data (#887). It is always
// wrapped together with ErrTransfer. What the firmware means by it is for the caller to word; this
// package only reports that it happened.
var ErrEmptyData = errors.New("mqttcmd: the device sent an empty data message")

func transferErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrTransfer, fmt.Sprintf(format, a...))
}

// fileMsg is the union of what stat/<t>/FILEDOWNLOAD carries. Payloads are decoded here and then
// dropped; they are never logged, stored or put into an error.
type fileMsg struct {
	Status  *string `json:"FileDownload"` // Started | Aborted | Done | Error N
	UpState *string `json:"FileUpload"`   // the same words on stat/<t>/FILEUPLOAD (upload.go)
	MaxSize *int    `json:"MaxSize"`      // upload: the chunk size the device accepts next
	Command *string `json:"Command"`      // "Unknown": minimal firmware echoing our command (with the password)
	File    *string `json:"File"`
	ID      *int    `json:"Id"`
	Size    *int    `json:"Size"`
	Data    *string `json:"Data"`
	Md5     *string `json:"Md5"`
}

func (c *Channel) onFile(m mqttbus.Message) {
	if m.Retained || len(m.Payload) == 0 { // the device always publishes, an empty response too
		return
	}
	c.mu.Lock()
	ch := c.files[m.Topic]
	c.mu.Unlock()
	if ch == nil || (m.Suffix != suffixDownload && m.Suffix != suffixUpload) {
		return
	}
	select {
	case ch <- m:
	default: // a flood must not block the bus; the caller times out
	}
}

// Download fetches the settings dump of d (FileDownload Type 2) and returns the bytes /dl would.
//
// The protocol is fragile (docs/research/tasmota-mqtt-transport.md, live test 2026-10-30), so it
// is followed to the letter: if the last transfer on this topic did not end with a matching MD5
// (or this process has not done one), "FileDownload 0" goes first; then the start command, which
// is answered by "Started" and the header (no "?" for the header); then exactly one "?" per Data
// message until {"Id","Md5"}. Any irregularity aborts with "FileDownload 0"; there is no retry.
// The topic is held for the whole transfer, so it is one command in flight like any other.
func (c *Channel) Download(ctx context.Context, d model.Device, password string) (data []byte, rerr error) {
	start, err := downloadStart(password)
	if err != nil {
		return nil, err
	}
	return c.transfer(ctx, d, transferSpec{start: start, minSize: minDumpSize, maxSize: maxDumpSize,
		bound: func(int) time.Duration { return c.o.FileTotal }})
}

// DownloadFile fetches one file of d's file system (FileDownload Type 8 with "File", USE_UFILESYS_MQTT,
// arendst/Tasmota#25278) by the same protocol as Download. The header must name path the way the firmware
// does (without its leading slash), and a file it announces larger than max is refused with
// tasmota.ErrFileTooLarge before any data is asked for. A firmware without the option answers the start with
// Error 3: ErrNoFileTransfer, sent before anything of the file; Error 2 is ErrFileUnavailable (no such file,
// a folder, or no memory for the 4 kB read buffer). path obeys tasmota.CheckFSPath and max is positive, or
// nothing is published. The transfer has FileTotal or, for a larger file, ReplyWait plus chunkAllowance per
// chunk of the announced size, whichever is longer.
func (c *Channel) DownloadFile(ctx context.Context, d model.Device, password, path string, max int64) ([]byte, error) {
	if err := tasmota.CheckFSPath(path); err != nil {
		return nil, fmt.Errorf("%w: the file path cannot be sent in a file transfer command: %w", ErrRefused, err) // tasmota.ErrBadPath, as over HTTP
	}
	if max <= 0 {
		return nil, fmt.Errorf("%w: no room for the file", ErrRefused)
	}
	if err := checkFilePassword(password); err != nil {
		return nil, err
	}
	// The path passed CheckFSPath: no quote, backslash or control character, so it goes in verbatim.
	start := `{"Password":"` + password + `","Type":` + strconv.Itoa(fileType) + `,"File":"` + path + `"}`
	return c.transfer(ctx, d, transferSpec{start: start, file: strings.TrimPrefix(path, "/"), minSize: 0, maxSize: max,
		tooLarge: true, bound: c.fileBound})
}

// chunkAllowance is the time one data chunk of a file may take (the "?", the device reading and encoding up to
// minChunk bytes, the publish back): about 3 kB/s at the least. minChunk is the smaller chunk of the two chips
// (ResponseSize 4000 on an ESP8266, (4000-21)/4*3-2 bytes), so the count is never too low.
const (
	chunkAllowance = time.Second
	minChunk       = 2982
)

// fileBound is how long a Type 8 transfer of size bytes may take in all: FileTotal, or ReplyWait plus
// chunkAllowance per chunk when that is longer. A device that sends slower than that is cut off, so it cannot
// hold its topic (and the backup the device lock) for long.
func (c *Channel) fileBound(size int) time.Duration {
	chunks := (size + minChunk - 1) / minChunk
	return max(c.o.FileTotal, c.o.ReplyWait+time.Duration(chunks)*chunkAllowance)
}

// transferSpec is what differs between the download types; the protocol run is the same.
type transferSpec struct {
	start            string // the start command's payload (carries the password)
	file             string // Type 8: the File the header must carry; "" for the settings
	minSize, maxSize int64  // the sizes the header may announce
	tooLarge         bool   // a size over maxSize is tasmota.ErrFileTooLarge (a file), not a broken header (a dump)
	bound            func(size int) time.Duration
}

func (c *Channel) transfer(ctx context.Context, d model.Device, spec transferSpec) (data []byte, rerr error) {
	if err := checkTopic(d); err != nil {
		return nil, err
	}
	if !c.bus.Connected() {
		return nil, fmt.Errorf("%w: %w", tasmota.ErrUnreachable, mqttbus.ErrNotConnected)
	}
	sem := c.sem(d.Topic)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err() // still queued: nothing was sent
	case <-c.done:
		return nil, c.closed()
	}
	defer func() { <-sem }()
	before := c.published(d.Topic)
	defer func() { // anything published for this transfer, the opening abort included, makes a failure "attempted"
		if rerr != nil && c.published(d.Topic) > before && !errors.Is(rerr, ErrPublished) {
			rerr = fmt.Errorf("%w: %w", ErrPublished, rerr)
		}
	}()
	if c.offline(d.Topic) {
		return nil, fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	}

	began := time.Now()
	hctx, cancel := context.WithDeadline(ctx, began.Add(c.o.FileTotal)) // up to the header; the data get spec.bound
	defer cancel()
	in := make(chan mqttbus.Message, waitDepth)
	c.mu.Lock()
	if err := c.firmwarePendingLocked(d.Topic); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.files[d.Topic] = in
	clean := c.fileClean[d.Topic]
	c.fileClean[d.Topic] = false // clean again only after a matching MD5
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.files, d.Topic)
		c.mu.Unlock()
	}()

	if !clean {
		if err := c.sendFile(hctx, d.Topic, "0"); err != nil {
			return nil, err
		}
		c.waitAborted(hctx, in)
	}
	data, sent, err := c.runTransfer(ctx, hctx, began, d.Topic, spec, in)
	if err != nil {
		if sent && !deviceReset(err) {
			c.abort(d.Topic) // the start may have reached the device: leave it idle
		}
		return nil, err
	}
	c.mu.Lock()
	c.fileClean[d.Topic] = true
	c.mu.Unlock()
	return data, nil
}

// deviceResetError marks an "Error N" answer: the device ended the transfer itself.
type deviceResetError struct{ err error }

func (e *deviceResetError) Error() string { return e.err.Error() }
func (e *deviceResetError) Unwrap() error { return e.err }

func deviceReset(err error) bool {
	var r *deviceResetError
	return errors.As(err, &r)
}

// runTransfer runs one transfer after the opening abort: the start and the header under hctx, the data under
// spec.bound of the announced size, counted from began. ctx is the caller's.
func (c *Channel) runTransfer(ctx, hctx context.Context, began time.Time, topic string, spec transferSpec, in chan mqttbus.Message) (data []byte, sent bool, err error) {
	if err := c.sendFile(hctx, topic, spec.start); err != nil {
		// A timeout here means the publish was attempted; a refusal or a spent context sent nothing.
		return nil, errors.Is(err, tasmota.ErrTimeout), err
	}
	// Anything before "Started" is a leftover of an earlier transfer (an "Aborted", a stray chunk).
	for {
		m, err := c.nextFile(hctx, in, "the start")
		if err != nil {
			return nil, true, err
		}
		switch {
		case m.Command != nil:
			// A firmware without FileDownload (minimal) echoed the command, password included.
			return nil, true, &deviceResetError{transferErr("the device does not support FileDownload (minimal firmware?)")}
		case m.Status == nil:
			continue
		case *m.Status == "Started":
		case strings.HasPrefix(*m.Status, "Error"):
			// Only here, in answer to the start, does Error 3 mean "this type is not in the firmware": the device
			// refused before it sent anything of the file.
			return nil, true, startErrorStatus(*m.Status, spec.file != "")
		default:
			continue
		}
		break
	}

	fail := func(format string, a ...any) error { return transferErr(format, a...) }
	hdr, err := c.nextFile(hctx, in, "the file header")
	if err != nil {
		return nil, true, err
	}
	if hdr.Status != nil && strings.HasPrefix(*hdr.Status, "Error") {
		return nil, true, errorStatus(*hdr.Status)
	}
	if hdr.File == nil || hdr.ID == nil || hdr.Size == nil {
		return nil, true, fail("the header after Started is not a file header")
	}
	if spec.file != "" && *hdr.File != spec.file { // the firmware names the file without its leading slash
		return nil, true, fail("the header names another file than the one asked for")
	}
	id, size := *hdr.ID, *hdr.Size
	switch {
	case spec.tooLarge && int64(size) > spec.maxSize:
		return nil, true, fmt.Errorf("%w: %w: the device announced %d bytes", ErrTransfer, tasmota.ErrFileTooLarge, size)
	case int64(size) < spec.minSize || int64(size) > spec.maxSize:
		return nil, true, fail("the device announced %d bytes, this transfer takes %d to %d", size, spec.minSize, spec.maxSize)
	}
	dctx, cancel := context.WithDeadline(ctx, began.Add(spec.bound(size)))
	defer cancel()

	var got bytes.Buffer
	sum := md5.New() //nolint:gosec // see import
	for {
		if err := c.sendFile(dctx, topic, "?"); err != nil {
			return nil, true, err
		}
		m, err := c.nextFile(dctx, in, "a data message")
		if err != nil {
			return nil, true, err
		}
		if m.Status != nil {
			if strings.HasPrefix(*m.Status, "Error") {
				return nil, true, errorStatus(*m.Status)
			}
			return nil, true, fail("the device ended the transfer (%s)", boundedStatus(*m.Status))
		}
		if m.ID == nil || *m.ID != id {
			return nil, true, fail("a message of another transfer arrived")
		}
		switch {
		case m.Md5 != nil:
			if got.Len() != size {
				return nil, true, fail("the device sent %d of %d bytes", got.Len(), size)
			}
			if !strings.EqualFold(*m.Md5, hex.EncodeToString(sum.Sum(nil))) {
				return nil, true, fail("the MD5 of the data does not match the one the device announced")
			}
			return got.Bytes(), true, nil
		case m.Data != nil:
			if *m.Data == "" {
				return nil, true, fmt.Errorf("%w: %w", ErrTransfer, ErrEmptyData)
			}
			raw, err := base64.StdEncoding.DecodeString(*m.Data)
			if err != nil {
				return nil, true, fail("a data message is not valid base64")
			}
			if got.Len()+len(raw) > size {
				return nil, true, fail("the device sent more than the announced %d bytes", size)
			}
			got.Write(raw)
			sum.Write(raw)
		default:
			return nil, true, fail("an unexpected message arrived during the transfer")
		}
	}
}

// errorStatus maps "Error N" of a message after the start to a typed error.
func errorStatus(s string) error { return &deviceResetError{errorStatusRaw(s)} }

// startErrorStatus maps "Error N" in answer to the start. For a file (Type 8) Error 3 is ErrNoFileTransfer and
// Error 2 ErrFileUnavailable (MqttFileDownloadValidate); for the settings they keep their generic wording.
func startErrorStatus(s string, file bool) error {
	if file {
		switch statusNumber(s) {
		case 2:
			return &deviceResetError{fmt.Errorf("%w: %w", ErrTransfer, ErrFileUnavailable)}
		case 3:
			return &deviceResetError{fmt.Errorf("%w: %w", ErrTransfer, ErrNoFileTransfer)}
		}
	}
	return errorStatus(s)
}

func statusNumber(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "Error")))
	return n
}

func errorStatusRaw(s string) error {
	n := statusNumber(s)
	switch n {
	case 1:
		return ErrPasswordRefused
	case 3:
		return transferErr("the device refused: the file type is not supported (Error 3)")
	case 5:
		return transferErr("the device refused: the command arrived on a group topic (Error 5)")
	}
	return transferErr("the device refused the download (Error %d)", n)
}

// boundedStatus keeps a status word short and printable before it goes into an error.
func boundedStatus(s string) string {
	if len(s) > 24 {
		s = s[:24]
	}
	return strconv.QuoteToASCII(s)
}

// nextFile returns the next non-empty FILEDOWNLOAD message, or why there is none.
func (c *Channel) nextFile(ctx context.Context, in chan mqttbus.Message, what string) (*fileMsg, error) {
	return c.nextFileOf(ctx, in, what, suffixDownload)
}

// nextFileOf is nextFile for one direction: the device keeps one transfer state, so messages of
// the other direction (a late "Aborted") are not answers.
func (c *Channel) nextFileOf(ctx context.Context, in chan mqttbus.Message, what, suffix string) (*fileMsg, error) {
	timer := time.NewTimer(c.o.ReplyWait)
	defer timer.Stop()
	for {
		select {
		case m := <-in:
			var fm fileMsg
			if m.Suffix != suffix || json.Unmarshal(m.Payload, &fm) != nil {
				continue // noise on the topic; never put the payload anywhere
			}
			return &fm, nil
		case <-timer.C:
			return nil, fmt.Errorf("mqtt: no answer for %s: %w: %w", what, ErrTransfer, tasmota.ErrTimeout)
		case <-ctx.Done():
			return nil, fmt.Errorf("mqtt: the transfer ran out of time at %s: %w: %w", what, ErrTransfer, tasmota.ErrTimeout)
		case <-c.done:
			return nil, c.closed()
		}
	}
}

// waitAborted drains the answer to the opening "FileDownload 0" (up to abortWait): it may be
// preceded by one more chunk of a download that was still running. It never fails; the start
// ignores whatever is left.
func (c *Channel) waitAborted(ctx context.Context, in chan mqttbus.Message) {
	c.waitAbortedOf(ctx, in, suffixDownload)
}

func (c *Channel) waitAbortedOf(ctx context.Context, in chan mqttbus.Message, suffix string) {
	timer := time.NewTimer(min(abortWait, c.o.ReplyWait))
	defer timer.Stop()
	for {
		select {
		case m := <-in:
			var fm fileMsg
			if m.Suffix != suffix || json.Unmarshal(m.Payload, &fm) != nil {
				continue
			}
			if st := fm.state(suffix); st != nil && *st == "Aborted" {
				return
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		case <-c.done:
			return
		}
	}
}

// abort ends a half-done transfer. The caller's context may be spent, so it has its own.
func (c *Channel) abort(topic string) {
	c.abortCmd(topic, cmdDownload)
}

func (c *Channel) abortCmd(topic, cmd string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.o.ReplyWait)
	defer cancel()
	// Best effort: if it is lost, the next Download starts with "FileDownload 0" again.
	_ = c.sendFileCmd(ctx, topic, cmd, "0")
}

// sendFile publishes FileDownload with payload, after the bus-wide gap. payload may carry the
// password: it is neither logged nor wrapped into an error.
func (c *Channel) sendFile(ctx context.Context, topic, payload string) error {
	return c.sendFileCmd(ctx, topic, cmdDownload, payload)
}

// payloadRoom is the payload budget for a command published to cmndTopic: the device closes its
// connection for a packet of 1200 bytes or more (5 header bytes, the topic with its length byte).
func payloadRoom(cmndTopic string) int { return cmndBufferSize - 5 - len(cmndTopic) - 1 }

// published is how many file transfer commands were handed to the bus for topic so far.
func (c *Channel) published(topic string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pubs[topic]
}

// sendFileCmd publishes cmd (FileDownload or FileUpload). The payload budget of the device's
// 1200 byte buffer is asserted before every publish: at 5 + len(topic) + len(payload) >= 1200 the
// device drops its connection without a DISCONNECT.
func (c *Channel) sendFileCmd(ctx context.Context, topic, cmd, payload string) error {
	if room := payloadRoom(c.bus.CommandTopic(topic, cmd)); len(payload) > room {
		return fmt.Errorf("%w: %w", ErrRefused, ErrBufferFull)
	}
	if err := ctx.Err(); err != nil { // a cancelled caller must not publish the password
		return fmt.Errorf("mqtt: %s not sent: %w", cmd, err)
	}
	if err := c.waitTurn(ctx); err != nil {
		return fmt.Errorf("mqtt: %s not sent: %w", cmd, err)
	}
	c.mu.Lock()
	c.pubs[topic]++ // counted before the publish: a publish that may have happened must count
	c.mu.Unlock()
	if err := c.bus.Command(ctx, topic, cmd, payload); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s may have been sent: %w", tasmota.ErrTimeout, cmd, err)
		}
		return fmt.Errorf("%w: %s not sent", tasmota.ErrUnreachable, cmd) // err dropped: the bus may quote the payload
	}
	return nil
}

// state is the status word of the direction suffix names.
func (m *fileMsg) state(suffix string) *string {
	if suffix == suffixUpload {
		return m.UpState
	}
	return m.Status
}

// downloadStart builds {"Password":…,"Type":2} in compact framing. A password the device's
// JSON parser might read differently (quote, backslash, control character) is refused instead
// of escaped: nothing is sent.
func downloadStart(password string) (string, error) {
	if err := checkFilePassword(password); err != nil {
		return "", err
	}
	return `{"Password":"` + password + `","Type":` + strconv.Itoa(downloadType) + `}`, nil
}

// checkFilePassword refuses a password that cannot be put into the start command verbatim. An empty one
// is sent as it is: it is the device's password when its MqttPassword is empty (#609); whether a device
// has a password to send at all is decided by the caller (model.FilePassword).
func checkFilePassword(password string) error {
	for _, r := range password {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: the MQTT password contains a character that cannot be sent in a file transfer command", ErrRefused)
		}
	}
	return nil
}
