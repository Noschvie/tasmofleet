package web

import (
	"context"
	"html"
	"html/template"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// systemActors are the fixed actor names background code and the CLI audit
// under. They are not people and get a badge of their own.
var systemActors = map[string]string{
	"system": "System", "cli": "CLI", "discovery": "Discovery", "mqtt": "MQTT", "scheduler": "Scheduler", "retention": "Retention",
}

// principalDir turns the stable subject that audit rows, jobs and backups
// store into the name a person recognises. The subject stays the key (an OIDC
// sub is a 64-character hash nobody can read); the name is the one captured
// at the last sign-in. Without a store, or for an unknown subject, the
// subject is shortened and carried in the tooltip, never printed whole.
type principalDir struct {
	st store.PrincipalStore // nil: no names, only the classification

	mu     sync.Mutex
	byKey  map[string]store.PrincipalRecord
	loaded time.Time
	seen   map[string]string // subject -> "name|kind" already written by this process
}

// principalTTL bounds how stale a name shown on a page can be; the table holds
// one row per person, so reloading it is cheap.
const principalTTL = 3 * time.Second

func newPrincipalDir(st store.PrincipalStore) *principalDir {
	return &principalDir{st: st, byKey: map[string]store.PrincipalRecord{}, seen: map[string]string{}}
}

func kindOf(method string) string {
	switch method {
	case "oidc":
		return "sso"
	case "local":
		return "local"
	}
	return "header"
}

// Record stores the name of p when it is new or has changed. It is called for
// every authenticated request; an unchanged principal costs a map lookup.
func (d *principalDir) Record(ctx context.Context, p model.Principal) {
	if d.st == nil || p.Subject == "" {
		return
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = p.Subject
	}
	if len(name) > 128 {
		name = name[:128]
	}
	sig := name + "|" + kindOf(p.Method)
	d.mu.Lock()
	same := d.seen[p.Subject] == sig
	d.mu.Unlock()
	if same {
		return
	}
	if err := d.st.UpsertPrincipal(ctx, store.PrincipalRecord{Subject: p.Subject, DisplayName: name, Kind: kindOf(p.Method)}); err != nil {
		slog.Warn("web: could not record the display name of a user", "err", err) // the page still works, with the short subject
		return
	}
	d.mu.Lock()
	d.seen[p.Subject] = sig
	d.loaded = time.Time{} // the next page shows the new name
	d.mu.Unlock()
}

func (d *principalDir) load() map[string]store.PrincipalRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != nil && time.Since(d.loaded) > principalTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if rows, err := d.st.ListPrincipals(ctx); err == nil {
			m := make(map[string]store.PrincipalRecord, len(rows))
			for _, r := range rows {
				m[r.Subject] = r
			}
			d.byKey, d.loaded = m, time.Now()
		}
	}
	return d.byKey
}

// Custom is the display name the person chose for themselves (empty: none, or the subject is unknown).
func (d *principalDir) Custom(subject string) string { return d.load()[subject].Custom }

func localName(subject string) (string, bool) { return strings.CutPrefix(subject, "local:") }

// Text is the plain-text form of a subject: the display name, "admin (local)"
// or the system actor's name.
func (d *principalDir) Text(subject string) string {
	if l, ok := systemActors[subject]; ok {
		return l
	}
	if n, ok := localName(subject); ok {
		return n + " (local)"
	}
	known := d.load()
	if r, ok := known[subject]; ok {
		if d.ambiguous(r, known) {
			return shortToken(r.DisplayName) + " \u00b7 " + subjectSuffix(subject)
		}
		return shortToken(r.DisplayName)
	}
	if longToken.FindString(subject) == subject {
		return "Unknown user" // a whole subject nobody recorded, as Who and Parts say
	}
	return shortToken(subject)
}

// shortToken shortens a long identifier to 8 characters ("user ab12cd34") and
// leaves anything readable alone.
func shortToken(s string) string {
	if longToken.MatchString(s) && len(s) >= 32 {
		return "user " + s[:8]
	}
	return s
}

// Who renders a subject for a page: the name, a badge for the kind of
// principal, the full subject in the tooltip. A string that is not a subject
// but contains one ("requested by <sub>") gets each long token replaced.
func (d *principalDir) Who(subject string) template.HTML {
	if subject == "" {
		return ""
	}
	if l, ok := systemActors[subject]; ok {
		return template.HTML(`<span class="who who-system"><span class="tag">` + html.EscapeString(l) + `</span></span>`) //nolint:gosec // G203: escaped
	}
	known := d.load()
	if n, ok := localName(subject); ok {
		// A chip like the other kinds, so the User column reads the same for every source.
		return whoSpan(n, "Local", subject)
	}
	if r, ok := known[subject]; ok {
		name := shortToken(r.DisplayName)
		if d.ambiguous(r, known) {
			name += " \u00b7 " + subjectSuffix(subject)
		}
		badge := ""
		switch r.Kind {
		case "sso":
			badge = "SSO"
		case "header":
			badge = "Proxy"
		}
		return whoSpan(name, badge, subject)
	}
	if !longToken.MatchString(subject) {
		return template.HTML(html.EscapeString(subject)) //nolint:gosec // G203: escaped
	}
	if longToken.FindString(subject) == subject {
		// A whole subject nobody recorded (a deleted account): say so instead of
		// showing a piece of the id as if it were a name. The tooltip keeps the id.
		return whoSpan("Unknown user", "", subject)
	}
	var out strings.Builder
	last := 0
	for _, loc := range longToken.FindAllStringIndex(subject, -1) {
		out.WriteString(html.EscapeString(subject[last:loc[0]]))
		tok := subject[loc[0]:loc[1]]
		if r, ok := known[tok]; ok {
			name := shortToken(r.DisplayName)
			if d.ambiguous(r, known) {
				name += " \u00b7 " + subjectSuffix(tok)
			}
			out.WriteString(string(whoSpan(name, "", tok)))
		} else {
			out.WriteString(string(whoSpan(shortToken(tok), "", tok)))
		}
		last = loc[1]
	}
	out.WriteString(html.EscapeString(subject[last:]))
	return template.HTML(out.String()) //nolint:gosec // G203: every part is escaped
}

// whoParts is a subject as a templ page draws it (the parts of Who): the name, a chip for the
// kind of principal, and the full subject for the tooltip. System actors are a chip only.
type whoParts struct {
	Name, Badge, Tip string
	System           bool
}

// Parts is Who for one subject, in parts. A string that only contains subjects (a "requested by"
// text) is not a subject: it gets its plain text.
func (d *principalDir) Parts(subject string) whoParts {
	if subject == "" {
		return whoParts{}
	}
	if l, ok := systemActors[subject]; ok {
		return whoParts{Name: l, System: true}
	}
	if n, ok := localName(subject); ok {
		return whoParts{Name: n, Badge: "Local", Tip: tip(subject)}
	}
	known := d.load()
	if r, ok := known[subject]; ok {
		name := shortToken(r.DisplayName)
		if d.ambiguous(r, known) {
			name += " \u00b7 " + subjectSuffix(subject)
		}
		badge := ""
		switch r.Kind {
		case "sso":
			badge = "SSO"
		case "header":
			badge = "Proxy"
		}
		return whoParts{Name: name, Badge: badge, Tip: tip(subject)}
	}
	if !longToken.MatchString(subject) {
		return whoParts{Name: subject}
	}
	if longToken.FindString(subject) == subject {
		return whoParts{Name: "Unknown user", Tip: tip(subject)}
	}
	return whoParts{Name: d.Text(subject), Tip: tip(subject)}
}

func whoSpan(name, badge, subject string) template.HTML {
	s := `<span class="who" title="` + html.EscapeString(tip(subject)) + `">` + html.EscapeString(name)
	if badge != "" {
		s += ` <span class="tag">` + html.EscapeString(badge) + `</span>`
	}
	return template.HTML(s + `</span>`) //nolint:gosec // G203: escaped
}

// subjectSuffix tells apart two people who share a name: the first characters
// of the subject, which the audit trail stores.
func subjectSuffix(subject string) string {
	subject = strings.TrimPrefix(subject, "local:")
	if len(subject) > 6 {
		return subject[:6]
	}
	return subject
}

// reservedName reports whether a display name could be mistaken for a system
// actor or a local account: a name a user chose at the identity provider must
// not let them pass as "System" or "admin (local)" in the audit log.
func reservedName(name string, known map[string]store.PrincipalRecord) bool {
	l := strings.ToLower(strings.TrimSpace(name))
	if strings.HasSuffix(l, "(local)") {
		return true
	}
	for key, label := range systemActors {
		if l == key || l == strings.ToLower(label) {
			return true
		}
	}
	for key, r := range known {
		if strings.HasPrefix(key, "local:") && strings.EqualFold(r.DisplayName, name) && r.Kind != "local" {
			return true
		}
	}
	return false
}

// ambiguous reports whether r's name needs the subject suffix: another subject
// has the same name (case-insensitive), or the name is reserved, or it equals
// a local account's name. Local accounts themselves never need one: they are
// shown as "name (local)".
func (d *principalDir) ambiguous(r store.PrincipalRecord, known map[string]store.PrincipalRecord) bool {
	if r.Kind == "local" {
		return false
	}
	if reservedName(r.DisplayName, known) {
		return true
	}
	for key, o := range known {
		if key == r.Subject {
			continue
		}
		if strings.EqualFold(o.DisplayName, r.DisplayName) {
			return true
		}
	}
	return false
}

// Candidates returns the stored subjects a typed name stands for: a subject
// typed whole, every principal whose display name matches case-insensitively,
// "name (local)" or "name" for a local account, a system actor by key or
// label. More than one candidate means the name is ambiguous; the audit page
// then asks which one is meant instead of merging them.
func (d *principalDir) Candidates(q string) (subjects []string, ambiguous bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, false
	}
	known := d.load()
	if _, ok := known[q]; ok {
		return []string{q}, false
	}
	if _, ok := systemActors[q]; ok || strings.HasPrefix(q, "local:") {
		return []string{q}, false
	}
	set := map[string]bool{}
	lower := strings.ToLower(q)
	for key, r := range known {
		if strings.EqualFold(r.DisplayName, q) {
			set[key] = true
		}
	}
	if local := strings.TrimSpace(strings.TrimSuffix(lower, "(local)")); local != "" && (strings.HasSuffix(lower, "(local)") || set["local:"+local] || d.hasLocal(known, local)) {
		set["local:"+local] = true
	}
	for key, l := range systemActors {
		if lower == key || lower == strings.ToLower(l) {
			set[key] = true
		}
	}
	if len(set) == 0 {
		// An unknown text still filters: a legacy actor string, or a local
		// account that never signed in since the names were recorded.
		return []string{q, "local:" + lower}, false
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, len(out) > 1
}

// SubjectsNamed returns the stored subjects whose display name contains q
// (case insensitive), sorted: the audit search box finds an SSO user's rows by
// name although the log stores the subject.
func (d *principalDir) SubjectsNamed(q string) []string {
	needle := strings.ToLower(strings.TrimSpace(q))
	if needle == "" {
		return nil
	}
	var out []string
	for key, r := range d.load() {
		if strings.Contains(strings.ToLower(r.DisplayName), needle) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (d *principalDir) hasLocal(known map[string]store.PrincipalRecord, name string) bool {
	_, ok := known["local:"+name]
	return ok
}
