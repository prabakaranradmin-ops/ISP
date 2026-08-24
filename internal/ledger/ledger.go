// Package ledger implements the general ledger (CRD-EXP-006, Phase 1 |
// DBD §6.2 "General ledger") — a real chart of accounts and double-entry
// journal for the business's own books.
//
// Distinct from internal/billing's wallet_ledgers (one subscriber's prepaid
// balance) and internal/revenue's lco_ledger (one franchise partner's
// commission): neither answers "what did the business itself earn and
// spend." This package is standalone by design — nothing here reads or
// writes any other table, and nothing else references this one. Auto-
// posting from wallet recharges, franchise commission, or received purchase
// orders is Phase 2, explicitly out of scope here; see the CRD.
package ledger

import "time"

// Account types and their normal balance side. Stored per account rather
// than derived at query time (see AccountType's own doc), but these are the
// only combination Phase 1 ever creates through CreateAccount.
const (
	TypeAsset     = "asset"
	TypeLiability = "liability"
	TypeEquity    = "equity"
	TypeIncome    = "income"
	TypeExpense   = "expense"

	BalanceDebit  = "debit"
	BalanceCredit = "credit"
)

// ValidAccountType reports whether t is a legal chart-of-accounts type.
func ValidAccountType(t string) bool {
	switch t {
	case TypeAsset, TypeLiability, TypeEquity, TypeIncome, TypeExpense:
		return true
	default:
		return false
	}
}

// NormalBalanceFor returns the conventional normal balance side for an
// account type — debit for assets/expenses, credit for
// liabilities/equity/income. CreateAccount uses this as the default so a
// caller does not have to know accounting convention to add an account
// correctly; a caller that genuinely needs a contra account can still pass
// an explicit override.
func NormalBalanceFor(accountType string) string {
	switch accountType {
	case TypeAsset, TypeExpense:
		return BalanceDebit
	default:
		return BalanceCredit
	}
}

// Account is one row in the chart of accounts.
type Account struct {
	ID            int    `json:"id"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	AccountType   string `json:"account_type"`
	NormalBalance string `json:"normal_balance"`
	IsActive      bool   `json:"is_active"`
}

// NewJournalLine is one leg of a journal entry to persist. Exactly one of
// Debit/Credit must be positive and the other zero — DebitAmount/
// CreditAmount are decimal strings, matching this codebase's rule that
// money never reaches a JSON boundary as a float.
type NewJournalLine struct {
	AccountID int
	Debit     string
	Credit    string
}

// NewJournalEntry is a manual entry to post — Phase 1's only source_type.
type NewJournalEntry struct {
	Description string
	Lines       []NewJournalLine
	CreatedBy   string
}

// JournalEntry is a stored entry with its lines, as an operator sees it.
type JournalEntry struct {
	ID          int           `json:"id"`
	EntryDate   time.Time     `json:"entry_date"`
	Description string        `json:"description"`
	SourceType  string        `json:"source_type"`
	CreatedBy   string        `json:"created_by"`
	CreatedAt   time.Time     `json:"created_at"`
	Lines       []JournalLine `json:"lines"`
}

// JournalLine is one leg of a stored entry, with the account's own code/name
// joined in — a listing that had to look up every account separately would
// be the kind of screen nobody actually uses.
type JournalLine struct {
	AccountID   int    `json:"account_id"`
	AccountCode string `json:"account_code"`
	AccountName string `json:"account_name"`
	Debit       string `json:"debit"`
	Credit      string `json:"credit"`
}

// TrialBalanceRow is one account's position — see DBD §6.2's own note on
// why Balance is signed by the account's normal_balance rather than left as
// a raw debit-minus-credit: an asset account with a negative Balance here
// is exactly the anomaly a trial balance exists to surface.
type TrialBalanceRow struct {
	AccountID     int    `json:"account_id"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	AccountType   string `json:"account_type"`
	NormalBalance string `json:"normal_balance"`
	DebitTotal    string `json:"debit_total"`
	CreditTotal   string `json:"credit_total"`
	Balance       string `json:"balance"`
}

// StatementRow is one account's movement (income statement) or position
// (balance sheet) within a reporting window.
type StatementRow struct {
	AccountID   int    `json:"account_id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	AccountType string `json:"account_type"`
	Amount      string `json:"amount"`
}
