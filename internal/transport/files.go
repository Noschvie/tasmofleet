package transport

import (
	"context"
	"errors"
	"fmt"

	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// ErrFilesNeedHTTP: the file system of a device can only be reached over HTTP (Tasmota has no file transfer over
// MQTT: FileUpload takes only firmware and settings, FileDownload only settings), and the device's HTTP is known to
// fail. Nothing was sent.
var ErrFilesNeedHTTP = errors.New("files need HTTP")

var _ tasmota.FileSystem = (*Router)(nil)

// fsHTTP returns the observed HTTP client as a FileSystem, or the error that says why it is none.
// Files travel over HTTP only, also for a backup or restore pinned to MQTT (#607): the pin keeps the dump on its
// transport, and the files have no other. While the device's HTTP is known to fail nothing is sent (each call would
// only wait for a timeout): ErrFilesNeedHTTP.
func (r *Router) fsHTTP(ctx context.Context, t tasmota.Target) (tasmota.FileSystem, error) {
	if err := r.FilesUsable(t); err != nil {
		if errors.Is(err, ErrFilesNeedHTTP) {
			noteTrace(ctx, None)
		}
		return nil, err
	}
	return r.http.(tasmota.FileSystem), nil
}

// FilesUsable is the file calls' gate on its own: ErrFilesNeedHTTP while the HTTP of t's device is known to fail, an
// ErrNotImplemented error when the HTTP client has no file access, nil otherwise. It sends nothing. The files of a
// transfer over MQTT ask it before their identity question over HTTP (#607), which would be asked for nothing.
func (r *Router) FilesUsable(t tasmota.Target) error {
	if id := t.DeviceID(); id != 0 && r.o.Facts != nil && r.o.Facts.HTTPState(id).State == HTTPFailing {
		return ErrFilesNeedHTTP
	}
	_, err := r.http.(*observer).fs() // NewRouter, the only constructor, wraps the HTTP client in an observer
	return err
}

func traceHTTP(ctx context.Context, err error) {
	if tasmota.NotSent(err) {
		noteTrace(ctx, None)
	} else {
		noteTrace(ctx, HTTP)
	}
}

// FSInfo implements tasmota.FileSystem over HTTP.
func (r *Router) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	fs, err := r.fsHTTP(ctx, t)
	if err != nil {
		return tasmota.FSInfo{}, err
	}
	i, err := fs.FSInfo(ctx, t)
	traceHTTP(ctx, err)
	return i, err
}

// ListFiles implements tasmota.FileSystem over HTTP.
func (r *Router) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	fs, err := r.fsHTTP(ctx, t)
	if err != nil {
		return nil, err
	}
	e, err := fs.ListFiles(ctx, t, dir)
	traceHTTP(ctx, err)
	return e, err
}

// DownloadFile implements tasmota.FileSystem over HTTP.
func (r *Router) DownloadFile(ctx context.Context, t tasmota.Target, path string, max int64) ([]byte, error) {
	fs, err := r.fsHTTP(ctx, t)
	if err != nil {
		return nil, err
	}
	b, err := fs.DownloadFile(ctx, t, path, max)
	traceHTTP(ctx, err)
	return b, err
}

// UploadFile implements tasmota.FileSystem over HTTP.
func (r *Router) UploadFile(ctx context.Context, t tasmota.Target, path string, data []byte) error {
	fs, err := r.fsHTTP(ctx, t)
	if err != nil {
		return err
	}
	err = fs.UploadFile(ctx, t, path, data)
	traceHTTP(ctx, err)
	return err
}

// ---- the observer passes file calls through and learns from them

var _ tasmota.FileSystem = (*observer)(nil)

func (o *observer) fs() (tasmota.FileSystem, error) {
	fs, ok := o.c.(tasmota.FileSystem)
	if !ok {
		return nil, fmt.Errorf("%w: the HTTP client has no file system access", tasmota.ErrNotImplemented)
	}
	return fs, nil
}

func (o *observer) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	fs, err := o.fs()
	if err != nil {
		return tasmota.FSInfo{}, err
	}
	i, err := fs.FSInfo(ctx, t)
	o.see(t, err)
	return i, err
}

func (o *observer) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	fs, err := o.fs()
	if err != nil {
		return nil, err
	}
	e, err := fs.ListFiles(ctx, t, dir)
	o.see(t, err)
	return e, err
}

func (o *observer) DownloadFile(ctx context.Context, t tasmota.Target, path string, max int64) ([]byte, error) {
	fs, err := o.fs()
	if err != nil {
		return nil, err
	}
	b, err := fs.DownloadFile(ctx, t, path, max)
	o.see(t, err)
	return b, err
}

func (o *observer) UploadFile(ctx context.Context, t tasmota.Target, path string, data []byte) error {
	fs, err := o.fs()
	if err != nil {
		return err
	}
	err = fs.UploadFile(ctx, t, path, data)
	o.see(t, err)
	return err
}

// ---- the restore upload (#430): HTTP only, like the files

var (
	_ tasmota.ConfigRestorer = (*Router)(nil)
	_ tasmota.ConfigRestorer = (*observer)(nil)
)

// RestoreConfig implements tasmota.ConfigRestorer over HTTP. A restore over MQTT is UploadConfig
// (FileUpload); a call pinned to MQTT is refused before anything is sent, and nothing fails over.
func (r *Router) RestoreConfig(ctx context.Context, t tasmota.Target, dump []byte) error {
	if p, ok := PathFrom(ctx); ok && p == MQTT {
		noteTrace(ctx, None)
		return fmt.Errorf("%w: %w: the restore upload is pinned to MQTT", tasmota.ErrNotSent, tasmota.ErrNotImplemented)
	}
	cr, ok := r.http.(tasmota.ConfigRestorer)
	if !ok {
		return fmt.Errorf("%w: %w: the HTTP client cannot restore a configuration", tasmota.ErrNotSent, tasmota.ErrNotImplemented)
	}
	err := cr.RestoreConfig(ctx, t, dump)
	traceHTTP(ctx, err)
	return err
}

func (o *observer) RestoreConfig(ctx context.Context, t tasmota.Target, dump []byte) error {
	cr, ok := o.c.(tasmota.ConfigRestorer)
	if !ok {
		return fmt.Errorf("%w: %w: the HTTP client cannot restore a configuration", tasmota.ErrNotSent, tasmota.ErrNotImplemented)
	}
	err := cr.RestoreConfig(ctx, t, dump)
	o.see(t, err)
	return err
}
