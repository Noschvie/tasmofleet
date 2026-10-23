package web

import (
	"cmp"
	"slices"
)

// Sorting of the tables beyond devices and audit. The registry of sortable
// columns lives here (not in tableDefs) so a table's columns can change
// without touching the sort rules; TestSortableColumnsExist keeps both in step.
// Every table sorts on the server by typed keys (versions as versions,
// addresses as addresses, times as times), the choice is saved per user
// (preference "sort.<table>") and the header links carry it in the URL.

// sortableCols lists the sortable column ids per table.
// The policy tables were its last entries (design v2, N3: the policy page lists cards); the
// machinery goes with the html/template tables (Z1).
var sortableCols = map[string][]string{}

// defaultSorts is the order a table has until the user picks another. An empty
// sort keeps the order the data arrives in.
var defaultSorts = map[string]tableSort{}

// multiSort lists the tables that share a page with another sortable table.
var multiSort = map[string]bool{}

// tableFollows names, for a table that only mirrors another one, the table whose saved
// layout (widths, hidden columns, order) and sort it takes. A follower has no Columns menu
// and no resize handles of its own; tables.js passes a resize of the leader on
// (data-follows). No table follows another since the firmware page lists releases as cards.
var tableFollows = map[string]string{}

// markSortable flags the registered columns of sortableCols as sortable, and those of a
// following table like the ones of the table it follows.
func markSortable(m map[string]tableDef) {
	for table, lead := range tableFollows {
		if d, ok := m[table]; ok {
			for i, cd := range d.Cols {
				d.Cols[i].Sort = slices.Contains(sortableCols[lead], cd.ID)
			}
		}
	}
	for table, ids := range sortableCols {
		d, ok := m[table]
		if !ok {
			continue
		}
		for i, cd := range d.Cols {
			if slices.Contains(ids, cd.ID) {
				d.Cols[i].Sort = true
			}
		}
	}
}

// rowKey compares two rows by one column; has says whether a row has a value
// for it. Rows without a value go last in both directions.
type rowKey[T any] func(a, b T) (c int, hasA, hasB bool)

func strKey[T any](f func(T) string, c func(a, b string) int) rowKey[T] {
	return func(a, b T) (int, bool, bool) {
		x, y := f(a), f(b)
		return c(x, y), x != "", y != ""
	}
}

func intKey[T any](f func(T) int64) rowKey[T] {
	return func(a, b T) (int, bool, bool) { return cmp.Compare(f(a), f(b)), true, true }
}

// sortBy orders rows by the column s names; an unknown or empty key keeps the
// incoming order, ties keep it too.
func sortBy[T any](rows []T, s tableSort, keys map[string]rowKey[T]) {
	key, ok := keys[s.Key]
	if !ok {
		return
	}
	slices.SortStableFunc(rows, func(a, b T) int {
		c, hasA, hasB := key(a, b)
		switch {
		case hasA != hasB:
			if hasA {
				return -1
			}
			return 1
		case !hasA:
			return 0
		case s.Dir == "desc":
			return -c
		}
		return c
	})
}
