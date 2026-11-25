package web

import (
	"net/url"
	"strconv"

	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// pagerSizes are the page sizes a paged table offers; the first is the default
// of a user who has saved none.
var pagerSizes = []int{25, 50, 100}

// pageSize reads the "per" URL parameter; anything but an offered size is the
// user's saved size def (pagerSizes[0] without one).
func pageSize(q url.Values, def int) int {
	n, _ := strconv.Atoi(q.Get("per"))
	if validPageSize(n) {
		return n
	}
	return def
}

// newPager builds the model of the paging control (ui.Pager: plain links, no script) for page of
// total items. path is the page's own path, q its current query (filters); page and per are
// replaced in every link. def is the user's default size, which needs no per parameter.
func newPager(path string, q url.Values, page, size, def, total int, noun string) ui.PagerData {
	pages := max(1, (total+size-1)/size)
	page = max(1, min(page, pages))
	href := func(n, per int) string {
		v := url.Values{}
		for k, vs := range q {
			if k != "page" && k != "per" {
				v[k] = vs
			}
		}
		if per != def {
			v.Set("per", strconv.Itoa(per))
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return path
		}
		return path + "?" + v.Encode()
	}
	p := ui.PagerData{Page: page, Pages: pages, Total: total, Size: size, Noun: noun}
	if page > 1 {
		p.Prev = href(page-1, size)
	}
	if page < pages {
		p.Next = href(page+1, size)
	}
	for _, s := range pagerSizes {
		// a size restarts at page 1; the size you are on stays where you are (it is marked as the current page)
		n := 1
		if s == size {
			n = page
		}
		p.Sizes = append(p.Sizes, ui.PagerSize{N: s, Href: href(n, s), Current: s == size})
	}
	return p
}
