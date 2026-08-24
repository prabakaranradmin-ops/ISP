package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/ledger"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// General ledger endpoints — CRD-EXP-006 Phase 1 | DBD §6.2 "General ledger".
//
// Owner/billing_admin only, same reach as every other financial screen in
// this package. Phase 1 only: there is no route here that posts a journal
// entry on behalf of a wallet recharge, franchise commission, or purchase
// order — those are Phase 2, not authorized by the design this implements
// (see the CRD). Every entry reachable through these routes is 'manual'.

// GeneralLedgerQuerier is the persistence surface the general ledger needs.
// Named to avoid colliding with invoices.go's own LedgerQuerier (the
// subscriber wallet ledger — a different table, a different concept).
// Satisfied by *db.LedgerStore.
type GeneralLedgerQuerier interface {
	CreateAccount(ctx context.Context, a ledger.Account) (*ledger.Account, error)
	ListAccounts(ctx context.Context) ([]ledger.Account, error)
	PostJournalEntry(ctx context.Context, e ledger.NewJournalEntry) (*ledger.JournalEntry, error)
	GetJournalEntry(ctx context.Context, id int) (*ledger.JournalEntry, error)
	ListJournalEntries(ctx context.Context, limit int) ([]ledger.JournalEntry, error)
	TrialBalance(ctx context.Context) ([]ledger.TrialBalanceRow, error)
	IncomeStatement(ctx context.Context, from, to time.Time) ([]ledger.StatementRow, error)
	BalanceSheet(ctx context.Context, asOf time.Time) ([]ledger.StatementRow, error)
}

type createAccountRequest struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	AccountType   string `json:"account_type"`
	NormalBalance string `json:"normal_balance,omitempty"`
}

// CreateLedgerAccount handles POST /api/v1/ledger/accounts.
func (h *Handler) CreateLedgerAccount(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	var req createAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Code == "" || req.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "code and name are required")
		return
	}
	if !ledger.ValidAccountType(req.AccountType) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "account_type must be one of: asset, liability, equity, income, expense")
		return
	}
	normalBalance := req.NormalBalance
	if normalBalance == "" {
		normalBalance = ledger.NormalBalanceFor(req.AccountType)
	} else if normalBalance != ledger.BalanceDebit && normalBalance != ledger.BalanceCredit {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "normal_balance must be \"debit\" or \"credit\"")
		return
	}

	created, err := h.generalLedger.CreateAccount(r.Context(), ledger.Account{
		Code: req.Code, Name: req.Name, AccountType: req.AccountType, NormalBalance: normalBalance,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "ERR_CONFLICT", "an account with that code already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create account failed")
		return
	}
	middleware.Audit(r.Context(), "ledger.account_create", strconv.Itoa(created.ID), map[string]any{"code": created.Code})
	writeJSON(w, http.StatusCreated, created)
}

// ListLedgerAccounts handles GET /api/v1/ledger/accounts.
func (h *Handler) ListLedgerAccounts(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	list, err := h.generalLedger.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list accounts failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type postJournalLineRequest struct {
	AccountID int    `json:"account_id"`
	Debit     string `json:"debit"`
	Credit    string `json:"credit"`
}

type postJournalEntryRequest struct {
	Description string                   `json:"description"`
	Lines       []postJournalLineRequest `json:"lines"`
}

// PostJournalEntry handles POST /api/v1/ledger/entries — always a manual
// entry (Phase 1's only source_type). Balance validation happens at
// *db.LedgerStore (a clear error) and again at trg_gl_journal_balanced (the
// real guarantee) — not duplicated a third time here.
func (h *Handler) PostJournalEntry(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	var req postJournalEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "description is required")
		return
	}
	if len(req.Lines) < 2 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "a journal entry needs at least two lines")
		return
	}

	lines := make([]ledger.NewJournalLine, len(req.Lines))
	for i, l := range req.Lines {
		if l.AccountID <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "every line needs an account_id")
			return
		}
		lines[i] = ledger.NewJournalLine{AccountID: l.AccountID, Debit: emptyIsZero(l.Debit), Credit: emptyIsZero(l.Credit)}
	}

	created, err := h.generalLedger.PostJournalEntry(r.Context(), ledger.NewJournalEntry{
		Description: req.Description, Lines: lines, CreatedBy: middleware.SubjectFromContext(r.Context()),
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	middleware.Audit(r.Context(), "ledger.entry_post", strconv.Itoa(created.ID), map[string]any{"description": created.Description})
	writeJSON(w, http.StatusCreated, created)
}

func emptyIsZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// ListJournalEntries handles GET /api/v1/ledger/entries.
func (h *Handler) ListJournalEntries(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	list, err := h.generalLedger.ListJournalEntries(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list journal entries failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GetJournalEntry handles GET /api/v1/ledger/entries/{id}.
func (h *Handler) GetJournalEntry(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	entry, err := h.generalLedger.GetJournalEntry(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "journal entry lookup failed")
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "journal entry not found")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// GetTrialBalance handles GET /api/v1/ledger/trial-balance.
func (h *Handler) GetTrialBalance(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	rows, err := h.generalLedger.TrialBalance(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "trial balance failed")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetIncomeStatement handles GET /api/v1/ledger/income-statement?from=&to=
// (YYYY-MM-DD; defaults to the current calendar month).
func (h *Handler) GetIncomeStatement(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	from, to, err := ledgerDateWindow(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	rows, err := h.generalLedger.IncomeStatement(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "income statement failed")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetBalanceSheet handles GET /api/v1/ledger/balance-sheet?as_of= (YYYY-MM-DD;
// defaults to now).
func (h *Handler) GetBalanceSheet(w http.ResponseWriter, r *http.Request) {
	if h.generalLedger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "general ledger is not configured")
		return
	}
	asOf := time.Now()
	if v := r.URL.Query().Get("as_of"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "as_of must be a date like 2026-08-31")
			return
		}
		asOf = t.AddDate(0, 0, 1) // inclusive of the named day
	}
	rows, err := h.generalLedger.BalanceSheet(r.Context(), asOf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "balance sheet failed")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func ledgerDateWindow(r *http.Request) (from, to time.Time, err error) {
	now := time.Now()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to = from.AddDate(0, 1, 0)
	if v := r.URL.Query().Get("from"); v != "" {
		from, err = time.Parse("2006-01-02", v)
		if err != nil {
			return from, to, err
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, perr := time.Parse("2006-01-02", v)
		if perr != nil {
			return from, to, perr
		}
		to = t.AddDate(0, 0, 1) // inclusive of the named day
	}
	return from, to, nil
}
