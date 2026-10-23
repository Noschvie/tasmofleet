package web

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
)

// CustomUploadPath is the one multipart route besides the TasmoAdmin import.
// The app's body guard lets it through (with the CSRF token in the header, so
// the CSRF middleware never parses the file) and caps it at
// firmware.MaxCustomUpload.
const CustomUploadPath = "/firmware/custom"

// maxFieldBytes bounds each text field of the upload form.
const maxFieldBytes = 1 << 10

func (h *handler) registerFirmwareCustom(route routeFunc, adm func(http.Handler) http.Handler) {
	route("POST "+CustomUploadPath, adm, h.firmwareCustomUpload)
}

// customView is an uploaded image as the firmware page shows it.
type customView struct {
	firmware.CustomImage
	InUse bool
}

// customData is the data of the upload answer (fwCustomResult).
type customData struct {
	Error  string
	Notice string
	Base   string
}

func (h *handler) customService() (firmware.Custom, bool) {
	c, ok := h.d.Firmware.(firmware.Custom)
	return c, ok
}

func (h *handler) customReply(w http.ResponseWriter, r *http.Request, status int, d customData) {
	d.Base = httpx.BasePath(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := fwCustomResult(d).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "upload answer not written", "err", err)
	}
}

// firmwareCustomUpload takes the multipart upload: text fields first, the
// image part last, streamed straight into firmware.Custom (no form parsing,
// no buffering of the file here). Container, chip family and the stored name
// are decided by the firmware package from the bytes alone.
func (h *handler) firmwareCustomUpload(w http.ResponseWriter, r *http.Request) {
	cu, ok := h.customService()
	if !ok {
		h.notFound(w, r)
		return
	}
	fail := func(status int, msg, audited string) {
		h.recordFirmware(r, "firmware.custom.upload", "", map[string]any{"reason": audited}, "refused")
		h.customReply(w, r, status, customData{Error: msg})
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		fail(http.StatusUnsupportedMediaType, "The upload must be multipart/form-data.", "not multipart")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, firmware.MaxCustomUpload)
	mr := multipart.NewReader(r.Body, params["boundary"])
	in := firmware.CustomInput{Actor: actor(r)}
	var res firmware.CustomImage
	done := false
	for parts := 0; !done; parts++ {
		if parts > 8 {
			fail(http.StatusBadRequest, "Too many form parts.", "too many parts")
			return
		}
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			h.customReadError(w, r, perr)
			return
		}
		if part.FileName() == "" {
			b, rerr := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if rerr != nil || len(b) > maxFieldBytes {
				fail(http.StatusBadRequest, "A form field is unreadable or too long.", "bad field")
				return
			}
			switch part.FormName() {
			case "label":
				in.Label = string(b)
			case "version":
				in.Version = string(b)
			case "variant":
				in.Variant = string(b)
			case "single_core":
				in.SingleCore = string(b) == "1"
			}
			continue
		}
		if part.FormName() != "image" {
			fail(http.StatusBadRequest, "Unexpected file field.", "unexpected file field")
			return
		}
		// The browser's file name and content type are display data only.
		in.FileName, in.Body = part.FileName(), part
		res, err = cu.AddCustom(r.Context(), in)
		done = true
	}
	if !done {
		fail(http.StatusUnprocessableEntity, "Choose an image file.", "no file")
		return
	}
	if err != nil {
		h.customAddError(w, r, in, err)
		return
	}
	a := res.Artifact
	h.recordFirmware(r, "firmware.custom.upload", "firmware:"+a.Release+"/"+a.Filename, map[string]any{
		"release": a.Release, "file": a.Filename, "family": string(a.Family), "variant": a.Variant, "label": res.Label,
		"original_name": res.OriginalName, "sha256": a.SHA256, "size": a.Size,
	}, "ok")
	// A full page load shows the new row in the list.
	w.Header().Set("HX-Redirect", httpx.BasePath(r.Context())+"/firmware")
	h.customReply(w, r, http.StatusOK, customData{Notice: "Stored " + res.Label + " as " + a.Release + "/" + a.Filename + "."})
}

func (h *handler) customReadError(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		h.recordFirmware(r, "firmware.custom.upload", "", map[string]any{"reason": "request too large"}, "refused")
		h.customReply(w, r, http.StatusRequestEntityTooLarge, customData{Error: "The upload is too large."})
		return
	}
	h.recordFirmware(r, "firmware.custom.upload", "", map[string]any{"reason": "unreadable"}, "refused")
	h.customReply(w, r, http.StatusBadRequest, customData{Error: "The upload could not be read."})
}

// customAddError maps the firmware package's refusals to a status and a
// message. The refusal texts are written by that package (they quote the
// cleaned file name and the variant, nothing from the image content).
func (h *handler) customAddError(w http.ResponseWriter, r *http.Request, in firmware.CustomInput, err error) {
	var mbe *http.MaxBytesError
	status, msg := http.StatusInternalServerError, "The image could not be stored."
	switch {
	case errors.As(err, &mbe):
		status, msg = http.StatusRequestEntityTooLarge, "The upload is too large."
	case errors.Is(err, firmware.ErrTooLarge):
		status, msg = http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, firmware.ErrCustomImage):
		status, msg = http.StatusUnprocessableEntity, err.Error()
	default:
		h.log.ErrorContext(r.Context(), "custom firmware upload failed", "err", err)
	}
	h.recordFirmware(r, "firmware.custom.upload", "", map[string]any{
		"version": in.Version, "label": in.Label, "original_name": firmware.CleanFileName(in.FileName), "reason": msg,
	}, "refused")
	h.customReply(w, r, status, customData{Error: msg})
}

// customOption is one uploaded image in the release selector of the update page.
type customOption struct {
	Key     string // posted as "custom:<key>" in the "release" field
	Version string
	Label   string
}

const customReleasePrefix = "custom:"

// customRelease returns the version of the uploaded image with the given key.
func (h *handler) customRelease(ctx context.Context, key string) (string, error) {
	cu, ok := h.customService()
	if !ok {
		return "", errors.New("custom images are not available")
	}
	imgs, err := cu.CustomImages(ctx)
	if err != nil {
		return "", errors.New("the uploaded images could not be read")
	}
	for _, img := range imgs {
		if img.Artifact.Source == firmware.CustomSource && img.Artifact.Custom == key {
			return img.Artifact.Release, nil
		}
	}
	return "", errors.New("that custom image no longer exists")
}

// auditCustomUse records that a transaction installs an uploaded image.
func (h *handler) auditCustomUse(r *http.Request, batch model.BatchID, reqs []ota.Request) {
	cu, ok := h.customService()
	if !ok || len(reqs) == 0 {
		return
	}
	imgs, err := cu.CustomImages(r.Context())
	if err != nil {
		h.log.WarnContext(r.Context(), "custom firmware use not audited: image list unreadable", "err", err)
		return
	}
	for _, img := range imgs {
		if img.Artifact.Source == firmware.CustomSource && reqs[0].Custom != "" && img.Artifact.Custom == reqs[0].Custom {
			h.recordFirmware(r, "firmware.custom.use", "transaction:"+strconv.FormatInt(int64(batch), 10), map[string]any{
				"release": img.Artifact.Release, "custom": img.Artifact.Custom, "file": img.Artifact.Filename, "label": img.Label, "sha256": img.Artifact.SHA256, "devices": len(reqs),
			}, "ok")
		}
	}
}

// customText names an upload for the preview: "custom: label (version)".
func (h *handler) customText(ctx context.Context, key, version string) string {
	if cu, ok := h.customService(); ok {
		if imgs, err := cu.CustomImages(ctx); err == nil {
			for _, img := range imgs {
				if img.Artifact.Source == firmware.CustomSource && img.Artifact.Custom == key {
					return "custom: " + img.Label + " (" + version + ")"
				}
			}
		}
	}
	return "custom (" + version + ")"
}
