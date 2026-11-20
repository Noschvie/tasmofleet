package web

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Why: a column that is declared sortable but has no comparator (or the other
// way round) would show a sort arrow that does nothing. The registry of sortable
// columns must name real columns, and every sortable column of a table must be
// in it, so a table that changes its columns cannot silently lose or fake sorting.
func TestSortableColumnsExist(t *testing.T) {
	for table, ids := range sortableCols {
		def, ok := tableDefs[table]
		if !ok {
			t.Errorf("sortableCols: unknown table %q", table)
			continue
		}
		for _, id := range ids {
			if cd, ok := def.col(id); !ok || !cd.Sort {
				t.Errorf("%s.%s is listed as sortable but is not a sortable column", table, id)
			}
		}
		if d := defaultSorts[table]; d.Key != "" {
			if cd, ok := def.col(d.Key); !ok || !cd.Sort {
				t.Errorf("%s: default sort %q is not a sortable column", table, d.Key)
			}
		}
	}
	for table, def := range tableDefs {
		if table == "devices" || table == "audit" {
			continue // sorted by their own handlers since v0.6.0
		}
		for _, cd := range def.Cols {
			lead := table
			if f := tableFollows[table]; f != "" {
				lead = f // a follower sorts like its leader
			}
			if cd.Sort && !slices.Contains(sortableCols[lead], cd.ID) {
				t.Errorf("%s.%s is sortable but not in sortableCols", table, cd.ID)
			}
		}
	}
	// The policy tables, the last ones registered here, became cards with design v2 (N3); the
	// tests below register two tables of their own (sortFixture).
}

// sortFixture registers two sortable tables that share one page ("t_rules", "t_versions"):
// since design v2 no html/template table of the app sorts on the server any more (N3 turned
// the policy tables into cards), and the generic machinery is tested on these until it goes
// with the html/template tables (Z1).
func sortFixture(t *testing.T) {
	t.Helper()
	tableDefs["t_versions"] = tableDef{Key: "t_versions", Cols: []colDef{
		srt(lk("version", "Version", 12)), srt(c("when", "When", 24)), srt(c("who", "Who", 22)), srt(c("note", "Note", 28)), act(lk("actions", "Actions", 14)),
	}}
	tableDefs["t_rules"] = tableDef{Key: "t_rules", Cols: []colDef{srt(lk("setting", "Setting", 40)), srt(c("severity", "Severity", 30)), c("notes", "Notes", 30)}}
	sortableCols["t_versions"] = []string{"version", "when", "who", "note"}
	sortableCols["t_rules"] = []string{"setting", "severity"}
	defaultSorts["t_versions"] = tableSort{Key: "version", Dir: "desc"}
	multiSort["t_versions"], multiSort["t_rules"] = true, true
	t.Cleanup(func() {
		for _, k := range []string{"t_versions", "t_rules"} {
			delete(tableDefs, k)
			delete(sortableCols, k)
			delete(defaultSorts, k)
			delete(multiSort, k)
		}
	})
}

// Why: pages with several sortable tables keep one order per table in the URL;
// a page with one table keeps the short sort/dir the older tables use.
func TestSortParamsPerTable(t *testing.T) {
	sortFixture(t)
	if k, d := sortParams("users"); k != "sort" || d != "dir" {
		t.Errorf("single table: %s %s", k, d)
	}
	if k, d := sortParams("t_rules"); k != "sort_t_rules" || d != "dir_t_rules" {
		t.Errorf("shared page: %s %s", k, d)
	}
	r := httptest.NewRequest("GET", "/x?sort_t_rules=severity&dir_t_rules=desc&sort=name", nil)
	ts, explicit := resolveSort(r.URL.Query(), nil, "t_rules", defaultSorts["t_rules"])
	if ts != (tableSort{"severity", "desc"}) || !explicit {
		t.Errorf("own parameters: %+v", ts)
	}
	ts, _ = resolveSort(r.URL.Query(), nil, "t_versions", defaultSorts["t_versions"])
	if ts != defaultSorts["t_versions"] {
		t.Errorf("another table's parameters must not leak: %+v", ts)
	}
}

// Why: the templates of the tables set no sort attributes; the page supplies
// them, so every table with sortable columns gets header links without its
// template being touched, and the links drop the paging and keep the filters.
func TestPageSortsGiveHeaderLinks(t *testing.T) {
	sortFixture(t)
	r := httptest.NewRequest("GET", "/tf/x?q=x&page=3&sort_t_versions=when&dir_t_versions=desc", nil)
	sorts := pageSorts(r, nil, "/tf/x")
	table := `<table data-table="t_versions"><thead><tr><th>Version</th><th>When</th><th>Who</th><th>Note</th><th>Actions</th></tr></thead><tbody><tr><td>a</td><td>1</td><td>s</td><td>c</td><td></td></tr></tbody></table>`
	out := string(applyTablePrefsSorted([]byte(table), nil, sorts))
	for _, want := range []string{
		`aria-sort="descending"`,
		`href="/tf/x?q=x&amp;sort_t_versions=version&amp;dir_t_versions=asc"`,
		`href="/tf/x?q=x&amp;sort_t_versions=when&amp;dir_t_versions=asc"`, // the current column flips
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "page=3") {
		t.Error("a sort link must start at the first page")
	}
	if strings.Contains(string(applyTablePrefsSorted([]byte(table), nil, nil)), "sortlink") {
		t.Error("without a page URL there are no sort links")
	}
}
