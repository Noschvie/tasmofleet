package web

import (
	"mime"
	"net/http"
)

func init() {
	// The standard table lacks .webmanifest; without it the manifest is served
	// as text/plain and browsers ignore it.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json") // only fails on a malformed extension
}

// serveFavicon answers /favicon.ico (browsers ask for it regardless of the
// <link> tags) from the embedded icon file.
func serveFavicon(w http.ResponseWriter, _ *http.Request) {
	b, err := Assets.ReadFile("static/favicon.ico")
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b) // a failed write means the client went away
}
