package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Commander is the part of tasmota.Client the fixer sends with.
type Commander interface {
	Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error)
}

// Reader reads settings from a device (settings.Service.ReadKeys).
type Reader interface {
	ReadKeys(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error)
}

// Backlog limits: Tasmota queues at most 30 entries and a long line risks
// being cut; small chunks keep every request well inside both.
const (
	maxChunkCommands = 8
	maxChunkBytes    = 300
)

// Fixer applies a plan's commands to one device and checks the result.
type Fixer struct {
	Client Commander
	Reader Reader
	// Wait pauses between verify attempts; nil waits for real. A device that
	// restarts after Hostname or Topic needs a few seconds to answer again.
	Wait func(ctx context.Context, d time.Duration) error
	// VerifyAttempts is how often the read-back is tried while the device does
	// not answer (default 6).
	VerifyAttempts int
	// Stop (may be nil) is done when TasmoFleet is stopping. It is not the context of a run, which
	// can end by its own deadline; it ends the save that follows a failed chunk at once, because
	// a container is killed a few seconds after the stop signal (see saveAfterFailure).
	Stop context.Context
}

type fixLine struct{ key, value, text string }

// parseLines checks every line the way the plan built it: a known setting and
// a valid value. Anything else is refused before a single command is sent.
func parseLines(lines []string) ([]fixLine, error) {
	if len(lines) == 0 {
		return nil, errors.New("no commands to apply")
	}
	seen := map[string]bool{}
	out := make([]fixLine, 0, len(lines))
	for _, l := range lines {
		key, value, ok := strings.Cut(l, " ")
		if !ok {
			return nil, fmt.Errorf("command %q has no value", l)
		}
		text, err := CommandText(key, value)
		if err != nil {
			return nil, fmt.Errorf("command %q: %w", l, err)
		}
		if text != l {
			return nil, fmt.Errorf("command %q is not in its canonical form %q", l, text)
		}
		if seen[strings.ToLower(key)] {
			return nil, fmt.Errorf("%s is set twice", key)
		}
		seen[strings.ToLower(key)] = true
		out = append(out, fixLine{key, value, text})
	}
	return out, nil
}

// Apply sends the commands batched with Backlog, in the given order, and then
// writes them to flash with exactly one SaveData. No save is added when the
// batch saves by itself: it contains SaveData (whose handler saves everything)
// or a setting that restarts the device. Nothing is sent when the lines are
// invalid or the current SaveData value (needed to save without changing it)
// cannot be read.
func (f *Fixer) Apply(ctx context.Context, d model.Device, lines []string) error {
	parsed, err := parseLines(lines)
	if err != nil {
		return err
	}
	saves, restarts := false, false
	for _, l := range parsed {
		saves = saves || strings.EqualFold(l.key, "SaveData")
		restarts = restarts || settings.RestartsDevice(l.key, l.value)
	}
	keep := ""
	if !saves && !restarts {
		got, err := f.Reader.ReadKeys(ctx, d.ID, []string{"SaveData"})
		if keep = got["SaveData"]; err != nil || keep == "" {
			return errors.New("nothing was sent: the current SaveData value could not be read, so the changes could not be written to flash")
		}
	}
	t := tasmota.TargetFor(d)
	// The chunks are those of always. Only when the router refuses one (ErrCutOff: a line that can cut off a
	// device reached over MQTT only, and Make compliant never has a person's consent) is it known that
	// nothing of it was sent: the lines that are not the cut-off ones go again, the others are skipped.
	var skipped []fixLine
	var applied []fixLine // the lines of the chunks that went out (no read-back yet)
	var failed []fixLine  // the chunk whose send failed: it may or may not have reached the device
	var send func(chunk []fixLine) error
	send = func(chunk []fixLine) error {
		texts := make([]string, len(chunk))
		restart := false
		for i, l := range chunk {
			texts[i] = l.text
			restart = restart || settings.RestartsDevice(l.key, l.value)
		}
		_, err := f.Client.Command(ctx, t, "Backlog "+strings.Join(texts, "; "))
		switch {
		case err == nil:
			applied = append(applied, chunk...)
		case errors.Is(err, transport.ErrCutOff):
			var others []fixLine
			for _, l := range chunk {
				if cmdpolicy.CutsMQTTOff(l.text) {
					skipped = append(skipped, l)
				} else {
					others = append(others, l)
				}
			}
			if len(others) == len(chunk) {
				failed = chunk
				return fmt.Errorf("sending %s: %w", strings.Join(texts, "; "), err) // refused, but not for a cut-off line: do not loop
			}
			for _, c := range chunks(others) {
				if err := send(c); err != nil {
					return err
				}
			}
		case restart && (errors.Is(err, tasmota.ErrUnreachable) || errors.Is(err, tasmota.ErrTimeout)):
			// the device is restarting; the read-back tells whether it applied
			applied = append(applied, chunk...)
		default:
			failed = chunk
			return fmt.Errorf("sending %s: %w", strings.Join(texts, "; "), err)
		}
		return nil
	}
	for _, chunk := range chunks(parsed) {
		if err := send(chunk); err != nil {
			return f.saveAfterFailure(ctx, d, t, keep, partialRun{parsed, applied, failed, skipped}, err)
		}
	}
	if len(skipped) == len(parsed) {
		return &SkippedError{Skipped: skipped}
	}
	if keep == "" && !saves && len(skipped) > 0 && !restartsAfterSkips(parsed, skipped) {
		// The lines that would have restarted (and so saved) the device were left out: save the rest.
		got, err := f.Reader.ReadKeys(ctx, d.ID, []string{"SaveData"})
		if keep = got["SaveData"]; err != nil || keep == "" {
			return errors.New("the settings are applied but the current SaveData value could not be read, so they were not written to flash")
		}
	}
	if keep != "" {
		if _, err := f.Client.Command(ctx, t, saveCommand(keep)); err != nil {
			return fmt.Errorf("the settings are applied but could not be written to flash: %w", err)
		}
	}
	if len(skipped) > 0 {
		return &SkippedError{Applied: len(parsed) - len(skipped), Skipped: skipped}
	}
	return nil
}

// saveTimeout is the time the write to flash after a failed chunk gets. It is a limit of its own:
// the run that failed may have used up its whole deadline, and the save is the one thing that
// still has to go out (the SaveData value is read with it). A stop of TasmoFleet ends the save
// before that: a container is killed a few seconds after the stop signal, so a save that waits on
// a device must not hold shutdown up (see saveAfterFailure).
const saveTimeout = 15 * time.Second

const notSavedStopping = "because TasmoFleet was stopping"

// stopping reports whether TasmoFleet is stopping.
func (f *Fixer) stopping() bool { return f.Stop != nil && f.Stop.Err() != nil }

// partialRun is what Apply knows when a chunk failed: every line of the plan, the ones sent
// (the device answered; nothing is read back yet), the chunk whose send failed (it may or may not
// have reached the device) and the ones left out on purpose (SkippedError). The rest of the
// plan was never sent.
type partialRun struct{ parsed, sent, failed, leftOut []fixLine }

// saveAfterFailure ends a run in which a chunk failed after earlier ones went out. Those lines
// are live on the device; with SaveData 0 (or a long save interval) they would vanish on its next
// restart, so they are written to flash first, with a context of their own, unless one of them
// saved by itself. The error says what is known: which lines were sent, whether they were saved,
// which chunk failed, and which lines were not sent (the later ones, and the ones left out on
// purpose), and still wraps the failure. With nothing sent the device is untouched and cause is
// returned as it is.
//
// TasmoFleet stopping (the engine cancels its jobs; Fixer.Stop is done) takes precedence: the stop
// signal gives a container only a few seconds, and a device that does not answer would hold the
// save up for saveTimeout. A run that was cancelled does not save at all, and a stop that arrives
// while the save runs ends it at once. In both cases the error and the log say that the lines
// were not written to flash because TasmoFleet was stopping; the job is resumed from its stored
// state at the next start, which sends and saves the whole plan again. A run that failed on its
// deadline is still saved, with a context of its own.
func (f *Fixer) saveAfterFailure(ctx context.Context, d model.Device, t tasmota.Target, keep string, run partialRun, cause error) error {
	if len(run.sent) == 0 {
		return cause
	}
	notSaved := "" // why the lines are not in flash; empty when they are
	if !savesItself(run.sent) {
		switch {
		case errors.Is(ctx.Err(), context.Canceled) || f.stopping():
			notSaved = notSavedStopping
		default:
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
			defer cancel()
			if f.Stop != nil {
				defer context.AfterFunc(f.Stop, cancel)() // a stop that comes now ends the save at once
			}
			if err := f.saveKeeping(sctx, d, t, keep); err != nil {
				notSaved = fmt.Sprintf("(%v)", err)
				if f.stopping() {
					notSaved = notSavedStopping
				}
			}
		}
		if notSaved == notSavedStopping {
			slog.Warn("policy: stopped before the settings that were sent were written to flash", "device", d.ID, "sent", len(run.sent), "of", len(run.parsed))
		}
	}
	var later []fixLine // never sent: after the failed chunk
	for _, l := range run.parsed {
		if !slices.Contains(run.sent, l) && !slices.Contains(run.failed, l) && !slices.Contains(run.leftOut, l) {
			later = append(later, l)
		}
	}
	msg := fmt.Sprintf("partly sent: %d of %d settings were sent and are written to flash, not read back (%s)", len(run.sent), len(run.parsed), keyNames(run.sent))
	if notSaved != "" {
		msg = fmt.Sprintf("partly sent: %d of %d settings were sent but are NOT written to flash %s, so a restart of the device loses them, and not read back (%s)", len(run.sent), len(run.parsed), notSaved, keyNames(run.sent))
	}
	msg += "; the failed step (" + keyNames(run.failed) + ") may or may not have reached the device"
	if len(later) > 0 {
		msg += "; not sent: " + keyNames(later)
	}
	if len(run.leftOut) > 0 {
		msg += "; not sent, left out on purpose (reached over MQTT only): " + keyNames(run.leftOut)
	}
	return fmt.Errorf("%s: %w", msg, cause)
}

// savesItself reports whether a line that was sent writes the device's settings to flash: a
// SaveData or a setting that restarts the device.
func savesItself(sent []fixLine) bool {
	for _, l := range sent {
		if strings.EqualFold(l.key, "SaveData") || settings.RestartsDevice(l.key, l.value) {
			return true
		}
	}
	return false
}

// saveKeeping writes the settings to flash with SaveData, re-sending the current value (keep, read
// before the first chunk, or read now when the batch needed none) so that it does not change.
func (f *Fixer) saveKeeping(ctx context.Context, d model.Device, t tasmota.Target, keep string) error {
	if keep == "" {
		got, err := f.Reader.ReadKeys(ctx, d.ID, []string{"SaveData"})
		if keep = got["SaveData"]; err != nil || keep == "" {
			return errors.New("the current SaveData value could not be read")
		}
	}
	if _, err := f.Client.Command(ctx, t, saveCommand(keep)); err != nil {
		return err
	}
	return nil
}

// saveCommand is the save that follows the chunks. Tasmota runs a Backlog from a queue, one
// command per SetOption34 delay, but a command sent on its own at once: a plain "SaveData" would
// write flash before the queued lines ran and lose them at the next restart (#559). Queued
// behind them (the queue is first in, first out) it runs after the last one.
func saveCommand(keep string) string { return "Backlog SaveData " + keep }

// keyNames lists the settings of the lines by name.
func keyNames(lines []fixLine) string {
	names := make([]string, len(lines))
	for i, l := range lines {
		names[i] = l.key
	}
	return strings.Join(names, ", ")
}

// restartsAfterSkips reports whether a line that was actually sent restarts the device.
func restartsAfterSkips(parsed, skipped []fixLine) bool {
	for _, l := range parsed {
		if settings.RestartsDevice(l.key, l.value) && !slices.Contains(skipped, l) {
			return true
		}
	}
	return false
}

// SkippedError says Apply sent what it could and left out the lines that can cut off a device reached over
// MQTT only. The lines it names were not sent and must not be verified. With Applied == 0 nothing was done.
type SkippedError struct {
	Applied int
	Skipped []fixLine
}

func (e *SkippedError) Error() string {
	names := make([]string, len(e.Skipped))
	for i, l := range e.Skipped {
		names[i] = l.key
	}
	if e.Applied == 0 {
		return fmt.Sprintf("nothing applied; skipped %s: reached over MQTT only (use the console)", strings.Join(names, ", "))
	}
	return fmt.Sprintf("applied %d; skipped %s: reached over MQTT only (use the console)", e.Applied, strings.Join(names, ", "))
}

// SkippedLines are the command lines that were left out.
func (e *SkippedError) SkippedLines() []string {
	out := make([]string, len(e.Skipped))
	for i, l := range e.Skipped {
		out[i] = l.text
	}
	return out
}

// AppliedAny reports whether anything was sent (a skipped-only result is a failure).
func (e *SkippedError) AppliedAny() bool { return e.Applied > 0 }

// chunks splits the lines into Backlog-sized runs, keeping their order. The
// restarting lines (from the first one on) are packed into runs of their own,
// so they are never split across a run boundary that plain lines could avoid:
// they join the last plain run only when all of them fit there.
// Why this is safe within one run: Tasmota only counts down a restart while
// the backlog is empty (Every250mSeconds, support_tasmota.ino:1580 with
// CommandsReady() = BACKLOG_EMPTY, support_tasmota.ino:1336), so every command
// queued by the same Backlog runs before restart_flag = 2 takes effect, and a
// second restarting command in the run still executes.
func chunks(lines []fixLine) [][]fixLine {
	cut := len(lines)
	for i, l := range lines {
		if settings.RestartsDevice(l.key, l.value) {
			cut = i
			break
		}
	}
	plain, tail := pack(lines[:cut]), pack(lines[cut:])
	if len(plain) > 0 && len(tail) == 1 {
		if merged := pack(append(append([]fixLine{}, plain[len(plain)-1]...), tail[0]...)); len(merged) == 1 {
			return append(plain[:len(plain)-1], merged[0])
		}
	}
	return append(plain, tail...)
}

// pack splits lines into runs within the Backlog limits.
func pack(lines []fixLine) [][]fixLine {
	var out [][]fixLine
	var cur []fixLine
	size := 0
	for _, l := range lines {
		if len(cur) > 0 && (len(cur) >= maxChunkCommands || size+len(l.text)+2 > maxChunkBytes) {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, l)
		size += len(l.text) + 2
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// Verify reads the changed settings back from the device and compares them
// with the wanted values. A device that does not answer (it may be restarting)
// is asked again a few times. It returns the values it read, also on a
// mismatch, so the caller can refresh what it shows.
func (f *Fixer) Verify(ctx context.Context, d model.Device, lines []string) (map[string]string, error) {
	parsed, err := parseLines(lines)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(parsed))
	for i, l := range parsed {
		keys[i] = l.key
	}
	attempts := f.VerifyAttempts
	if attempts < 1 {
		attempts = 6
	}
	// Over HTTP the reply to a Backlog comes before the device has run it: the first read can show
	// the old value of every line but the first (#559). A mismatch is therefore read again a few
	// times before it counts; the last read wins. The queue only moves while we wait (every command
	// the device handles pushes its timer back by SetOption34), so the first wait grows with the
	// number of settings that differ. A value the device really refuses costs about
	// settleReads seconds plus a quarter second per differing setting, no more.
	for settle := 0; ; settle++ {
		var got map[string]string
		for i := 0; ; i++ {
			got, err = f.Reader.ReadKeys(ctx, d.ID, keys)
			if err == nil {
				break
			}
			if i+1 >= attempts {
				return got, fmt.Errorf("the settings were sent but could not be read back: %w", err)
			}
			if err := f.wait(ctx, 5*time.Second); err != nil {
				return got, err
			}
		}
		var bad []string
		stale := 0 // values that differ (not absent): the queue may not have run them yet
		for _, l := range parsed {
			have, ok := got[l.key]
			switch {
			case !ok:
				bad = append(bad, l.key+" is not reported by the device")
			case !equal(have, l.value):
				bad = append(bad, fmt.Sprintf("%s is %s, expected %s", l.key, have, l.value))
				stale++
			}
		}
		if len(bad) == 0 {
			return got, nil
		}
		if stale == 0 || settle >= settleReads {
			return got, errors.New("after applying: " + strings.Join(bad, "; "))
		}
		pause := settleWait
		if settle == 0 {
			pause = max(pause, time.Duration(stale+1)*backlogLineWait)
		}
		if err := f.wait(ctx, pause); err != nil {
			return got, err
		}
	}
}

// settleReads and settleWait bound the wait for a Backlog to run. Tasmota has no cap on the queue
// (only 12.x had 30 entries), a plan can hold up to MaxRules (100) lines, and the device runs one
// per SetOption34 delay (200 ms by default, 255 ms at most; backlogLineWait) once it is left
// alone: each read pushes the queue back, so the first wait covers every line still wrong.
const (
	settleReads     = 6
	settleWait      = time.Second
	backlogLineWait = 250 * time.Millisecond
)

func (f *Fixer) wait(ctx context.Context, d time.Duration) error {
	if f.Wait != nil {
		return f.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
