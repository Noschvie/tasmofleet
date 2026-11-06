package model

import "time"

// BackupOutcome is what one scheduled backup run did with one device (#409). A skipped device was
// not tried, so it is neither a success nor a failure: only failures colour the status bar and
// raise a notification.
type BackupOutcome string

// Backup outcomes.
const (
	BackupOutcomeOK      BackupOutcome = "ok"
	BackupOutcomeFailed  BackupOutcome = "failed"
	BackupOutcomeSkipped BackupOutcome = "skipped"
)

// BackupRunTrigger says who started a fleet run, or what a marker row records. Only scheduled runs
// are recorded as runs; the other two are markers without items that settle a scheduled time (#601).
type BackupRunTrigger string

// Backup run triggers.
const (
	BackupRunScheduled BackupRunTrigger = "scheduled"
	// BackupRunMissed: the scheduled time passed while TasmoFleet was down and was too long ago to
	// start late; the run was skipped (StartedAt is when that was noticed).
	BackupRunMissed BackupRunTrigger = "missed"
	// BackupRunRescheduled: the schedule was changed (or created by a migration) at StartedAt; a
	// time of day that had passed before is not run late.
	BackupRunRescheduled BackupRunTrigger = "rescheduled"
)

// BackupRun is one fleet-wide backup run (the schedule, BACKUP_DAYS and BACKUP_TIME) with its per-device
// items. Tried = OK + Failed; Skipped devices are counted apart.
type BackupRun struct {
	ID         int64
	Trigger    BackupRunTrigger
	StartedAt  time.Time
	FinishedAt time.Time // zero while the run is in progress
	// Completed is false for a run that was cut short (shutdown); its counts cover what was done.
	Completed bool
	Tried     int
	OK        int
	Failed    int
	Skipped   int
	Items     []BackupRunItem
}

// BackupRunItem is the result for one device of a BackupRun.
type BackupRunItem struct {
	// DeviceID is 0 when the device was deleted after the run; DeviceName still says whose it was.
	DeviceID   DeviceID
	DeviceName string
	Outcome    BackupOutcome
	Reason     string // a human text without credentials; empty for ok
	// Since is when a skipped device went offline, zero when there is no such time (never answered,
	// busy, or an item stored before 0044, whose Reason then still carries the time as text).
	Since time.Time
	At    time.Time
}

// BackupDeviceRow is one device on the Devices, Backups page: what it has stored and how the last
// scheduled run went. It is built from stored data only, without device I/O.
type BackupDeviceRow struct {
	DeviceID  DeviceID
	Name      string
	NewestAt  time.Time // zero = no backup
	Runs      int       // backup runs stored
	Kept      int       // of which pinned with Keep
	Bytes     int64
	Files     int    // files in the newest run's file archive; 0 = none
	FilesNote string // why there are none, e.g. "settings only: no file system"; empty otherwise
	// LastOutcome, LastReason and LastAt are the device's item in the last scheduled run; empty
	// when the device was not part of it.
	LastOutcome BackupOutcome
	LastReason  string
	LastAt      time.Time
	// LastSince is the Since of that item.
	LastSince time.Time
	// SkipReason is set when a run started now would skip the device ("offline", "never answered");
	// empty when it would be tried. SkipSince is since when it is offline, zero when unknown.
	SkipReason string
	SkipSince  time.Time
}
