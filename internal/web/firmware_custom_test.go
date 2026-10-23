package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

func customEnv(t *testing.T) (*env, *fakeOTA, *store.SQLite) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cache, err := firmware.New(firmware.Options{Dir: filepath.Join(dir, "fw"), BaseURL: "http://172.31.12.61",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Inspect: func(h []byte) (model.ChipFamily, error) {
			info, err := fwselect.InspectImage(h)
			return info.Family, err
		}}, st)
	if err != nil {
		t.Fatal(err)
	}
	o := &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}}
	e := setupWith(t, func(d *Deps) {
		d.Firmware, d.Artifacts, d.OTA, d.Jobs, d.Batches = cache, st, o, &fakeJobs{}, fakeBatches{}
	})
	return e, o, st
}

func headerOf(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "firmware-headers", file+".hdr"))
	if err != nil {
		t.Fatal(err)
	}
	return append(b, bytes.Repeat([]byte{0x42}, 4000)...)
}

// uploadForm builds the multipart body of the firmware page: text fields
// first, the image last, like the browser sends them.
func uploadForm(t *testing.T, fields [][2]string, name string, image []byte) (string, io.Reader) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	fw, err := mw.CreateFormFile("image", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(image)
	_ = mw.Close()
	return mw.FormDataContentType(), &buf
}

func (e *env) uploadFirmware(t *testing.T, role string, ct string, body io.Reader) (*result, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv.URL+"/tf/firmware/custom", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("HX-Request", "true")
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return &result{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

var goodFields = [][2]string{{"label", "my <b>build</b>"}, {"version", "16.0.0.5"}, {"variant", ""}}

// Why: a custom image changes what runs on devices, so only admins may add
// one. Anonymous callers and operators get nothing stored.
func TestCustomUploadIsAdminOnly(t *testing.T) {
	e, _, st := customEnv(t)
	for role, want := range map[string]int{"": http.StatusUnauthorized, "operator": http.StatusForbidden} {
		ct, body := uploadForm(t, goodFields, "x.bin", headerOf(t, "tasmota.bin.gz"))
		if resp, _ := e.uploadFirmware(t, role, ct, body); resp.StatusCode != want {
			t.Errorf("role %q: status %d, want %d", role, resp.StatusCode, want)
		}
	}
	if arts, _ := st.ListArtifacts(context.Background()); len(arts) != 0 {
		t.Fatalf("a refused caller stored %v", arts)
	}
}

// Why: the happy path end to end through the real cache and store: the image
// is stored under the family-derived name, the page lists it (label escaped),
// the update page offers it as "custom" and never preselects it, and the
// upload is audited with the uploader and the checksum.
func TestCustomUploadListedOfferedAndAudited(t *testing.T) {
	e, _, st := customEnv(t)
	ct, body := uploadForm(t, goodFields, "../evil.bin", headerOf(t, "tasmota.bin.gz"))
	resp, out := e.uploadFirmware(t, "admin", ct, body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Redirect") != "/tf/firmware" {
		t.Fatalf("status %d redirect %q: %s", resp.StatusCode, resp.Header.Get("HX-Redirect"), out)
	}
	arts, _ := st.ListArtifacts(context.Background())
	if len(arts) != 1 || arts[0].Source != "custom" || arts[0].Filename != "tasmota.bin.gz" || arts[0].Family != model.ChipESP8266 {
		t.Fatalf("artifacts = %+v", arts)
	}

	_, page := e.do(t, "GET", "/firmware", "admin", nil)
	if !strings.Contains(page, "my &lt;b&gt;build&lt;/b&gt;") || strings.Contains(page, "my <b>build</b>") {
		t.Errorf("label not escaped on the firmware page")
	}
	_, opPage := e.do(t, "GET", "/firmware", "operator", nil)
	if strings.Contains(opPage, `name="image"`) || strings.Contains(opPage, "/firmware/artifacts/1/delete") {
		t.Errorf("operators must not see the upload form or delete links")
	}
	_, form := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(form, ">Custom firmware 16.0.0.5<") || !strings.Contains(form, "my &lt;b&gt;build&lt;/b&gt; · uploaded") {
		t.Errorf("custom release not offered:\n%s", form)
	}
	if regexp.MustCompile(`value="custom:[^"]*" checked`).MatchString(form) || strings.Contains(form, `value="16.0.0.5"`) {
		t.Error("a custom image must never be preselected")
	}

	var got *model.AuditEvent
	for _, ev := range e.aud.events {
		if ev.Action == "firmware.custom.upload" {
			got = &ev
		}
	}
	if got == nil || got.Result != "ok" || got.Actor == "" || !strings.Contains(got.Detail, arts[0].SHA256) || !strings.Contains(got.Detail, `"original_name":"evil.bin"`) {
		t.Fatalf("audit = %+v", got)
	}
}

// Why: a wrong or hostile file must be refused with a clear message, stored
// nowhere, and still leave an audit trail (a refused upload is a security event).
func TestCustomUploadRefusalsAreAuditedAndStoreNothing(t *testing.T) {
	e, _, st := customEnv(t)
	safeboot := headerOf(t, "tasmota32-safeboot.bin")
	tests := []struct {
		name   string
		fields [][2]string
		file   string
		image  []byte
		status int
	}{
		{"garbage", goodFields, "x.bin", bytes.Repeat([]byte("abc"), 1000), http.StatusUnprocessableEntity},
		{"safeboot by name", goodFields, "tasmota32-safeboot.bin", safeboot, http.StatusUnprocessableEntity},
		{"bad version", [][2]string{{"label", "a"}, {"version", "../../x"}}, "x.bin", headerOf(t, "tasmota.bin.gz"), http.StatusUnprocessableEntity},
		{"oversize", goodFields, "x.bin", append(headerOf(t, "tasmota.bin.gz"), bytes.Repeat([]byte{1}, int(firmware.MaxCustomUpload)+1024)...), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		ct, body := uploadForm(t, tc.fields, tc.file, tc.image)
		resp, out := e.uploadFirmware(t, "admin", ct, body)
		if resp.StatusCode != tc.status || !strings.Contains(out, `role="alert"`) {
			t.Errorf("%s: status %d, body %q", tc.name, resp.StatusCode, out)
		}
	}
	if arts, _ := st.ListArtifacts(context.Background()); len(arts) != 0 {
		t.Fatalf("refused uploads stored %v", arts)
	}
	refused := 0
	for _, ev := range e.aud.events {
		if ev.Action == "firmware.custom.upload" && ev.Result == "refused" {
			refused++
		}
	}
	if refused != len(tests) {
		t.Errorf("%d of %d refusals audited", refused, len(tests))
	}
}

// Why: the file part must come after the text fields (the browser sends the
// form in DOM order). A file sent first has no label or version to be checked
// against, so it is refused instead of guessed at.
func TestCustomUploadFileBeforeFieldsIsRefused(t *testing.T) {
	e, _, st := customEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("image", "x.bin")
	_, _ = fw.Write(headerOf(t, "tasmota.bin.gz"))
	_ = mw.WriteField("label", "late")
	_ = mw.WriteField("version", "16.0.0.5")
	_ = mw.Close()
	if resp, _ := e.uploadFirmware(t, "admin", mw.FormDataContentType(), &buf); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status %d", resp.StatusCode)
	}
	if arts, _ := st.ListArtifacts(context.Background()); len(arts) != 0 {
		t.Fatalf("stored %v", arts)
	}
}

// Why: "delete unused" must never remove an upload (it cannot be downloaded
// again), while the per-file delete works and warns that it is final.
func TestCustomImageDeleteIsExplicitOnly(t *testing.T) {
	e, _, st := customEnv(t)
	ct, body := uploadForm(t, goodFields, "x.bin", headerOf(t, "tasmota.bin.gz"))
	if resp, _ := e.uploadFirmware(t, "admin", ct, body); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	_, page := e.do(t, "GET", "/firmware?select=unused", "admin", nil)
	if strings.Contains(page, "checked") && strings.Contains(page, "16.0.0.5") && strings.Contains(page, `aria-label="Select 16.0.0.5 tasmota.bin.gz" checked`) {
		t.Errorf("select-unused picks the upload:\n%s", page)
	}
	_, confirm := e.do(t, "GET", "/firmware/artifacts/1/delete", "admin", nil)
	if !strings.Contains(confirm, "cannot be downloaded again") {
		t.Errorf("confirmation does not say the deletion is final:\n%s", confirm)
	}
	if resp, _ := e.do(t, "POST", "/firmware/artifacts/1/delete", "admin", url.Values{"confirm": {"yes"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
	if arts, _ := st.ListArtifacts(context.Background()); len(arts) != 0 {
		t.Fatalf("left %v", arts)
	}
}

// Why: using an upload is a security-relevant act; the transaction that
// installs it must be traceable to the image (label and checksum).
func TestCustomUseIsAudited(t *testing.T) {
	e, o, st := customEnv(t)
	ct, body := uploadForm(t, goodFields, "x.bin", headerOf(t, "tasmota.bin.gz"))
	if resp, _ := e.uploadFirmware(t, "admin", ct, body); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	arts, _ := st.ListArtifacts(context.Background())
	form := url.Values{"id": {"1"}, "release": {"custom:" + arts[0].Custom}, "policy": {"reinstall"}}
	if resp, _ := e.do(t, "POST", "/updates", "operator", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("submit status %d", resp.StatusCode)
	}
	if len(o.submitReqs) != 1 || o.submitReqs[0].Release != "16.0.0.5" || o.submitReqs[0].Custom != arts[0].Custom {
		t.Fatalf("submitted %+v", o.submitReqs)
	}
	n := 0
	for _, ev := range e.aud.events {
		if ev.Action == "firmware.custom.use" && ev.Result == "ok" && strings.Contains(ev.Detail, `"label":"my \u003cb\u003ebuild\u003c/b\u003e"`) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("custom use audited %d times: %+v", n, e.aud.events)
	}
}

// Why: a stale or forged "custom:" value must not fall back to the upstream
// release of some version; the update is refused.
func TestUpdateWithUnknownCustomKeyIsRefused(t *testing.T) {
	e, o, _ := customEnv(t)
	form := url.Values{"id": {"1"}, "release": {"custom:deadbeef00"}, "policy": {"reinstall"}}
	if resp, _ := e.do(t, "POST", "/updates", "operator", form); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(o.submitReqs) != 0 {
		t.Fatalf("submitted %+v", o.submitReqs)
	}
}
