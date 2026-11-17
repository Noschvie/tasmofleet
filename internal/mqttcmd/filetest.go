package mqttcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// CheckFilePassword refuses a password that a file transfer command cannot carry verbatim (ErrRefused, without
// the password in the error); the facts source asks it before any transfer is chosen. "" can be carried.
func CheckFilePassword(password string) error { return checkFilePassword(password) }

// TestPassword asks the device whether it accepts password as its MqttPassword, without a transfer
// (#609). It sends the start of a settings download, FileDownload {"Password":…,"Type":2}: Tasmota
// checks the password before it allocates anything and answers "Error 1" when it differs; "Started"
// means it was accepted. After "Started" the device holds a copy of its settings in RAM (nothing is
// written to flash); FileDownload 0 ends that at once. Tasmota answers that abort with one more chunk
// of the dump before "Aborted" (CmndFileDownload sends a chunk while its buffer is set,
// xdrv_02_1_mqtt_file.ino 16.0.0), so an accepted test puts the file header and one data chunk on the
// broker: up to 2980 B on an ESP8266 and 4480 B on an ESP32 (docs/research/tasmota-mqtt-transport.md
// 41-44), which is most of the settings dump or all of it, with every password stored there (Wi-Fi,
// web, MqttPassword), Tasmota-obfuscated only.
//
// nil: accepted. ErrPasswordRefused: refused. Anything else says nothing about the password; it is
// marked ErrPublished once anything was published, like Download. The topic is held for the whole
// test, and it opens with FileDownload 0 like a Download when the topic's state is unknown; a test
// never marks the topic clean, so the next Download opens with the abort again.
func (c *Channel) TestPassword(ctx context.Context, d model.Device, password string) (rerr error) {
	if err := checkTopic(d); err != nil {
		return err
	}
	start, err := downloadStart(password)
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
	before := c.published(d.Topic)
	defer func() {
		if rerr != nil && c.published(d.Topic) > before && !errors.Is(rerr, ErrPublished) {
			rerr = fmt.Errorf("%w: %w", ErrPublished, rerr)
		}
	}()
	if c.offline(d.Topic) {
		return fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	}

	ctx, cancel := context.WithTimeout(ctx, c.o.FileTotal)
	defer cancel()
	in := make(chan mqttbus.Message, waitDepth)
	c.mu.Lock()
	if err := c.firmwarePendingLocked(d.Topic); err != nil {
		c.mu.Unlock()
		return err
	}
	c.files[d.Topic] = in
	clean := c.fileClean[d.Topic]
	c.fileClean[d.Topic] = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.files, d.Topic)
		c.mu.Unlock()
	}()

	if !clean {
		if err := c.sendFile(ctx, d.Topic, "0"); err != nil {
			return err
		}
		c.waitAborted(ctx, in)
	}
	if err := c.sendFile(ctx, d.Topic, start); err != nil {
		if errors.Is(err, tasmota.ErrTimeout) { // the start may have been published
			c.abort(d.Topic)
		}
		return err
	}
	for {
		m, err := c.nextFile(ctx, in, "the start")
		if err != nil {
			c.abort(d.Topic) // the start may have reached the device: leave it idle
			return err
		}
		switch {
		case m.Command != nil:
			// A firmware without FileDownload (minimal) echoed the command, password included.
			return transferErr("the device does not support FileDownload (minimal firmware?)")
		case m.Status == nil:
			continue // a leftover of an earlier transfer
		case *m.Status == "Started":
			// Accepted. End the download before any "?" asks for data; its own context, since the
			// caller's may be spent by now.
			c.abort(d.Topic)
			actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), c.o.ReplyWait)
			c.waitAborted(actx, in)
			acancel()
			return nil
		case strings.HasPrefix(*m.Status, "Error"):
			return errorStatus(*m.Status) // the device ended it itself (Error 1: the password was refused)
		}
	}
}
