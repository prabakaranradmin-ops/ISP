package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/ledger"
	"github.com/rs/zerolog/log"
)

// GeneralLedgerStore backs the Ledger screen — CRD-EXP-006 Phase 1, the
// console counterpart to internal/api/ledger.go. Every entry reachable
// through this screen is a manual entry; there is no button that auto-posts
// on behalf of a wallet recharge, franchise commission, or purchase order
// (Phase 2, not implemented — see the CRD).
//
// Redefined per package rather than sharing api.GeneralLedgerQuerier,
// matching every other store interface in this file. Satisfied by
// *db.LedgerStore.
type GeneralLedgerStore interface {
	CreateAccount(ctx context.Context, a ledger.Account) (*ledger.Account, error)
	ListAccounts(ctx context.Context) ([]ledger.Account, error)
	PostJournalEntry(ctx context.Context, e ledger.NewJournalEntry) (*ledger.JournalEntry, error)
	GetJournalEntry(ctx context.Context, id int) (*ledger.JournalEntry, error)
	ListJournalEntries(ctx context.Context, limit int) ([]ledger.JournalEntry, error)
	TrialBalance(ctx context.Context) ([]ledger.TrialBalanceRow, error)
	IncomeStatement(ctx context.Context, from, to time.Time) ([]ledger.StatementRow, error)
	BalanceSheet(ctx context.Context, asOf time.Time) ([]ledger.StatementRow, error)
}

type ledgerData struct {
	Accounts     []ledger.Account
	Entries      []ledger.JournalEntry
	TrialBalance []ledger.TrialBalanceRow
	IncomeRows   []ledger.StatementRow
	BalanceRows  []ledger.StatementRow
	Tab          string
}

// Ledger shows the chart of accounts, the journal, and the trial balance/
// P&L/balance sheet reports, switched by a plain ?tab= query — server-
// rendered, matching Reports' own reasoning for not using a JS tab
// component here.
func (h *Handler) Ledger(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "ledger")
	if !ok {
		return
	}
	h.renderLedger(w, r, s, "", "")
}

func (h *Handler) renderLedger(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "General Ledger", "ledger")
	d.Message, d.Error = message, errMsg

	if h.generalLedger == nil {
		d.Error = "The general ledger is not configured on this deployment."
		h.render(w, "ledger", d)
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "journal"
	}
	ld := ledgerData{Tab: tab}

	accounts, err := h.generalLedger.ListAccounts(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list ledger accounts failed")
		d.Error = "Could not load the chart of accounts."
		h.render(w, "ledger", d)
		return
	}
	ld.Accounts = accounts

	switch tab {
	case "trial-balance":
		rows, err := h.generalLedger.TrialBalance(r.Context())
		if err != nil {
			log.Error().Err(err).Msg("staffui: trial balance failed")
			d.Error = "Could not load the trial balance."
		}
		ld.TrialBalance = rows
	case "income-statement":
		from, to := monthWindow(r)
		rows, err := h.generalLedger.IncomeStatement(r.Context(), from, to)
		if err != nil {
			log.Error().Err(err).Msg("staffui: income statement failed")
			d.Error = "Could not load the income statement."
		}
		ld.IncomeRows = rows
	case "balance-sheet":
		rows, err := h.generalLedger.BalanceSheet(r.Context(), time.Now().AddDate(0, 0, 1))
		if err != nil {
			log.Error().Err(err).Msg("staffui: balance sheet failed")
			d.Error = "Could not load the balance sheet."
		}
		ld.BalanceRows = rows
	default: // "journal"
		entries, err := h.generalLedger.ListJournalEntries(r.Context(), 100)
		if err != nil {
			log.Error().Err(err).Msg("staffui: list journal entries failed")
			d.Error = "Could not load the journal."
		}
		ld.Entries = entries
	}

	d.Data = ld
	h.render(w, "ledger", d)
}

// monthWindow returns the current calendar month unless overridden, the
// same default internal/api/ledger.go's ledgerDateWindow uses.
func monthWindow(r *http.Request) (from, to time.Time) {
	now := time.Now()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to = from.AddDate(0, 1, 0)
	return from, to
}

// CreateLedgerAccountForm adds a chart-of-accounts entry.
func (h *Handler) CreateLedgerAccountForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "ledger")
	if !ok {
		return
	}
	if h.generalLedger == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The general ledger is not configured.")
		return
	}

	code := strings.TrimSpace(r.PostFormValue("code"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	accountType := r.PostFormValue("account_type")
	if code == "" || name == "" {
		h.renderLedger(w, r, s, "", "Code and name are required.")
		return
	}
	if !ledger.ValidAccountType(accountType) {
		h.renderLedger(w, r, s, "", "Account type must be one of: asset, liability, equity, income, expense.")
		return
	}

	_, err := h.generalLedger.CreateAccount(r.Context(), ledger.Account{
		Code: code, Name: name, AccountType: accountType,
		NormalBalance: ledger.NormalBalanceFor(accountType),
	})
	if err != nil {
		log.Error().Err(err).Str("code", code).Msg("staffui: create ledger account failed")
		h.renderLedger(w, r, s, "", "Could not add that account — check the code is not already in use.")
		return
	}
	h.renderLedger(w, r, s, fmt.Sprintf("Account %s (%s) added.", code, name), "")
}

// PostJournalEntryForm posts a manual entry. The form carries a fixed
// four-line grid (most entries are two lines; four covers the common
// three-or-four-way split without a dynamic add-row control) — any line
// left with both amounts blank is simply dropped before posting.
func (h *Handler) PostJournalEntryForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "ledger")
	if !ok {
		return
	}
	if h.generalLedger == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The general ledger is not configured.")
		return
	}

	description := strings.TrimSpace(r.PostFormValue("description"))
	if description == "" {
		h.renderLedger(w, r, s, "", "Description is required.")
		return
	}

	var lines []ledger.NewJournalLine
	for i := 1; i <= 4; i++ {
		suffix := strconv.Itoa(i)
		accountRaw := strings.TrimSpace(r.PostFormValue("account_id_" + suffix))
		debit := strings.TrimSpace(r.PostFormValue("debit_" + suffix))
		credit := strings.TrimSpace(r.PostFormValue("credit_" + suffix))
		if accountRaw == "" || (debit == "" && credit == "") {
			continue
		}
		accountID, err := strconv.Atoi(accountRaw)
		if err != nil {
			h.renderLedger(w, r, s, "", fmt.Sprintf("Line %d has an invalid account.", i))
			return
		}
		if debit == "" {
			debit = "0"
		}
		if credit == "" {
			credit = "0"
		}
		lines = append(lines, ledger.NewJournalLine{AccountID: accountID, Debit: debit, Credit: credit})
	}
	if len(lines) < 2 {
		h.renderLedger(w, r, s, "", "A journal entry needs at least two lines.")
		return
	}

	created, err := h.generalLedger.PostJournalEntry(r.Context(), ledger.NewJournalEntry{
		Description: description, Lines: lines, CreatedBy: s.Username,
	})
	if err != nil {
		log.Error().Err(err).Msg("staffui: post journal entry failed")
		h.renderLedger(w, r, s, "", "Could not post that entry: "+err.Error())
		return
	}
	h.renderLedger(w, r, s, fmt.Sprintf("Journal entry #%d posted.", created.ID), "")
}
