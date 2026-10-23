package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// Why: after an upgrade a browser must never keep an old stylesheet. Pages link
// assets with a content hash, the hashed URL may be cached forever, an unhashed
// or stale one is revalidated, and a changed file therefore has a new URL.
func TestStaticAssetsAreCacheBusted(t *testing.T) {
	h := http.StripPrefix("/static/", cacheStatic(http.FileServerFS(mustStatic(t))))
	link := asset("app.css")
	if !strings.HasPrefix(link, "app.css?v=") || len(link) != len("app.css?v=")+8 {
		t.Fatalf("asset link %q", link)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/"+link, nil))
	if cc := rec.Header().Get("Cache-Control"); rec.Code != 200 || !strings.Contains(cc, "immutable") || !strings.Contains(cc, "31536000") {
		t.Fatalf("hashed url: %d %q", rec.Code, cc)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/app.css?v=deadbeef", nil))
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") || cc != "no-cache" {
		t.Fatalf("stale hash must be revalidated, got %q", cc)
	}
	etag := rec.Header().Get("ETag")
	req := httptest.NewRequest("GET", "/static/app.css", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("etag revalidation: %d", rec.Code)
	}
}

// Why: every page (signed in or not) must use hashed asset links, or the
// problem above comes back through one forgotten template.
func TestPagesLinkHashedAssets(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	for _, want := range []string{"/static/app.css?v=", "/static/select.js?v=", "/static/htmx-2.0.11.min.js?v="} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func mustStatic(t *testing.T) fs.FS {
	t.Helper()
	s, err := fs.Sub(Assets, "static")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Why (v0.10.1): the fonts were the only assets without the hash. Every page load
// revalidated them, the text painted in the fallback font and then swapped (a flash
// on every navigation). Every url() in the served stylesheet must carry the hash,
// and that URL must be cached immutable like any other asset.
func TestStylesheetURLsAreCacheBusted(t *testing.T) {
	h := http.StripPrefix("/static/", cacheStatic(http.FileServerFS(mustStatic(t))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/"+asset("app.css"), nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("app.css: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	refs := cssURLRe.FindAllStringSubmatch(rec.Body.String(), -1)
	all := regexp.MustCompile(`url\(`).FindAllString(rec.Body.String(), -1)
	if len(refs) != 0 || len(all) == 0 {
		t.Fatalf("%d url() references without the hash (of %d)", len(refs), len(all))
	}
	for _, font := range []string{"geist-latin.woff2", "geist-latin-ext.woff2", "geist-mono-latin.woff2", "geist-mono-latin-ext.woff2"} {
		link := asset(font)
		if !strings.Contains(rec.Body.String(), `url("`+link+`")`) {
			t.Errorf("served stylesheet does not use %q", link)
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", "/static/"+link, nil))
		if cc := r.Header().Get("Cache-Control"); r.Code != 200 || !strings.Contains(cc, "immutable") {
			t.Errorf("%s: %d %q", link, r.Code, cc)
		}
	}
}

// Why: the stylesheet's hash must change when only a font changes, or a browser
// keeps the cached (immutable) stylesheet that points at the old font.
func TestStylesheetHashFollowsItsFonts(t *testing.T) {
	css := &fstest.MapFile{Data: []byte(`@font-face { src: url("f.woff2") format("woff2"); }`)}
	a, bodies := hashAssets(fstest.MapFS{"app.css": css, "f.woff2": {Data: []byte("one")}})
	b, _ := hashAssets(fstest.MapFS{"app.css": css, "f.woff2": {Data: []byte("two")}})
	if a["app.css"] == b["app.css"] {
		t.Fatal("stylesheet hash did not change with its font")
	}
	if want := `url("f.woff2?v=` + a["f.woff2"] + `")`; !strings.Contains(string(bodies["app.css"]), want) {
		t.Fatalf("rewritten stylesheet %q, want %q", bodies["app.css"], want)
	}
}

// Why: the fonts are requested before the stylesheet is parsed, from the same versioned URL. Geist
// Mono is on almost every page (addresses, versions, ids), so it is preloaded next to the sans.
func TestLayoutPreloadsTheFonts(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	for _, font := range []string{"geist-latin.woff2", "geist-mono-latin.woff2"} {
		want := `/static/` + asset(font) + `" as="font" type="font/woff2" crossorigin>`
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
