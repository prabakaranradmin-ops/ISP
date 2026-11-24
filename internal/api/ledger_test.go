//go:build integration

// The general ledger's HTTP surface — FR-ACC-001..005.
//
// internal/api/ledger.go was at 0%: 135 statements, nine handlers, and the
// only part of this system whose output somebody signs. A trial balance, an
// income statement and a balance sheet are what an ISP's accountant files
// returns from, so the failures that matter are the quiet ones: a date window
// off by a day, a duplicate account code accepted as a second "4000 Revenue",
// a lookup miss rendered as an empty statement rather than a 404.
//
// Balance validation is deliberately not duplicated here — the store reports
// it and trg_gl_journal_balanced guarantees it — so what these tests pin down
// at this layer is that an unbalanced entry's rejection reaches the caller as
// a validation error with the reason, not as a 500.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/ledger"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

type fakeLedger struct {
	accounts []ledger.Account
	entries  []ledger.JournalEntry
	entry    *ledger.JournalEntry
	trial    []ledger.TrialBalanceRow
	rows     []ledger.StatementRow

	// What the handler passed down. The date window is the whole question on
	// the two statement endpoints.
	gotAccount ledger.Account
	gotEntry   ledger.NewJournalEntry
	gotFrom    time.Time
	gotTo      time.Time
	gotAsOf    time.Time
	gotLimit   int

	createErr error
	listErr   error
	postErr   error
	getErr    error
	trialErr  error
	stmtErr   error
}

func (f *fakeLedger) CreateAccount(_ context.Context, a ledger.Account) (*ledger.Account, error) {
	f.gotAccount = a
	if f.createErr != nil {
		return nil, f.createErr
	}
	a.ID = 42
	a.IsActive = true
	return &a, nil
}

func (f *fakeLedger) ListAccounts(context.Context) ([]ledger.Account, error) {
	return f.accounts, f.listErr
}

func (f *fakeLedger) PostJournalEntry(_ context.Context, e ledger.NewJournalEntry) (*ledger.JournalEntry, error) {
	f.gotEntry = e
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &ledger.JournalEntry{ID: 7, Description: e.Description, CreatedBy: e.CreatedBy}, nil
}

func (f *fakeLedger) GetJournalEntry(_ context.Context, _ int) (*ledger.JournalEntry, error) {
	return f.entry, f.getErr
}

func (f *fakeLedger) ListJournalEntries(_ context.Context, limit int) ([]ledger.JournalEntry, error) {
	f.gotLimit = limit
	return f.entries, f.listErr
}

func (f *fakeLedger) TrialBalance(context.Context) ([]ledger.TrialBalanceRow, error) {
	return f.trial, f.trialErr
}

func (f *fakeLedger) IncomeStatement(_ context.Context, from, to time.Time) ([]ledger.StatementRow, error) {
	f.gotFrom, f.gotTo = from, to
	return f.rows, f.stmtErr
}

func (f *fakeLedger) BalanceSheet(_ context.Context, asOf time.Time) ([]ledger.StatementRow, error) {
	f.gotAsOf = asOf
	return f.rows, f.stmtErr
}

// ledgerCall dispatches to the handler the path implies, with a subject on
// the context as the auth middleware leaves it.
func ledgerCall(h *api.Handler, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r = r.WithContext(middleware.WithSubject(r.Context(), "meena"))

	path := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 5 && parts[3] == "entries" {
		r.SetPathValue("id", parts[4])
	}

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/ledger/accounts") && method == http.MethodPost:
		h.CreateLedgerAccount(w, r)
	case strings.HasSuffix(path, "/ledger/accounts"):
		h.ListLedgerAccounts(w, r)
	case strings.HasSuffix(path, "/ledger/entries") && method == http.MethodPost:
		h.PostJournalEntry(w, r)
	case strings.HasSuffix(path, "/ledger/entries"):
		h.ListJournalEntries(w, r)
	case strings.Contains(path, "/ledger/entries/"):
		h.GetJournalEntry(w, r)
	case strings.Contains(path, "/trial-balance"):
		h.GetTrialBalance(w, r)
	case strings.Contains(path, "/income-statement"):
		h.GetIncomeStatement(w, r)
	case strings.Contains(path, "/balance-sheet"):
		h.GetBalanceSheet(w, r)
	default:
		panic("ledgerCall: no handler for " + target)
	}
	return w
}

func ledgerHandler(l api.GeneralLedgerQuerier) *api.Handler {
	return api.NewHandler(api.HandlerDeps{GeneralLedger: l})
}

// Every endpoint reports an unconfigured ledger as 503 with a reason, rather
// than panicking or answering 200 with an empty statement — which an
// accountant would read as "no revenue this month".
func TestFR_ACC_001_AnUnconfiguredLedgerIsReportedOnEveryEndpoint(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{})

	for name, tc := range map[string]struct {
		method, target, body string
	}{
		"create account":   {http.MethodPost, "/api/v1/ledger/accounts", `{"code":"4000","name":"Revenue","account_type":"income"}`},
		"list accounts":    {http.MethodGet, "/api/v1/ledger/accounts", ""},
		"post entry":       {http.MethodPost, "/api/v1/ledger/entries", `{"description":"x","lines":[{"account_id":1,"debit":"1"},{"account_id":2,"credit":"1"}]}`},
		"list entries":     {http.MethodGet, "/api/v1/ledger/entries", ""},
		"get entry":        {http.MethodGet, "/api/v1/ledger/entries/7", ""},
		"trial balance":    {http.MethodGet, "/api/v1/ledger/trial-balance", ""},
		"income statement": {http.MethodGet, "/api/v1/ledger/income-statement", ""},
		"balance sheet":    {http.MethodGet, "/api/v1/ledger/balance-sheet", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := ledgerCall(h, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d — %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ERR_UNAVAILABLE") {
				t.Errorf("the refusal should name the reason, got %s", rec.Body.String())
			}
		})
	}
}

// A duplicate account code is a conflict, not a 500. Two accounts both coded
// 4000 is a chart of accounts that no longer adds up, and the operator needs
// to be told which it is.
func TestFR_ACC_001_ADuplicateAccountCodeIsAConflict(t *testing.T) {
	h := ledgerHandler(&fakeLedger{
		createErr: errors.New(`ERROR: duplicate key value violates unique constraint "gl_accounts_code_key"`),
	})
	rec := ledgerCall(h, http.MethodPost, "/api/v1/ledger/accounts",
		`{"code":"4000","name":"Service Revenue","account_type":"income"}`)

	if rec.Code != http.StatusConflict {
		t.Errorf("want 409, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("the message should say what the conflict is, got %s", rec.Body.String())
	}
	// The constraint name is a schema detail and has no business reaching an
	// API client.
	if strings.Contains(rec.Body.String(), "gl_accounts_code_key") {
		t.Errorf("the database's constraint name leaked to the caller: %s", rec.Body.String())
	}
}

// The normal balance is derived from the account type when it is not given,
// because getting it backwards silently inverts that account on every
// statement it appears in.
func TestFR_ACC_001_TheNormalBalanceIsDerivedFromTheAccountType(t *testing.T) {
	for _, tc := range []struct {
		accountType string
		want        string
	}{
		{"asset", ledger.BalanceDebit},
		{"expense", ledger.BalanceDebit},
		{"liability", ledger.BalanceCredit},
		{"equity", ledger.BalanceCredit},
		{"income", ledger.BalanceCredit},
	} {
		t.Run(tc.accountType, func(t *testing.T) {
			f := &fakeLedger{}
			rec := ledgerCall(ledgerHandler(f), http.MethodPost, "/api/v1/ledger/accounts",
				fmt.Sprintf(`{"code":"1","name":"X","account_type":%q}`, tc.accountType))
			if rec.Code != http.StatusCreated {
				t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
			}
			if f.gotAccount.NormalBalance != tc.want {
				t.Errorf("normal balance = %q, want %q", f.gotAccount.NormalBalance, tc.want)
			}
		})
	}

	// An explicit value is honoured — a contra account is the legitimate
	// reason to override the default.
	f := &fakeLedger{}
	rec := ledgerCall(ledgerHandler(f), http.MethodPost, "/api/v1/ledger/accounts",
		`{"code":"4900","name":"Sales Returns","account_type":"income","normal_balance":"debit"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.gotAccount.NormalBalance != ledger.BalanceDebit {
		t.Errorf("an explicit contra balance was overridden: %q", f.gotAccount.NormalBalance)
	}
}

func TestFR_ACC_001_AccountValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":           {`{`, http.StatusBadRequest},
		"no code":            {`{"name":"Revenue","account_type":"income"}`, http.StatusUnprocessableEntity},
		"no name":            {`{"code":"4000","account_type":"income"}`, http.StatusUnprocessableEntity},
		"unknown type":       {`{"code":"4000","name":"Revenue","account_type":"profit"}`, http.StatusUnprocessableEntity},
		"no type":            {`{"code":"4000","name":"Revenue"}`, http.StatusUnprocessableEntity},
		"bad normal balance": {`{"code":"4000","name":"Revenue","account_type":"income","normal_balance":"sideways"}`, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLedger{}
			rec := ledgerCall(ledgerHandler(f), http.MethodPost, "/api/v1/ledger/accounts", tc.body)
			if rec.Code != tc.want {
				t.Errorf("want %d, got %d — %s", tc.want, rec.Code, rec.Body.String())
			}
			if f.gotAccount.Code != "" {
				t.Errorf("invalid input reached the store: %+v", f.gotAccount)
			}
		})
	}
}

// A journal entry records who posted it, from the token rather than the body.
// "Who typed this adjustment" is the first question asked about a manual
// entry, and a client-supplied name would be worthless as evidence.
func TestFR_ACC_002_AnEntryRecordsWhoPostedItFromTheToken(t *testing.T) {
	f := &fakeLedger{}
	rec := ledgerCall(ledgerHandler(f), http.MethodPost, "/api/v1/ledger/entries",
		`{"description":"opening balance","created_by":"somebody_else",
		  "lines":[{"account_id":1,"debit":"100.00"},{"account_id":2,"credit":"100.00"}]}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.gotEntry.CreatedBy != "meena" {
		t.Errorf("created_by = %q; it must come from the token, not the body", f.gotEntry.CreatedBy)
	}
	if len(f.gotEntry.Lines) != 2 {
		t.Fatalf("the store got %d lines", len(f.gotEntry.Lines))
	}
}

// An omitted side is zero, not empty. An empty string reaching a numeric
// column is a 500 at the store instead of a balanced entry.
func TestFR_ACC_002_AnOmittedSideIsZeroNotEmpty(t *testing.T) {
	f := &fakeLedger{}
	ledgerCall(ledgerHandler(f), http.MethodPost, "/api/v1/ledger/entries",
		`{"description":"x","lines":[{"account_id":1,"debit":"100.00"},{"account_id":2,"credit":"100.00"}]}`)

	for i, l := range f.gotEntry.Lines {
		if l.Debit == "" || l.Credit == "" {
			t.Errorf("line %d reached the store with an empty amount: %+v", i, l)
		}
	}
	if f.gotEntry.Lines[0].Credit != "0" || f.gotEntry.Lines[1].Debit != "0" {
		t.Errorf("the unused side should be \"0\", got %+v", f.gotEntry.Lines)
	}
}

// An unbalanced entry is the store's refusal, passed through as a validation
// error with its reason — not a 500, and not swallowed.
func TestFR_ACC_002_AnUnbalancedEntryIsRefusedWithTheReason(t *testing.T) {
	const why = "ledger: debits 100.00 do not equal credits 90.00"
	h := ledgerHandler(&fakeLedger{postErr: errors.New(why)})

	rec := ledgerCall(h, http.MethodPost, "/api/v1/ledger/entries",
		`{"description":"x","lines":[{"account_id":1,"debit":"100.00"},{"account_id":2,"credit":"90.00"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "do not equal") {
		t.Errorf("the caller should be told why, got %s", rec.Body.String())
	}
}

func TestFR_ACC_002_EntryValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":         {`{`, http.StatusBadRequest},
		"no description":   {`{"lines":[{"account_id":1,"debit":"1"},{"account_id":2,"credit":"1"}]}`, http.StatusUnprocessableEntity},
		"no lines":         {`{"description":"x"}`, http.StatusUnprocessableEntity},
		"one line":         {`{"description":"x","lines":[{"account_id":1,"debit":"1"}]}`, http.StatusUnprocessableEntity},
		"missing account":  {`{"description":"x","lines":[{"debit":"1"},{"account_id":2,"credit":"1"}]}`, http.StatusUnprocessableEntity},
		"account zero":     {`{"description":"x","lines":[{"account_id":0,"debit":"1"},{"account_id":2,"credit":"1"}]}`, http.StatusUnprocessableEntity},
		"negative account": {`{"description":"x","lines":[{"account_id":-1,"debit":"1"},{"account_id":2,"credit":"1"}]}`, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLedger{}
			rec := ledgerCall(ledgerHandler(f), http.MethodPost, "/api/v1/ledger/entries", tc.body)
			if rec.Code != tc.want {
				t.Errorf("want %d, got %d — %s", tc.want, rec.Code, rec.Body.String())
			}
			if f.gotEntry.Description != "" {
				t.Errorf("invalid input reached the store: %+v", f.gotEntry)
			}
		})
	}
}

// A journal entry that does not exist is 404, not an empty entry. An empty
// one reads as "this posting had no lines", which is a different and much
// more alarming claim.
func TestFR_ACC_002_AMissingEntryIsNotFound(t *testing.T) {
	h := ledgerHandler(&fakeLedger{entry: nil})
	rec := ledgerCall(h, http.MethodGet, "/api/v1/ledger/entries/404", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}

	if rec := ledgerCall(h, http.MethodGet, "/api/v1/ledger/entries/not-a-number", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric id, got %d", rec.Code)
	}

	broken := ledgerHandler(&fakeLedger{getErr: errors.New("store down")})
	if rec := ledgerCall(broken, http.MethodGet, "/api/v1/ledger/entries/7", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500 when the lookup fails, got %d", rec.Code)
	}
}

// The income statement's window is inclusive of the named end day. Off by one
// here moves a month's revenue into the next month — the error nobody notices
// until two filings disagree.
func TestFR_ACC_004_TheIncomeStatementWindowIncludesTheNamedEndDay(t *testing.T) {
	f := &fakeLedger{}
	rec := ledgerCall(ledgerHandler(f), http.MethodGet,
		"/api/v1/ledger/income-statement?from=2026-11-01&to=2026-11-30", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	if got := f.gotFrom.Format("2006-01-02"); got != "2026-11-01" {
		t.Errorf("from = %s, want 2026-11-01", got)
	}
	// Exclusive upper bound one day past the named day, so 30 November's own
	// postings are inside the window.
	if got := f.gotTo.Format("2006-01-02"); got != "2026-12-01" {
		t.Errorf("to = %s, want 2026-12-01 — an exclusive bound of 2026-11-30 would drop "+
			"everything posted on the 30th", got)
	}
}

// With no window at all it is the current calendar month, not all of history.
func TestFR_ACC_004_TheDefaultWindowIsTheCurrentMonth(t *testing.T) {
	f := &fakeLedger{}
	if rec := ledgerCall(ledgerHandler(f), http.MethodGet, "/api/v1/ledger/income-statement", ""); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	now := time.Now()
	wantFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	if !f.gotFrom.Equal(wantFrom) {
		t.Errorf("from = %s, want the first of this month (%s)", f.gotFrom, wantFrom)
	}
	if !f.gotTo.Equal(wantFrom.AddDate(0, 1, 0)) {
		t.Errorf("to = %s, want the first of next month", f.gotTo)
	}
}

// The balance sheet's as_of is inclusive of its named day too.
func TestFR_ACC_005_TheBalanceSheetAsOfIncludesItsNamedDay(t *testing.T) {
	f := &fakeLedger{}
	if rec := ledgerCall(ledgerHandler(f), http.MethodGet,
		"/api/v1/ledger/balance-sheet?as_of=2026-03-31", ""); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := f.gotAsOf.Format("2006-01-02"); got != "2026-04-01" {
		t.Errorf("as_of reached the store as %s; a balance sheet at 31 March must include "+
			"31 March, which is an exclusive bound of 1 April", got)
	}
}

func TestFR_ACC_004_ABadDateIsAValidationError(t *testing.T) {
	for name, target := range map[string]string{
		"from":  "/api/v1/ledger/income-statement?from=31-11-2026",
		"to":    "/api/v1/ledger/income-statement?to=last%20tuesday",
		"as_of": "/api/v1/ledger/balance-sheet?as_of=2026-13-45",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLedger{}
			rec := ledgerCall(ledgerHandler(f), http.MethodGet, target, "")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
			if !f.gotFrom.IsZero() || !f.gotAsOf.IsZero() {
				t.Error("a malformed date still queried the store")
			}
		})
	}
}

// The reads return what the store returns, and a store failure is a 500 with
// a code rather than a half-written body.
func TestFR_ACC_003_TheReadsPassThroughAndFailuresAreReported(t *testing.T) {
	f := &fakeLedger{
		accounts: []ledger.Account{{ID: 1, Code: "4000", Name: "Service Revenue", AccountType: "income"}},
		entries:  []ledger.JournalEntry{{ID: 7, Description: "opening balance"}},
		entry:    &ledger.JournalEntry{ID: 7, Description: "opening balance"},
		trial: []ledger.TrialBalanceRow{
			{AccountID: 1, Code: "4000", DebitTotal: "0.00", CreditTotal: "104400.00", Balance: "104400.00"},
		},
		rows: []ledger.StatementRow{{AccountID: 1, Code: "4000", Amount: "104400.00"}},
	}
	h := ledgerHandler(f)

	for name, target := range map[string]string{
		"accounts":         "/api/v1/ledger/accounts",
		"entries":          "/api/v1/ledger/entries",
		"entry":            "/api/v1/ledger/entries/7",
		"trial balance":    "/api/v1/ledger/trial-balance",
		"income statement": "/api/v1/ledger/income-statement",
		"balance sheet":    "/api/v1/ledger/balance-sheet",
	} {
		t.Run(name, func(t *testing.T) {
			rec := ledgerCall(h, http.MethodGet, target, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
				t.Errorf("content type %q", ct)
			}
			var any any
			if err := json.Unmarshal(rec.Body.Bytes(), &any); err != nil {
				t.Errorf("the body is not valid JSON: %v — %s", err, rec.Body.String())
			}
		})
	}

	// The listing bound is applied rather than left to the store's default,
	// so a ledger with years of history cannot answer with all of it.
	if f.gotLimit <= 0 {
		t.Errorf("list entries passed limit %d; an unbounded listing of a general ledger "+
			"is a page that never loads", f.gotLimit)
	}

	broken := ledgerHandler(&fakeLedger{
		listErr:  errors.New("store down"),
		trialErr: errors.New("store down"),
		stmtErr:  errors.New("store down"),
	})
	for name, target := range map[string]string{
		"accounts":         "/api/v1/ledger/accounts",
		"entries":          "/api/v1/ledger/entries",
		"trial balance":    "/api/v1/ledger/trial-balance",
		"income statement": "/api/v1/ledger/income-statement",
		"balance sheet":    "/api/v1/ledger/balance-sheet",
	} {
		t.Run("failure/"+name, func(t *testing.T) {
			rec := ledgerCall(broken, http.MethodGet, target, "")
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("want 500, got %d — %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ERR_INTERNAL") {
				t.Errorf("the failure should carry a code, got %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "store down") {
				t.Errorf("the store's own error reached the caller: %s", rec.Body.String())
			}
		})
	}
}

// Posting an account and an entry are both audited, because a chart of
// accounts and a manual journal entry are exactly the changes an auditor asks
// to see a trail for.
func TestFR_ACC_002_AccountsAndEntriesAreAudited(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	h := ledgerHandler(&fakeLedger{})
	ledgerCall(h, http.MethodPost, "/api/v1/ledger/accounts",
		`{"code":"4000","name":"Service Revenue","account_type":"income"}`)
	ledgerCall(h, http.MethodPost, "/api/v1/ledger/entries",
		`{"description":"opening balance","lines":[{"account_id":1,"debit":"1.00"},{"account_id":2,"credit":"1.00"}]}`)

	entries := sink.entries()
	if len(entries) != 2 {
		t.Fatalf("want two audit entries, got %d", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Action] = true
	}
	for _, want := range []string{"ledger.account_create", "ledger.entry_post"} {
		if !seen[want] {
			t.Errorf("%q was not audited; recorded: %v", want, seen)
		}
	}
}
