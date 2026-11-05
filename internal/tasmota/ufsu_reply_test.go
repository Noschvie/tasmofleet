package tasmota

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ufsuTestPage is HandleUploadUFSDone's page (xdrv_50_filesystem.ino) built from the language's
// words: WSContentSendStyle's head without any reload script, the coloured heading, then
// "<br>" + "</div>" after a success or "<br><br>" + reason after a failure, then the link back.
// It is written out here on purpose and not taken from the simulator, so that the two cannot
// drift together.
func ufsuTestPage(l ufsuLanguage, ok bool, reason, colour string) string {
	p := `<!DOCTYPE html><html lang="` + l.lang + `" class=""><head><meta charset='utf-8'><title>Information</title><script>var x=null,lt,to,tp,pc='';</script>` +
		`<style>body{}</style></head><body><div style='text-align:left;display:inline-block;'><div style='text-align:center;'><b>` + l.upload + ` <font color='#` + colour + `'>`
	if ok {
		p += l.successful + `</font></b><br>`
	} else {
		p += l.failed + `</font></b><br><br>` + reason
	}
	return p + `</div><br><p></p><form action='ufsd' method='get'><input type='hidden' name='download' value='/' /> <button>` + l.manageFS + `</button></form></div></body></html>`
}

func ufsuLang(t *testing.T, file string) ufsuLanguage {
	t.Helper()
	for _, l := range ufsuLanguages {
		if l.file == file {
			return l
		}
	}
	t.Fatalf("no language %s", file)
	return ufsuLanguage{}
}

// uploadFileTo runs UploadFile against a device whose /ufsu answers with page.
func uploadFileTo(t *testing.T, page string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ufsd" {
			_, _ = w.Write([]byte("<html>Manage File system</html>"))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	return newClient(Options{}).(FileSystem).UploadFile(ctxS(t), target(t, srv, 1, "", ""), "/a.txt", []byte("x"))
}

// Why (#618): a file restore on an ESP32 whose Tasmota build has a localized web UI ended "FILE
// LOST" although the file was written: the client looked for the English "Successful</font>", so
// every reply of such a device was unknown. The words are translated; the page structure is not.
// Success and failure must be told apart in every language build, and a failure must name its
// reason (the caller shows it, and a certain refusal is not an unknown outcome).
func TestUploadFileReadsTheReplyInEveryLanguage(t *testing.T) {
	for _, l := range ufsuLanguages {
		t.Run(l.file, func(t *testing.T) {
			if err := uploadFileTo(t, ufsuTestPage(l, true, "", "008000")); err != nil {
				t.Errorf("success page read as %v", err)
			}
			err := uploadFileTo(t, ufsuTestPage(l, false, l.reason, "ff5661"))
			if !errors.Is(err, ErrUploadRefused) {
				t.Fatalf("failure page read as %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), strings.TrimSpace(l.reason)) {
				t.Errorf("reason %q is not in %q", l.reason, err)
			}
		})
	}
}

// Why: the colours come from WebColor (the theme can be changed with the WebColor command) and
// say nothing certain. A failure in the success colour must stay a failure and a success in the
// failure colour a success, or a themed device would be misread.
func TestUploadFileIgnoresTheHeadingColour(t *testing.T) {
	l := ufsuLanguages[0]
	if err := uploadFileTo(t, ufsuTestPage(l, true, "", "ff5661")); err != nil {
		t.Errorf("success in the failure colour: %v", err)
	}
	if err := uploadFileTo(t, ufsuTestPage(l, false, "No space", "008000")); !errors.Is(err, ErrUploadRefused) {
		t.Errorf("failure in the success colour: %v", err)
	}
}

// Why: fail closed. A failure read as success lets a restore believe a file was written (and
// counts it as verified or skips the put-back); an unknown reply is handled with the put-back.
// Whatever follows the heading, a failure page never reads as success, also when its reason text
// carries the success markup or the page is cut short.
func TestUploadFileFailureNeverReadsAsSuccess(t *testing.T) {
	l := ufsuLanguages[0] // af: tab in the heading word, a fair stand-in for odd translations
	head := `<div style='text-align:center;'><b>` + l.upload + ` <font color='#ff5661'>` + l.failed + `</font></b><br><br>`
	for name, page := range map[string]string{
		"reason with the success tail":    head + `x</font></b><br></div>`,
		"reason with a second heading":    head + `<font color='#008000'>ok</font></b><br></div>`,
		"empty reason":                    head + `</div><br>`,
		"English failure":                 `Upload <font color='#ff5661'>Failed</font></b><br><br>No file selected</div>`,
		"failure cut after the heading":   head,
		"failure with the reload script":  `<script>setTimeout(function(){location.href='.';},10000);</script>` + head + `Not enough space</div>`,
		"failure after a stray heading":   `<b>x</b><br></div>` + head + `Not enough space</div>`,
		"failure with the restart line":   head + `Not enough space</div><div>Device will restart in a few seconds</div>`,
		"heading with a nested tag":       `<font color='#ff5661'><b>x</b></font></b><br><br>Not enough space</div>`,
		"colour not six hex digits":       `<font color='#ff'>` + l.failed + `</font></b><br><br>Not enough space</div>`,
		"failure heading without closing": `<font color='#ff5661'>` + l.failed + `</b><br><br>Not enough space</div>`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := uploadFileTo(t, page); err == nil {
				t.Errorf("read as success: %s", page)
			}
		})
	}
}

// Why: only the page HandleUploadUFSDone sends is an answer to the upload. A device before
// v13.4 (through v13.3.0) answers 303 and sends no result at all, a proxy or a captive portal sends something of its
// own, a cut reply has no end. All of these are unknown (the caller then checks the file), never a
// success and never a refusal with an invented reason.
func TestUploadFileUnknownPagesStayUnknown(t *testing.T) {
	l := ufsuLanguages[0]
	for name, page := range map[string]string{
		"empty":                            ``,
		"some other page":                  `<html><body>Hello</body></html>`,
		"English wording alone":            `Upload <font color='#008000'>Successful</font>`,
		"heading only":                     `<b>Upload <font color='#008000'>Successful</font></b>`,
		"success cut before the div":       `<b>` + l.upload + ` <font color='#008000'>` + l.successful + `</font></b><br>`,
		"success with text after the br":   `<b>` + l.upload + ` <font color='#008000'>` + l.successful + `</font></b><br>more</div>`,
		"heading with a nested tag":        `<b>x <font color='#008000'><i>` + l.successful + `</i></font></b><br></div>`,
		"no colour attribute":              `<b>x <font>` + l.successful + `</font></b><br></div>`,
		"double quoted colour":             `<b>x <font color="#008000">` + l.successful + `</font></b><br></div>`,
		"heading not closed with the bold": `<b>x <font color='#008000'>` + l.successful + `</font><br></div>`,
	} {
		t.Run(name, func(t *testing.T) {
			err := uploadFileTo(t, page)
			if !errors.Is(err, ErrBadResponse) || errors.Is(err, ErrUploadRefused) {
				t.Errorf("got %v, want an unknown reply (ErrBadResponse)", err)
			}
		})
	}
}

// Why: the firmware upload page (HandleUploadDone: reload script, restart line) is not the /ufsu
// page. Its success reads as "heading, <br><br>, text", which is the shape of a failure: a device
// that answers /ufsu with it is refused, never taken for a written file. (Reusing the firmware
// reader here would do the opposite and call every real /ufsu success a refusal.)
func TestUploadFileDoesNotTakeTheFirmwarePageForSuccess(t *testing.T) {
	l := ufsuLanguages[0]
	page := `<script>setTimeout(function(){location.href='.';},10000);</script><b>` + l.upload + ` <font color='#008000'>` + l.successful +
		`</font></b><br><br><div>Device will restart in a few seconds</div>`
	if err := uploadFileTo(t, page); err == nil {
		t.Error("the firmware upload page was read as a written file")
	}
}
