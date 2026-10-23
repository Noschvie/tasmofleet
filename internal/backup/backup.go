// Package backup stores device configuration backups under <data>/backups:
//
//   - the raw config dump from /dl (.dmp) - authoritative, the only thing
//     used for restore (Tasmota web upload /u2, M2);
//   - a JSON status snapshot built from Status 1..11 queried one by one;
//   - when a Decoder is configured (decode-config, shipped in the image), a
//     decoded JSON of the .dmp, stamped with the decoder version. If the
//     device firmware is newer than the decoder supports, only the raw dump
//     is kept and the backup is flagged "decoder outdated".
//
// Retention: keep the newest N per device (default 10) plus anything newer
// than MaxAge; pruning runs after every successful backup.
package backup

import (
	"context"
	"errors"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors.
var (
	ErrDecoderOutdated = errors.New("backup: decoder older than device firmware")
	ErrNotImplemented  = errors.New("backup: not implemented")
	// ErrForeignBackup: a selected backup belongs to another device.
	ErrForeignBackup = errors.New("backup: backup belongs to another device")
	// ErrNothingSelected: a delete without any backup selected.
	ErrNothingSelected = errors.New("backup: no backup selected")

	errEmptyDump = errors.New("backup: device returned an empty config dump")
)

// Decoder turns a .dmp into JSON. The production implementation execs the
// decode-config binary (LGPL-3.0, invoked as a separate program, not linked)
// with a timeout and no network; tests use a fake.
type Decoder interface {
	// Version returns the decoder version, e.g. "16.0.0".
	Version(ctx context.Context) (string, error)
	// Decode returns decoded JSON for the dump.
	Decode(ctx context.Context, dmp []byte) ([]byte, error)
}

// Service takes and lists backups.
type Service interface {
	// Backup takes a dump + snapshot (+ decoded JSON) of one device.
	Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error)
	List(ctx context.Context, id model.DeviceID) ([]model.Backup, error)
	// Open returns the file content of a stored backup.
	Open(ctx context.Context, id model.BackupID) ([]byte, model.Backup, error)
}
