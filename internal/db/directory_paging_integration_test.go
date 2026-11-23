//go:build integration

// The subscriber directory has to be walkable, not just searchable.
//
// It capped at 500 rows and told the operator to narrow the search. That is
// fine while they arrive knowing a name, a phone number or a CAF number, and
// useless when the row they want is one they cannot name — the newest signup,
// or the 900th account in a status they are working through. At the 20,000
// subscribers NFR-BIZ-001 sizes for, the cap put most of the base out of
// reach of the screen entirely.
package db_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

func TestDirectoryPaging_WalksTheWholeBaseWithoutRepeatingOrSkipping(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")

	const total = 125
	for i := 1; i <= total; i++ {
		seedSubscriber(ctx, t, pool, i, seedOpts{Username: fmt.Sprintf("sub%03d@isp", i), PlanID: 1})
	}

	console := database.Console()
	const pageSize = 20

	seen := map[int]int{}
	offset, pages := 0, 0
	for {
		page, err := console.ListDirectory(ctx, staffui.DirectoryFilter{Limit: pageSize, Offset: offset})
		if err != nil {
			t.Fatalf("ListDirectory(offset=%d): %v", offset, err)
		}
		if page.Matched != total {
			t.Errorf("page at offset %d reports %d matching, want %d", offset, page.Matched, total)
		}
		if page.Offset != offset || page.Limit != pageSize {
			t.Errorf("page should carry its own offset/limit, got %d/%d want %d/%d",
				page.Offset, page.Limit, offset, pageSize)
		}
		for _, row := range page.Rows {
			seen[row.ID]++
		}
		pages++
		if pages > 20 {
			t.Fatal("the pager never reported a last page; it would loop forever in the console too")
		}
		if !page.HasNext() {
			break
		}
		offset = page.NextOffset()
	}

	if pages != 7 { // 125 rows at 20 a page: six full, one of five.
		t.Errorf("walked %d pages, want 7", pages)
	}
	if len(seen) != total {
		t.Errorf("walking every page saw %d distinct subscribers, want %d — the pager skipped some", len(seen), total)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("subscriber %d appeared on %d pages; an unstable sort would do this", id, times)
		}
	}
}

func TestDirectoryPaging_PagingKeepsTheFilterApplied(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")

	// 30 active, 30 hard-suspended, interleaved so a page boundary falls
	// inside the group rather than tidily between the two.
	for i := 1; i <= 60; i++ {
		status := "active"
		if i%2 == 0 {
			status = "hard_suspended"
		}
		seedSubscriber(ctx, t, pool, i, seedOpts{
			Username: fmt.Sprintf("sub%03d@isp", i), PlanID: 1, Status: status,
		})
	}

	console := database.Console()
	f := staffui.DirectoryFilter{Status: "hard_suspended", Limit: 10}

	first, err := console.ListDirectory(ctx, f)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Matched != 30 {
		t.Fatalf("filter should match 30 suspended subscribers, got %d", first.Matched)
	}

	f.Offset = first.NextOffset()
	second, err := console.ListDirectory(ctx, f)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.Matched != 30 {
		t.Errorf("the second page lost the filter: %d matching, want 30", second.Matched)
	}
	for _, row := range second.Rows {
		if row.Status != "hard_suspended" {
			t.Errorf("page 2 carries %s subscriber %d; paging dropped the status filter",
				row.Status, row.ID)
		}
	}
	// And the pages are disjoint.
	onFirst := map[int]bool{}
	for _, r := range first.Rows {
		onFirst[r.ID] = true
	}
	for _, r := range second.Rows {
		if onFirst[r.ID] {
			t.Errorf("subscriber %d is on both pages", r.ID)
		}
	}
}

// A page past the end is reachable from a stale link or a hand-edited URL and
// must come back empty rather than error, still reporting how many matched so
// the screen can offer a way back.
func TestDirectoryPaging_OffsetBeyondTheEndIsEmptyNotAnError(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	for i := 1; i <= 5; i++ {
		seedSubscriber(ctx, t, pool, i, seedOpts{Username: fmt.Sprintf("sub%d@isp", i), PlanID: 1})
	}

	console := database.Console()
	page, err := console.ListDirectory(ctx, staffui.DirectoryFilter{Limit: 10, Offset: 500})
	if err != nil {
		t.Fatalf("an offset past the end must not error: %v", err)
	}
	if len(page.Rows) != 0 {
		t.Errorf("want no rows past the end, got %d", len(page.Rows))
	}
	if page.Total != 5 {
		t.Errorf("the unfiltered total should still be reported, got %d want 5", page.Total)
	}
	if page.HasNext() {
		t.Error("a page past the end must not offer a next page")
	}
	if !page.HasPrev() {
		t.Error("a page past the end must offer a way back")
	}

	// A negative offset arrives the same way and must be floored, not
	// passed to PostgreSQL, which rejects it outright.
	page, err = console.ListDirectory(ctx, staffui.DirectoryFilter{Limit: 10, Offset: -40})
	if err != nil {
		t.Fatalf("a negative offset must be floored, not rejected: %v", err)
	}
	if len(page.Rows) != 5 {
		t.Errorf("a negative offset should show the first page, got %d rows", len(page.Rows))
	}
}
