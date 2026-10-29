package config

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// docRow is one row of the variable table in docs/configuration.md.
type docRow struct{ kind, def, secret string }

// readConfigDoc parses the table under "## Variables".
func readConfigDoc(t *testing.T) map[string]docRow {
	t.Helper()
	b, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(b), "\n## Variables\n")
	if !ok {
		t.Fatal(`docs/configuration.md has no "## Variables" section`)
	}
	section, _, _ := strings.Cut(after, "\n## ")
	row := regexp.MustCompile("^\\| `" + Prefix + "([A-Z0-9_]+)` +\\|")
	rows := map[string]docRow{}
	for line := range strings.SplitSeq(section, "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := strings.Split(strings.ReplaceAll(line, `\|`, "\x00"), "|")
		if len(cells) < 6 {
			t.Fatalf("%s: row has too few columns: %q", m[1], line)
		}
		cell := func(i int) string { return strings.TrimSpace(strings.ReplaceAll(cells[i], "\x00", "|")) }
		if _, dup := rows[m[1]]; dup {
			t.Errorf("%s is documented twice", m[1])
		}
		rows[m[1]] = docRow{kind: cell(2), def: strings.Trim(cell(3), "`"), secret: cell(4)}
	}
	return rows
}

// Why (#153): the reference is the one place an operator looks. A variable the
// code reads but the doc omits, a wrong kind, a wrong secret/_FILE promise or a
// stale default would send people to the wrong settings, so the doc is checked
// against the env tags and fails when it drifts.
func TestEveryVariableIsDocumented(t *testing.T) {
	rows := readConfigDoc(t)
	seen := map[string]bool{}
	rt := reflect.TypeFor[Config]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		key := f.Tag.Get("env")
		if key == "" {
			continue
		}
		seen[key] = true
		row, ok := rows[key]
		if !ok {
			t.Errorf("%s%s is not documented in docs/configuration.md", Prefix, key)
			continue
		}
		wantKind := "bootstrap"
		switch f.Tag.Get("ui") {
		case "true":
			wantKind = "legacy"
		case "lock":
			wantKind = "lock"
		}
		if row.kind != wantKind {
			t.Errorf("%s: documented as %q, the tags say %q", key, row.kind, wantKind)
		}
		if secret := f.Tag.Get("secret") == "true"; secret != strings.HasPrefix(row.secret, "yes") || secret != strings.Contains(row.secret, "_FILE") {
			t.Errorf("%s: secret column %q does not match secret:%q (secrets, and only they, accept _FILE)", key, row.secret, f.Tag.Get("secret"))
		}
		if def := f.Tag.Get("default"); def != "" && row.def != def {
			t.Errorf("%s: documented default %q, the code says %q", key, row.def, def)
		}
	}
	// Keys Load reads without a field tag.
	for key, kind := range map[string]string{"CONFIG_FILE": "bootstrap", "POLICY_RULES": "legacy"} {
		seen[key] = true
		if rows[key].kind != kind {
			t.Errorf("%s: documented as %q, want %q", key, rows[key].kind, kind)
		}
	}
	for key := range rows {
		if !seen[key] {
			t.Errorf("docs/configuration.md documents %s%s, which the code does not read", Prefix, key)
		}
	}
	for _, k := range removedKeys {
		if _, ok := rows[k]; ok {
			t.Errorf("%s is removed; it belongs in the removed table, not in the variable table", k)
		}
	}
}
