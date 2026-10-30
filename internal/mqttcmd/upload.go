package mqttcmd

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: the Tasmota file transfer protocol mandates MD5
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// The two file types the uploader pushes. Nothing else: it must not become a generic file pusher.
const (
	UploadFirmware = 1 // UPL_TASMOTA: OTA firmware (D19, experimental)
	UploadSettings = 2 // UPL_SETTINGS: a settings dump (restore)

	maxUploadSize = 1<<31 - 1
	maxNameLen    = 64
)

// ErrNoRoom: the device answered "Error 2" to FileUpload: the image does not fit its OTA space, or
// the settings buffer cannot be allocated. Nothing was written. It also matches ErrTransfer.
var ErrNoRoom = errors.New("mqttcmd: the device has no room for the file (Error 2)")

// ErrOutcomeUnknown: every byte was sent but the device never confirmed ("Done" lost, or the
// connection ended while waiting). The device may well have applied the upload and be restarting.
// Callers probe the device (settings: BootCount; firmware: version) and never resend.
var ErrOutcomeUnknown = errors.New("mqttcmd: the upload was sent but the device never confirmed it; check the device, do not resend")

// Upload sends r (size bytes) to d with FileUpload: typ 2 is a settings dump, typ 1 a firmware
// image. name is the File field. progress, if set, is called with the bytes acknowledged so far.
// The overall time limit is ctx; each message waits at most ReplyWait.
//
// Protocol (docs/research/tasmota-mqtt-transport.md section 3, live test 2026-10-30):
// start {"Password","File","Id","Type","Size"} -> "Started" and {"Id","MaxSize"}; then
// {"Id","Data"} chunks of the latest MaxSize, one acknowledgement each; then {"Id","Md5"} ->
// "Done". The device has one transfer state for both directions, so an upload after an unclean
// transfer (this process cannot tell what the device holds) opens with "FileUpload 0".
//
// After a start was (maybe) published, every failure except the device's own "Error N" ends with
// "FileUpload 0". Nothing is retried or resent. No payload is ever logged or put into an error:
// the start carries the password, and a minimal firmware echoes it back.
func (c *Channel) Upload(ctx context.Context, d model.Device, typ int, name string, size int64, r io.Reader, password string, progress func(sent int64)) error {
	if err := checkUpload(d, typ, name, size, r); err != nil {
		return err
	}
	id, err := newUploadID()
	if err != nil {
		return err
	}
	start, err := uploadStart(password, name, id, typ, size)
	if err != nil {
		return err
	}
	if !c.bus.Connected() {
		return fmt.Errorf("%w: %w", tasmota.ErrUnreachable, mqttbus.ErrNotConnected)
	}
	sem := c.sem(d.Topic)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err() // still queued: nothing was sent
	case <-c.done:
		return c.closed()
	}
	defer func() { <-sem }()
	if c.offline(d.Topic) {
		return fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	}

	in := make(chan mqttbus.Message, waitDepth)
	c.mu.Lock()
	c.files[d.Topic] = in
	clean := c.fileClean[d.Topic] // shared with Download: one FMqtt state on the device
	c.fileClean[d.Topic] = false  // clean again only after "Done"
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.files, d.Topic)
		c.mu.Unlock()
	}()

	if !clean {
		if err := c.sendFileCmd(ctx, d.Topic, cmdUpload, "0"); err != nil {
			return err
		}
		c.waitAbortedOf(ctx, in, suffixUpload)
	}
	sent, err := c.runUpload(ctx, d.Topic, start, id, typ, size, r, in, progress)
	if err != nil {
		if sent && !deviceReset(err) {
			c.abortCmd(d.Topic, cmdUpload) // the start may have reached the device: leave it idle
		}
		return err
	}
	c.mu.Lock()
	c.fileClean[d.Topic] = true
	c.mu.Unlock()
	return nil
}

func checkUpload(d model.Device, typ int, name string, size int64, r io.Reader) error {
	if err := checkTopic(d); err != nil {
		return err
	}
	if typ != UploadFirmware && typ != UploadSettings {
		return fmt.Errorf("%w: FileUpload type %d is not supported (only 1 and 2)", ErrRefused, typ)
	}
	if r == nil {
		return fmt.Errorf("%w: no data to upload", ErrRefused)
	}
	if name == "" || len(name) > maxNameLen {
		return fmt.Errorf("%w: the file name must be 1 to %d characters", ErrRefused, maxNameLen)
	}
	for _, ch := range name {
		if ch == '"' || ch == '\\' || ch < 0x20 || ch == 0x7f {
			return fmt.Errorf("%w: the file name contains a character that cannot be sent", ErrRefused)
		}
	}
	switch {
	case size <= 0 || size > maxUploadSize:
		return fmt.Errorf("%w: invalid upload size %d", ErrRefused, size)
	case typ == UploadSettings && (size < minDumpSize || size > maxDumpSize):
		return fmt.Errorf("%w: a settings dump has %d to %d bytes, not %d", ErrRefused, minDumpSize, maxDumpSize, size)
	}
	return nil
}

// newUploadID picks the transfer Id. The device keeps file_id in a uint8_t: a larger value is
// truncated there and the acknowledgements would no longer carry ours (the plan said 1..999).
func newUploadID() (int, error) {
	var b [1]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("mqttcmd: no random transfer id: %w", err)
		}
		if b[0] != 0 {
			return int(b[0]), nil
		}
	}
}

// uploadStart builds {"Password":…,"File":…,"Id":…,"Type":…,"Size":…} in compact framing.
func uploadStart(password, name string, id, typ int, size int64) (string, error) {
	if err := checkFilePassword(password); err != nil {
		return "", err
	}
	return `{"Password":"` + password + `","File":"` + name + `","Id":` + strconv.Itoa(id) +
		`,"Type":` + strconv.Itoa(typ) + `,"Size":` + strconv.FormatInt(size, 10) + `}`, nil
}

// uploadErrorStatus maps "Error N" of FileUpload to a typed error (the device ended the transfer).
func uploadErrorStatus(s string, typ int) error {
	n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "Error")))
	switch n {
	case 1:
		return &deviceResetError{ErrPasswordRefused}
	case 2:
		what := "the settings buffer cannot be allocated"
		if typ == UploadFirmware {
			what = "the image does not fit"
		}
		return &deviceResetError{fmt.Errorf("%w: %w: the device refused: %s (Error 2)", ErrTransfer, ErrNoRoom, what)}
	case 3:
		return &deviceResetError{transferErr("the device refused: the file type is not supported (Error 3)")}
	case 5:
		return &deviceResetError{transferErr("the device refused: the command arrived on a group topic (Error 5)")}
	}
	return &deviceResetError{transferErr("the device refused the upload (Error %d)", n)}
}

// runUpload returns sent=true once the start was (maybe) published.
func (c *Channel) runUpload(ctx context.Context, topic, start string, id, typ int, size int64, r io.Reader, in chan mqttbus.Message, progress func(int64)) (sent bool, err error) {
	if err := c.sendFileCmd(ctx, topic, cmdUpload, start); err != nil {
		// A timeout here means the publish was attempted; a refusal or a spent context sent nothing.
		return errors.Is(err, tasmota.ErrTimeout), err
	}
	// Anything before "Started" is a leftover of an earlier transfer (an "Aborted", a stray chunk).
	for {
		m, err := c.nextFileOf(ctx, in, "the start", suffixUpload)
		if err != nil {
			return true, err
		}
		switch {
		case m.Command != nil:
			// A firmware without FileUpload (minimal, safeboot) echoed the command, password included.
			return true, &deviceResetError{transferErr("the device does not support FileUpload (minimal or safeboot firmware?)")}
		case m.UpState == nil:
			continue
		case *m.UpState == "Started":
		case strings.HasPrefix(*m.UpState, "Error"):
			return true, uploadErrorStatus(*m.UpState, typ)
		default:
			continue
		}
		break
	}

	// ack reads the {"Id","MaxSize"} after the start or a chunk.
	ack := func(what string) (int, error) {
		m, err := c.nextFileOf(ctx, in, what, suffixUpload)
		if err != nil {
			return 0, err
		}
		if m.UpState != nil {
			if strings.HasPrefix(*m.UpState, "Error") {
				return 0, uploadErrorStatus(*m.UpState, typ)
			}
			return 0, transferErr("the device ended the transfer (%s)", boundedStatus(*m.UpState))
		}
		if m.ID == nil || *m.ID != id {
			return 0, transferErr("a message of another transfer arrived")
		}
		if m.MaxSize == nil || *m.MaxSize <= 0 {
			return 0, transferErr("the device acknowledged %s without a chunk size", what)
		}
		return *m.MaxSize, nil
	}
	maxSize, err := ack("the start")
	if err != nil {
		return true, err
	}

	sum := md5.New() //nolint:gosec // see import
	var done int64
	buf := make([]byte, 0, 4096)
	for done < size {
		n := int(min(int64(maxSize), size-done)) // the latest MaxSize: the device may shrink it
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return true, transferErr("the data ended after %d of %d bytes", done, size)
		}
		sum.Write(buf)
		payload := `{"Id":` + strconv.Itoa(id) + `,"Data":"` + base64.StdEncoding.EncodeToString(buf) + `"}`
		if err := c.sendFileCmd(ctx, topic, cmdUpload, payload); err != nil {
			return true, err
		}
		if maxSize, err = ack(fmt.Sprintf("the chunk at byte %d", done)); err != nil {
			return true, err
		}
		done += int64(n)
		if progress != nil {
			progress(done)
		}
	}
	var extra [1]byte
	if n, _ := r.Read(extra[:]); n > 0 {
		return true, transferErr("the data is longer than the announced %d bytes", size)
	}

	md5msg := `{"Id":` + strconv.Itoa(id) + `,"Md5":"` + hex.EncodeToString(sum.Sum(nil)) + `"}`
	if err := c.sendFileCmd(ctx, topic, cmdUpload, md5msg); err != nil {
		if errors.Is(err, tasmota.ErrTimeout) { // maybe published
			return true, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
		}
		return true, err
	}
	m, err := c.nextFileOf(ctx, in, "the final confirmation", suffixUpload)
	if err != nil {
		return true, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	if m.UpState == nil {
		return true, transferErr("the device answered the MD5 with something other than a status")
	}
	switch st := *m.UpState; {
	case st == "Done":
		return true, nil
	case strings.HasPrefix(st, "Error"):
		return true, uploadErrorStatus(st, typ)
	case st == "Failed" && typ == UploadFirmware:
		return true, transferErr("the device rejected the image after the upload (Failed); nothing was switched")
	case st == "Failed":
		return true, transferErr("the device rejected the settings dump after the upload (Failed)")
	case strings.EqualFold(st, "MD5 mismatch"):
		return true, transferErr("the device computed another MD5 than we sent; nothing was applied")
	default:
		return true, transferErr("the device ended the transfer (%s)", boundedStatus(st))
	}
}
