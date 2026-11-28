package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/ledger"
)

// The General Ledger screen.
//
// No FR id: the SRS has no requirement covering the general ledger, which is
// recorded in ledger_test.go on the API side and in the findings from the
// traceability check. Named descriptively rather than given a borrowed id.
//
// ledger.go was at 11.8% — 105 statements on the console screen an
// accountant works from. Two defects were found writing this:
//
//   - monthWindow, given a valid ?from and an invalid ?to, returned the
//     parsed from with this month's end, while the page announced "showing
//     this month instead". The statement on screen covered a period nobody
//     asked for, labelled as a different one.
//   - A journal line with an amount and no account was dropped silently.
//     Two such lines that cancelled each other left the rest balanced, so the
//     entry posted with them missing and the operator believed four lines had
//     gone in.

var errLedgerStore = errors.New("store: gl tables unreachable")

type fakeGL struct {
	accounts []ledger.Account
	entries  []ledger.JournalEntry
	trial    []ledger.TrialBalanceRow
	rows     []ledger.StatementRow

	gotAccount ledger.Account
	gotEntry   *ledger.NewJournalEntry
	gotFrom    time.Time
	gotTo      time.Time
	gotAsOf    time.Time
	gotLimit   int

	listErr, trialErr, stmtErr, journalErr, createErr, postErr error
}

func (f *fakeGL) CreateAccount(_ context.Context, a ledger.Account) (*ledger.Account, error) {
	f.gotAccount = a
	if f.createErr != nil {
		return nil, f.createErr
	}
	a.ID = 42
	return &a, nil
}

func (f *fakeGL) ListAccounts(context.Context) ([]ledger.Account, error) {
	return f.accounts, f.listErr
}

func (f *fakeGL) PostJournalEntry(_ context.Context, e ledger.NewJournalEntry) (*ledger.JournalEntry, error) {
	f.gotEntry = &e
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &ledger.JournalEntry{ID: 9, Description: e.Description}, nil
}

func (f *fakeGL) GetJournalEntry(context.Context, int) (*ledger.JournalEntry, error) {
	return nil, nil
}

func (f *fakeGL) ListJournalEntries(_ context.Context, limit int) ([]ledger.JournalEntry, error) {
	f.gotLimit = limit
	return f.entries, f.journalErr
}

func (f *fakeGL) TrialBalance(context.Context) ([]ledger.TrialBalanceRow, error) {
	return f.trial, f.trialErr
}

func (f *fakeGL) IncomeStatement(_ context.Context, from, to time.Time) ([]ledger.StatementRow, error) {
	f.gotFrom, f.gotTo = from, to
	return f.rows, f.stmtErr
}

func (f *fakeGL) BalanceSheet(_ context.Context, asOf time.Time) ([]ledger.StatementRow, error) {
	f.gotAsOf = asOf
	return f.rows, f.stmtErr
}

func serveGL(h *Handler, s Session, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(r.URL.Path, "/accounts"):
		h.CreateLedgerAccountForm(w, r)
	case strings.HasSuffix(r.URL.Path, "/entries"):
		h.PostJournalEntryForm(w, r)
	default:
		h.Ledger(w, r)
	}
	return w
}

func glHandler(f *fakeGL) *Handler {
	h := &Handler{}
	if f != nil {
		h.generalLedger = f
	}
	return h
}

func twoLines() url.Values {
	return url.Values{
		"description":  {"opening balance"},
		"account_id_1": {"1"}, "debit_1": {"100.00"},
		"account_id_2": {"2"}, "credit_2": {"100.00"},
	}
}

// A valid ?from with an invalid ?to must fall back to this month entirely,
// because the page says it is showing this month. Before the fix it returned
// the parsed from with this month's end: a statement from January to the end
// of November, labelled "this month".
func TestAnInvalidStatementDateFallsBackToThisMonthEntirely(t *testing.T) {
	now := time.Now()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	for name, q := range map[string]string{
		"bad from, no to":   "from=last-tuesday",
		"bad to, no from":   "to=2026-13-45",
		"good from, bad to": "from=2026-01-01&to=not-a-date",
		"bad from, good to": "from=not-a-date&to=2026-01-31",
		"both bad":          "from=x&to=y",
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/staff/ledger?tab=income-statement&"+q, nil)
			from, to, ok := monthWindow(r)
			if ok {
				t.Fatalf("%s was accepted as a valid window", q)
			}
			if !from.Equal(thisMonth) || !to.Equal(thisMonth.AddDate(0, 1, 0)) {
				t.Errorf("%s fell back to %s..%s; the page says \"showing this month\", so "+
					"it has to be %s..%s",
					q, from.Format("2006-01-02"), to.Format("2006-01-02"),
					thisMonth.Format("2006-01-02"), thisMonth.AddDate(0, 1, 0).Format("2006-01-02"))
			}
		})
	}
}

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
		// A calendar date typed by an operator is a date in their zone.
		if from.Location() != time.Local {
			t.Errorf("from is in %v, not the operator's zone", from.Location())
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

// And the screen reports the fallback rather than silently showing a period
// nobody asked for.
func TestTheScreenSaysWhenItFellBack(t *testing.T) {
	f := &fakeGL{}
	body := pageText(serveGL(glHandler(f), owner(), http.MethodGet,
		"/staff/ledger?tab=income-statement&from=2026-01-01&to=garbage", nil).Body.String())
	if !strings.Contains(body, "showing this month instead") {
		t.Errorf("no fallback message: %s", firstLines(body))
	}
	now := time.Now()
	if f.gotFrom.Month() != now.Month() || f.gotFrom.Year() != now.Year() {
		t.Errorf("the store was asked for a statement from %s while the page says this month",
			f.gotFrom.Format("2006-01-02"))
	}
}

// A line with an amount and no account is an error naming the line, not a
// line silently dropped. Two such lines that cancel each other would leave
// the rest balanced, and the entry would post with them missing.
func TestAJournalLineWithAnAmountAndNoAccountIsRefused(t *testing.T) {
	form := twoLines()
	// Two incomplete lines that cancel: dropped together, the entry still
	// balances, which is exactly how they used to vanish.
	form.Set("debit_3", "50.00")
	form.Set("credit_4", "50.00")

	f := &fakeGL{}
	rec := serveGL(glHandler(f), owner(), http.MethodPost, "/staff/ledger/entries", form)

	if f.gotEntry != nil {
		t.Fatalf("the entry posted with %d line(s); lines 3 and 4 had amounts and no account "+
			"and were dropped without a word", len(f.gotEntry.Lines))
	}
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "Line 3") {
		t.Errorf("the refusal should name the line, got: %s", firstLines(body))
	}
}

// A line left entirely blank is still dropped — that is what the fixed
// four-line grid is for.
func TestAnEntirelyBlankJournalLineIsDropped(t *testing.T) {
	f := &fakeGL{}
	rec := serveGL(glHandler(f), owner(), http.MethodPost, "/staff/ledger/entries", twoLines())
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if f.gotEntry == nil || len(f.gotEntry.Lines) != 2 {
		t.Fatalf("want two lines posted, got %+v", f.gotEntry)
	}
	// And a line with an account but no amount is blank too: an operator
	// tabbing past a pre-filled account dropdown has not written a line.
	form := twoLines()
	form.Set("account_id_3", "5")
	f2 := &fakeGL{}
	serveGL(glHandler(f2), owner(), http.MethodPost, "/staff/ledger/entries", form)
	if f2.gotEntry == nil || len(f2.gotEntry.Lines) != 2 {
		t.Errorf("an account with no amount was posted as a line: %+v", f2.gotEntry)
	}
}

// The entry records who posted it, from the session, and the unused side of
// each line reaches the store as "0".
func TestAJournalEntryRecordsThePosterAndZeroFillsTheUnusedSide(t *testing.T) {
	f := &fakeGL{}
	form := twoLines()
	form.Set("created_by", "somebody_else")
	serveGL(glHandler(f), sess("billing_admin"), http.MethodPost, "/staff/ledger/entries", form)

	if f.gotEntry == nil {
		t.Fatal("nothing posted")
	}
	if f.gotEntry.CreatedBy != "billing_admin_user" {
		t.Errorf("created_by = %q; it must come from the session", f.gotEntry.CreatedBy)
	}
	if f.gotEntry.Lines[0].Credit != "0" || f.gotEntry.Lines[1].Debit != "0" {
		t.Errorf("unused sides = %+v", f.gotEntry.Lines)
	}
}

func TestJournalEntryValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no description": {url.Values{
			"account_id_1": {"1"}, "debit_1": {"1"}, "account_id_2": {"2"}, "credit_2": {"1"},
		}, "Description is required"},
		"one line": {url.Values{
			"description": {"x"}, "account_id_1": {"1"}, "debit_1": {"1"},
		}, "at least two lines"},
		"bad account": {url.Values{
			"description": {"x"}, "account_id_1": {"cash"}, "debit_1": {"1"},
			"account_id_2": {"2"}, "credit_2": {"1"},
		}, "Line 1 has an invalid account"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGL{}
			rec := serveGL(glHandler(f), owner(), http.MethodPost, "/staff/ledger/entries", tc.form)
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if f.gotEntry != nil {
				t.Error("an invalid entry reached the store")
			}
		})
	}
}

// The store's balance refusal reaches the operator, since it says what to
// fix.
func TestAnUnbalancedEntryShowsTheStoresReason(t *testing.T) {
	f := &fakeGL{postErr: errors.New("ledger: debits 100.00 do not equal credits 90.00")}
	body := pageText(serveGL(glHandler(f), owner(), http.MethodPost,
		"/staff/ledger/entries", twoLines()).Body.String())
	if !strings.Contains(body, "do not equal") {
		t.Errorf("the reason is missing: %s", firstLines(body))
	}
}

func TestLedgerAccountCreation(t *testing.T) {
	f := &fakeGL{}
	rec := serveGL(glHandler(f), owner(), http.MethodPost, "/staff/ledger/accounts", url.Values{
		"code": {"  4000 "}, "name": {" Service Revenue "}, "account_type": {"income"},
	})
	if !strings.Contains(pageText(rec.Body.String()), "Account 4000 (Service Revenue) added") {
		t.Errorf("no confirmation: %s", firstLines(rec.Body.String()))
	}
	if f.gotAccount.Code != "4000" || f.gotAccount.Name != "Service Revenue" {
		t.Errorf("stored as %+v; code and name should be trimmed", f.gotAccount)
	}
	if f.gotAccount.NormalBalance != ledger.BalanceCredit {
		t.Errorf("an income account was given normal balance %q", f.gotAccount.NormalBalance)
	}

	for name, form := range map[string]url.Values{
		"no code":  {"name": {"x"}, "account_type": {"income"}},
		"no name":  {"code": {"1"}, "account_type": {"income"}},
		"bad type": {"code": {"1"}, "name": {"x"}, "account_type": {"profit"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGL{}
			serveGL(glHandler(f), owner(), http.MethodPost, "/staff/ledger/accounts", form)
			if f.gotAccount.Code != "" || f.gotAccount.Name != "" {
				t.Error("an invalid account reached the store")
			}
		})
	}

	dup := &fakeGL{createErr: errLedgerStore}
	body := pageText(serveGL(glHandler(dup), owner(), http.MethodPost, "/staff/ledger/accounts",
		url.Values{"code": {"4000"}, "name": {"x"}, "account_type": {"income"}}).Body.String())
	if !strings.Contains(body, "already in use") {
		t.Errorf("want a duplicate hint, got: %s", firstLines(body))
	}
	if strings.Contains(body, errLedgerStore.Error()) {
		t.Error("the store's own error reached the page")
	}
}

// Each tab asks the store for its own report, and only that one.
func TestEachTabLoadsItsOwnReport(t *testing.T) {
	for tab, check := range map[string]func(*testing.T, *fakeGL){
		"": func(t *testing.T, f *fakeGL) {
			if f.gotLimit <= 0 {
				t.Error("the journal was listed without a bound")
			}
		},
		"income-statement": func(t *testing.T, f *fakeGL) {
			if f.gotFrom.IsZero() {
				t.Error("the income statement was not requested")
			}
		},
		"balance-sheet": func(t *testing.T, f *fakeGL) {
			// Inclusive of today: a balance sheet as of now must include
			// entries posted earlier today.
			if !f.gotAsOf.After(time.Now()) {
				t.Errorf("as-of %s excludes today's postings", f.gotAsOf)
			}
		},
	} {
		t.Run("tab="+tab, func(t *testing.T) {
			f := &fakeGL{accounts: []ledger.Account{{ID: 1, Code: "4000", Name: "Service Revenue"}}}
			target := "/staff/ledger"
			if tab != "" {
				target += "?tab=" + tab
			}
			if rec := serveGL(glHandler(f), owner(), http.MethodGet, target, nil); rec.Code != http.StatusOK {
				t.Fatalf("got %d", rec.Code)
			}
			check(t, f)
		})
	}
}

// Each report's failure says which report, without the store's error.
func TestLedgerStoreFailuresNameTheReport(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *fakeGL
		tab  string
		want string
	}{
		"accounts": {&fakeGL{listErr: errLedgerStore}, "", "Could not load the chart of accounts"},
		"journal":  {&fakeGL{journalErr: errLedgerStore}, "", "Could not load the journal"},
		"trial":    {&fakeGL{trialErr: errLedgerStore}, "trial-balance", "Could not load the trial balance"},
		"income":   {&fakeGL{stmtErr: errLedgerStore}, "income-statement", "Could not load the income statement"},
		"balance":  {&fakeGL{stmtErr: errLedgerStore}, "balance-sheet", "Could not load the balance sheet"},
	} {
		t.Run(name, func(t *testing.T) {
			target := "/staff/ledger"
			if tc.tab != "" {
				target += "?tab=" + tc.tab
			}
			body := pageText(serveGL(glHandler(tc.f), owner(), http.MethodGet, target, nil).Body.String())
			if !strings.Contains(body, tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(body))
			}
			if strings.Contains(body, errLedgerStore.Error()) {
				t.Error("the store's own error reached the page")
			}
		})
	}
}

// Owner and billing admin only.
func TestOnlyOwnerAndBillingReachTheLedger(t *testing.T) {
	f := &fakeGL{}
	for _, role := range []string{"csr", "technician", "noc_engineer", "lco"} {
		for _, tc := range []struct{ method, target string }{
			{http.MethodGet, "/staff/ledger"},
			{http.MethodPost, "/staff/ledger/accounts"},
			{http.MethodPost, "/staff/ledger/entries"},
		} {
			if rec := serveGL(glHandler(f), sess(role), tc.method, tc.target, twoLines()); rec.Code == http.StatusOK {
				t.Errorf("%s reached %s %s", role, tc.method, tc.target)
			}
		}
	}
	if f.gotEntry != nil || f.gotAccount.Code != "" {
		t.Error("a refused role reached the store")
	}

	bare := glHandler(nil)
	if !strings.Contains(pageText(serveGL(bare, owner(), http.MethodGet, "/staff/ledger", nil).Body.String()),
		"not configured") {
		t.Error("an unconfigured ledger did not say so")
	}
	for _, target := range []string{"/staff/ledger/accounts", "/staff/ledger/entries"} {
		if rec := serveGL(bare, owner(), http.MethodPost, target, twoLines()); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s: want 503, got %d", target, rec.Code)
		}
	}
}
