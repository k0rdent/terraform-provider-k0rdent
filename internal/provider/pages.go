package provider

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// Every list the provider reads goes through here, so the questions every list
// has to answer get one answer: when is it over, what if it never is, and what
// does a failed page say.
//
// Every page is read rather than the first. A truncated list is worse than a
// slow one: a configuration that looks something up by name and does not find
// it plans against a null, and the failure surfaces somewhere else entirely.
//
// A caller supplies one closure that reads one page. It doesn't see the bound,
// the end-of-list rules or how a failure is reported, and it keeps its own
// sorting, because the order a list is shown in belongs to the data source.

// page is one page as a fetch closure read it.
type page[T any] struct {
	items []T
	// next is the cursor for token paging. Absent or "" means the list is over.
	next *string
	// total is offset paging's optional `totalCount`.
	total *int64
}

// rowsOf reads a list field that may be absent. Absent, `null` and `[]` all
// mean a page with nothing on it.
func rowsOf[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

// pager is how much to ask for and how long to keep asking.
type pager struct{ size, maxPages int }

// Every list asks for the contract's largest page. maxCatalogPages of them is
// 20,000 rows, which no catalog approaches, and stops a server that keeps
// saying there is more.
const (
	catalogPageSize = 500
	maxCatalogPages = 40
)

var catalog = pager{size: catalogPageSize, maxPages: maxCatalogPages}

// OffsetPageLimit is the `limit` every offset list sends. Only the acceptance
// suite lowers it. On the offset contract the page applied is the page
// requested, so a fake that answers honestly can't shrink a page the way it
// shrinks a cursor page, and a test with a handful of objects would otherwise
// never see a second one.
var OffsetPageLimit = catalogPageSize

// offsetPager asks for OffsetPageLimit rows a page and keeps the bound at the
// same number of rows as a full-size catalog read, so a smaller page means more
// pages rather than a shorter list.
func offsetPager() pager {
	rows := catalogPageSize * maxCatalogPages
	return pager{size: OffsetPageLimit, maxPages: (rows + OffsetPageLimit - 1) / OffsetPageLimit}
}

// listByToken reads every page of a list on cursor paging. noun names the list
// in a diagnostic, "networks" for example. On false, diags says why and
// nothing is returned: a list that's silently partial is the failure this
// exists to prevent.
func listByToken[T any](diags *diag.Diagnostics, noun string, fetch func(token *string, size *int) (page[T], error)) ([]T, bool) {
	return listByTokenAt(diags, catalog, noun, fetch)
}

// cappedPager is the catalog bound for an operation whose page is capped below
// the catalog page. It keeps the bound in rows, as offsetPager does, so the
// smaller page means more pages rather than a shorter list.
func cappedPager(size int) pager {
	rows := catalogPageSize * maxCatalogPages
	return pager{size: size, maxPages: (rows + size - 1) / size}
}

// listByTokenAt is listByToken with a page size the operation sets. An
// operation that enforces a smaller maximum answers 422 to the catalog page,
// rather than clamping it, so asking for more than it takes is a failed read.
func listByTokenAt[T any](diags *diag.Diagnostics, p pager, noun string, fetch func(token *string, size *int) (page[T], error)) ([]T, bool) {
	items, err := byToken(p, noun, fetch)
	if err != nil {
		addListError(diags, noun, err)
		return nil, false
	}
	return items, true
}

// listByOffset is listByToken for an operation that has migrated to `offset`
// and `limit`. The two styles are mutually exclusive and sending both is a 422,
// so the caller has to know which one its operation takes.
//
// key names a row, and a row whose key was already read is dropped. An offset
// is a position, not a row: a row created ahead of the reader between two
// pages pushes one already read onto the next page, and security groups sort
// newest first, so that's the common case there. Their cursor was a keyset, and
// address pools' was the last id read, so neither did this before. A list with
// one id in it twice breaks a configuration that keys a map on it.
// The other half, a row deleted ahead of the reader pulling an unread one back
// onto a page already read, can't be seen from here. An empty key is never
// dropped, so a row the caller can't name still reaches it.
func listByOffset[T any](diags *diag.Diagnostics, noun string, key func(T) string, fetch func(offset, limit int32) (page[T], error)) ([]T, bool) {
	items, err := readByOffset(noun, key, fetch)
	if err != nil {
		addListError(diags, noun, err)
		return nil, false
	}
	return items, true
}

// readByOffset is listByOffset returning the fetch's own error, for a caller
// that reports failures its own way.
func readByOffset[T any](noun string, key func(T) string, fetch func(offset, limit int32) (page[T], error)) ([]T, error) {
	items, err := byOffset(offsetPager(), noun, fetch)
	if err != nil {
		return nil, err
	}
	return firstOfEach(items, key), nil
}

// firstOfEach keeps the first row with each key, in order.
func firstOfEach[T any](items []T, key func(T) string) []T {
	seen := make(map[string]bool, len(items))
	out := items[:0:0]
	for _, item := range items {
		k := key(item)
		if k != "" && seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, item)
	}
	return out
}

// byToken follows `nextPageToken` to the end. It returns the fetch's own error
// untouched, for a caller that reports failures its own way.
func byToken[T any](p pager, noun string, fetch func(token *string, size *int) (page[T], error)) ([]T, error) {
	var (
		all   []T
		token *string
	)
	for range p.maxPages {
		size := p.size
		got, err := fetch(token, &size)
		if err != nil {
			return nil, err
		}
		all = append(all, got.items...)
		// An empty page with a token is not the end. Only the token says so.
		if got.next == nil || *got.next == "" {
			return all, nil
		}
		token = got.next
	}
	return nil, errListDidNotEnd{noun: noun, maxPages: p.maxPages}
}

// byOffset is byToken for offset paging.
func byOffset[T any](p pager, noun string, fetch func(offset, limit int32) (page[T], error)) ([]T, error) {
	var all []T
	for range p.maxPages {
		got, err := fetch(int32(len(all)), int32(p.size))
		if err != nil {
			return nil, err
		}
		all = append(all, got.items...)
		switch {
		// An empty page ends the walk whatever the total says. Trusting
		// `totalCount` alone would spin against a server whose count and rows
		// disagree.
		case len(got.items) == 0:
			return all, nil
		// The count is optional. Without it, only a short page says the list
		// is over.
		case got.total == nil && len(got.items) < p.size:
			return all, nil
		case got.total != nil && int64(len(all)) >= *got.total:
			return all, nil
		}
	}
	return nil, errListDidNotEnd{noun: noun, maxPages: p.maxPages}
}

// errListDidNotEnd is the provider's own bound, not an upstream failure.
type errListDidNotEnd struct {
	noun     string
	maxPages int
}

func (e errListDidNotEnd) Error() string {
	return fmt.Sprintf("The platform was still reporting more %s after %d pages. Reading further "+
		"could loop, so nothing is returned rather than a list that's silently partial.", e.noun, e.maxPages)
}

// unreadPage is a response that came back without a page in it. The closure
// hands over the raw response so the diagnostic can quote the platform's
// status, reason and request id.
func unreadPage(resp *http.Response, body []byte) error { return pageResponse{resp, body} }

type pageResponse struct {
	resp *http.Response
	body []byte
}

func (r pageResponse) Error() string { return responseError(r.resp, r.body).Error() }

// refusePage is a failure the caller reads its own way, typically a 404 on a
// path built from configuration where naming the missing parent is the useful
// part. The diagnostic is added as given.
func refusePage(summary, detail string) error { return pageRefusal{summary, detail} }

type pageRefusal struct{ summary, detail string }

func (r pageRefusal) Error() string { return r.detail }

// addListError turns a paging failure into the diagnostic for it.
func addListError(diags *diag.Diagnostics, noun string, err error) {
	var (
		refusal pageRefusal
		resp    pageResponse
		unended errListDidNotEnd
	)
	switch {
	case errors.As(err, &refusal):
		diags.AddError(refusal.summary, refusal.detail)
	case errors.As(err, &resp):
		addAPIError(diags, apiCall("list the "+noun), resp.resp, resp.body, nil)
	case errors.As(err, &unended):
		// Local: the provider's own bound, not an upstream failure.
		diags.AddError(fmt.Sprintf("The list of %s did not end", noun), unended.Error())
	default:
		addAPIError(diags, apiCall("list the "+noun), nil, nil, err)
	}
}
