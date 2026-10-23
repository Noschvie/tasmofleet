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

// Why: the keys are typed. Versions compare as versions (16.10 after 16.9),
// addresses as addresses, times as times; text sorted as text would put them in
// the wrong order and the user would not trust the arrow.
func TestTypedSortKeys(t *testing.T) {
	type addrRow struct{ name, addr string }
	rows := []addrRow{{"b", "10.0.0.10"}, {"a", "10.0.0.9"}, {"c", ""}}
	byAddr := map[string]rowKey[addrRow]{"address": strKey(func(r addrRow) string { return r.addr }, compareAddresses)}
	sortBy(rows, tableSort{"address", "asc"}, byAddr)
	if rows[0].name != "a" || rows[1].name != "b" {
		t.Errorf("addresses numerically: %v", rows)
	}
	sortBy(rows, tableSort{"address", "desc"}, byAddr)
	if rows[2].name != "c" {
		t.Errorf("a row without a value goes last in both directions: %v", rows)
	}

	type verRow struct{ id int64 }
	byVer := map[string]rowKey[verRow]{"version": intKey(func(v verRow) int64 { return v.id })}
	vers := []verRow{{2}, {10}, {9}}
	sortBy(vers, tableSort{"version", "desc"}, byVer)
	if vers[0].id != 10 || vers[2].id != 2 {
		t.Errorf("versions numerically: %+v", vers)
	}

	// An unknown key keeps the incoming order: nothing is reordered by mistake.
	before := slices.Clone(vers)
	sortBy(vers, tableSort{"nope", "asc"}, byVer)
	if !slices.Equal(before, vers) {
		t.Error("an unknown sort key must not reorder")
	}
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
