package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Static files keep one URL across releases, so a long cache lifetime would
// leave browsers on the old stylesheet after an upgrade. Every asset link in a
// page therefore carries ?v=<first 8 hex of the file's SHA-256>: a new build
// changes the URL, the old one is never asked for again, and the hashed URL can
// be cached "immutable". Requests without the hash (the manifest, icons
// referenced from outside) are revalidated with an ETag instead.
//
// A stylesheet's own url(...) references (the Geist fonts) get the same
// ?v=<hash>: unversioned, every page load revalidated the fonts, the text was
// painted in the fallback font first and then swapped (a flash on every
// navigation). The stylesheet is served rewritten, and its hash is taken over
// the rewritten bytes, so a new font also gives the stylesheet a new URL.
var (
	assetOnce   sync.Once
	assetHashes map[string]string
	assetBodies map[string][]byte // rewritten stylesheets, served instead of the file
)

func loadAssetHashes() {
	static, err := fs.Sub(Assets, "static")
	if err != nil {
		assetHashes = map[string]string{}
		return
	}
	assetHashes, assetBodies = hashAssets(static)
}

// cssURLRe matches url("name") with a plain file name (no path, no query).
var cssURLRe = regexp.MustCompile(`url\("([A-Za-z0-9._-]+)"\)`)

func hashAssets(static fs.FS) (map[string]string, map[string][]byte) {
	hashes, bodies := map[string]string{}, map[string][]byte{}
	sum := func(b []byte) string {
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])[:8]
	}
	entries, err := fs.ReadDir(static, ".")
	if err != nil {
		return hashes, bodies
	}
	var css []string
	for _, e := range entries { // flat directory; an unreadable file just gets no hash and no immutable caching
		if e.IsDir() {
			continue
		}
		if b, err := fs.ReadFile(static, e.Name()); err == nil {
			hashes[e.Name()] = sum(b)
			if strings.HasSuffix(e.Name(), ".css") {
				css = append(css, e.Name())
				bodies[e.Name()] = b
			}
		}
	}
	for _, name := range css { // after every other file has its hash
		b := cssURLRe.ReplaceAllFunc(bodies[name], func(m []byte) []byte {
			ref := string(cssURLRe.FindSubmatch(m)[1])
			if h := hashes[ref]; h != "" && !strings.HasSuffix(ref, ".css") {
				return []byte(`url("` + ref + `?v=` + h + `")`)
			}
			return m
		})
		bodies[name] = b
		hashes[name] = sum(b)
	}
	return hashes, bodies
}

func assetHash(name string) string {
	assetOnce.Do(loadAssetHashes)
	return assetHashes[name]
}

// asset returns "name?v=<hash>" for use after "/static/" in a template.
func asset(name string) string {
	if h := assetHash(name); h != "" {
		return name + "?v=" + h
	}
	return name
}

// cacheStatic serves static files: with the current hash in ?v= they are
// immutable for a year, otherwise they must be revalidated (ETag).
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		h := assetHash(name)
		switch {
		case h != "" && r.URL.Query().Get("v") == h:
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case h != "":
			etag := `"` + h + `"`
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		if b, ok := assetBodies[name]; ok {
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
			return
		}
		next.ServeHTTP(w, r)
	})
}
