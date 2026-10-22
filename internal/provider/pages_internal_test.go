package provider

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// A small pager so a test can fill a page and hit the bound by hand.
var testPager = pager{size: 2, maxPages: 3}

func tokenOf(s string) *string { return &s }

func TestTokenPagingReadsEveryPage(t *testing.T) {
	pages := map[string]page[string]{
		"":   {items: []string{"a", "b"}, next: tokenOf("p2")},
		"p2": {next: tokenOf("p3")}, // an empty page with a token is not the end
		"p3": {items: []string{"c"}, next: tokenOf("")},
	}
	var asked []string
	got, err := byToken(testPager, "networks", func(token *string, size *int) (page[string], error) {
		if *size != testPager.size {
			t.Errorf("asked for %d rows, want %d", *size, testPager.size)
		}
		asked = append(asked, deref(token))
		return pages[deref(token)], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v, want every page in order", got)
	}
	if !slices.Equal(asked, []string{"", "p2", "p3"}) {
		t.Errorf("asked for %v, want each token once", asked)
	}
}

func TestOffsetPagingStopsWhereTheTotalSays(t *testing.T) {
	total := int64(3)
	var offsets []int32
	got, err := byOffset(testPager, "projects", func(offset, limit int32) (page[int32], error) {
		if limit != int32(testPager.size) {
			t.Errorf("limit %d, want %d", limit, testPager.size)
		}
		offsets = append(offsets, offset)
		rows := []int32{offset, offset + 1}[:min(2, 3-offset)]
		return page[int32]{items: rows, total: &total}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !slices.Equal(offsets, []int32{0, 2}) {
		t.Errorf("got %v from offsets %v, want 3 rows from 0 and 2", got, offsets)
	}
}

// Without `totalCount` a full page says nothing, and only a short one ends the
// list. Stopping at the first full page was the bug in the project list.
func TestOffsetPagingWithoutATotalReadsUntilAShortPage(t *testing.T) {
	calls := 0
	got, err := byOffset(testPager, "projects", func(offset, _ int32) (page[int32], error) {
		calls++
		if offset >= 4 {
			return page[int32]{items: []int32{offset}}, nil
		}
		return page[int32]{items: []int32{offset, offset + 1}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || calls != 3 {
		t.Errorf("got %d rows in %d calls, want 5 in 3", len(got), calls)
	}
}

// A total the rows never reach would otherwise spin until the bound.
func TestOffsetPagingStopsOnAnEmptyPageWhateverTheTotal(t *testing.T) {
	total := int64(100)
	calls := 0
	got, err := byOffset(testPager, "projects", func(offset, _ int32) (page[int32], error) {
		calls++
		if offset > 0 {
			return page[int32]{total: &total}, nil
		}
		return page[int32]{items: []int32{0, 1}, total: &total}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || calls != 2 {
		t.Errorf("got %d rows in %d calls, want 2 in 2", len(got), calls)
	}
}

// A list that never ends returns nothing, rather than the part it read, and
// the error names the list.
func TestPagingStopsAtTheBound(t *testing.T) {
	calls := 0
	got, err := byToken(testPager, "VPC peerings", func(*string, *int) (page[string], error) {
		calls++
		return page[string]{items: []string{"x"}, next: tokenOf("same")}, nil
	})
	if got != nil || err == nil || calls != testPager.maxPages {
		t.Fatalf("got %v, %v after %d calls, want nothing and an error after %d", got, err, calls, testPager.maxPages)
	}

	_, err = byOffset(testPager, "projects", func(int32, int32) (page[int32], error) {
		return page[int32]{items: []int32{1, 2}}, nil
	})
	if err == nil {
		t.Fatal("a list of full pages with no total ended")
	}

	var diags diag.Diagnostics
	addListError(&diags, "VPC peerings", errListDidNotEnd{noun: "VPC peerings", maxPages: testPager.maxPages})
	if s := diags.Errors()[0].Summary(); s != "The list of VPC peerings did not end" {
		t.Errorf("summary %q does not name the list", s)
	}
}

// A failure on a later page drops the pages before it, and reaches the
// diagnostic the same way a failure on the first page would.
func TestPagingStopsOnAFailedPage(t *testing.T) {
	cases := map[string]struct {
		err     error
		summary string
		detail  string
	}{
		"transport": {
			err: errors.New("connection reset"), summary: "Could not list the subnets",
			detail: "The request did not complete: connection reset",
		},
		"unreadable": {
			err: unreadPage(&http.Response{StatusCode: 500, Status: "500 Internal Server Error"},
				[]byte(`{"error":{"code":500,"message":"boom","requestId":"req-9"}}`)),
			summary: "Could not list the subnets", detail: "req-9",
		},
		"caller's own": {
			err:     refusePage("No such network", `No network named "n" in project "p".`),
			summary: "No such network", detail: `No network named "n"`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := byToken(testPager, "subnets", func(token *string, _ *int) (page[string], error) {
				if token == nil {
					return page[string]{items: []string{"a"}, next: tokenOf("p2")}, nil
				}
				return page[string]{}, tc.err
			})
			if got != nil || err == nil {
				t.Fatalf("got %v, %v, want nothing and the error", got, err)
			}
			var diags diag.Diagnostics
			addListError(&diags, "subnets", err)
			e := diags.Errors()
			if len(e) != 1 || e[0].Summary() != tc.summary || !strings.Contains(e[0].Detail(), tc.detail) {
				t.Errorf("got %v, want %q containing %q", e, tc.summary, tc.detail)
			}
		})
	}
}

// A row read twice is kept once, in the place it was first read, and `offset`
// still advances by every row received. A row without a key is never dropped,
// so a caller that can't read one still reports it its own way.
func TestOffsetListingDropsARowReadTwice(t *testing.T) {
	total := int64(4)
	var offsets []int32
	var diags diag.Diagnostics
	got, ok := listByOffset(&diags, "security groups", func(s string) string { return strings.TrimPrefix(s, "?") },
		func(offset, _ int32) (page[string], error) {
			offsets = append(offsets, offset)
			if offset == 0 {
				return page[string]{items: []string{"b", "a"}, total: &total}, nil
			}
			return page[string]{items: []string{"a", "c", "?", "?"}, total: &total}, nil
		})
	if !ok {
		t.Fatal(diags)
	}
	if want := []string{"b", "a", "c", "?", "?"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !slices.Equal(offsets, []int32{0, 2}) {
		t.Errorf("offsets %v, want 0 then 2", offsets)
	}
}

// A smaller page must not lower the number of rows a list may hold. The
// acceptance suite asks for two rows a page, and the fake serves 501 address
// pools, which is far past 40 pages of two.
func TestASmallOffsetPageStillReadsALongList(t *testing.T) {
	was := OffsetPageLimit
	OffsetPageLimit = 2
	t.Cleanup(func() { OffsetPageLimit = was })
	total := int64(501)
	var diags diag.Diagnostics
	got, ok := listByOffset(&diags, "address pools", func(n int32) string { return strconv.Itoa(int(n)) },
		func(offset, limit int32) (page[int32], error) {
			var items []int32
			for n := offset; n < offset+limit && int64(n) < total; n++ {
				items = append(items, n)
			}
			return page[int32]{items: items, total: &total}, nil
		})
	if !ok {
		t.Fatal(diags)
	}
	if len(got) != int(total) {
		t.Errorf("read %d rows, want %d", len(got), total)
	}
}
