// Package importer turns TasmoAdmin data files (devices.csv, MyConfig.json,
// .device-password.key) into an inventory import: Plan is a dry run that
// reads the files and decides create/merge/skip per row without touching
// devices or the database, Apply writes the plan, probing devices (bounded)
// for MAC and identity first. The CLI (tasmofleet import) and the web upload
// (POST /import/tasmoadmin) both use it, so they produce the same plan.
//
// The inputs are untrusted uploads. Nothing here touches the disk; readers are
// capped, the key is used in memory only, and no decrypted password appears in
// an error, a log line or the Report. Byte slices the importer owns are
// cleared after use as a best effort; Go cannot guarantee that no copy
// (strings, cipher state, I/O buffers) stays in memory until the next GC.
package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmoadmin"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Action values of Row.Action.
const (
	ActionCreate = "create"
	ActionMerge  = "merge"
	ActionSkip   = "skip"
)

// Limits. Real TasmoAdmin files are a few KiB; a fleet has tens of devices.
const (
	MaxFileBytes      = 1 << 20 // per input file
	MaxRows           = 1000
	MaxReportedErrors = 50 // row errors kept in a Plan; the rest is only counted
	maxKeyBytes       = 4096
)

// Errors.
var (
	ErrTooLarge = errors.New("importer: input exceeds the size limit")
	ErrTooMany  = errors.New("importer: too many rows")
	ErrNoInput  = errors.New("importer: devices.csv is missing")
)

// Plan is the result of a dry run. Rows carry the devices including their
// decrypted passwords, so a Plan must stay in memory: it has no String and
// logs only counts.
type Plan struct {
	Rows   []Row
	Errors []tasmoadmin.RowError // at most MaxReportedErrors
	// ErrorsOmitted counts row errors beyond MaxReportedErrors.
	ErrorsOmitted int
	// Rejected counts CSV lines that produced no row at all. A row that only
	// carries a warning (bad port, undecryptable password) is planned and not
	// rejected, and a line with several errors counts once.
	Rejected int
	Warnings []string // MyConfig.json problems
	Hint     string   // where TasmoAdmin served firmware from
	Probe    bool     // Apply probes the devices
}

// LogValue keeps passwords out of structured logs.
func (p Plan) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("rows", len(p.Rows)), slog.Int("errors", len(p.Errors)), slog.Bool("probe", p.Probe))
}

// Row is one device of the plan.
type Row struct {
	Device model.Device
	Action string // ActionCreate, ActionMerge or ActionSkip
	Reason string
}

// HasPassword tells the UI whether a password was imported without exposing it.
func (r Row) HasPassword() bool { return r.Device.HTTPPassword != "" }

// Report is the outcome of Apply.
type Report struct {
	Created, Updated, Failed, Unprobed, Skipped int
	Lines                                       []Line
	Hint                                        string
}

// Line is one reported device.
type Line struct {
	Name, Address, Status, Detail string // Status: created, updated, failed, skipped
	Port                          int    // 0: the default port
}

// Service is the contract shared by the CLI and the web.
type Service interface {
	// Plan is a dry run: it never writes and never contacts a device.
	Plan(ctx context.Context, devicesCSV, myConfig, keyFile io.Reader, probe bool) (Plan, error)
	// Apply writes the plan; with plan.Probe it probes the devices first.
	Apply(ctx context.Context, actor string, p Plan) (Report, error)
}

// Inventory is the part of inventory.Service the importer uses.
type Inventory interface {
	List(ctx context.Context) ([]model.Device, error)
	Upsert(ctx context.Context, actor string, d model.Device) (model.DeviceID, bool, error)
}

// AddressChecker is optionally implemented by the Inventory (inventory.Manager):
// it applies the inventory's own address rules, so a plan can mark a row that
// Upsert would refuse (an address outside the device networks) as skipped
// instead of promising a create that fails on apply.
type AddressChecker interface {
	CheckAddress(address string) error
}

// Prober learns MAC and identity from a device (app.Prober).
type Prober interface {
	Describe(ctx context.Context, d model.Device) (model.Device, error)
}

// Options configure New.
type Options struct {
	Inventory Inventory
	Prober    Prober         // nil: never probe
	Audit     audit.Recorder // optional
	// Concurrency bounds the probes in flight (default 4); ProbeTimeout bounds one probe (default 20 s).
	Concurrency  int
	ProbeTimeout time.Duration
}

// Importer implements Service.
type Importer struct{ o Options }

// New builds an Importer.
func New(o Options) *Importer {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = 20 * time.Second
	}
	return &Importer{o: o}
}

var _ Service = (*Importer)(nil)

// readCapped reads at most MaxFileBytes and fails loudly above it.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}

// Plan implements Service.
func (im *Importer) Plan(ctx context.Context, devicesCSV, myConfig, keyFile io.Reader, probe bool) (Plan, error) {
	plan := Plan{Probe: probe && im.o.Prober != nil}
	if devicesCSV == nil {
		return plan, ErrNoInput
	}
	var key []byte
	if keyFile != nil {
		b, err := readCapped(keyFile, maxKeyBytes)
		if err != nil {
			return plan, fmt.Errorf("key file: %w", err)
		}
		key = bytes.TrimSpace(b)
		if len(key) == 0 {
			key = nil
		}
		defer clear(b) // best effort, see the package comment
	}
	raw, err := readCapped(devicesCSV, MaxFileBytes)
	if err != nil {
		return plan, fmt.Errorf("devices.csv: %w", err)
	}
	devs, rowErrs, err := tasmoadmin.ParseDevices(bytes.NewReader(raw), key)
	if err != nil {
		return plan, err // the message never carries the key
	}
	if len(devs)+len(rowErrs) > MaxRows || errors.Is(errors.Join(rowErrs...), tasmoadmin.ErrTooManyRecords) {
		return plan, fmt.Errorf("%w: more than %d rows (limit %d)", ErrTooMany, MaxRows, MaxRows)
	}
	rejected := map[int]bool{}
	for _, e := range rowErrs {
		var line *tasmoadmin.RowError
		if errors.As(e, &line) && !errors.Is(e, tasmoadmin.ErrBadPort) && !errors.Is(e, tasmoadmin.ErrEncryptedNoKey) && !errors.Is(e, tasmoadmin.ErrDecrypt) {
			rejected[line.Line] = true
		}
		if len(plan.Errors) >= MaxReportedErrors {
			plan.ErrorsOmitted++
			continue
		}
		var re *tasmoadmin.RowError
		if errors.As(e, &re) {
			plan.Errors = append(plan.Errors, *re)
		} else {
			plan.Errors = append(plan.Errors, tasmoadmin.RowError{Err: e})
		}
	}
	plan.Rejected = len(rejected)
	if myConfig != nil {
		b, err := readCapped(myConfig, MaxFileBytes)
		if err == nil {
			var cfg tasmoadmin.Config
			if cfg, err = tasmoadmin.ParseConfig(bytes.NewReader(b)); err == nil && cfg.OtaServerIP != "" {
				plan.Hint = fmt.Sprintf("TasmoAdmin served firmware from %s:%s; set TASMOFLEET_FIRMWARE_BASE_URL to the address devices will use for tasmofleet", cfg.OtaServerIP, cfg.OtaServerPort)
			}
		}
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("MyConfig.json ignored: %v", err))
		}
	}
	existing, err := im.o.Inventory.List(ctx)
	if err != nil {
		return plan, err
	}
	var refuse func(model.Device) string
	if ac, ok := im.o.Inventory.(AddressChecker); ok {
		refuse = func(d model.Device) string {
			switch err := ac.CheckAddress(d.Address); {
			case err == nil:
				return ""
			case errors.Is(err, inventory.ErrOutsideNetwork):
				return "address is outside the configured device networks"
			default:
				return "address is not valid"
			}
		}
	}
	plan.Rows = classify(devs, existing, refuse)
	return plan, nil
}

// classify decides per row. Matching mirrors inventory.Upsert before the
// probe (topic, then address and port); the MAC is only known after probing.
func classify(devs, existing []model.Device, refuse func(model.Device) string) []Row {
	byTopic := map[string]model.Device{}
	byAddr := map[string]model.Device{}
	for _, e := range existing {
		if e.Topic != "" {
			byTopic[e.Topic] = e
		}
		byAddr[addrKey(e)] = e
	}
	seenTopic := map[string]int{}
	seenAddr := map[string]int{}
	rows := make([]Row, 0, len(devs))
	for i, d := range devs {
		row := Row{Device: d, Action: ActionCreate}
		why := ""
		if refuse != nil {
			why = refuse(d)
		}
		switch {
		case strings.TrimSpace(d.Name) == "":
			row.Action, row.Reason = ActionSkip, "no usable name"
		case why != "":
			row.Action, row.Reason = ActionSkip, why
		case seenAddr[addrKey(d)] != 0:
			row.Action, row.Reason = ActionSkip, fmt.Sprintf("same address as row %d", seenAddr[addrKey(d)])
		case d.Topic != "" && seenTopic[d.Topic] != 0:
			row.Action, row.Reason = ActionSkip, fmt.Sprintf("same topic as row %d", seenTopic[d.Topic])
		default:
			if e, ok := byTopic[d.Topic]; ok && d.Topic != "" {
				row.Action, row.Reason = predictMerge(e, d, "topic matches "+e.Name)
			} else if e, ok := byAddr[addrKey(d)]; ok {
				row.Action, row.Reason = predictMerge(e, d, "address matches "+e.Name)
			}
		}
		if row.Action != ActionSkip {
			seenAddr[addrKey(d)] = i + 1
			if d.Topic != "" {
				seenTopic[d.Topic] = i + 1
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// predictMerge applies the probe-free identity rules of inventory.Upsert to a
// matched record, so the preview does not promise a merge that Upsert will
// refuse (it never writes credentials to, or moves, a record it cannot
// vouch for). Rules that need the device's MAC are only known after the probe
// and are checked by Upsert when applying.
func predictMerge(existing, in model.Device, why string) (action, reason string) {
	if addrKey(existing) != addrKey(in) {
		switch {
		case existing.MAC == "":
			return ActionSkip, "will be refused: " + existing.Name + " is not bound to a MAC, its address cannot be moved by an import"
		case existing.HasCredentials() || in.HasCredentials():
			return ActionSkip, "will be refused: " + existing.Name + " has credentials, its address can only be changed by editing the device"
		}
		return ActionMerge, why + "; the new address is verified against the MAC when applying"
	}
	return ActionMerge, why
}

func addrKey(d model.Device) string {
	port := d.Port
	if port == 0 {
		port = 80
	}
	return fmt.Sprintf("%s:%d", strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Address), ".")), port)
}

// Apply implements Service. Probes run concurrently but bounded, the writes
// run in row order so the report is stable. A cancelled context returns the
// partial report with the context error.
func (im *Importer) Apply(ctx context.Context, actor string, p Plan) (Report, error) {
	rep := Report{Hint: p.Hint, Lines: make([]Line, 0, len(p.Rows))}
	devs := make([]model.Device, len(p.Rows))
	probeErr := make([]error, len(p.Rows))
	for i, r := range p.Rows {
		devs[i] = r.Device
	}
	if p.Probe && im.o.Prober != nil {
		im.probeAll(ctx, p, devs, probeErr)
	}
	var err error
	for i, r := range p.Rows {
		d := devs[i]
		line := Line{Name: d.Name, Address: d.Address, Port: d.Port}
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
			break
		}
		if r.Action == ActionSkip {
			line.Status, line.Detail = "skipped", r.Reason
			rep.Skipped++
			rep.Lines = append(rep.Lines, line)
			continue
		}
		if probeErr[i] != nil {
			rep.Unprobed++
			line.Detail = "not probed: " + probeProblem(probeErr[i])
		}
		_, created, uerr := im.o.Inventory.Upsert(ctx, actor, d)
		switch {
		case uerr != nil:
			rep.Failed++
			line.Status, line.Detail = "failed", storeProblem(uerr)
		case created:
			rep.Created++
			line.Status = "created"
		default:
			rep.Updated++
			line.Status = "updated"
		}
		rep.Lines = append(rep.Lines, line)
	}
	if im.o.Audit != nil {
		result := "ok"
		if err != nil {
			result = "interrupted: " + err.Error()
		}
		im.o.Audit.Record(ctx, model.AuditEvent{
			Actor: actor, Action: "import.tasmoadmin", Target: "inventory", Result: result,
			Detail: fmt.Sprintf(`{"rows":%d,"created":%d,"updated":%d,"failed":%d,"unprobed":%d,"skipped":%d,"probe":%t}`,
				len(p.Rows), rep.Created, rep.Updated, rep.Failed, rep.Unprobed, rep.Skipped, p.Probe),
		})
	}
	return rep, err
}

// probeAll fills devs from the devices, at most Concurrency at a time. A
// device that does not answer keeps its imported values (probeErr is set).
func (im *Importer) probeAll(ctx context.Context, p Plan, devs []model.Device, probeErr []error) {
	sem := make(chan struct{}, im.o.Concurrency)
	var wg sync.WaitGroup
	for i, r := range p.Rows {
		if r.Action == ActionSkip {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			pctx, cancel := context.WithTimeout(ctx, im.o.ProbeTimeout)
			defer cancel()
			probed, err := im.o.Prober.Describe(pctx, devs[i])
			if err != nil {
				probeErr[i] = err
				return
			}
			probed.Name = devs[i].Name // the imported name wins
			devs[i] = probed
		}()
	}
	wg.Wait()
}

// probeProblem and storeProblem turn the error chains of a probe and of the
// inventory into one sentence for the report. The chains ("inventory: invalid
// device: inventory: address outside ...") are for developers, not for the page.
func probeProblem(err error) string {
	switch {
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return "the address is outside the configured device networks."
	case errors.Is(err, tasmota.ErrUnreachable):
		return "no device answered at this address."
	case errors.Is(err, tasmota.ErrTimeout):
		return "the device took too long to answer."
	case errors.Is(err, tasmota.ErrAuth):
		return "the device asked for a web password."
	case errors.Is(err, tasmota.ErrBadResponse):
		return "the answer could not be read as a Tasmota device."
	}
	return "the probe failed."
}

func storeProblem(err error) string {
	switch {
	case errors.Is(err, inventory.ErrOutsideNetwork):
		return "The address is outside the configured device networks."
	case errors.Is(err, inventory.ErrIdentityMismatch):
		return "A device with a different MAC is already stored at this address or topic."
	case errors.Is(err, inventory.ErrNeedsVerification), errors.Is(err, inventory.ErrMoveUnverified):
		return "The stored device cannot be moved by an import. Edit its address on the device page."
	case errors.Is(err, inventory.ErrConflict):
		return "The device changed while the import ran. Check it and import again."
	case errors.Is(err, inventory.ErrInvalid):
		return "The inventory refused the values of this row."
	}
	return "The inventory could not store this device."
}
