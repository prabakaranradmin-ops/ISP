package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// RevenueStore serves the nightly reconciliation job and franchise operations.
// Satisfies revenue.RevenueQuerier, revenue.FranchiseQuerier and
// revenue.SubscriberLister.
type RevenueStore struct{ pool dbPool }

var (
	_ revenue.RevenueQuerier   = (*RevenueStore)(nil)
	_ revenue.FranchiseQuerier = (*RevenueStore)(nil)
	_ revenue.SubscriberLister = (*RevenueStore)(nil)
)

// GetUnbilledActiveSubscribers counts active subscribers whose current billing
// period has no invoice.
//
// FR: FR-REV-001 | uses idx_revenue_unbilled
func (s *RevenueStore) GetUnbilledActiveSubscribers(ctx context.Context) (int, error) {
	const q = `
		SELECT COUNT(*)
		FROM subscribers s
		WHERE s.status = 'active'
		  AND NOT EXISTS (
		      SELECT 1 FROM invoices i
		      WHERE i.subscriber_id = s.id
		        AND i.created_at >= date_trunc('month', NOW())
		  )`

	var count int
	if err := s.pool.QueryRow(ctx, q).Scan(&count); err != nil {
		return 0, fmt.Errorf("db: count unbilled subscribers: %w", err)
	}
	return count, nil
}

// GetLedgerVariance returns the total drift between the wallet ledger and the
// balances denormalised onto subscribers.
//
// Only the subscriber_wallet leg is summed. Including the gateway clearing leg
// would net every recharge to zero and make the check structurally unable to
// detect a discrepancy.
//
// FR: FR-REV-002
func (s *RevenueStore) GetLedgerVariance(ctx context.Context) (decimal.Decimal, error) {
	const q = `
		WITH ledger AS (
			SELECT subscriber_id,
			       SUM(CASE WHEN entry_type = 'credit' THEN amount ELSE -amount END) AS net
			FROM wallet_ledgers
			WHERE account = 'subscriber_wallet'
			GROUP BY subscriber_id
		)
		SELECT COALESCE(SUM(s.wallet_balance - COALESCE(l.net, 0)), 0)::text
		FROM subscribers s
		LEFT JOIN ledger l ON l.subscriber_id = s.id`

	var variance string
	if err := s.pool.QueryRow(ctx, q).Scan(&variance); err != nil {
		return decimal.Zero, fmt.Errorf("db: compute ledger variance: %w", err)
	}
	return parseDecimal(variance)
}

// GetTotalWalletBalance sums every subscriber's wallet balance.
func (s *RevenueStore) GetTotalWalletBalance(ctx context.Context) (decimal.Decimal, error) {
	const q = `SELECT COALESCE(SUM(wallet_balance), 0)::text FROM subscribers`

	var total string
	if err := s.pool.QueryRow(ctx, q).Scan(&total); err != nil {
		return decimal.Zero, fmt.Errorf("db: sum wallet balances: %w", err)
	}
	return parseDecimal(total)
}

// UpsertRevenueSnapshot writes the day's snapshot, replacing any earlier run for
// the same date so a re-run does not double-count.
func (s *RevenueStore) UpsertRevenueSnapshot(ctx context.Context, snap revenue.RevenueSnapshot) error {
	const q = `
		INSERT INTO revenue_snapshots (
			snapshot_date, unbilled_subscriber_count, ledger_variance, total_wallet_balance
		) VALUES ($1::date, $2, $3::numeric, $4::numeric)`

	const clear = `DELETE FROM revenue_snapshots WHERE snapshot_date = $1::date`

	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, clear, snap.SnapshotDate); err != nil {
			return fmt.Errorf("db: clear revenue snapshot: %w", err)
		}
		if _, err := tx.Exec(ctx, q,
			snap.SnapshotDate, snap.UnbilledSubscriberCount,
			snap.LedgerVariance.String(), snap.TotalWalletBalance.String(),
		); err != nil {
			return fmt.Errorf("db: insert revenue snapshot: %w", err)
		}
		return nil
	})
}

// BuildCollectionsForecast projects renewals for the next `days` days, splitting
// each day into subscribers expected to renew and those at risk.
//
// At risk means the wallet cannot cover the plan price, or the account is
// already in a suspension stage — the two cases where a renewal silently fails.
//
// FR: FR-REV-004
func (s *RevenueStore) BuildCollectionsForecast(ctx context.Context, days int) ([]revenue.CollectionsForecast, error) {
	if days <= 0 {
		days = 30
	}
	const q = `
		SELECT CURRENT_DATE                              AS forecast_date,
		       s.plan_expiry::date                       AS forecast_for_date,
		       COUNT(*) FILTER (WHERE NOT at_risk)       AS expected_renewals,
		       COUNT(*) FILTER (WHERE at_risk)           AS at_risk_renewals,
		       COALESCE(SUM(p.price) FILTER (WHERE NOT at_risk), 0)::text AS expected_revenue,
		       COALESCE(SUM(p.price) FILTER (WHERE at_risk), 0)::text     AS at_risk_revenue
		FROM (
			SELECT s.id, s.plan_id, s.plan_expiry,
			       (s.wallet_balance < p.price
			        OR s.dunning_state IN ('soft_suspended','hard_suspended','grace_period')) AS at_risk
			FROM subscribers s
			JOIN plans p ON p.id = s.plan_id
			WHERE s.plan_expiry IS NOT NULL
			  AND s.plan_expiry::date BETWEEN CURRENT_DATE AND CURRENT_DATE + ($1::int)
			  AND s.status <> 'terminated'
		) s
		JOIN plans p ON p.id = s.plan_id
		GROUP BY s.plan_expiry::date
		ORDER BY forecast_for_date`

	rows, err := s.pool.Query(ctx, q, days)
	if err != nil {
		return nil, fmt.Errorf("db: build collections forecast: %w", err)
	}
	defer rows.Close()

	forecasts := make([]revenue.CollectionsForecast, 0, days)
	for rows.Next() {
		var (
			f                      revenue.CollectionsForecast
			expectedRev, atRiskRev string
		)
		if err := rows.Scan(&f.ForecastDate, &f.ForecastForDate,
			&f.ExpectedRenewals, &f.AtRiskRenewals, &expectedRev, &atRiskRev); err != nil {
			return nil, fmt.Errorf("db: scan forecast row: %w", err)
		}
		if f.ExpectedRevenue, err = parseDecimal(expectedRev); err != nil {
			return nil, err
		}
		if f.AtRiskRevenue, err = parseDecimal(atRiskRev); err != nil {
			return nil, err
		}
		forecasts = append(forecasts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate forecast rows: %w", err)
	}
	return forecasts, nil
}

// UpsertCollectionsForecast replaces the forecast generated today.
func (s *RevenueStore) UpsertCollectionsForecast(ctx context.Context, forecasts []revenue.CollectionsForecast) error {
	const clear = `DELETE FROM collections_forecast WHERE forecast_date = CURRENT_DATE`
	const insert = `
		INSERT INTO collections_forecast (
			forecast_date, forecast_for_date, expected_renewals, at_risk_renewals,
			expected_revenue, at_risk_revenue
		) VALUES ($1::date, $2::date, $3, $4, $5::numeric, $6::numeric)`

	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, clear); err != nil {
			return fmt.Errorf("db: clear collections forecast: %w", err)
		}
		for _, f := range forecasts {
			if _, err := tx.Exec(ctx, insert,
				f.ForecastDate, f.ForecastForDate, f.ExpectedRenewals, f.AtRiskRenewals,
				f.ExpectedRevenue.String(), f.AtRiskRevenue.String(),
			); err != nil {
				return fmt.Errorf("db: insert collections forecast: %w", err)
			}
		}
		return nil
	})
}

// ── Franchise ───────────────────────────────────────────────────────────────

// GetFranchiseByID loads one franchise.
func (s *RevenueStore) GetFranchiseByID(ctx context.Context, franchiseID int) (*revenue.Franchise, error) {
	const q = `SELECT id, name, commission_rate_pct::text, status FROM franchises WHERE id = $1`

	var (
		f    revenue.Franchise
		rate string
	)
	err := s.pool.QueryRow(ctx, q, franchiseID).Scan(&f.ID, &f.Name, &rate, &f.Status)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: franchise %d: %w", franchiseID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get franchise %d: %w", franchiseID, err)
	}
	if f.CommissionRatePct, err = parseDecimal(rate); err != nil {
		return nil, err
	}
	return &f, nil
}

// CalculateAndStoreLCOCommission writes one lco_ledger row and, atomically
// with it, the GL entry that recognises the commission expense and what is
// now owed to the partner (CRD-EXP-006 Phase 2: Dr Franchise Commission
// Expense, Cr Commission Payable to Partners).
//
// The commission is computed by the caller (revenue.CalculateLCOCommission) and
// passed in already rounded, so the stored figure is exactly what was reported.
//
// Idempotent on transaction_ref (idx_lco_ledger_txn_ref, migration 045): a
// recharge can be retried with the same token, and
// revenue.SettleCommissionForRecharge has no way of knowing that on its
// own — the guard has to live here, at the write, the same reasoning
// wallet_ledgers' own idx_wallet_token already applies one layer up. A
// conflict means this commission was already settled, so both the ledger
// row and its GL entry are skipped rather than double-posted; a token-less
// entry (TransactionRef == "") never matches the partial index and is
// always inserted, same as before this phase.
func (s *RevenueStore) CalculateAndStoreLCOCommission(ctx context.Context, entry revenue.LCOCommissionEntry) error {
	const insertLedger = `
		INSERT INTO lco_ledger (
			franchise_id, subscriber_id, recharge_amount, commission_amount, transaction_ref
		) VALUES ($1, $2, $3::numeric, $4::numeric, NULLIF($5,''))
		ON CONFLICT (transaction_ref) WHERE transaction_ref IS NOT NULL DO NOTHING
		RETURNING id`

	return inTx(ctx, s.pool, func(dbTx pgx.Tx) error {
		var ledgerID int
		err := dbTx.QueryRow(ctx, insertLedger,
			entry.FranchiseID, entry.SubscriberID,
			entry.RechargeAmount.String(), entry.CommissionAmount.String(), entry.TransactionRef,
		).Scan(&ledgerID)
		if isNoRows(err) {
			// Conflict: already settled for this transaction_ref.
			return nil
		}
		if err != nil {
			return fmt.Errorf("db: store LCO commission for franchise %d: %w", entry.FranchiseID, err)
		}

		if entry.CommissionAmount.IsZero() {
			// A balanced GL entry cannot have a zero-amount line
			// (chk_gl_line_nonzero) — a zero-rate partner still gets its
			// lco_ledger row above for the record, just no journal entry.
			return nil
		}

		var journalID int
		if err := dbTx.QueryRow(ctx, `
			INSERT INTO gl_journal_entries (description, source_type, source_id, created_by)
			VALUES ($1, 'lco_commission', $2, 'system')
			RETURNING id`,
			fmt.Sprintf("Franchise commission — partner %d, subscriber %d", entry.FranchiseID, entry.SubscriberID),
			ledgerID,
		).Scan(&journalID); err != nil {
			return fmt.Errorf("db: insert commission GL journal entry: %w", err)
		}

		const insertLine = `
			INSERT INTO gl_journal_lines (journal_entry_id, account_id, debit, credit)
			VALUES ($1, $2, $3::numeric, $4::numeric)`

		// Resolved rather than sub-selected inline so a schema that is behind
		// this binary says so — see glAccountID's own comment. Code 2100
		// arrives in migration 045 alongside this code path.
		expenseID, err := glAccountID(ctx, dbTx, "5000")
		if err != nil {
			return err
		}
		payableID, err := glAccountID(ctx, dbTx, "2100")
		if err != nil {
			return err
		}
		if _, err := dbTx.Exec(ctx, insertLine, journalID, expenseID, entry.CommissionAmount.String(), "0"); err != nil {
			return fmt.Errorf("db: insert commission expense line: %w", err)
		}
		if _, err := dbTx.Exec(ctx, insertLine, journalID, payableID, "0", entry.CommissionAmount.String()); err != nil {
			return fmt.Errorf("db: insert commission payable line: %w", err)
		}
		return nil
	})
}

// GetSubscriberFranchiseID reports which franchise (if any) a subscriber
// belongs to. Used by revenue.SettleCommissionForRecharge to decide whether
// a recharge owes a partner commission — a subscriber signed up directly
// (franchise_id IS NULL) never does.
func (s *RevenueStore) GetSubscriberFranchiseID(ctx context.Context, subscriberID int) (*int, error) {
	const q = `SELECT franchise_id FROM subscribers WHERE id = $1`

	var franchiseID *int
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(&franchiseID)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get franchise for subscriber %d: %w", subscriberID, err)
	}
	return franchiseID, nil
}

// ListFranchises returns franchise partners within a scope. A nil
// franchiseID means unrestricted (ISP-wide) visibility; a non-nil one
// returns at most that partner, so a franchise-scoped caller listing
// partners sees only their own.
//
// FR: FR-FRN-004
func (s *RevenueStore) ListFranchises(ctx context.Context, franchiseID *int) ([]revenue.FranchiseRecord, error) {
	// Same NULL-guarded-predicate shape as ListSubscribers below, and for
	// the same reason: one statement means one place the scope filter can
	// be forgotten.
	const q = `
		SELECT id, name, owner_name, mobile_number, commission_rate_pct::text, status, created_at
		FROM franchises
		WHERE ($1::int IS NULL OR id = $1::int)
		ORDER BY id`

	rows, err := s.pool.Query(ctx, q, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: list franchises: %w", err)
	}
	defer rows.Close()

	out := make([]revenue.FranchiseRecord, 0, 16)
	for rows.Next() {
		var f revenue.FranchiseRecord
		if err := rows.Scan(&f.ID, &f.Name, &f.OwnerName, &f.MobileNumber,
			&f.CommissionRatePct, &f.Status, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan franchise row: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate franchises: %w", err)
	}
	return out, nil
}

// CreateFranchise onboards a partner (FR-FRN-006).
func (s *RevenueStore) CreateFranchise(ctx context.Context, req revenue.CreateFranchiseRequest) (*revenue.FranchiseRecord, error) {
	const q = `
		INSERT INTO franchises (name, owner_name, mobile_number, commission_rate_pct, status)
		VALUES ($1, $2, $3, $4::numeric, 'active')
		RETURNING id, name, owner_name, mobile_number, commission_rate_pct::text, status, created_at`

	var f revenue.FranchiseRecord
	err := s.pool.QueryRow(ctx, q, req.Name, req.OwnerName, req.MobileNumber, req.CommissionRatePct).
		Scan(&f.ID, &f.Name, &f.OwnerName, &f.MobileNumber, &f.CommissionRatePct, &f.Status, &f.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("db: create franchise %q: %w", req.Name, err)
	}
	return &f, nil
}

// franchisePnLSQL aggregates one row per franchise.
//
// The two aggregates come from separate subqueries rather than a single join:
// joining subscribers and lco_ledger in one pass multiplies rows (a franchise
// with 3 subscribers and 4 recharges yields 12), which silently inflates both
// the subscriber count and the recharge totals. That is exactly the class of
// error a revenue report must not make.
//
// COALESCE throughout so a franchise with no subscribers and no recharges
// reports zeroes rather than being absent — an onboarded partner who has not
// billed anyone yet is a legitimate, reportable state.
const franchisePnLSQL = `
	SELECT f.id,
	       f.name,
	       f.status,
	       f.commission_rate_pct::text,
	       COALESCE(subs.cnt, 0)                    AS subscriber_count,
	       COALESCE(led.cnt, 0)                     AS recharge_count,
	       -- ::numeric(14,2) before ::text on every one of these, not just
	       -- ::text: COALESCE(numeric_column, 0) promotes the bare integer
	       -- literal 0 to numeric with no fixed scale, so an idle franchise
	       -- (nothing in lco_ledger, every COALESCE falls to its default)
	       -- rendered "0" while an active one rendered "500.00" — the same
	       -- column reporting a different decimal format depending on
	       -- whether the franchise has any recharges yet. Found by a test
	       -- seeding an onboarded-but-idle partner, not by reading the SQL.
	       COALESCE(led.total_recharges, 0)::numeric(14,2)::text AS total_recharges,
	       COALESCE(led.commission, 0)::numeric(14,2)::text      AS commission_earned,
	       (COALESCE(led.total_recharges, 0) - COALESCE(led.commission, 0))::numeric(14,2)::text AS net_to_isp
	  FROM franchises f
	  LEFT JOIN (
	      SELECT franchise_id, count(*) AS cnt
	        FROM subscribers
	       WHERE franchise_id IS NOT NULL
	       GROUP BY franchise_id
	  ) subs ON subs.franchise_id = f.id
	  LEFT JOIN (
	      SELECT franchise_id,
	             count(*)                  AS cnt,
	             sum(recharge_amount)      AS total_recharges,
	             sum(commission_amount)    AS commission
	        FROM lco_ledger
	       WHERE ($2::timestamptz IS NULL OR created_at >= $2::timestamptz)
	         AND ($3::timestamptz IS NULL OR created_at <  $3::timestamptz)
	       GROUP BY franchise_id
	  ) led ON led.franchise_id = f.id
	 WHERE ($1::int IS NULL OR f.id = $1::int)
	 ORDER BY f.id`

// GetFranchisePnL returns the P&L for one partner over an optional date
// window. Returns (nil, nil) when the franchise does not exist — the same
// not-found convention TicketStore.UpdateTicketAdmin uses, chosen because
// internal/db imports internal/api (not the reverse), so a sentinel error
// declared here could not be compared against in the HTTP handler.
//
// A missing franchise must not be reported as an all-zero row: "no such
// partner" and "onboarded but has billed nothing" are different answers.
//
// FR: FR-FRN-003
func (s *RevenueStore) GetFranchisePnL(ctx context.Context, franchiseID int, from, to *time.Time) (*revenue.FranchisePnL, error) {
	rows, err := s.listPnL(ctx, &franchiseID, from, to)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListConsolidatedPnL returns every partner's P&L plus the ISP-wide totals
// (FR-FRN-003 — "the parent ISP sees a consolidated P&L across all LCO
// partners", CRD-FRN-001).
func (s *RevenueStore) ListConsolidatedPnL(ctx context.Context, from, to *time.Time) (*revenue.ConsolidatedPnL, error) {
	partners, err := s.listPnL(ctx, nil, from, to)
	if err != nil {
		return nil, err
	}

	// Totals are summed in Go from the same decimal values the per-partner
	// rows report, not recomputed in a second query: two queries could
	// disagree if a recharge lands between them, and a consolidated total
	// that does not equal the sum of its parts is worse than no total.
	totalRecharges, commission, net := decimal.Zero, decimal.Zero, decimal.Zero
	for _, p := range partners {
		r, err := parseDecimal(p.TotalRecharges)
		if err != nil {
			return nil, err
		}
		c, err := parseDecimal(p.CommissionEarned)
		if err != nil {
			return nil, err
		}
		n, err := parseDecimal(p.NetToISP)
		if err != nil {
			return nil, err
		}
		totalRecharges = totalRecharges.Add(r)
		commission = commission.Add(c)
		net = net.Add(n)
	}

	return &revenue.ConsolidatedPnL{
		Partners:         partners,
		TotalRecharges:   totalRecharges.StringFixed(2),
		CommissionEarned: commission.StringFixed(2),
		NetToISP:         net.StringFixed(2),
	}, nil
}

func (s *RevenueStore) listPnL(ctx context.Context, franchiseID *int, from, to *time.Time) ([]revenue.FranchisePnL, error) {
	rows, err := s.pool.Query(ctx, franchisePnLSQL, franchiseID, from, to)
	if err != nil {
		return nil, fmt.Errorf("db: franchise P&L: %w", err)
	}
	defer rows.Close()

	out := make([]revenue.FranchisePnL, 0, 16)
	for rows.Next() {
		var p revenue.FranchisePnL
		if err := rows.Scan(&p.FranchiseID, &p.FranchiseName, &p.Status, &p.CommissionRatePct,
			&p.SubscriberCount, &p.RechargeCount, &p.TotalRecharges,
			&p.CommissionEarned, &p.NetToISP); err != nil {
			return nil, fmt.Errorf("db: scan franchise P&L row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate franchise P&L: %w", err)
	}
	return out, nil
}

// ListSubscribers returns subscribers within a franchise scope. A nil
// franchiseID means unrestricted (ISP-wide) visibility.
//
// FR: FR-FRN-001 | uses idx_franchise_subscribers
func (s *RevenueStore) ListSubscribers(ctx context.Context, franchiseID *int) ([]revenue.SubscriberRow, error) {
	// One statement with a NULL-guarded predicate rather than two: a second
	// query string is a second place for the scope filter to be forgotten.
	const q = `
		SELECT id, username, franchise_id
		FROM subscribers
		WHERE ($1::int IS NULL OR franchise_id = $1::int)
		ORDER BY id`

	rows, err := s.pool.Query(ctx, q, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: list subscribers: %w", err)
	}
	defer rows.Close()

	out := make([]revenue.SubscriberRow, 0, 32)
	for rows.Next() {
		var r revenue.SubscriberRow
		if err := rows.Scan(&r.ID, &r.Username, &r.FranchiseID); err != nil {
			return nil, fmt.Errorf("db: scan subscriber row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate subscribers: %w", err)
	}
	return out, nil
}

// ── FR-REV-003: collections ─────────────────────────────────────────────────

// GetCollectionsByDunningStage returns exposure per dunning stage.
//
// 'active' is excluded: a current subscriber owes nothing, and including
// them would put the entire book in a row labelled outstanding. Ordered by
// the ladder rather than alphabetically, so the screen reads in the
// direction a subscriber actually travels.
func (s *RevenueStore) GetCollectionsByDunningStage(ctx context.Context) ([]revenue.CollectionsStageRow, error) {
	// The plan price is what a lapsed subscriber must pay to become
	// current, which is the only defensible per-subscriber figure in a
	// prepaid model with no receivable to age - see the note in
	// internal/revenue/collections.go. LEFT JOIN so a subscriber whose
	// plan row is missing still appears in the count instead of silently
	// dropping out of a total an operator is reconciling against.
	const q = `
		SELECT s.dunning_state,
		       COUNT(*),
		       COALESCE(SUM(p.price), 0)
		  FROM subscribers s
		  LEFT JOIN plans p ON p.id = s.plan_id
		 WHERE s.dunning_state <> 'active'
		   AND s.status <> 'terminated'
		 GROUP BY s.dunning_state
		 ORDER BY CASE s.dunning_state
		            WHEN 'remind_7d'      THEN 1
		            WHEN 'remind_3d'      THEN 2
		            WHEN 'remind_1d'      THEN 3
		            WHEN 'grace_period'   THEN 4
		            WHEN 'soft_suspended' THEN 5
		            WHEN 'hard_suspended' THEN 6
		            ELSE 7
		          END`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: collections by dunning stage: %w", err)
	}
	defer rows.Close()

	var out []revenue.CollectionsStageRow
	for rows.Next() {
		var r revenue.CollectionsStageRow
		if err := rows.Scan(&r.DunningState, &r.Subscribers, &r.Outstanding); err != nil {
			return nil, fmt.Errorf("db: scan collections row: %w", err)
		}
		r.ServiceStopped = revenue.ServiceStoppedIn(r.DunningState)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMonthlyRecovery returns collections for the last n calendar months,
// most recent first.
//
// Credits only, and only those carrying a transaction token: a token means
// the money came through the payment gateway or was recorded as a cash
// receipt, which is what "collected" means here. Staff-issued goodwill
// credits (internal/api's adjustment path, counter-account
// adjustment_clearing) have no token and are deliberately excluded -
// counting them would let a collections figure be inflated by writing
// credits to oneself.
func (s *RevenueStore) GetMonthlyRecovery(ctx context.Context, months int) ([]revenue.RecoveryMonth, error) {
	if months <= 0 {
		months = 2
	}
	const q = `
		SELECT date_trunc('month', created_at) AS m,
		       COALESCE(SUM(amount), 0),
		       COUNT(DISTINCT subscriber_id)
		  FROM wallet_ledgers
		 WHERE entry_type = 'credit'
		   AND transaction_token IS NOT NULL
		   AND created_at >= date_trunc('month', NOW()) - make_interval(months => $1)
		 GROUP BY m
		 ORDER BY m DESC`

	rows, err := s.pool.Query(ctx, q, months-1)
	if err != nil {
		return nil, fmt.Errorf("db: monthly recovery: %w", err)
	}
	defer rows.Close()

	var out []revenue.RecoveryMonth
	for rows.Next() {
		var m revenue.RecoveryMonth
		if err := rows.Scan(&m.Month, &m.Collected, &m.Payers); err != nil {
			return nil, fmt.Errorf("db: scan recovery month: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
