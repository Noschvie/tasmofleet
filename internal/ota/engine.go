package ota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hellwand/tasmofleet/internal/audit"
	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/clock"
	"github.com/hellwand/tasmofleet/internal/fwselect"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// DeviceAPI is what the engine needs from a device. The production
// implementation wraps tasmota.Client; tests use the simulator or a fake.
type DeviceAPI interface {
	// ProbeIdentity runs Status 2 (and the bare Upgrade probe if Status 2 is
	// unknown), Status 4, Status 11 and OtaUrl sequentially (never Status 0).
	ProbeIdentity(ctx context.Context, d model.Device) (ident model.Identity, otaURL string, uptime int64, err error)
	// Probe is the cheap liveness/version check used while waiting.
	Probe(ctx context.Context, d model.Device) (ident model.Identity, uptime int64, err error)
	SetOtaURL(ctx context.Context, d model.Device, url string) error
	// Upgrade sends "Upgrade 1" and returns the URL echoed in the reply
	// ("Version X from <url>"); the engine compares it with the URL it set.
	Upgrade(ctx context.Context, d model.Device) (echoURL string, err error)
}

// Backups is what the engine needs for the pre-OTA backup.
type Backups interface {
	Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error)
}

// Firmware is what the engine needs from the cache.
type Firmware interface {
	Ensure(ctx context.Context, release, filename string) (model.Artifact, error)
	URLFor(a model.Artifact) string
}

// CustomFirmware is implemented by the cache next to Firmware: the files of an
// uploaded image, found by the upload's key. The engine uses it instead of
// Ensure for jobs that install an upload (Request.Custom, Job.TargetCustom).
type CustomFirmware interface {
	EnsureCustom(ctx context.Context, key, release, filename string) (model.Artifact, error)
}

// ensure resolves a file of the job's firmware: an upload's file when custom
// is set, the upstream release's file otherwise (exactly as before uploads
// existed). It never falls back from one to the other.
func (e *Engine) ensure(ctx context.Context, custom, release, filename string) (model.Artifact, error) {
	if custom == "" {
		return e.fw.Ensure(ctx, release, filename)
	}
	cf, ok := e.fw.(CustomFirmware)
	if !ok {
		return model.Artifact{}, errors.New("ota: this firmware cache cannot serve custom images")
	}
	return cf.EnsureCustom(ctx, custom, release, filename)
}

// Notifier receives every persisted transition (SSE hub, metrics, audit).
type Notifier interface {
	JobChanged(j model.Job, ev model.JobEvent)
}

// Options configure the engine.
type Options struct {
	Machine     Config
	MaxParallel int // fleet-wide concurrent jobs past "queued", default 2
	// Audit records "transaction.retry" and the policy fixes; nil = not recorded.
	Audit audit.Recorder
	// Policy runs "Make compliant" jobs; nil makes them fail with a clear message.
	Policy PolicyApplier
	// Templates reads, sends and verifies GPIO templates (#327); nil makes such jobs fail with a clear message.
	Templates TemplateApplier
	// Commands sends the free-form command of a "Send command" job through the
	// command policy (devicecmd.Service); nil makes such jobs fail with a clear message.
	Commands CommandRunner
	// Principals resolves the current role of a job's creator before each
	// command is sent and when a command transaction is retried.
	Principals PrincipalResolver
	// Paths chooses the path of each update (HTTP or MQTT, D2). nil means every update is an HTTP
	// one, as before MQTT transport.
	Paths UpdatePaths
	// HoldFlashing is called when a job is about to end while its device is flashing a firmware the job did
	// not choose: the Upgrade reply named another URL than the one the job set (#504). Ending the job
	// releases the device, so the caller must keep new updates away from it, as it does after a typed
	// firmware command (#347). It runs before the job is released and must not block. nil = no hold.
	HoldFlashing func(model.DeviceID)
}

// Errors returned by Submit and Cancel.
var (
	ErrRefused   = errors.New("ota: devices refused")
	ErrNotActive = errors.New("ota: job is not active")
	// ErrBatchRunning: a batch cannot be retried while it still has jobs running.
	ErrBatchRunning = errors.New("ota: the batch still has running jobs")
	// ErrNothingToRetry: no device of the batch is failed or cancelled and still needs an update.
	ErrNothingToRetry = errors.New("ota: nothing to retry")
	// ErrNotRetryable: a GPIO template transaction is never replayed; a fresh preview is made instead.
	ErrNotRetryable = errors.New("ota: this kind of transaction is checked again, not retried")
	// ErrNotCancellable: a "Make compliant" job that is already sending its
	// commands runs to its end; stopping it halfway would leave the device
	// with some settings changed and not saved.
	ErrNotCancellable = errors.New("ota: job cannot be cancelled now")
)

// Refusal names one device Submit did not accept and why.
type Refusal struct {
	DeviceID model.DeviceID
	Reason   string
}

// RefusedError lists every device of a Submit call that was refused. The
// batch is all-or-nothing: nothing is created when any device is refused,
// so the caller can show the list and resubmit the rest. It matches
// ErrRefused with errors.Is.
type RefusedError struct {
	Refused []Refusal
}

func (e *RefusedError) Error() string {
	parts := make([]string, 0, len(e.Refused))
	for _, r := range e.Refused {
		parts = append(parts, fmt.Sprintf("device %d: %s", r.DeviceID, r.Reason))
	}
	return "ota: devices refused: " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrRefused) true.
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// selector is the fwselect surface the engine uses. Production code uses
// the fwselect functions; tests substitute a fake.
type selector interface {
	Decide(id model.Identity, c fwselect.Choice) (fwselect.Decision, error)
	Select(id model.Identity, c fwselect.Choice) (fwselect.Selection, error)
	Plan(id model.Identity, targetSize int64) (model.Plan, error)
	Guard(id model.Identity, a model.Artifact, otaURL string, c fwselect.Choice) error
}

type fwselectFuncs struct{}

func (fwselectFuncs) Decide(id model.Identity, c fwselect.Choice) (fwselect.Decision, error) {
	return fwselect.Decide(id, c)
}

func (fwselectFuncs) Select(id model.Identity, c fwselect.Choice) (fwselect.Selection, error) {
	return fwselect.Select(id, c)
}

func (fwselectFuncs) Plan(id model.Identity, size int64) (model.Plan, error) {
	return fwselect.Plan(id, size)
}

func (fwselectFuncs) Guard(id model.Identity, a model.Artifact, u string, c fwselect.Choice) error {
	return fwselect.Guard(id, a, u, c)
}

type nopNotifier struct{}

func (nopNotifier) JobChanged(model.Job, model.JobEvent) {}

// Engine runs jobs. Run resumes active jobs and schedules queued ones;
// Submit creates a batch.
type Engine struct {
	opts    Options
	jobs    store.JobStore
	devices store.DeviceStore
	api     DeviceAPI
	audit   audit.Recorder
	fw      Firmware
	backups Backups
	clk     clock.Clock
	notify  Notifier
	sel     selector
	paths   UpdatePaths
	// safebootPush opens the push into safeboot (#30); NewEngine takes it from safebootPushEnabled.
	safebootPush bool
	log          *slog.Logger

	tune atomic.Pointer[Tuning]

	mu      sync.Mutex
	workers map[model.JobID]chan Event // inbox of each running worker
	fixing  map[model.JobID]bool       // running policy jobs, which cannot be cancelled
	byDev   map[model.DeviceID]model.JobID
	wake    chan struct{}
}

// NewEngine wires an engine. A zero Options.Machine means DefaultConfig,
// MaxParallel <= 0 means 2, a nil Notifier is allowed.
func NewEngine(o Options, jobs store.JobStore, devices store.DeviceStore, api DeviceAPI, fw Firmware, b Backups, clk clock.Clock, n Notifier) *Engine {
	if o.Machine == (Config{}) {
		o.Machine = DefaultConfig()
	}
	if o.MaxParallel <= 0 {
		o.MaxParallel = 2
	}
	if n == nil {
		n = nopNotifier{}
	}
	return &Engine{
		opts: o, audit: o.Audit, jobs: jobs, devices: devices, api: api, fw: fw, backups: b, clk: clk, notify: n,
		sel: fwselectFuncs{}, paths: o.Paths, safebootPush: safebootPushEnabled, log: slog.Default().With("component", "ota"),
		workers: map[model.JobID]chan Event{}, fixing: map[model.JobID]bool{}, byDev: map[model.DeviceID]model.JobID{}, wake: make(chan struct{}, 1),
	}
}

// Tuning is the part of the engine configuration that can change while the
// engine runs (the settings page). Changes apply to jobs from their next step.
type Tuning struct {
	MaxParallel   int
	RestoreOtaURL bool
}

// SetTuning replaces the tunable settings. MaxParallel < 1 is ignored.
func (e *Engine) SetTuning(t Tuning) {
	if t.MaxParallel < 1 {
		return
	}
	e.tune.Store(&t)
}

func (e *Engine) maxParallel() int {
	if t := e.tune.Load(); t != nil {
		return t.MaxParallel
	}
	return e.opts.MaxParallel
}

// machine returns the state machine configuration in force now.
func (e *Engine) machine() Config {
	cfg := e.opts.Machine
	if t := e.tune.Load(); t != nil {
		cfg.RestoreOtaURL = t.RestoreOtaURL
	}
	return cfg
}

// Request is one device in a Submit call.
type Request struct {
	DeviceID model.DeviceID
	Release  string
	// Custom is the key of the uploaded image to install (firmware.Custom,
	// Artifact.Custom); Release is then that image's version. Empty = the
	// upstream release.
	Custom string
	// Delivery is how the image reaches the device; empty is the default
	// pull. DeliveryPush means "push where possible": a device or plan that
	// cannot take an upload gets the download instead (PreviewItem says
	// which and why), and the job stores the delivery it really uses.
	Delivery           model.Delivery
	Variant            string // empty = keep detected
	AllowVariantChange bool
	// Build is the explicit chip build of an ESP32-S2 ("s2" or "s2cdc"), chosen
	// per update by the user; empty for every other device (#88). Without it an
	// S2 is refused. It is stored on the job (Job.TargetBuild).
	Build string
	// Language is the language build the person says the device runs ("en", "de", ...), required when
	// the engine cannot read it from the device (over MQTT, D10) and the device has language builds.
	// Ignored for a device whose language was read from its web page.
	Language   string
	Policy     model.UpdatePolicy
	SkipBackup bool
	// OriginalOtaURL is set by retries: the OtaUrl to put back at the end,
	// taken from the job being retried. A failed update leaves the device
	// with tasmofleet's own URL, which a fresh preflight would otherwise
	// record as the one to restore.
	OriginalOtaURL string
}

// PreviewItem is the dry-run result for one device: what would happen.
type PreviewItem struct {
	DeviceID model.DeviceID
	Decision string // "upgrade", "reinstall", "downgrade", "skip", "refuse"
	Plan     model.Plan
	Filename string
	Reason   string // for skip/refuse: the guard or policy error text
	// BuildChoice is set when the refusal is fwselect.ErrBuildChoiceNeeded (or an S2's variant change): the
	// user may choose a build (fwselect.BuildOptions of the device) and recheck.
	BuildChoice bool
	// Delivery is what the job would use: a push request falls back to the
	// download for a device that cannot take an upload, and DeliveryNote
	// says why ("" when the request is kept).
	Delivery     model.Delivery
	DeliveryNote string
	// ViaSafeboot: the push goes through safeboot (the device restarts into it, the image is
	// uploaded there). Only with Delivery push.
	ViaSafeboot bool
	// Path is how the update would reach the device (transport.Choose) and PathText the sentence why;
	// both empty without a path chooser. Transport is what the job would store.
	Path      transport.Path
	PathText  string
	Transport model.Transport
	// LanguageChoice: the refusal is only that the language build of the device cannot be read; the
	// person may choose it (Request.Language). LanguageDeclared: the person's choice was used, and
	// the job keeps it in its identity.
	LanguageChoice   bool
	LanguageDeclared bool
	Language         string
}

const (
	decisionRefuse = "refuse"
	detailSkip     = "already at target"
)

// Time budgets of actions that are not single device commands. Plan may
// download a firmware image into the cache; a backup is several requests.
const (
	planTimeoutFactor   = 10
	backupTimeoutFactor = 4
)

// refusalError marks a decision that must not be retried (policy, selection,
// guard): it becomes EvGuardRefused. Other errors are transient
// (EvCommandFailed).
type refusalError struct{ err error }

func (r *refusalError) Error() string { return r.err.Error() }
func (r *refusalError) Unwrap() error { return r.err }

func refused(err error) error { return &refusalError{err: err} }

func isRefusal(err error) bool {
	var r *refusalError
	return errors.As(err, &r)
}

func choiceFor(r Request) fwselect.Choice {
	pol := r.Policy
	if pol == "" {
		pol = model.PolicyOnlyNewer
	}
	return fwselect.Choice{Release: r.Release, Variant: r.Variant, AllowVariantChange: r.AllowVariantChange, Build: r.Build, Policy: pol}
}

// planChoice is the choice of the preflight plan: Submit persists
// TargetVariant only when the user explicitly allowed a variant change.
func planChoice(j model.Job) fwselect.Choice {
	return fwselect.Choice{Release: j.TargetRelease, Variant: j.TargetVariant, AllowVariantChange: j.TargetVariant != "", Build: j.TargetBuild, Policy: j.Policy}
}

// guardChoice is the choice for the Guard re-runs after planning, when
// TargetVariant holds the selected variant. A variant different from the
// one detected in preflight was either chosen explicitly (checked at
// Submit) or chosen by Select (leaving minimal).
func guardChoice(j model.Job) fwselect.Choice {
	return fwselect.Choice{Release: j.TargetRelease, Variant: j.TargetVariant, AllowVariantChange: j.TargetVariant != j.IdentityBefore.Variant, Build: j.TargetBuild, Policy: j.Policy}
}

type planResult struct {
	decision fwselect.Decision
	sel      fwselect.Selection
	plan     model.Plan
	target   model.Artifact
	minimal  model.Artifact
	// delivery is what the job uses: a push request becomes the download
	// for a device or plan that cannot take an upload; fallback says why.
	delivery model.Delivery
	fallback string
	// viaSafeboot: a push whose image does not fit the running partition goes through safeboot
	// (restart into it, upload there); only with delivery push (#30).
	viaSafeboot bool
}

// planFor runs Decide, Select, firmware.Ensure, Plan and Guard for every
// artifact the job would send. It is shared by Preview and the plan action
// so the dry run shows exactly what the job will do.
func (e *Engine) planFor(ctx context.Context, d model.Device, id model.Identity, c fwselect.Choice, custom string, delivery model.Delivery) (planResult, error) {
	var p planResult
	dec, err := e.sel.Decide(id, c)
	if err != nil {
		return p, refused(fmt.Errorf("policy: %w", err))
	}
	p.decision = dec
	if dec == fwselect.DecideSkip {
		return p, nil
	}
	p.delivery = delivery
	if delivery == model.DeliveryPush {
		if err := pushDeviceRefusal(id); err != nil {
			p.delivery, p.fallback = model.DeliveryPull, err.Error()
		}
	}
	if p.sel, err = e.sel.Select(id, c); err != nil {
		return p, refused(fmt.Errorf("select: %w", err))
	}
	if p.target, err = e.ensure(ctx, custom, c.Release, p.sel.Filename); err != nil {
		return p, fmt.Errorf("firmware %s: %w", p.sel.Filename, err)
	}
	if p.plan, err = e.sel.Plan(id, p.target.Size); err != nil {
		return p, refused(fmt.Errorf("plan: %w", err))
	}
	if p.delivery == model.DeliveryPush {
		err := pushPlanRefusal(p.plan)
		if err == nil {
			err = pushSpaceRefusal(id, p.target.Size)
		}
		var space *pushSpaceError
		switch {
		case err == nil:
			// Free says the image fits, but a single-OTA ESP32 (/up calls upl(this)) must never get the
			// plain upload into its running app (#191): safeboot route or download.
			p.viaSafeboot, p.fallback = e.safebootRoute(ctx, d, id)
			if !p.viaSafeboot && p.fallback != "" {
				p.delivery = model.DeliveryPull
			}
		case errors.As(err, &space):
			p.delivery, p.fallback = model.DeliveryPull, err.Error()
			// An ESP32 with the safeboot layout can take the image there (#30).
			if via, note := e.safebootRoute(ctx, d, id); via {
				p.delivery, p.fallback, p.viaSafeboot = model.DeliveryPush, "", true
			} else if note != "" {
				p.fallback = note
			}
		default:
			p.delivery, p.fallback = model.DeliveryPull, err.Error()
		}
	}
	if err := e.sel.Guard(id, p.target, e.fw.URLFor(p.target), c); err != nil {
		return p, refused(err)
	}
	staged := p.plan == model.PlanStaged
	if id.Variant == variantMinimal || (!staged && id.Family != model.ChipESP8266) {
		return p, nil
	}
	if p.sel.Minimal == "" {
		if staged {
			return p, refused(errors.New("staged plan without a minimal image"))
		}
		return p, nil
	}
	// Every ESP8266 plan needs the minimal image in the cache: Tasmota's own
	// fallback (image too large for the flash) derives its URL from the
	// directory of OtaUrl and would hit a 404 otherwise. A direct plan fits
	// the free flash by definition, so it proceeds without minimal if the
	// prefetch fails; a staged plan cannot.
	if p.minimal, err = e.ensure(ctx, custom, c.Release, p.sel.Minimal); err != nil {
		if !staged && ctx.Err() == nil {
			e.log.Warn("ota: minimal image not prefetched, Tasmota's own fallback would fail", "file", p.sel.Minimal, "err", err)
			return p, nil
		}
		return p, fmt.Errorf("firmware %s: %w", p.sel.Minimal, err)
	}
	if err := e.sel.Guard(id, p.minimal, e.fw.URLFor(p.minimal), c); err != nil {
		return p, refused(err)
	}
	return p, nil
}

// stageArtifact resolves the artifact and URL of the job's current stage
// and re-runs Guard. It runs immediately before every OtaUrl and Upgrade.
func (e *Engine) stageArtifact(ctx context.Context, j model.Job) (model.Artifact, string, error) {
	c := guardChoice(j)
	s, err := e.sel.Select(j.IdentityBefore, c)
	if err != nil {
		return model.Artifact{}, "", refused(fmt.Errorf("select: %w", err))
	}
	name, want := s.Filename, j.TargetArtifact
	if j.Stage == model.StageMinimal {
		name, want = s.Minimal, j.MinimalArtifact
	}
	a, err := e.ensure(ctx, j.TargetCustom, j.TargetRelease, name)
	if err != nil {
		return model.Artifact{}, "", fmt.Errorf("firmware %s: %w", name, err)
	}
	if a.ID != want {
		return model.Artifact{}, "", refused(fmt.Errorf("artifact %s is #%d, planned #%d", name, a.ID, want))
	}
	u := e.fw.URLFor(a)
	if err := e.sel.Guard(j.IdentityBefore, a, u, c); err != nil {
		return model.Artifact{}, "", refused(err)
	}
	return a, u, nil
}

// Preview runs detection, selection, policy and guard without touching the
// devices' configuration (read-only probes only). The UI shows it before
// Submit. Per-device problems are reported as "refuse" rows; the error is
// only for inventory failures.
func (e *Engine) Preview(ctx context.Context, reqs []Request) ([]PreviewItem, error) {
	items := make([]PreviewItem, 0, len(reqs))
	for _, r := range reqs {
		it, err := e.previewOne(ctx, r)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

func (e *Engine) previewOne(ctx context.Context, r Request) (PreviewItem, error) {
	it := PreviewItem{DeviceID: r.DeviceID, Decision: decisionRefuse}
	d, err := e.devices.GetDevice(ctx, r.DeviceID)
	if errors.Is(err, store.ErrNotFound) {
		it.Reason = "unknown device"
		return it, nil
	}
	if err != nil {
		return it, fmt.Errorf("ota: preview device %d: %w", r.DeviceID, err)
	}
	return e.previewDevice(ctx, d, r), nil
}

// previewDevice reports every device-level problem as a refuse row.
func (e *Engine) previewDevice(ctx context.Context, d model.Device, r Request) PreviewItem {
	it := PreviewItem{DeviceID: r.DeviceID, Decision: decisionRefuse}
	if d.OTAExcluded {
		it.Reason = "excluded from OTA"
		return it
	}
	if r.Delivery != model.DeliveryPull && r.Delivery != model.DeliveryPush {
		it.Reason = "unknown delivery " + strconv.Quote(string(r.Delivery))
		return it
	}
	pctx, cancel := context.WithTimeout(ctx, e.opts.Machine.CommandTimeout)
	ident, _, _, err := e.api.ProbeIdentity(pctx, d)
	cancel()
	if err != nil {
		it.Reason = "probe failed: " + err.Error()
		return it
	}
	if r.Language != "" && !ident.LanguageKnown {
		lang, err := declaredLanguage(r.Language)
		if err != nil {
			it.Reason = err.Error()
			return it
		}
		ident.Language, ident.LanguageKnown = lang, true
		it.LanguageDeclared, it.Language = true, lang
	}
	fctx, cancel := context.WithTimeout(ctx, planTimeoutFactor*e.opts.Machine.CommandTimeout)
	p, err := e.planFor(fctx, d, ident, choiceFor(r), r.Custom, r.Delivery)
	cancel()
	if err != nil {
		it.Reason = err.Error()
		// An S2 refused for its variant (a custom or unrecognised image runs) still offers the build:
		// the replacement needs the user's explicit variant as well, which the page explains.
		if errors.Is(err, fwselect.ErrBuildChoiceNeeded) || (ident.Family == model.ChipESP32S2 && errors.Is(err, fwselect.ErrVariantChange)) {
			it.BuildChoice = true
		}
		it.LanguageChoice = errors.Is(err, fwselect.ErrLanguageUnknown)
		return it
	}
	it.Decision = string(p.decision)
	if p.decision == fwselect.DecideSkip {
		it.Reason = detailSkip
		return it
	}
	it.Plan = p.plan
	it.Filename = p.sel.Filename
	it.Delivery, it.DeliveryNote, it.ViaSafeboot = p.delivery, p.fallback, p.viaSafeboot
	e.choosePath(ctx, d, r, p, &it)
	return it
}

// Submit validates and persists a batch; jobs start as queued. It is
// all-or-nothing: devices that are excluded, already have an active job, or
// whose preview is skip/refuse are returned in a *RefusedError and no job
// is created.
func (e *Engine) Submit(ctx context.Context, actor string, reqs []Request) (model.BatchID, error) {
	id, _, err := e.submit(ctx, actor, reqs, 0, false)
	return id, err
}

// submit is Submit; retryOf links the new batch to the one it retries and
// tolerateSkip turns devices that need nothing (already at target) from
// refusals into a returned list, so a retry is not blocked by a device that
// has meanwhile been fixed.
func (e *Engine) submit(ctx context.Context, actor string, reqs []Request, retryOf model.BatchID, tolerateSkip bool) (model.BatchID, []Refusal, error) {
	if len(reqs) == 0 {
		return 0, nil, errors.New("ota: empty batch")
	}
	var ref []Refusal
	var rest []Request
	seen := map[model.DeviceID]bool{}
	for _, r := range reqs {
		if seen[r.DeviceID] {
			ref = append(ref, Refusal{r.DeviceID, "listed twice"})
			continue
		}
		seen[r.DeviceID] = true
		act, err := e.jobs.ListJobs(ctx, store.JobFilter{DeviceID: r.DeviceID, States: activeStates})
		if err != nil {
			return 0, nil, fmt.Errorf("ota: submit: %w", err)
		}
		if len(act) > 0 {
			ref = append(ref, Refusal{r.DeviceID, fmt.Sprintf("already has active job %d", act[0].ID)})
			continue
		}
		rest = append(rest, r)
	}
	items, err := e.Preview(ctx, rest)
	if err != nil {
		return 0, nil, err
	}
	var skipped []Refusal
	skip := map[model.DeviceID]bool{}
	for _, it := range items {
		switch {
		case it.Decision == string(fwselect.DecideSkip) && tolerateSkip:
			skipped = append(skipped, Refusal{it.DeviceID, it.Reason})
			skip[it.DeviceID] = true
		case it.Decision == decisionRefuse || it.Decision == string(fwselect.DecideSkip):
			ref = append(ref, Refusal{it.DeviceID, it.Decision + ": " + it.Reason})
		}
	}
	if len(ref) > 0 {
		return 0, nil, &RefusedError{Refused: ref}
	}
	if len(skip) > 0 {
		kept := rest[:0:0]
		for _, r := range rest {
			if !skip[r.DeviceID] {
				kept = append(kept, r)
			}
		}
		rest = kept
	}
	if len(rest) == 0 {
		return 0, skipped, ErrNothingToRetry
	}
	// Each job stores the delivery its preview decided (push where possible).
	delivery := map[model.DeviceID]model.Delivery{}
	viaSafeboot := map[model.DeviceID]bool{}
	byItem := map[model.DeviceID]PreviewItem{}
	for _, it := range items {
		delivery[it.DeviceID], viaSafeboot[it.DeviceID] = it.Delivery, it.ViaSafeboot
		byItem[it.DeviceID] = it
	}
	now := e.clk.Now()
	jobs := make([]model.Job, 0, len(rest))
	for _, r := range rest {
		c := choiceFor(r)
		j := model.Job{DeviceID: r.DeviceID, State: model.JobQueued, Policy: c.Policy, SkipBackup: r.SkipBackup,
			TargetRelease: r.Release, TargetBuild: r.Build, TargetCustom: r.Custom, Delivery: delivery[r.DeviceID], ViaSafeboot: viaSafeboot[r.DeviceID], OriginalOtaURL: r.OriginalOtaURL, CreatedBy: actor, CreatedAt: now, UpdatedAt: now}
		if r.AllowVariantChange {
			j.TargetVariant = r.Variant
		}
		it := byItem[r.DeviceID]
		j.Transport = it.Transport
		if it.LanguageDeclared {
			// The choice is kept as the language of the identity the preflight will complete (and never
			// replace by a guess): a job has no column of its own for it.
			j.IdentityBefore = model.Identity{Language: it.Language, LanguageKnown: true}
		}
		jobs = append(jobs, j)
	}
	var id model.BatchID
	if retryOf != 0 {
		bs, ok := e.jobs.(store.BatchStore)
		if !ok {
			return 0, nil, errors.New("ota: the job store cannot link a retry to its batch")
		}
		id, _, err = bs.CreateRetryBatch(ctx, actor, retryOf, jobs)
	} else {
		id, _, err = e.jobs.CreateBatch(ctx, actor, jobs)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("ota: submit: %w", err)
	}
	e.signal()
	return id, skipped, nil
}

var activeStates = []model.JobState{
	model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL,
	model.JobSafebootSwitch, model.JobSafebootRecover,
	model.JobUpgradeIssued, model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobApplying,
}

// Cancel requests cancellation. Jobs past ActUpgrade cannot stop the device
// flashing; they are marked cancelled only once the device is reachable again.
func (e *Engine) Cancel(ctx context.Context, actor string, id model.JobID) error {
	ev := Event{Kind: EvCancel, Err: "cancelled by " + actor}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inbox, ok := e.workers[id]; ok {
		if e.fixing[id] {
			return fmt.Errorf("%w: job %d is changing settings on the device", ErrNotCancellable, id)
		}
		select {
		case inbox <- ev:
			return nil
		default:
			return fmt.Errorf("ota: job %d is busy, try again", id)
		}
	}
	j, err := e.jobs.GetJob(ctx, id)
	if err != nil {
		return fmt.Errorf("ota: cancel: %w", err)
	}
	if j.State.Terminal() {
		return fmt.Errorf("%w: job %d is %s", ErrNotActive, id, j.State)
	}
	if _, _, ok := e.apply(ctx, j, ev); !ok {
		return fmt.Errorf("ota: cancel job %d: could not persist", id)
	}
	return nil
}

// CancelBatch cancels every active job of the batch as Cancel does for one
// job (spec 9.6). Finished jobs are left alone. It returns ErrNotActive
// when nothing in the batch is active and store.ErrNotFound for an unknown
// batch; if some jobs could not be cancelled the others still are, and the
// errors are joined.
func (e *Engine) CancelBatch(ctx context.Context, actor string, id model.BatchID) error {
	all, err := e.jobs.ListJobs(ctx, store.JobFilter{BatchID: id})
	if err != nil {
		return fmt.Errorf("ota: cancel batch %d: %w", id, err)
	}
	if len(all) == 0 {
		return fmt.Errorf("ota: cancel batch %d: %w", id, store.ErrNotFound)
	}
	var errs []error
	n := 0
	for _, j := range all {
		if j.State.Terminal() {
			continue
		}
		switch err := e.Cancel(ctx, actor, j.ID); {
		case err == nil:
			n++
		case errors.Is(err, ErrNotActive):
			// finished between the listing and the cancel
		default:
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if n == 0 {
		return fmt.Errorf("%w: batch %d has no active job", ErrNotActive, id)
	}
	return nil
}

// Hint feeds an MQTT stat/<t>/UPGRADE report ("Failed ..." or "Successful
// ...") to the device's running job. Only a failure changes anything: it
// ends the wait for the accept window early. Nothing depends on hints; a
// report for a device without a running job, or one the worker has no room
// for, is dropped, because the probes find out anyway.
func (e *Engine) Hint(deviceID model.DeviceID, report string) {
	report = strings.TrimSpace(report)
	if !strings.HasPrefix(report, "Failed") {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.byDev[deviceID]
	if !ok {
		return
	}
	select {
	case e.workers[id] <- Event{Kind: EvUpgradeReport, Err: report}:
	default:
		e.log.Debug("ota hint dropped, worker busy", "device", deviceID)
	}
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run resumes all non-terminal jobs and processes work until ctx is done.
// It waits for the workers before returning; their state is persisted, so
// the next Run resumes them.
func (e *Engine) Run(ctx context.Context) error {
	active, err := e.jobs.ActiveJobs(ctx)
	if err != nil {
		return fmt.Errorf("ota: resume: %w", err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, j := range active {
		if j.State != model.JobQueued {
			// Resumed jobs hold a slot even beyond MaxParallel: the
			// devices are mid-update and must be followed.
			e.startWorker(ctx, &wg, j, Event{Kind: EvResume})
		}
	}
	for {
		if err := e.schedule(ctx, &wg); err != nil && ctx.Err() == nil {
			e.log.Error("ota scheduling failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.wake:
		}
	}
}

// schedule starts queued jobs FIFO by job ID while slots are free. A job of a
// transaction with its own cap (BatchParallel, bulk options) waits only for
// that cap: it neither waits for nor takes a fleet-wide slot.
func (e *Engine) schedule(ctx context.Context, wg *sync.WaitGroup) error {
	active, err := e.jobs.ActiveJobs(ctx)
	if err != nil {
		return err
	}
	sort.Slice(active, func(a, b int) bool { return active[a].ID < active[b].ID })
	running := map[model.BatchID]int{}
	e.mu.Lock()
	free := e.maxParallel() - len(e.workers)
	for _, j := range active {
		if _, ok := e.workers[j.ID]; ok {
			running[j.BatchID]++
		}
	}
	e.mu.Unlock()
	for _, j := range active {
		if j.State != model.JobQueued {
			continue
		}
		capped := j.BatchParallel > 0
		if capped && running[j.BatchID] >= j.BatchParallel || !capped && free <= 0 { // resumed jobs may hold more than MaxParallel slots
			continue
		}
		if e.startWorker(ctx, wg, j, Event{Kind: EvStart}) {
			running[j.BatchID]++
			if !capped {
				free--
			}
		}
	}
	return nil
}

func (e *Engine) startWorker(ctx context.Context, wg *sync.WaitGroup, j model.Job, first Event) bool {
	e.mu.Lock()
	if _, ok := e.workers[j.ID]; ok {
		e.mu.Unlock()
		return false
	}
	inbox := make(chan Event, 8)
	e.workers[j.ID] = inbox
	e.byDev[j.DeviceID] = j.ID
	e.fixing[j.ID] = !j.IsUpdate()
	e.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.workers, j.ID)
			delete(e.fixing, j.ID)
			delete(e.byDev, j.DeviceID)
			e.mu.Unlock()
			e.signal()
		}()
		switch {
		case j.IsPolicy():
			e.workPolicy(ctx, j)
			return
		case j.IsCommand():
			e.workCommand(ctx, j)
			return
		case j.IsBackup():
			e.workBackup(ctx, j)
			return
		case j.IsTemplate():
			e.workTemplate(ctx, j)
			return
		}
		e.work(ctx, j, first, inbox)
	}()
	return true
}

func waiting(s model.JobState) bool {
	return s == model.JobUpgradeIssued || s == model.JobAwaitReboot || s == model.JobSafebootSwitch || s == model.JobSafebootRecover
}

// work drives one job: Step, persist, then run the requested actions and
// feed their results back. It returns when the job is terminal, when
// persisting fails (the stored state is resumed on the next start) or when
// ctx is done.
func (e *Engine) work(ctx context.Context, j model.Job, ev Event, inbox <-chan Event) {
	var pending []Action
	fromAction := false
	for {
		before := j
		next, acts, ok := e.apply(ctx, j, ev)
		if !ok {
			return
		}
		j = next
		if len(acts) > 0 {
			pending = acts
		}
		if j.State.Terminal() {
			return
		}
		// A probe result that did not move a waiting job: check its
		// deadline. Probing first means a device that rebooted just
		// before the deadline is still verified, never re-flashed.
		if fromAction && waiting(j.State) && j.State == before.State && !e.clk.Now().Before(j.DeadlineAt) {
			ev, fromAction = Event{Kind: EvTimeout, Addr: e.safebootAddr(ctx, j)}, false
			continue
		}
		var got bool
		ev, fromAction, got = e.next(ctx, j, pending, inbox)
		if !got {
			return
		}
		if fromAction {
			pending = nil // executed; never run the same action twice by accident
		}
	}
}

// next waits for the next event: an inbox event (cancel) or the result of
// the pending action after its delay.
func (e *Engine) next(ctx context.Context, j model.Job, pending []Action, inbox <-chan Event) (Event, bool, bool) {
	select {
	case ev := <-inbox:
		return ev, false, true
	default:
	}
	var act *Action
	for i := range pending {
		if pending[i].Kind != ActNotify {
			act = &pending[i]
			break
		}
	}
	if act == nil {
		e.log.Error("ota job has nothing to do, waiting for cancel", "job", j.ID, "state", j.State)
		select {
		case ev := <-inbox:
			return ev, false, true
		case <-ctx.Done():
			return Event{}, false, false
		}
	}
	if act.Delay > 0 {
		select {
		case <-e.clk.After(act.Delay):
		case ev := <-inbox:
			return ev, false, true
		case <-ctx.Done():
			return Event{}, false, false
		}
	}
	if ctx.Err() != nil {
		return Event{}, false, false
	}
	ev := e.exec(ctx, j, *act)
	if ev.Addr == "" {
		ev.Addr = e.safebootAddr(ctx, j) // "" for every job that is not a push into safeboot
	}
	return ev, true, true
}

// apply runs Step and persists the result before anything else happens
// (write-ahead). It reports false when persisting failed.
func (e *Engine) apply(ctx context.Context, j model.Job, ev Event) (model.Job, []Action, bool) {
	now := e.clk.Now()
	next, acts := Step(j, ev, now, e.machine())
	if next == j {
		return j, acts, true
	}
	detail := ev.Err
	if ev.Kind == EvPlanned && ev.Skip {
		detail = detailSkip
	}
	// A terminal row always carries the reason, also when an earlier
	// retry already set the same LastError.
	if detail == "" && (next.LastError != j.LastError || next.State.Terminal()) {
		detail = next.LastError
	}
	jev := model.JobEvent{JobID: j.ID, At: now, From: j.State, To: next.State, Event: string(ev.Kind), Detail: detail}
	if err := e.jobs.SaveTransition(ctx, next, jev); err != nil {
		e.persistFailed("ota transition not persisted; job stops here and resumes from the stored state", err,
			"job", j.ID, "from", j.State, "to", next.State)
		return j, nil, false
	}
	e.notify.JobChanged(next, jev)
	return next, acts, true
}

// exec performs one action and turns its result into an event.
func (e *Engine) exec(ctx context.Context, j model.Job, a Action) Event {
	ctx = pinned(ctx, j) // every call below, also the ones with their own timeout, stays on the job's path
	cfg := e.machine()
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err != nil {
		msg := "inventory: " + err.Error()
		switch a.Kind {
		case ActProbe:
			return Event{Kind: EvProbeOffline, Err: msg}
		case ActBackup:
			return Event{Kind: EvBackupFailed, Err: msg}
		case ActProbeIdentity, ActPlan, ActSetOtaURL, ActUpgrade, ActSafebootSwitch, ActSafebootBack, ActRestoreOtaURL, ActNotify:
		}
		return Event{Kind: EvCommandFailed, Err: msg}
	}
	timeout := cfg.CommandTimeout
	switch a.Kind {
	case ActPlan:
		timeout *= planTimeoutFactor
	case ActBackup:
		timeout *= backupTimeoutFactor
	case ActProbeIdentity, ActSetOtaURL, ActUpgrade, ActSafebootSwitch, ActSafebootBack, ActProbe, ActRestoreOtaURL, ActNotify:
	}
	if a.Kind == ActUpgrade && j.Delivery == model.DeliveryPush {
		timeout *= uploadTimeoutFactor
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch a.Kind {
	case ActProbeIdentity:
		ident, otaURL, up, err := e.api.ProbeIdentity(cctx, d)
		if err != nil {
			return Event{Kind: EvCommandFailed, Err: err.Error()}
		}
		// The stored identity keeps its DetectedAt: that stamp marks an HTTP probe (which keeps MQTT-announced
		// identities away from it) and makes the device's GroupTopic read look stale to the transport's write
		// rule, so a job must not move it for a device that has one (a mixed batch would block the MQTT jobs'
		// OtaUrl writes). Only a device that never had one gets it, and only from an HTTP job. A job over MQTT
		// writes what it verified (version, image) and keeps the language fields as the MQTT refresh does.
		stored := ident
		stored.DetectedAt = d.Identity.DetectedAt
		switch {
		case j.Transport == model.TransportMQTT:
			stored.Language = d.Identity.Language
			stored.LanguageKnown = d.Identity.LanguageKnown && ident.Version == d.Identity.Version && ident.Image == d.Identity.Image
		case stored.DetectedAt.IsZero():
			stored.DetectedAt = e.clk.Now()
		}
		if err := e.devices.UpdateIdentity(cctx, d.ID, stored); err != nil {
			e.log.Warn("ota: could not store device identity", "device", d.ID, "err", err)
		}
		if j.State == model.JobPreflight && j.IdentityBefore.LanguageKnown && !ident.LanguageKnown {
			// The language the person chose in the preview (D10), for a device that cannot tell it.
			ident.Language, ident.LanguageKnown = j.IdentityBefore.Language, true
		}
		return Event{Kind: EvIdentity, Identity: ident, OtaURL: otaURL, Uptime: up}
	case ActPlan:
		p, err := e.planFor(cctx, d, j.IdentityBefore, planChoice(j), j.TargetCustom, j.Delivery)
		if err != nil {
			return failEvent(err)
		}
		if p.decision == fwselect.DecideSkip {
			return Event{Kind: EvPlanned, Skip: true}
		}
		if p.delivery != j.Delivery || p.viaSafeboot != j.ViaSafeboot {
			// Submitted as a push (through safeboot or not), but the device or plan no longer
			// allows exactly that: never switch the delivery unseen.
			why := p.fallback
			if why == "" {
				why = "the way to the device changed"
			}
			return failEvent(refused(errors.New(why + "; the update was planned as a push upload, start a new one")))
		}
		ev := Event{Kind: EvPlanned, Plan: p.plan, TargetVariant: p.sel.Variant, TargetArtifact: p.target.ID}
		// Recorded for direct plans too: Tasmota's own fallback may fetch the
		// minimal image of the release, so the cache must keep it while the job
		// is active (it is only flashed by the staged flow). 0 if none was cached.
		ev.MinimalArtifact = p.minimal.ID
		return ev
	case ActBackup:
		if _, err := e.backups.Backup(backup.WithRun(cctx, backup.Run{Trigger: model.TriggerUpdate, Ref: j.TargetRelease}), j.CreatedBy, j.DeviceID); err != nil {
			return Event{Kind: EvBackupFailed, Err: err.Error()}
		}
		return Event{Kind: EvBackupDone}
	case ActSetOtaURL:
		art, u, err := e.stageArtifact(cctx, j)
		if err != nil {
			return failEvent(err)
		}
		ev := Event{Kind: EvCommandOK}
		if j.Delivery == model.DeliveryPush {
			// No OtaUrl: the image is uploaded in the next step. Check now that
			// it can be read, so a damaged cache file fails the job cleanly.
			if err := e.pushCheck(cctx, art, j.ViaSafeboot); err != nil {
				return failEvent(err)
			}
			return ev
		}
		if err := e.api.SetOtaURL(cctx, d, u); err != nil {
			return Event{Kind: EvCommandFailed, Err: err.Error()}
		}
		return ev
	case ActUpgrade:
		art, u, err := e.stageArtifact(cctx, j)
		if err != nil {
			return failEvent(err)
		}
		if j.Delivery == model.DeliveryPush {
			if j.ViaSafeboot {
				// Right after Guard: the device must serve the plain upload form, i.e. run safeboot
				// now. The upload page of the app (upl(this)) would send the image to the wrong place.
				if ev, ok := e.safebootUploadReady(cctx, d); !ok {
					return ev
				}
			}
			return e.push(cctx, d, art)
		}
		echo, err := e.api.Upgrade(cctx, d)
		if err != nil {
			return Event{Kind: EvCommandFailed, Err: err.Error()}
		}
		if echo != u {
			// The reply names a URL, so the device has accepted the upgrade and is downloading from it. The
			// job fails because the file was not checked, but failing releases the device while it flashes:
			// hold it first, so no second update starts on it (#504).
			if e.opts.HoldFlashing != nil {
				e.opts.HoldFlashing(j.DeviceID)
			}
			return Event{Kind: EvCommandOK, Err: fmt.Sprintf("device flashes %q, expected %q", echo, u)}
		}
		return Event{Kind: EvCommandOK}
	case ActSafebootSwitch:
		// Guard once more, then the one restart into safeboot. The state was persisted before this ran.
		if _, _, err := e.stageArtifact(cctx, j); err != nil {
			return failEvent(err)
		}
		if j.CancelRequested {
			// Cancelled before the restart was sent: do not start it.
			return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "cancelled by the user"}
		}
		return e.switchBoot(cctx, d, tasmota.BootSafeboot)
	case ActSafebootBack:
		return e.switchBoot(cctx, d, tasmota.BootApp)
	case ActProbe:
		ident, up, err := e.api.Probe(cctx, d)
		if err != nil {
			return Event{Kind: EvProbeOffline, Err: err.Error()}
		}
		return Event{Kind: EvProbeOnline, Identity: ident, Uptime: up}
	case ActRestoreOtaURL:
		if err := e.api.SetOtaURL(cctx, d, a.URL); err != nil {
			return Event{Kind: EvCommandFailed, Err: err.Error()}
		}
		return Event{Kind: EvCommandOK}
	case ActNotify:
	}
	return Event{}
}

func failEvent(err error) Event {
	if isRefusal(err) {
		return Event{Kind: EvGuardRefused, Err: err.Error()}
	}
	return Event{Kind: EvCommandFailed, Err: err.Error()}
}

// RetryResult is what SubmitRetry did.
type RetryResult struct {
	Batch   model.BatchID    // the new batch (transaction)
	Devices []model.DeviceID // the devices in it
	// Skipped are devices whose fresh plan says there is nothing to do (for
	// instance already at the target): not an error, not retried.
	Skipped []Refusal
}

// SubmitRetry creates a NEW batch for the failed and cancelled devices of
// batchID (all of them, or only deviceIDs), linked to it (Batch.RetryOf).
// Release, variant choice, policy and options (backup) are
// the original's; everything else is planned afresh from the device's
// current state, with a fresh preflight and guard: a device that crashed
// mid-flash and now runs minimal, or that is already at the target, is
// handled by the normal planning.
//
// It refuses while the original still has running jobs (ErrBatchRunning),
// for a selected device that is not failed or cancelled in the original, and
// for devices that have an active job or are refused by planning (all in a
// *RefusedError; nothing is created then). store.ErrNotFound for an unknown
// batch, ErrNothingToRetry when nothing is left to retry (the skipped
// devices are still returned).
func (e *Engine) SubmitRetry(ctx context.Context, actor string, batchID model.BatchID, deviceIDs []model.DeviceID) (RetryResult, error) {
	res, err := e.submitRetry(ctx, actor, batchID, deviceIDs)
	if e.audit != nil {
		detail, _ := json.Marshal(map[string]any{"retry_of": batchID, "batch": res.Batch, "devices": res.Devices, "selected": deviceIDs, "skipped": len(res.Skipped)})
		result := "ok"
		if err != nil {
			result = err.Error()
		}
		e.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "transaction.retry", Target: "transaction:" + strconv.FormatInt(int64(batchID), 10), Detail: string(detail), Result: result})
	}
	return res, err
}

func (e *Engine) submitRetry(ctx context.Context, actor string, batchID model.BatchID, deviceIDs []model.DeviceID) (RetryResult, error) {
	var res RetryResult
	if _, ok := e.jobs.(store.BatchStore); !ok {
		return res, errors.New("ota: the job store cannot link a retry to its batch")
	}
	all, err := e.jobs.ListJobs(ctx, store.JobFilter{BatchID: batchID})
	if err != nil {
		return res, fmt.Errorf("ota: retry batch %d: %w", batchID, err)
	}
	if len(all) == 0 {
		return res, fmt.Errorf("ota: retry batch %d: %w", batchID, store.ErrNotFound)
	}
	byDevice := map[model.DeviceID]model.Job{}
	for _, j := range all {
		if !j.State.Terminal() {
			return res, fmt.Errorf("%w: job %d is %s", ErrBatchRunning, j.ID, j.State)
		}
		byDevice[j.DeviceID] = j
	}
	var picked []model.Job
	var ref []Refusal
	if len(deviceIDs) == 0 {
		for _, j := range all {
			if j.State == model.JobFailed || j.State == model.JobCancelled {
				picked = append(picked, j)
			}
		}
	} else {
		seen := map[model.DeviceID]bool{}
		for _, id := range deviceIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			switch j, ok := byDevice[id]; {
			case !ok:
				ref = append(ref, Refusal{id, fmt.Sprintf("not part of batch %d", batchID)})
			case j.State != model.JobFailed && j.State != model.JobCancelled:
				ref = append(ref, Refusal{id, fmt.Sprintf("job %d is %s, only failed or cancelled jobs are retried", j.ID, j.State)})
			default:
				picked = append(picked, j)
			}
		}
	}
	if len(ref) > 0 {
		return res, &RefusedError{Refused: ref}
	}
	if len(picked) == 0 {
		return res, ErrNothingToRetry
	}
	sort.Slice(picked, func(a, b int) bool { return picked[a].ID < picked[b].ID })
	if picked[0].IsTemplate() {
		return res, ErrNotRetryable
	}
	if !picked[0].IsUpdate() {
		return e.retryPolicy(ctx, actor, batchID, picked)
	}
	reqs := make([]Request, 0, len(picked))
	for _, j := range picked {
		r := retryRequest(j)
		if d, err := e.devices.GetDevice(ctx, j.DeviceID); err == nil && !d.OTAExcluded {
			// A device that crashed mid-flash may be stuck on minimal/safeboot,
			// which has no variant of its own: it gets the job's target build
			// (the default tasmota build if the job never got that far), the
			// same choice the update dialog makes for such devices.
			pctx, cancel := context.WithTimeout(ctx, e.opts.Machine.CommandTimeout)
			ident, _, _, perr := e.api.ProbeIdentity(pctx, d)
			cancel()
			if perr == nil && isIntermediate(ident.Variant) {
				r.Variant, r.AllowVariantChange = j.TargetVariant, true
				if r.Variant == "" || isIntermediate(r.Variant) {
					r.Variant = "tasmota"
				}
			}
		}
		reqs = append(reqs, r)
	}
	id, skipped, err := e.submit(ctx, actor, reqs, batchID, true)
	res.Skipped = skipped
	if err != nil {
		return res, err
	}
	res.Batch = id
	skip := map[model.DeviceID]bool{}
	for _, s := range skipped {
		skip[s.DeviceID] = true
	}
	for _, r := range reqs {
		if !skip[r.DeviceID] {
			res.Devices = append(res.Devices, r.DeviceID)
		}
	}
	return res, nil
}

// retryRequest is the request the original job was made from. The variant is
// carried only where it differs from the one the device had, which is when
// it was chosen (or forced by minimal) and approved the first time.
func retryRequest(j model.Job) Request {
	r := Request{DeviceID: j.DeviceID, Release: j.TargetRelease, Custom: j.TargetCustom, Delivery: j.Delivery, Policy: j.Policy, SkipBackup: j.SkipBackup,
		OriginalOtaURL: j.OriginalOtaURL}
	if j.TargetVariant != "" && j.TargetVariant != j.IdentityBefore.Variant {
		r.Variant, r.AllowVariantChange = j.TargetVariant, true
	}
	r.Build = j.TargetBuild
	if j.IdentityBefore.LanguageKnown {
		// Kept for a device that still cannot tell its language; a device that can is read afresh.
		r.Language = j.IdentityBefore.Language
		if r.Language == "" {
			r.Language = fwselect.LanguageEnglish
		}
	}
	return r
}
