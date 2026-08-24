package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/maaransoft/isp-bss-oss/internal/ledger"
)

// LedgerStore serves the general ledger (CRD-EXP-006, Phase 1).
type LedgerStore struct{ pool dbPool }

// Ledger exposes the general-ledger store.
func (d *DB) Ledger() *LedgerStore { return &LedgerStore{pool: d.pool} }

// CreateAccount adds a chart-of-accounts entry.
func (s *LedgerStore) CreateAccount(ctx context.Context, a ledger.Account) (*ledger.Account, error) {
	const q = `
		INSERT INTO chart_of_accounts (code, name, account_type, normal_balance)
		VALUES ($1, $2, $3, $4)
		RETURNING id, code, name, account_type, normal_balance, is_active`

	var out ledger.Account
	err := s.pool.QueryRow(ctx, q, a.Code, a.Name, a.AccountType, a.NormalBalance).
		Scan(&out.ID, &out.Code, &out.Name, &out.AccountType, &out.NormalBalance, &out.IsActive)
	if err != nil {
		return nil, fmt.Errorf("db: create ledger account %q: %w", a.Code, err)
	}
	return &out, nil
}

// ListAccounts returns the chart of accounts, active first then by code.
func (s *LedgerStore) ListAccounts(ctx context.Context) ([]ledger.Account, error) {
	const q = `
		SELECT id, code, name, account_type, normal_balance, is_active
		  FROM chart_of_accounts
		 ORDER BY is_active DESC, code`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list ledger accounts: %w", err)
	}
	defer rows.Close()

	out := make([]ledger.Account, 0, 16)
	for rows.Next() {
		var a ledger.Account
		if err := rows.Scan(&a.ID, &a.Code, &a.Name, &a.AccountType, &a.NormalBalance, &a.IsActive); err != nil {
			return nil, fmt.Errorf("db: scan ledger account row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PostJournalEntry writes an entry and all its lines in one transaction.
// Balance is validated here (returning a clear error before ever reaching
// the database) and again by trg_gl_journal_balanced, deferred to commit —
// the trigger is the real guarantee; this check exists so a caller sees
// "lines do not balance" instead of a raw constraint-trigger message from
// deep inside a commit failure.
func (s *LedgerStore) PostJournalEntry(ctx context.Context, e ledger.NewJournalEntry) (*ledger.JournalEntry, error) {
	var sum float64 // sum of (debit - credit); a coarse pre-check, not the source of truth
	for _, line := range e.Lines {
		d, err := parseDecimal(line.Debit)
		if err != nil {
			return nil, fmt.Errorf("db: line debit %q: %w", line.Debit, err)
		}
		c, err := parseDecimal(line.Credit)
		if err != nil {
			return nil, fmt.Errorf("db: line credit %q: %w", line.Credit, err)
		}
		diff, _ := d.Sub(c).Float64()
		sum += diff
	}
	if sum > 0.005 || sum < -0.005 {
		return nil, fmt.Errorf("db: journal entry does not balance: debits minus credits = %.2f", sum)
	}

	var entryID int
	var entryDate, createdAt time.Time
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO gl_journal_entries (description, source_type, created_by)
			VALUES ($1, 'manual', $2)
			RETURNING id, entry_date, created_at`,
			e.Description, e.CreatedBy,
		).Scan(&entryID, &entryDate, &createdAt); err != nil {
			return fmt.Errorf("insert journal entry: %w", err)
		}

		for _, line := range e.Lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO gl_journal_lines (journal_entry_id, account_id, debit, credit)
				VALUES ($1, $2, $3::numeric, $4::numeric)`,
				entryID, line.AccountID, line.Debit, line.Credit,
			); err != nil {
				return fmt.Errorf("insert journal line for account %d: %w", line.AccountID, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("db: post journal entry: %w", err)
	}

	return s.GetJournalEntry(ctx, entryID)
}

// GetJournalEntry loads one entry with its lines, or nil when no such id.
func (s *LedgerStore) GetJournalEntry(ctx context.Context, id int) (*ledger.JournalEntry, error) {
	const headQ = `
		SELECT id, entry_date, description, source_type, created_by, created_at
		  FROM gl_journal_entries WHERE id = $1`

	var e ledger.JournalEntry
	err := s.pool.QueryRow(ctx, headQ, id).Scan(
		&e.ID, &e.EntryDate, &e.Description, &e.SourceType, &e.CreatedBy, &e.CreatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get journal entry %d: %w", id, err)
	}

	const linesQ = `
		SELECT l.account_id, a.code, a.name, l.debit::text, l.credit::text
		  FROM gl_journal_lines l
		  JOIN chart_of_accounts a ON a.id = l.account_id
		 WHERE l.journal_entry_id = $1
		 ORDER BY l.id`
	rows, err := s.pool.Query(ctx, linesQ, id)
	if err != nil {
		return nil, fmt.Errorf("db: list lines for journal entry %d: %w", id, err)
	}
	defer rows.Close()

	for rows.Next() {
		var l ledger.JournalLine
		if err := rows.Scan(&l.AccountID, &l.AccountCode, &l.AccountName, &l.Debit, &l.Credit); err != nil {
			return nil, fmt.Errorf("db: scan journal line: %w", err)
		}
		e.Lines = append(e.Lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &e, nil
}

// ListJournalEntries returns entry headers, newest first (no lines — call
// GetJournalEntry for one entry's detail, the same list-then-drill-in shape
// every other screen in this console uses).
func (s *LedgerStore) ListJournalEntries(ctx context.Context, limit int) ([]ledger.JournalEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
		SELECT id, entry_date, description, source_type, created_by, created_at
		  FROM gl_journal_entries
		 ORDER BY entry_date DESC, id DESC
		 LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list journal entries: %w", err)
	}
	defer rows.Close()

	out := make([]ledger.JournalEntry, 0, 16)
	for rows.Next() {
		var e ledger.JournalEntry
		if err := rows.Scan(&e.ID, &e.EntryDate, &e.Description, &e.SourceType, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan journal entry row: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrialBalance reads v_gl_trial_balance (DBD §6.2).
func (s *LedgerStore) TrialBalance(ctx context.Context) ([]ledger.TrialBalanceRow, error) {
	const q = `
		SELECT account_id, code, name, account_type, normal_balance,
		       debit_total::text, credit_total::text, balance::text
		  FROM v_gl_trial_balance
		 ORDER BY code`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: trial balance: %w", err)
	}
	defer rows.Close()

	out := make([]ledger.TrialBalanceRow, 0, 16)
	for rows.Next() {
		var r ledger.TrialBalanceRow
		if err := rows.Scan(&r.AccountID, &r.Code, &r.Name, &r.AccountType, &r.NormalBalance,
			&r.DebitTotal, &r.CreditTotal, &r.Balance); err != nil {
			return nil, fmt.Errorf("db: scan trial balance row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IncomeStatement calls gl_income_statement(from, to) (DBD §6.2).
func (s *LedgerStore) IncomeStatement(ctx context.Context, from, to time.Time) ([]ledger.StatementRow, error) {
	return s.queryStatement(ctx, `SELECT account_id, code, name, account_type, amount::text FROM gl_income_statement($1, $2)`, from, to)
}

// BalanceSheet calls gl_balance_sheet(as_of) (DBD §6.2).
func (s *LedgerStore) BalanceSheet(ctx context.Context, asOf time.Time) ([]ledger.StatementRow, error) {
	return s.queryStatement(ctx, `SELECT account_id, code, name, account_type, amount::text FROM gl_balance_sheet($1)`, asOf)
}

func (s *LedgerStore) queryStatement(ctx context.Context, q string, args ...any) ([]ledger.StatementRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: ledger statement query: %w", err)
	}
	defer rows.Close()

	out := make([]ledger.StatementRow, 0, 16)
	for rows.Next() {
		var r ledger.StatementRow
		if err := rows.Scan(&r.AccountID, &r.Code, &r.Name, &r.AccountType, &r.Amount); err != nil {
			return nil, fmt.Errorf("db: scan ledger statement row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
