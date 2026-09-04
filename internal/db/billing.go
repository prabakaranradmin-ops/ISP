package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// BillingStore serves wallet, dunning, ledger and invoice operations.
// Satisfies billing.WalletQuerier, billing.DunningQuerier, api.LedgerQuerier
// and api.InvoiceQuerier.
type BillingStore struct{ pool dbPool }

var (
	_ billing.WalletQuerier      = (*BillingStore)(nil)
	_ billing.DunningQuerier     = (*BillingStore)(nil)
	_ billing.RenewalScanQuerier = (*BillingStore)(nil)
	_ api.LedgerQuerier          = (*BillingStore)(nil)
	_ api.InvoiceQuerier         = (*BillingStore)(nil)
	_ api.RefundQuerier          = (*BillingStore)(nil)
)

// GetSubscriberBalance reads the current wallet balance.
func (s *BillingStore) GetSubscriberBalance(ctx context.Context, subscriberID int) (decimal.Decimal, error) {
	const q = `SELECT wallet_balance::text FROM subscribers WHERE id = $1`

	var balance string
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(&balance)
	if isNoRows(err) {
		return decimal.Zero, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("db: get balance for subscriber %d: %w", subscriberID, err)
	}
	return parseDecimal(balance)
}

// GetTransactionByToken looks up a prior recharge by its idempotency token.
// Returns (nil, nil) when the token has not been seen.
func (s *BillingStore) GetTransactionByToken(ctx context.Context, token string) (*billing.Transaction, error) {
	if token == "" {
		return nil, nil
	}
	const q = `
		SELECT id, subscriber_id, entry_type, amount::text, balance_after::text,
		       COALESCE(transaction_token, ''), COALESCE(description, '')
		FROM wallet_ledgers
		WHERE transaction_token = $1
		LIMIT 1`

	var (
		tx                   billing.Transaction
		amount, balanceAfter string
	)
	err := s.pool.QueryRow(ctx, q, token).Scan(
		&tx.ID, &tx.SubscriberID, &tx.EntryType, &amount, &balanceAfter,
		&tx.TransactionToken, &tx.Description,
	)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get transaction by token: %w", err)
	}
	if tx.Amount, err = parseDecimal(amount); err != nil {
		return nil, err
	}
	if tx.BalanceAfter, err = parseDecimal(balanceAfter); err != nil {
		return nil, err
	}
	return &tx, nil
}

// RecordRecharge writes both ledger legs and the new wallet balance in one
// transaction, so a crash can never leave the ledger and subscribers.wallet_balance
// disagreeing — which the nightly reconciliation would then report as variance.
//
// FR: FR-BIL-003, FR-BIL-005 | DBD §6.2 wallet_ledgers
func (s *BillingStore) RecordRecharge(ctx context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	const insertLeg = `
		INSERT INTO wallet_ledgers (
			subscriber_id, franchise_id, account, entry_type,
			amount, balance_after, transaction_token, description, adjusted_by_username
		) VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7,$8,NULLIF($9,''))
		RETURNING id`

	const updateBalance = `UPDATE subscribers SET wallet_balance = $2::numeric WHERE id = $1`

	var tx *billing.Transaction

	err := inTx(ctx, s.pool, func(dbTx pgx.Tx) error {
		// The counter-leg carries no token: idx_wallet_token is unique over
		// non-null tokens, so both legs holding it would collide with themselves.
		if _, err := dbTx.Exec(ctx, insertLeg,
			p.Debit.SubscriberID, p.Debit.FranchiseID, p.Debit.Account, p.Debit.EntryType,
			p.Debit.Amount.String(), p.Debit.BalanceAfter.String(), nil, p.Debit.Description,
			p.Debit.AdjustedBy,
		); err != nil {
			return fmt.Errorf("db: insert debit leg: %w", err)
		}

		var creditID int
		if err := dbTx.QueryRow(ctx, insertLeg,
			p.Credit.SubscriberID, p.Credit.FranchiseID, p.Credit.Account, p.Credit.EntryType,
			p.Credit.Amount.String(), p.Credit.BalanceAfter.String(), p.Credit.TransactionToken,
			p.Credit.Description, p.Credit.AdjustedBy,
		).Scan(&creditID); err != nil {
			return fmt.Errorf("db: insert credit leg: %w", err)
		}

		if _, err := dbTx.Exec(ctx, updateBalance, p.SubscriberID, p.NewBalance.String()); err != nil {
			return fmt.Errorf("db: update wallet balance: %w", err)
		}

		if err := postWalletGLEntry(ctx, dbTx, p, creditID); err != nil {
			return err
		}

		token := ""
		if p.Credit.TransactionToken != nil {
			token = *p.Credit.TransactionToken
		}
		tx = &billing.Transaction{
			ID:               creditID,
			SubscriberID:     p.Credit.SubscriberID,
			EntryType:        p.Credit.EntryType,
			Amount:           p.Credit.Amount,
			BalanceAfter:     p.NewBalance,
			TransactionToken: token,
			Description:      p.Credit.Description,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// postWalletGLEntry posts the general-ledger side of a wallet posting
// (CRD-EXP-006 Phase 2), in the same transaction as the wallet_ledgers legs
// it mirrors so the two can never disagree. Every wallet_ledgers leg's own
// EntryType already is its GL line's debit/credit side — see
// billing.GLAccountCode's own doc comment — so this only has to resolve
// each leg's account code and insert.
//
// walletLegID is the credit leg's wallet_ledgers row (the one RecordRecharge
// returns as the Transaction), kept as source_id purely for traceability
// from a GL line back to the subledger row that caused it.
func postWalletGLEntry(ctx context.Context, dbTx pgx.Tx, p billing.RechargePosting, walletLegID int) error {
	debitCode := billing.GLAccountCode(p.Debit.Account)
	creditCode := billing.GLAccountCode(p.Credit.Account)
	if debitCode == "" || creditCode == "" {
		return fmt.Errorf("db: no GL account mapped for wallet leg account %q/%q", p.Debit.Account, p.Credit.Account)
	}

	createdBy := p.Credit.AdjustedBy
	if createdBy == "" {
		createdBy = "system"
	}

	var entryID int
	if err := dbTx.QueryRow(ctx, `
		INSERT INTO gl_journal_entries (description, source_type, source_id, created_by)
		VALUES ($1, 'wallet_ledger', $2, $3)
		RETURNING id`,
		p.Credit.Description, walletLegID, createdBy,
	).Scan(&entryID); err != nil {
		return fmt.Errorf("db: insert wallet GL journal entry: %w", err)
	}

	// Each leg posts to whichever of debit/credit its own EntryType names —
	// a leg is never both, chk_gl_line_not_both would reject the ambiguity.
	legs := []struct {
		code      string
		entryType string
		amount    decimal.Decimal
	}{
		{debitCode, p.Debit.EntryType, p.Debit.Amount},
		{creditCode, p.Credit.EntryType, p.Credit.Amount},
	}
	legs, err := splitTaxLeg(legs, debitCode, p.TaxAmount)
	if err != nil {
		return err
	}

	for _, leg := range legs {
		accountID, err := glAccountID(ctx, dbTx, leg.code)
		if err != nil {
			return err
		}
		debit, credit := "0", "0"
		if leg.entryType == "debit" {
			debit = leg.amount.String()
		} else {
			credit = leg.amount.String()
		}
		if _, err := dbTx.Exec(ctx, `
			INSERT INTO gl_journal_lines (journal_entry_id, account_id, debit, credit)
			VALUES ($1, $2, $3::numeric, $4::numeric)`,
			entryID, accountID, debit, credit,
		); err != nil {
			return fmt.Errorf("db: insert wallet GL journal line (%s): %w", leg.code, err)
		}
	}
	return nil
}

// glLeg is one line of a GL journal entry, before it is resolved to an
// account id and inserted.
type glLeg = struct {
	code      string
	entryType string
	amount    decimal.Decimal
}

// splitTaxLeg divides the revenue leg of a taxed posting into the revenue it
// actually earned and the GST it merely collected on the government's behalf
// (migration 047).
//
// The split happens here, in the general ledger, and deliberately not in
// wallet_ledgers. The wallet subledger answers "what happened to this
// subscriber's money", and from the subscriber's side one charge left their
// wallet — splitting it there would make every wallet leg pair into a
// triple, break the balance_after invariant each row carries, and change
// what the nightly reconciliation (FR-REV-002) compares. The tax split is an
// accounting classification of the same movement, which is exactly what a
// general ledger is for.
//
// A refund of a taxed charge reverses through the identical path: the
// counter leg's EntryType flips to "debit", so the tax line debits 2200 and
// reduces the liability, which is the correct treatment for tax on money
// given back.
func splitTaxLeg(legs []glLeg, counterCode string, tax decimal.Decimal) ([]glLeg, error) {
	if tax.IsZero() {
		return legs, nil
	}
	// Tax on anything but a revenue charge would mean a caller has mapped a
	// tax component onto an adjustment, a refund clearing account or a
	// gateway settlement, none of which create an output-tax liability.
	// Refusing is right: the alternative is a plausible-looking 2200 balance
	// that no return can be filed from.
	if counterCode != glRevenueAccount {
		return nil, fmt.Errorf("db: tax amount %s posted against non-revenue account %s", tax, counterCode)
	}
	out := make([]glLeg, 0, len(legs)+1)
	for _, leg := range legs {
		if leg.code != counterCode {
			out = append(out, leg)
			continue
		}
		net := leg.amount.Sub(tax)
		if net.LessThanOrEqual(decimal.Zero) {
			return nil, fmt.Errorf("db: tax amount %s is not within revenue leg amount %s", tax, leg.amount)
		}
		out = append(out,
			glLeg{code: leg.code, entryType: leg.entryType, amount: net},
			glLeg{code: billing.GLAccountGSTPayable, entryType: leg.entryType, amount: tax},
		)
	}
	return out, nil
}

// glRevenueAccount is the only account a tax split may be taken out of; it
// mirrors billing.GLAccountCode's mapping for AccountRevenueClearing.
const glRevenueAccount = "4000"

// glAccountID resolves a chart-of-accounts code to its id.
//
// Separated from the INSERT, rather than left as an inline subquery, so a
// missing account is reported as itself. Inlined, an unknown code makes the
// subquery yield NULL and the failure surfaces as a not-null violation on
// gl_journal_lines.account_id — which reads as a bug in the posting code
// rather than as what it actually is: the binary running ahead of its
// migration. That ordering is a live hazard, not a hypothetical one —
// codes 5200 and 2100 arrive in migration 045, and deploying GL Phase 2's
// binary before applying it would otherwise roll back every staff wallet
// adjustment with a message pointing nowhere near the cause.
func glAccountID(ctx context.Context, dbTx pgx.Tx, code string) (int, error) {
	var id int
	err := dbTx.QueryRow(ctx, `SELECT id FROM chart_of_accounts WHERE code = $1`, code).Scan(&id)
	if isNoRows(err) {
		return 0, fmt.Errorf(
			"db: chart of accounts has no code %q — the general ledger schema is behind this binary; apply the pending migrations (bootstrap.exe) before serving traffic", code)
	}
	if err != nil {
		return 0, fmt.Errorf("db: resolve GL account %q: %w", code, err)
	}
	return id, nil
}

// GetSubscriberDunningState returns the current dunning stage and plan expiry.
func (s *BillingStore) GetSubscriberDunningState(ctx context.Context, subscriberID int) (billing.DunningState, time.Time, error) {
	const q = `SELECT dunning_state, COALESCE(plan_expiry, NOW()) FROM subscribers WHERE id = $1`

	var (
		state  string
		expiry time.Time
	)
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(&state, &expiry)
	if isNoRows(err) {
		return "", time.Time{}, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("db: get dunning state for subscriber %d: %w", subscriberID, err)
	}
	return billing.DunningState(state), expiry, nil
}

// ListDunningCandidates returns every subscriber whose dunning stage may need
// to advance, newest expiry first.
//
// The filter is deliberately broad — anything with a plan expiry inside the
// dunning window, plus anything already mid-dunning regardless of date. Which
// stage a subscriber actually belongs in is decided in one place,
// billing.NextDunningState, rather than being split between a SQL predicate
// here and Go there; two copies of that rule would drift.
//
// Terminated subscribers are excluded: they have left, and re-suspending them
// every hour would be noise.
func (s *BillingStore) ListDunningCandidates(ctx context.Context) ([]billing.DunningCandidate, error) {
	const q = `
		SELECT id, username, COALESCE(mobile_number, ''), dunning_state, plan_expiry
		  FROM subscribers
		 WHERE plan_expiry IS NOT NULL
		   AND status <> 'terminated'
		   AND (plan_expiry <= NOW() + INTERVAL '7 days'
		        OR dunning_state <> 'active')
		 ORDER BY plan_expiry DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list dunning candidates: %w", err)
	}
	defer rows.Close()

	var out []billing.DunningCandidate
	for rows.Next() {
		var (
			c     billing.DunningCandidate
			state string
		)
		if err := rows.Scan(&c.SubscriberID, &c.Username, &c.MobileNumber, &state, &c.PlanExpiry); err != nil {
			return nil, fmt.Errorf("db: scan dunning candidate: %w", err)
		}
		c.State = billing.DunningState(state)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate dunning candidates: %w", err)
	}
	return out, nil
}

// SetSubscriberDunningState advances the dunning stage and the derived
// subscribers.status that RADIUS enforces on the next Access-Request.
//
// Both columns move together in one statement: a dunning state of
// hard_suspended with a status still 'active' would keep a non-paying
// subscriber online.
func (s *BillingStore) SetSubscriberDunningState(ctx context.Context, subscriberID int, state billing.DunningState, status string) error {
	// The ctx CTE attributes the status change for migration 031's capture
	// trigger. This path runs from the dunning scanner, which has no JWT, so
	// the caller annotates its context with middleware.WithSubject — that is
	// what keeps an automatic suspension distinguishable from an operator's.
	const q = `
		WITH ctx AS (
			SELECT set_config('app.actor', $4, true)              AS actor,
			       set_config('app.change_reason', 'dunning', true) AS reason
		)
		UPDATE subscribers SET dunning_state = $2, status = $3
		FROM ctx
		WHERE subscribers.id = $1 AND ctx.actor IS NOT NULL`

	tag, err := s.pool.Exec(ctx, q, subscriberID, string(state), status, actorFromContext(ctx))
	if err != nil {
		return fmt.Errorf("db: set dunning state for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	return nil
}

// ListRenewalCandidates returns subscribers whose plan has already expired
// and whose wallet balance covers their current plan's price — the auto-
// renewal scanner's candidate set (FR-BIL-009).
//
// Terminated subscribers are excluded for the same reason ListDunningCandidates
// excludes them: they have left, and there is nothing to renew them into.
func (s *BillingStore) ListRenewalCandidates(ctx context.Context) ([]billing.RenewalCandidate, error) {
	const q = `
		SELECT s.id, s.username, s.franchise_id, s.registered_state, s.dunning_state,
		       p.name, p.price::text, p.validity_days, p.volume_gb, s.plan_expiry
		  FROM subscribers s
		  JOIN plans p ON p.id = s.plan_id
		 WHERE s.status <> 'terminated'
		   AND s.plan_expiry IS NOT NULL
		   AND s.plan_expiry <= NOW()
		   AND s.wallet_balance >= p.price`
	// The balance test is a coarse pre-filter, not the affordability rule.
	//
	// plans.price is GST-exclusive, so a renewal actually costs price + GST
	// and the scanner debits the invoice total. This still selects on price
	// alone because that is a strict superset — anyone who cannot afford the
	// base certainly cannot afford the taxed total — and the precise check
	// belongs where the money moves: the debit returns ErrInsufficientBalance
	// and renew() leaves that subscriber for dunning.
	//
	// Joining gst_rates here to filter exactly would duplicate the rate
	// resolution (effective_from ordering and all) in a second place, and a
	// disagreement between the two would show up as subscribers who are
	// selected every tick and never renewed.

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list renewal candidates: %w", err)
	}
	defer rows.Close()

	var out []billing.RenewalCandidate
	for rows.Next() {
		var (
			c     billing.RenewalCandidate
			state string
			price string
		)
		if err := rows.Scan(
			&c.SubscriberID, &c.Username, &c.FranchiseID, &c.RegisteredState, &state,
			&c.PlanName, &price, &c.PlanValidityDays, &c.PlanVolumeGB, &c.PlanExpiry,
		); err != nil {
			return nil, fmt.Errorf("db: scan renewal candidate: %w", err)
		}
		c.DunningState = billing.DunningState(state)
		if c.PlanPrice, err = parseDecimal(price); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate renewal candidates: %w", err)
	}
	return out, nil
}

// SetPlanExpiry extends a subscriber's plan validity after a successful
// auto-renewal. Mirrors PortalStore.SetPlanExpiry (used by portal renewal) —
// kept as its own method on BillingStore rather than a shared cross-store
// dependency, matching how every other store in this package owns its own
// small queries against the pool it already holds.
func (s *BillingStore) SetPlanExpiry(ctx context.Context, subscriberID int, expiry time.Time) error {
	const q = `UPDATE subscribers SET plan_expiry = $2 WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, subscriberID, expiry); err != nil {
		return fmt.Errorf("db: set plan expiry for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// CreateRefund persists a refund's business record, linked to the wallet
// ledger row WalletService.Post wrote for the debit. This deployment has no
// live gateway refund API, so every refund is written as already processed.
//
// FR: FR-BIL-011 | DBD §6.2 payment_refunds
func (s *BillingStore) CreateRefund(ctx context.Context, subscriberID, ledgerEntryID int, amount decimal.Decimal, reason, refundedBy string) (int, error) {
	const q = `
		INSERT INTO payment_refunds (subscriber_id, ledger_entry_id, amount, reason, status, refunded_by_username)
		VALUES ($1, $2, $3::numeric, $4, 'processed', $5)
		RETURNING id`

	var id int
	err := s.pool.QueryRow(ctx, q, subscriberID, ledgerEntryID, amount.String(), reason, refundedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: create refund for subscriber %d: %w", subscriberID, err)
	}
	return id, nil
}

// GetInvoiceInputs returns the two subscriber-specific inputs a renewal
// invoice needs beyond the amount actually charged: the state that decides
// intrastate vs interstate GST, and the current plan's data volume for
// FR-BIL-007's usage summary.
func (s *BillingStore) GetInvoiceInputs(ctx context.Context, subscriberID int) (registeredState string, planVolumeGB int, err error) {
	const q = `
		SELECT s.registered_state, p.volume_gb
		FROM subscribers s
		JOIN plans p ON p.id = s.plan_id
		WHERE s.id = $1`

	err = s.pool.QueryRow(ctx, q, subscriberID).Scan(&registeredState, &planVolumeGB)
	if isNoRows(err) {
		return "", 0, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return "", 0, fmt.Errorf("db: get invoice inputs for subscriber %d: %w", subscriberID, err)
	}
	return registeredState, planVolumeGB, nil
}

// CreateInvoice persists a computed GST invoice.
//
// The chk_gst_logic CHECK rejects an invoice carrying both intrastate and
// interstate tax, so a miscomputed invoice fails here rather than reaching GSTR-1.
func (s *BillingStore) CreateInvoice(ctx context.Context, inv billing.Invoice) (int, error) {
	const q = `
		INSERT INTO invoices (
			subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
			total_amount, gst_rate_id, gb_included, gb_used
		) VALUES ($1,$2::numeric,$3::numeric,$4::numeric,$5::numeric,$6::numeric,$7,$8,$9::numeric)
		RETURNING id`

	var id int
	err := s.pool.QueryRow(ctx, q,
		inv.SubscriberID, inv.BaseAmount.String(), inv.CgstAmount.String(),
		inv.SgstAmount.String(), inv.IgstAmount.String(), inv.TotalAmount.String(),
		inv.GstRateID, inv.GbIncluded, inv.GbUsed.String(),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: create invoice for subscriber %d: %w", inv.SubscriberID, err)
	}
	return id, nil
}

// GetActiveGstRate returns the rate effective now.
func (s *BillingStore) GetActiveGstRate(ctx context.Context) (billing.GstRate, error) {
	const q = `
		SELECT id, cgst_rate::text, sgst_rate::text, igst_rate::text
		FROM gst_rates
		WHERE effective_from <= NOW()
		ORDER BY effective_from DESC
		LIMIT 1`

	var (
		rate             billing.GstRate
		cgst, sgst, igst string
	)
	err := s.pool.QueryRow(ctx, q).Scan(&rate.ID, &cgst, &sgst, &igst)
	if isNoRows(err) {
		return billing.GstRate{}, fmt.Errorf("db: no effective GST rate: %w", ErrNotFound)
	}
	if err != nil {
		return billing.GstRate{}, fmt.Errorf("db: get active GST rate: %w", err)
	}
	if rate.CgstRate, err = parseDecimal(cgst); err != nil {
		return billing.GstRate{}, err
	}
	if rate.SgstRate, err = parseDecimal(sgst); err != nil {
		return billing.GstRate{}, err
	}
	if rate.IgstRate, err = parseDecimal(igst); err != nil {
		return billing.GstRate{}, err
	}
	return rate, nil
}

// ── Ledger (API-004) ────────────────────────────────────────────────────────

// ListLedgerEntries returns a subscriber's wallet_ledgers rows, newest first,
// optionally bounded by [from, to].
func (s *BillingStore) ListLedgerEntries(ctx context.Context, subscriberID int, from, to *time.Time, limit int) ([]api.LedgerEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
		SELECT id, entry_type, account, amount::text, balance_after::text,
		       COALESCE(transaction_token, ''), COALESCE(description, ''), created_at
		FROM wallet_ledgers
		WHERE subscriber_id = $1
		  AND ($2::timestamptz IS NULL OR created_at >= $2)
		  AND ($3::timestamptz IS NULL OR created_at <= $3)
		ORDER BY created_at DESC
		LIMIT $4`

	rows, err := s.pool.Query(ctx, q, subscriberID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list ledger entries for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	entries := make([]api.LedgerEntry, 0, limit)
	for rows.Next() {
		var e api.LedgerEntry
		if err := rows.Scan(&e.ID, &e.EntryType, &e.Account, &e.Amount, &e.BalanceAfter,
			&e.TransactionToken, &e.Description, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan ledger row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate ledger entries: %w", err)
	}
	return entries, nil
}

// ── Invoices (API-004) ──────────────────────────────────────────────────────

// ListInvoices returns invoice summaries for a subscriber, newest first.
func (s *BillingStore) ListInvoices(ctx context.Context, subscriberID int) ([]api.InvoiceSummary, error) {
	const q = `
		SELECT id, subscriber_id, base_amount::text, cgst_amount::text, sgst_amount::text,
		       igst_amount::text, total_amount::text, gb_included, gb_used::text, created_at
		FROM invoices
		WHERE subscriber_id = $1
		ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, q, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list invoices for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	invoices := make([]api.InvoiceSummary, 0, 12)
	for rows.Next() {
		var inv api.InvoiceSummary
		if err := rows.Scan(&inv.ID, &inv.SubscriberID, &inv.BaseAmount, &inv.CGSTAmount, &inv.SGSTAmount,
			&inv.IGSTAmount, &inv.TotalAmount, &inv.GBIncluded, &inv.GBUsed, &inv.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan invoice row: %w", err)
		}
		invoices = append(invoices, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate invoices: %w", err)
	}
	return invoices, nil
}

// GetInvoiceDetail loads everything the PDF template needs for one invoice:
// the invoice row joined to the subscriber, their plan, and the GST rate the
// invoice was computed under.
func (s *BillingStore) GetInvoiceDetail(ctx context.Context, invoiceID int) (*api.InvoiceDetail, error) {
	const q = `
		SELECT i.id, i.subscriber_id, i.base_amount::text, i.cgst_amount::text, i.sgst_amount::text,
		       i.igst_amount::text, i.total_amount::text, i.gb_included, i.gb_used::text, i.created_at,
		       s.username, s.mobile_number, s.registered_state,
		       p.name, g.cgst_rate::text, g.sgst_rate::text, g.igst_rate::text,
		       p.rate_limit_string, COALESCE(p.fup_throttle_string, ''), s.fup_active
		FROM invoices i
		JOIN subscribers s ON s.id = i.subscriber_id
		JOIN plans p       ON p.id = s.plan_id
		JOIN gst_rates g   ON g.id = i.gst_rate_id
		WHERE i.id = $1`

	var (
		d                      api.InvoiceDetail
		rateLimit, fupThrottle string
		fupActive              bool
	)
	err := s.pool.QueryRow(ctx, q, invoiceID).Scan(
		&d.ID, &d.SubscriberID, &d.BaseAmount, &d.CGSTAmount, &d.SGSTAmount,
		&d.IGSTAmount, &d.TotalAmount, &d.GBIncluded, &d.GBUsed, &d.CreatedAt,
		&d.SubscriberName, &d.MobileNumber, &d.RegisteredState,
		&d.PlanName, &d.CGSTRate, &d.SGSTRate, &d.IGSTRate,
		&rateLimit, &fupThrottle, &fupActive,
	)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get invoice detail %d: %w", invoiceID, err)
	}

	d.FUPApplied = fupActive && fupThrottle != ""
	if d.FUPApplied {
		d.SpeedActive = fupThrottle
	} else {
		d.SpeedActive = rateLimit
	}
	return &d, nil
}

// ── FR-BIL-006: GSTR-1 ──────────────────────────────────────────────────────

// ListInvoicesForGSTR1 returns every invoice issued in a calendar month,
// with the recipient details a return needs.
//
// One flat row set rather than three aggregate queries: the B2B/B2C split
// and the HSN summary all derive from the same invoices, and computing
// them in Go (billing.BuildReturn) keeps the arithmetic testable without a
// database and guarantees the three sections reconcile because they are
// summed from one pass over one slice.
//
// The month is bounded by a half-open range on created_at so the query can
// use an index rather than wrapping the column in date_trunc.
func (s *BillingStore) ListInvoicesForGSTR1(ctx context.Context, year int, month time.Month) ([]billing.InvoiceRow, error) {
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	const q = `
		SELECT i.id, i.created_at, i.subscriber_id,
		       s.username, COALESCE(s.gstin, ''), s.registered_state,
		       i.base_amount, i.cgst_amount, i.sgst_amount, i.igst_amount, i.total_amount
		  FROM invoices i
		  JOIN subscribers s ON s.id = i.subscriber_id
		 WHERE i.created_at >= $1 AND i.created_at < $2
		 ORDER BY i.id`

	rows, err := s.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("db: list invoices for GSTR-1: %w", err)
	}
	defer rows.Close()

	var out []billing.InvoiceRow
	for rows.Next() {
		var r billing.InvoiceRow
		if err := rows.Scan(&r.InvoiceID, &r.InvoiceDate, &r.SubscriberID,
			&r.SubscriberName, &r.RecipientGSTIN, &r.RecipientState,
			&r.TaxableValue, &r.CGST, &r.SGST, &r.IGST, &r.Total); err != nil {
			return nil, fmt.Errorf("db: scan GSTR-1 invoice row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
