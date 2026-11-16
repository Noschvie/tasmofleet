package firmware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/fwselect"
)

const (
	maxIndexBytes = 16 << 20 // the real GitHub reply for per_page=10 is ~5.6 MB (release notes, 280+ assets each)
	maxRedirects  = 5
)

// srcIndex is the parsed index of one source: version -> file -> download URL.
type srcIndex map[string]map[string]string

// relMeta is what a source says about a release besides its files: the
// release body (changelog) and the release page. Only the GitHub source
// has it. The body is cut to MaxNotesBytes when it is read.
type relMeta struct {
	Notes     string
	Truncated bool
	URL       string // validated with SafeLink
	Published time.Time
}

// checkURL enforces the SSRF rules for one source: https unless the source
// is configured for http, and host (with or without port) on its allowlist.
func (s Source) checkURL(u *url.URL) error {
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && s.AllowHTTP:
	default:
		return fmt.Errorf("%w: scheme %q for source %s", ErrSourceNotAllowed, u.Scheme, s.Name)
	}
	for _, h := range s.AllowedHosts {
		if strings.EqualFold(h, u.Host) || strings.EqualFold(h, u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("%w: host %q for source %s", ErrSourceNotAllowed, u.Host, s.Name)
}

// get performs a GET for source s. The first URL and every redirect hop are
// checked against the source's allowlist before any request is sent.
func (c *cache) get(ctx context.Context, s Source, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceNotAllowed, err)
	}
	if err := s.checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tasmofleet")
	cl := *c.client // copy: CheckRedirect is per source
	cl.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("firmware: more than %d redirects", maxRedirects)
		}
		return s.checkURL(r.URL)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() // error path, the status is what matters
		return nil, fmt.Errorf("firmware: %s answered %s", u.Host, resp.Status)
	}
	return resp, nil
}

func (c *cache) getSmall(ctx context.Context, s Source, rawURL string) ([]byte, error) {
	resp, err := c.get(ctx, s, rawURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // read-only body
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxIndexBytes {
		return nil, fmt.Errorf("%w: index of %s", ErrTooLarge, s.Name)
	}
	return b, nil
}

// fetchIndex reads the index of one source.
func (c *cache) fetchIndex(ctx context.Context, s Source) (srcIndex, map[string]relMeta, error) {
	switch s.Kind {
	case "github-releases":
		return c.fetchGitHub(ctx, s)
	case "ota-tasmota":
		idx, err := c.fetchOTA(ctx, s)
		return idx, nil, err
	default:
		return nil, nil, fmt.Errorf("firmware: source %s has unknown kind %q", s.Name, s.Kind)
	}
}

type ghRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Body       string `json:"body"`
	HTMLURL    string `json:"html_url"`
	Published  string `json:"published_at"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (c *cache) fetchGitHub(ctx context.Context, s Source) (srcIndex, map[string]relMeta, error) {
	base := strings.TrimSuffix(s.BaseURL, "/")
	b, err := c.getSmall(ctx, s, base+"/releases?per_page=10")
	if err != nil {
		return nil, nil, err
	}
	var rels []ghRelease
	if err := json.Unmarshal(b, &rels); err != nil {
		return nil, nil, fmt.Errorf("firmware: %s index is not the expected JSON: %w", s.Name, err)
	}
	idx := srcIndex{}
	meta := map[string]relMeta{}
	for _, r := range rels {
		addGHRelease(idx, meta, r)
	}
	// The settings-migration step (#892) must stay in the index after it left the newest releases. A failure
	// leaves it out, and Ensure then fails loud for it (not in the index, or offered by one source only).
	if _, ok := idx[fwselect.StepRelease]; !ok {
		var r ghRelease
		b, err := c.getSmall(ctx, s, base+"/releases/tags/v"+fwselect.StepRelease)
		if err == nil {
			err = json.Unmarshal(b, &r)
		}
		if err != nil {
			c.log.Warn("firmware: step release not read", "source", s.Name, "release", fwselect.StepRelease, "err", err)
		} else if strings.TrimPrefix(r.Tag, "v") == fwselect.StepRelease {
			addGHRelease(idx, meta, r)
		}
	}
	return idx, meta, nil
}

// addGHRelease adds the OTA files and the metadata of one GitHub release to the index.
func addGHRelease(idx srcIndex, meta map[string]relMeta, r ghRelease) {
	// Only stable releases: an rc must never show up in the update dialog.
	if r.Draft || r.Prerelease {
		return
	}
	ver := strings.TrimPrefix(r.Tag, "v")
	if !validRelease(ver) {
		return
	}
	m := relMeta{}
	m.Notes, m.Truncated = TruncateNotes(r.Body)
	m.URL, _ = SafeLink(r.HTMLURL) // empty when missing or unsafe
	m.Published, _ = time.Parse(time.RFC3339, r.Published)
	meta[ver] = m
	for _, a := range r.Assets {
		if _, err := parseName(a.Name); err != nil {
			continue // maps, factory images, anything that is no OTA candidate
		}
		if idx[ver] == nil {
			idx[ver] = map[string]string{}
		}
		idx[ver][a.Name] = a.URL
	}
}

var (
	// ota.tasmota.com writes href=name unquoted in its file table, other links quoted.
	hrefRe    = regexp.MustCompile(`(?i)href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>"']+))`)
	headingRe = regexp.MustCompile(`(?is)<(?:h[1-3]|title)[^>]*>(.*?)</(?:h[1-3]|title)>`)
	versionRe = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)
)

// otaDirs are the two release directories of ota.tasmota.com (spec 7.1), and otaStepDirs their archive
// directories of the settings-migration step release (fwselect.StepRelease, #892), in the same order.
var (
	otaDirs     = []string{"/tasmota/release/", "/tasmota32/release/"}
	otaStepDirs = []string{"/tasmota/release-" + fwselect.StepRelease + "/", "/tasmota32/release-" + fwselect.StepRelease + "/"}
)

func (c *cache) fetchOTA(ctx context.Context, s Source) (srcIndex, error) {
	idx := srcIndex{}
	for i, dir := range otaDirs {
		ver, err := c.readOTADir(ctx, s, dir, idx)
		if err != nil {
			return nil, err
		}
		if ver == fwselect.StepRelease {
			continue // the release directory still is the step release
		}
		// After the next release only the archive directory keeps the step. A failure there leaves it out
		// (Ensure then fails loud for it) and never costs the source its current release.
		step := otaStepDirs[i]
		if ver, err := c.readOTADir(ctx, s, step, idx); err != nil || ver != fwselect.StepRelease {
			c.log.Warn("firmware: step release not read", "source", s.Name, "dir", step, "listed", ver, "err", err)
		}
	}
	return idx, nil
}

// readOTADir adds the files of one ota.tasmota.com directory to idx and returns the release it lists. The
// files of a directory whose path ends in release-<version>/ are added only when it lists that version.
func (c *cache) readOTADir(ctx context.Context, s Source, dir string, idx srcIndex) (string, error) {
	b, err := c.getSmall(ctx, s, strings.TrimSuffix(s.BaseURL, "/")+dir)
	if err != nil {
		return "", err
	}
	ver, files, err := parseOTAListing(string(b))
	if err != nil {
		return "", fmt.Errorf("firmware: %s%s: %w", s.Name, dir, err)
	}
	if want, pinned := strings.CutPrefix(path.Base(dir), "release-"); pinned && want != ver {
		return ver, nil
	}
	esp32Dir := strings.HasPrefix(dir, "/tasmota32/")
	for _, f := range files {
		// The directory is fixed by the file family, so a name listed in
		// the wrong directory cannot make us fetch from an odd place.
		if strings.HasPrefix(f, "tasmota32") != esp32Dir {
			continue
		}
		if idx[ver] == nil {
			idx[ver] = map[string]string{}
		}
		idx[ver][f] = strings.TrimSuffix(s.BaseURL, "/") + dir + f
	}
	return ver, nil
}

// parseOTAListing extracts the version (first x.y.z in a heading or title)
// and the OTA image file names (only plain href file names that parse as
// Tasmota OTA names; links, sort headers and parent directories are ignored).
func parseOTAListing(html string) (string, []string, error) {
	var ver string
	for _, m := range headingRe.FindAllStringSubmatch(html, -1) {
		if v := versionRe.FindStringSubmatch(m[1]); v != nil {
			ver = v[1]
			break
		}
	}
	if ver == "" || !validRelease(ver) {
		return "", nil, fmt.Errorf("no release version found in listing")
	}
	seen := map[string]bool{}
	var files []string
	for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
		name := m[1] + m[2] + m[3]
		if _, err := parseName(name); err != nil || seen[name] {
			continue
		}
		seen[name] = true
		files = append(files, name)
	}
	sort.Strings(files)
	return ver, files, nil
}
