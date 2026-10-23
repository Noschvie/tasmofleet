package web

import (
	"net/url"
	"testing"
)

// Why: the pager is the same at every width (Previous, Next, the position, the sizes), so every
// link has to keep the filters of the page, and the sizes restart at page 1.
func TestPagerLinksKeepTheFilters(t *testing.T) {
	q := url.Values{"actor": {"adam"}, "page": {"6"}}
	p := newPager("/tf/audit", q, 6, 25, 25, 500, "events")
	if p.Pages != 20 || p.Prev != "/tf/audit?actor=adam&page=5" || p.Next != "/tf/audit?actor=adam&page=7" {
		t.Fatalf("links: %+v", p)
	}
	if p.Sizes[1].Href != "/tf/audit?actor=adam&per=50" || !p.Sizes[0].Current {
		t.Fatalf("sizes: %+v", p.Sizes)
	}
}

func TestPagerEdges(t *testing.T) {
	p := newPager("/audit", nil, 99, 25, 25, 0, "events")
	if p.Page != 1 || p.Pages != 1 || p.Prev != "" || p.Next != "" {
		t.Fatalf("empty: %+v", p)
	}
	if pageSize(url.Values{"per": {"7"}}, 25) != 25 || pageSize(url.Values{"per": {"100"}}, 25) != 100 || pageSize(nil, 50) != 50 {
		t.Fatal("page size must be one of the offered sizes")
	}
}
