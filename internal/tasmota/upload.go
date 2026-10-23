package tasmota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// uploadTimeout covers the whole transfer: the device writes flash while it
// receives the body, which takes well over a minute on a slow link.
const uploadTimeout = 3 * time.Minute

// Errors of UploadFirmware, next to the shared ones.
var (
	// ErrUploadRefused: the device answered with its "Upload Failed" page.
	// Nothing was flashed.
	ErrUploadRefused = errors.New("tasmota: device refused the upload")
	// ErrUploadUnsupported: the device's /up page has no plain upload form
	// (for example an ESP32 on a single OTA partition). Nothing was sent.
	ErrUploadUnsupported = errors.New("tasmota: device offers no plain firmware upload")
)

// Uploader is implemented by the production client next to Client. It is a
// separate interface so the many fakes of Client stay unchanged.
type Uploader interface {
	// UploadFirmware does what the web UI's "Upgrade by file upload" does:
	// GET /up, then a multipart POST of size bytes from body to /u2. A nil
	// error means the device accepted the image and restarts. Any other
	// error after the body was started (timeout, lost connection, a reply
	// that is neither Successful nor Failed) leaves it open whether the
	// device flashes: the caller must probe, never resend. ErrUploadRefused
	// and ErrUploadUnsupported are certain: nothing was flashed.
	UploadFirmware(ctx context.Context, t Target, filename string, size int64, body io.Reader) error
}

var _ Uploader = (*httpClient)(nil)

// The /up page's upload form, as Tasmota's HTTP_FORM_RST_UPG writes it. The
// form action matters: Tasmota only flashes a POST /u2 after a GET of /up has
// told it what is being uploaded. Without that, the POST answers "Successful"
// and restarts the device without flashing anything.
var upUploadForm = regexp.MustCompile(`(?i)<form[^>]*action=['"]u2\?fsz=['"][^>]*>[\s\S]*?name=['"]u2['"]`)

var htmlTag = regexp.MustCompile(`<[^>]*>`)

func (c *httpClient) UploadFirmware(ctx context.Context, t Target, filename string, size int64, body io.Reader) error {
	if size <= 0 {
		return &deviceError{ErrBadResponse, "empty firmware image"}
	}
	// One slot for both requests: the upload type is global device state, so
	// no other request may come between GET /up and the POST.
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(uploadTimeout))
	defer cancel()

	page, err := c.fetch(rctx, ctx, t, http.MethodGet, "/up", "", nil, "", 0)
	if err != nil {
		return err
	}
	if !upUploadForm.Match(page) {
		return &deviceError{ErrUploadUnsupported, "the upgrade page has no file upload form"}
	}

	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if _, err := mw.CreateFormFile("u2", path.Base(filename)); err != nil {
		return &deviceError{ErrBadResponse, "invalid file name"}
	}
	headLen := head.Len()
	if err := mw.Close(); err != nil {
		return &deviceError{ErrBadResponse, "invalid multipart body"}
	}
	tail := bytes.NewReader(head.Bytes()[headLen:])
	headR := bytes.NewReader(head.Bytes()[:headLen])
	// Tasmota's web server cannot read chunked bodies: the length is exact.
	total := int64(headLen) + size + int64(tail.Len())
	reader := io.MultiReader(headR, &exactReader{r: body, left: size}, tail)
	reply, err := c.fetch(rctx, ctx, t, http.MethodPost, "/u2", "fsz="+url.QueryEscape(strconv.FormatInt(size, 10)),
		reader, mw.FormDataContentType(), total)
	if err != nil {
		return err
	}
	text := string(reply)
	switch {
	case strings.Contains(text, "Failed</font>"):
		return &deviceError{ErrUploadRefused, t.redact(refusalText(text))}
	case strings.Contains(text, "Successful</font>"):
		return nil
	}
	return &deviceError{ErrBadResponse, "upload reply is neither Successful nor Failed"}
}

// exactReader yields exactly left bytes of r and fails when r ends early. The
// device answers once the multipart closing boundary arrives, without waiting
// for the announced length: after a short image the boundary must not follow,
// or the cut image could be flashed.
type exactReader struct {
	r    io.Reader
	left int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	if err == io.EOF {
		if e.left > 0 {
			return n, io.ErrUnexpectedEOF
		}
		err = nil
	}
	return n, err
}

// refusalText is the reason on the Failed page: the text after the heading.
func refusalText(page string) string {
	_, after, ok := strings.Cut(page, "Failed</font></b><br><br>")
	if !ok {
		return "no reason given"
	}
	s := strings.TrimSpace(htmlTag.ReplaceAllString(after, " "))
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		return "no reason given"
	}
	return s
}

// fetch performs one request inside an already acquired device slot.
func (c *httpClient) fetch(rctx, parent context.Context, t Target, method, p, rawQuery string, body io.Reader, ctype string, length int64) ([]byte, error) {
	u := url.URL{Scheme: "http", Host: t.hostPort(), Path: p, RawQuery: rawQuery}
	req, err := http.NewRequestWithContext(rctx, method, u.String(), body)
	if err != nil {
		return nil, &deviceError{ErrBadResponse, "invalid target"}
	}
	if body != nil {
		req.ContentLength = length
		req.Header.Set("Content-Type", ctype)
	}
	if t.user != "" || t.pass != "" {
		user := t.user
		if user == "" {
			user = "admin" // Tasmota's fixed web user
		}
		req.SetBasicAuth(user, t.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.mapErr(parent, t, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body, nothing to recover
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &deviceError{ErrAuth, "HTTP 401"}
	case resp.StatusCode != http.StatusOK:
		return nil, &deviceError{ErrBadResponse, "HTTP " + strconv.Itoa(resp.StatusCode)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.o.MaxResponse+1))
	if err != nil {
		return nil, c.mapErr(parent, t, err)
	}
	if int64(len(b)) > c.o.MaxResponse {
		return nil, &deviceError{ErrBadResponse, "response larger than limit"}
	}
	return b, nil
}
