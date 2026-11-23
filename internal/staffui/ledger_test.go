package staffui

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The console's ledger window has to honour ?from= and ?to=.
//
// It took the request and ignored it: the income statement was pinned to the
// current calendar month however the URL was written, while the same window
// on the JSON API honoured both parameters. The doc comment claimed
// "unless overridden", which was the part that made it a defect rather than a
// decision. unparam found it by noticing the parameter was never read, which
// no test could have done — there was no test, and a test written against the
// old behaviour would have asserted the bug.
func TestMonthWindow(t *testing.T) {
	now := time.Now()
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	t.Run("no parameters is this calendar month", func(t *testing.T) {
		from, to, ok := monthWindow(httptest.NewRequest("GET", "/staff/ledger?tab=income-statement", nil))
		if !ok {
			t.Error("an absent date is not a bad date")
		}
		if !from.Equal(firstOfThisMonth) {
			t.Errorf("from = %s, want the first of this month %s", from, firstOfThisMonth)
		}
		if want := firstOfThisMonth.AddDate(0, 1, 0); !to.Equal(want) {
			t.Errorf("to = %s, want %s", to, want)
		}
	})

	t.Run("from and to are honoured", func(t *testing.T) {
		from, to, ok := monthWindow(httptest.NewRequest("GET",
			"/staff/ledger?tab=income-statement&from=2026-04-01&to=2026-06-30", nil))
		if !ok {
			t.Fatal("those are dates")
		}
		if from.Format("2006-01-02") != "2026-04-01" {
			t.Errorf("from = %s, want 2026-04-01", from.Format("2006-01-02"))
		}
		// Inclusive of the named day, as the API does it: a quarter ending
		// 30 June must include 30 June, so the exclusive bound is 1 July.
		if to.Format("2006-01-02") != "2026-07-01" {
			t.Errorf("to = %s, want 2026-07-01 so that 30 June is included", to.Format("2006-01-02"))
		}
	})

	t.Run("only from, only to", func(t *testing.T) {
		from, to, ok := monthWindow(httptest.NewRequest("GET", "/staff/ledger?from=2026-01-15", nil))
		if !ok || from.Format("2006-01-02") != "2026-01-15" {
			t.Errorf("from alone should apply, got %s (ok=%v)", from.Format("2006-01-02"), ok)
		}
		if want := firstOfThisMonth.AddDate(0, 1, 0); !to.Equal(want) {
			t.Errorf("to should keep its default, got %s want %s", to, want)
		}

		from, to, ok = monthWindow(httptest.NewRequest("GET", "/staff/ledger?to=2026-01-31", nil))
		if !ok || to.Format("2006-01-02") != "2026-02-01" {
			t.Errorf("to alone should apply inclusively, got %s (ok=%v)", to.Format("2006-01-02"), ok)
		}
		if !from.Equal(firstOfThisMonth) {
			t.Errorf("from should keep its default, got %s", from)
		}
	})

	t.Run("a bad date keeps the month and says so", func(t *testing.T) {
		for _, bad := range []string{"not-a-date", "2026-13-01", "01-04-2026", "2026/04/01"} {
			from, to, ok := monthWindow(httptest.NewRequest("GET", "/staff/ledger?from="+bad, nil))
			if ok {
				t.Errorf("from=%q should report a bad date", bad)
			}
			// And it must still return a usable window rather than a zero
			// one, or the screen would query from year zero.
			if !from.Equal(firstOfThisMonth) || to.Before(from) {
				t.Errorf("from=%q left an unusable window %s..%s", bad, from, to)
			}
		}
	})
}
