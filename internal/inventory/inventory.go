// Package inventory is the service layer over store.DeviceStore: validation
// (names, addresses inside the configured device networks, unique MAC/topic),
// normalisation (MAC lower case with colons), audit records and events.
// Web handlers and importers call this package, never the store directly
// for devices.
package inventory

import (
	"context"
	"errors"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors.
var (
	ErrInvalid        = errors.New("inventory: invalid device")
	ErrOutsideNetwork = errors.New("inventory: address outside configured device networks")
	ErrNotImplemented = errors.New("inventory: not implemented")
)

// DeleteOptions are the choices of a device delete.
type DeleteOptions struct {
	Backups string // "keep" or "delete": what happened to the device's backups
}

// Service manages devices.
type Service interface {
	List(ctx context.Context) ([]model.Device, error)
	Get(ctx context.Context, id model.DeviceID) (model.Device, error)
	Create(ctx context.Context, actor string, d model.Device) (model.DeviceID, error)
	Update(ctx context.Context, actor string, d model.Device) error
	Delete(ctx context.Context, actor string, id model.DeviceID) error
	// DeleteWith is Delete with the user's choice about the device's backups,
	// recorded in the audit entry. The backups themselves are the backup
	// service's: the store keeps them (device_id NULL) unless the caller
	// deleted them first.
	DeleteWith(ctx context.Context, actor string, id model.DeviceID, opt DeleteOptions) error
	// Upsert matches by MAC, then topic, then address; used by importers and discovery.
	Upsert(ctx context.Context, actor string, d model.Device) (id model.DeviceID, created bool, err error)
}
