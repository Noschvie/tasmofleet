package web

import (
	"net/url"
	"strconv"
)

// pagerSizes are the page sizes a paged table offers; the first is the default
// of a user who has saved none.
var pagerSizes = []int{25, 50, 100}

type pagerSize struct {
	N       int
	Href    string
	Current bool
}

// pager is the one paging control of every paged table. It is made of plain
// links (no script) and keeps the filters of the page in every URL.
type pager struct {
	Page, Pages, Total, Size int
	Noun                     string
	Prev, Next               string // empty: nothing there to go to
	Sizes                    []pagerSize
}

// pageSize reads the "per" URL parameter; anything but an offered size is the
// user's saved size def (pagerSizes[0] without one).
func pageSize(q url.Values, def int) int {
	n, _ := strconv.Atoi(q.Get("per"))
	if validPageSize(n) {
		return n
	}
	return def
}

// newPager builds the control for page of total items. path is the page's own
// path, q its current query (filters); page and per are replaced in every link.
// def is the user's default size, which needs no per parameter.
func newPager(path string, q url.Values, page, size, def, total int, noun string) pager {
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
	p := pager{Page: page, Pages: pages, Total: total, Size: size, Noun: noun}
	if page > 1 {
		p.Prev = href(page-1, size)
	}
	if page < pages {
		p.Next = href(page+1, size)
	}
	for _, s := range pagerSizes {
		p.Sizes = append(p.Sizes, pagerSize{N: s, Href: href(1, s), Current: s == size})
	}
	return p
}
