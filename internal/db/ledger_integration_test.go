//go:build integration

// A ledger posting driven by an event happens once, however many times the
// event is delivered.
//
// gl_journal_entries has carried source_type and source_id since migration
// 043, and until 075 nothing stopped the same source being posted twice. That
// matters because the automated postings — a wallet recharge, a received
// purchase order, a franchise commission — all arrive through a retrying job
// queue or a retried request, and a doubled journal entry is the kind of
// wrong nobody notices until a trial balance is held against a bank
// statement months later.
//
// Manual entries are deliberately exempt: an operator posting the same
// correction twice is their business, and they carry no source_id to be
// unique on.
package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/ledger"
)

// accountID resolves a chart-of-accounts code to its id. The codes come from
// migration 043's seed, which restoreSeededReferenceData leaves alone.
func accountID(ctx context.Context, t *testing.T, store interface {
	ListAccounts(context.Context) ([]ledger.Account, error)
}, code string) int {
	t.Helper()
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, a := range accounts {
		if a.Code == code {
			return a.ID
		}
	}
	t.Fatalf("no account with code %s; migration 043 seeds 1000/4000/etc", code)
	return 0
}

// balanced is a two-line entry that balances, which PostJournalEntry requires.
func balanced(cash, revenue int, amount, description string) ledger.NewJournalEntry {
	return ledger.NewJournalEntry{
		Description: description,
		CreatedBy:   "billing1",
		Lines: []ledger.NewJournalLine{
			{AccountID: cash, Debit: amount, Credit: "0"},
			{AccountID: revenue, Debit: "0", Credit: amount},
		},
	}
}

func TestLedgerIdempotency_ASourceEventPostsOnce(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Ledger()
	cash := accountID(ctx, t, store, "1000")
	revenue := accountID(ctx, t, store, "4000")

	sourceID := 4242
	first := balanced(cash, revenue, "590.00", "Recharge — subscriber 1")
	first.SourceType = "wallet_ledger"
	first.SourceID = &sourceID

	posted, err := store.PostJournalEntry(ctx, first)
	if err != nil {
		t.Fatalf("first posting: %v", err)
	}
	if posted.SourceType != "wallet_ledger" {
		t.Errorf("source_type read back as %q, want wallet_ledger", posted.SourceType)
	}
	if posted.SourceID == nil || *posted.SourceID != sourceID {
		t.Errorf("source_id read back as %v, want %d", posted.SourceID, sourceID)
	}

	// The same event delivered again — a retried task, a resent webhook.
	again, err := store.PostJournalEntry(ctx, first)
	if err == nil {
		t.Fatal("the same source event posted twice; the books are now doubled")
	}
	if !errors.Is(err, ledger.ErrAlreadyPosted) {
		t.Errorf("a retry must be distinguishable from a fault so a handler can treat it as done, got %v", err)
	}
	if again != nil {
		t.Errorf("a refused posting must not return an entry, got %+v", again)
	}

	// And exactly one entry exists for it.
	entries, err := store.ListJournalEntries(ctx, 100)
	if err != nil {
		t.Fatalf("ListJournalEntries: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.SourceType == "wallet_ledger" && e.SourceID != nil && *e.SourceID == sourceID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly 1 entry for the event, found %d", n)
	}
}

func TestLedgerIdempotency_DifferentSourcesAndManualEntriesAreUnaffected(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Ledger()
	cash := accountID(ctx, t, store, "1000")
	revenue := accountID(ctx, t, store, "4000")

	// A different id under the same kind, and the same id under a different
	// kind, are both separate events.
	for _, tc := range []struct {
		kind string
		id   int
	}{
		{"wallet_ledger", 1},
		{"wallet_ledger", 2},
		{"purchase_order", 1},
		{"lco_commission", 1},
	} {
		e := balanced(cash, revenue, "100.00", tc.kind)
		id := tc.id
		e.SourceType, e.SourceID = tc.kind, &id
		if _, err := store.PostJournalEntry(ctx, e); err != nil {
			t.Errorf("%s/%d should be its own event: %v", tc.kind, tc.id, err)
		}
	}

	// Manual entries carry no source and are not held to the rule — an
	// operator may post the same correction twice, and the index is partial
	// so that nothing stops them.
	for i := 0; i < 3; i++ {
		m := balanced(cash, revenue, "50.00", "Manual correction")
		if _, err := store.PostJournalEntry(ctx, m); err != nil {
			t.Fatalf("manual entry %d was refused: %v", i+1, err)
		}
	}

	entries, err := store.ListJournalEntries(ctx, 100)
	if err != nil {
		t.Fatalf("ListJournalEntries: %v", err)
	}
	if len(entries) != 7 {
		t.Errorf("want 4 sourced entries plus 3 manual = 7, got %d", len(entries))
	}
	manual := 0
	for _, e := range entries {
		if e.SourceType == ledger.SourceManual {
			if e.SourceID != nil {
				t.Errorf("a manual entry should carry no source id, got %d", *e.SourceID)
			}
			manual++
		}
	}
	if manual != 3 {
		t.Errorf("want 3 manual entries, got %d", manual)
	}
}

// The refusal has to name the event, or an operator reading the log cannot
// tell which posting was skipped.
func TestLedgerIdempotency_TheRefusalNamesTheEvent(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Ledger()
	cash := accountID(ctx, t, store, "1000")
	revenue := accountID(ctx, t, store, "4000")

	id := 77
	e := balanced(cash, revenue, "10.00", "Commission")
	e.SourceType, e.SourceID = "lco_commission", &id
	if _, err := store.PostJournalEntry(ctx, e); err != nil {
		t.Fatalf("first posting: %v", err)
	}
	_, err := store.PostJournalEntry(ctx, e)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "lco_commission") || !strings.Contains(err.Error(), "77") {
		t.Errorf("the refusal should say which event it was, got %q", err)
	}
}
