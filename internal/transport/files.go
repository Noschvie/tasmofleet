package transport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// ErrFilesNeedHTTP: the file calls of the FileSystem go over HTTP, and the device's HTTP is known to fail. Nothing was
// sent. (A backup reads files over MQTT through MQTTListFiles and MQTTDownloadFile where the firmware can, #924.)
var ErrFilesNeedHTTP = errors.New("files need HTTP")

// ErrFilesNotOverMQTT: MQTTListFiles or MQTTDownloadFile sent nothing, because the device is not usable for a
// transfer over MQTT right now (the same rule as the dump's FileDownload: strict MQTT conditions, a password the
// device has not refused, MQTT settings unchanged since the caller read the device) or the channel has no file
// transfer. It wraps the reason; the caller decides whether HTTP may carry the files instead.
var ErrFilesNotOverMQTT = errors.New("files cannot go over MQTT")

// MQTTFileDownloader is what the command channel offers for FileDownload Type 8 (mqttcmd.Channel.DownloadFile).
type MQTTFileDownloader interface {
	DownloadFile(ctx context.Context, d model.Device, password, path string, max int64) ([]byte, error)
}

var _ tasmota.FileSystem = (*Router)(nil)

// fsHTTP returns the observed HTTP client as a FileSystem, or the error that says why it is none.
// The FileSystem calls travel over HTTP only, also for a backup or restore pinned to MQTT (#607): a restore under an
// MQTT pin writes its files over HTTP. A backup that reads its files over MQTT asks MQTTListFiles and MQTTDownloadFile
// for that (#924). While the device's HTTP is known to fail nothing is sent (each call would only wait for a
// timeout): ErrFilesNeedHTTP.
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

// ---- the files of a backup over MQTT (#924)

// MQTTListFiles lists folder dir of the device over MQTT (UfsList on its topic) and reads the answer by the rules of
// the HTTP listing (tasmota.ParseUfsList). d is the device as the caller read it before the transfer began (the
// backup's snapshot): the call refuses, sending nothing, when the device's MQTT settings changed since, or when the
// device is not usable for a transfer over MQTT (mqttFileDevice). It never moves to HTTP.
//
// UfsList lists the device's selected file system (dfsp), which is the SD card while one is mounted; the caller asks
// only devices without one (tasmota.FSInfo.HasSD). The command is sent as a read: cmdpolicy counts "UfsList /dir" as a
// write only because it takes an argument, and it changes nothing but the folder the web upload would write to.
func (r *Router) MQTTListFiles(ctx context.Context, d model.Device, dir string) ([]tasmota.FileEntry, error) {
	if dir != "/" {
		if err := tasmota.CheckFSPath(dir); err != nil {
			return nil, err
		}
	}
	now, _, err := r.mqttFileDevice(ctx, d)
	if err != nil {
		return nil, err
	}
	raw, err := r.sendMQTT(ctx, now, "UfsList "+dir, cmdClass{})
	if err != nil {
		return nil, err
	}
	return tasmota.ParseUfsList(raw)
}

// MQTTDownloadFile reads one file of the device over MQTT (FileDownload Type 8, mqttcmd.Channel.DownloadFile) with
// the device's MqttPassword, under the same rules as the dump's FileDownload and MQTTListFiles: nothing is published
// unless the device is usable for a transfer now and its MQTT settings are those of d. A refused password (Error 1)
// is marked like the dump's. It never moves to HTTP: an Error 3 (mqttcmd.ErrNoFileTransfer, the firmware has no file
// transfer) is the caller's to handle.
func (r *Router) MQTTDownloadFile(ctx context.Context, d model.Device, path string, max int64) ([]byte, error) {
	now, pw, err := r.mqttFileDevice(ctx, d)
	if err != nil {
		return nil, err
	}
	dl, ok := r.mqttDoer().(MQTTFileDownloader)
	if !ok {
		noteTrace(ctx, None)
		return nil, fmt.Errorf("%w: %w: the command channel has no file transfer", ErrFilesNotOverMQTT, tasmota.ErrNotImplemented)
	}
	b, err := dl.DownloadFile(ctx, now, pw, path, max)
	if err == nil || !notPublished(err) || errors.Is(err, mqttcmd.ErrPublished) {
		noteTrace(ctx, MQTT)
	} else {
		noteTrace(ctx, None)
	}
	if errors.Is(err, mqttcmd.ErrPasswordRefused) {
		r.markPasswordBad(ctx, now.ID)
	}
	return b, err
}

// mqttFileDevice is the check before a file call over MQTT: the transfer rule of the dump (transferBlocker: strict
// MQTT conditions and the password), the device read fresh and compared with the caller's snapshot d, the password
// resolved from that read (downloadPassword). Any refusal is ErrFilesNotOverMQTT with nothing sent.
func (r *Router) mqttFileDevice(ctx context.Context, d model.Device) (model.Device, string, error) {
	refuse := func(why string) (model.Device, string, error) {
		noteTrace(ctx, None)
		return model.Device{}, "", fmt.Errorf("%w: %s", ErrFilesNotOverMQTT, why)
	}
	if p, ok := PathFrom(ctx); ok && p != MQTT {
		return refuse("the job is pinned to HTTP")
	}
	if r.mqttDoer() == nil || r.o.Facts == nil {
		return refuse("MQTT is not connected")
	}
	if _, ok := r.mqttDoer().(MQTTFileDownloader); !ok {
		return refuse("the command channel has no file transfer")
	}
	now, err := r.o.Devices.GetDevice(ctx, d.ID)
	if err != nil {
		return model.Device{}, "", err
	}
	if mqttSettingsChanged(now, d) {
		return refuse("the device's MQTT settings changed since the backup began")
	}
	f, err := r.o.Facts.FactsFor(ctx, now)
	if err != nil {
		return model.Device{}, "", err
	}
	if why := transferBlocker(f, "read its files"); why != "" {
		return refuse(why)
	}
	pw, why := r.downloadPassword(now)
	if why != "" {
		return refuse(why)
	}
	return now, pw, nil
}

// markPasswordBad records that the device refused its MqttPassword (Error 1), for every transfer that sends it.
func (r *Router) markPasswordBad(ctx context.Context, id model.DeviceID) {
	if r.o.Marker == nil {
		return
	}
	// The caller may be cancelled by now; the mark must still be written.
	if err := r.o.Marker.MarkMQTTPasswordBad(context.WithoutCancel(ctx), id, time.Now()); err != nil {
		r.o.Log.Warn("transport: could not record the refused MQTT password", "device", int64(id), "err", err)
	}
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
	o.see(ctx, t, err)
	return i, err
}

func (o *observer) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	fs, err := o.fs()
	if err != nil {
		return nil, err
	}
	e, err := fs.ListFiles(ctx, t, dir)
	o.see(ctx, t, err)
	return e, err
}

func (o *observer) DownloadFile(ctx context.Context, t tasmota.Target, path string, max int64) ([]byte, error) {
	fs, err := o.fs()
	if err != nil {
		return nil, err
	}
	b, err := fs.DownloadFile(ctx, t, path, max)
	o.see(ctx, t, err)
	return b, err
}

func (o *observer) UploadFile(ctx context.Context, t tasmota.Target, path string, data []byte) error {
	fs, err := o.fs()
	if err != nil {
		return err
	}
	err = fs.UploadFile(ctx, t, path, data)
	o.see(ctx, t, err)
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
	o.see(ctx, t, err)
	return err
}
