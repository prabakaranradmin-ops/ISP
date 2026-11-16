package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/crm"
)

// CRMStore serves the lead pipeline. Satisfies api.LeadQuerier.
type CRMStore struct{ pool dbPool }

var _ api.LeadQuerier = (*CRMStore)(nil)

const leadColumns = `
	id, full_name, mobile_number, COALESCE(email, ''), source, status,
	franchise_id, COALESCE(assigned_to_username, ''), COALESCE(notes, ''),
	COALESCE(lost_reason, ''), converted_subscriber_id,
	created_at, updated_at, converted_at`

func scanLead(row interface{ Scan(dest ...any) error }) (*crm.Lead, error) {
	var l crm.Lead
	err := row.Scan(
		&l.ID, &l.FullName, &l.MobileNumber, &l.Email, &l.Source, &l.Status,
		&l.FranchiseID, &l.AssignedTo, &l.Notes, &l.LostReason, &l.ConvertedSubscriberID,
		&l.CreatedAt, &l.UpdatedAt, &l.ConvertedAt,
	)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// CreateLead persists a new prospect.
func (s *CRMStore) CreateLead(ctx context.Context, l crm.Lead) (*crm.Lead, error) {
	const q = `
		WITH ins AS (
			INSERT INTO leads (full_name, mobile_number, email, source, franchise_id, assigned_to_username, notes)
			VALUES ($1, $2, NULLIF($3,''), $4, $5, NULLIF($6,''), NULLIF($7,''))
			RETURNING *
		)
		SELECT ` + leadColumns + ` FROM ins`

	created, err := scanLead(s.pool.QueryRow(ctx, q,
		l.FullName, l.MobileNumber, l.Email, l.Source, l.FranchiseID, l.AssignedTo, l.Notes))
	if err != nil {
		return nil, fmt.Errorf("db: create lead: %w", err)
	}
	return created, nil
}

// GetLead loads one lead. A missing row returns (nil, nil).
func (s *CRMStore) GetLead(ctx context.Context, id int) (*crm.Lead, error) {
	const q = `SELECT ` + leadColumns + ` FROM leads WHERE id = $1`
	l, err := scanLead(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get lead %d: %w", id, err)
	}
	return l, nil
}

// ListLeads lists the pipeline, optionally filtered by status, source and
// franchise. The franchise filter is how an LCO's pipeline stays theirs —
// the handler derives that scope from the caller's token, never the query.
func (s *CRMStore) ListLeads(ctx context.Context, status, source *string, franchiseID *int) ([]crm.Lead, error) {
	const q = `
		SELECT ` + leadColumns + `
		FROM leads
		WHERE ($1::text IS NULL OR status = $1)
		  AND ($2::text IS NULL OR source = $2)
		  AND ($3::int  IS NULL OR franchise_id = $3)
		ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, q, status, source, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: list leads: %w", err)
	}
	defer rows.Close()

	var out []crm.Lead
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan lead: %w", err)
		}
		out = append(out, *l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate leads: %w", err)
	}
	return out, nil
}

// UpdateLead applies a partial pipeline update. Cannot reach 'converted':
// that status is only set by ConvertLead, which also creates the subscriber
// the chk_lead_converted_has_subscriber constraint requires.
func (s *CRMStore) UpdateLead(ctx context.Context, id int, status, assignedTo, notes, lostReason *string) (*crm.Lead, error) {
	const q = `
		WITH upd AS (
			UPDATE leads
			SET status               = COALESCE($2, status),
			    assigned_to_username = COALESCE($3, assigned_to_username),
			    notes                = COALESCE($4, notes),
			    lost_reason          = COALESCE($5, lost_reason)
			WHERE id = $1
			RETURNING *
		)
		SELECT ` + leadColumns + ` FROM upd`

	l, err := scanLead(s.pool.QueryRow(ctx, q, id, status, assignedTo, notes, lostReason))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update lead %d: %w", id, err)
	}
	return l, nil
}

// ConvertLead atomically claims the lead and creates the subscriber it
// becomes, in one transaction.
//
// The claim is a conditional UPDATE matching only a not-yet-converted row,
// which is what makes two staff converting the same prospect at the same
// moment resolve to exactly one subscriber (MDS §4.16). The loser gets
// (nil, nil) — the same "did not land" signal ClaimApprovalRequest uses —
// rather than an error, so the handler can tell it apart from a real
// failure and answer 409.
//
// Subscriber creation runs inside the same transaction rather than
// alongside it (unlike CreateSubscriber's best-effort KYC step): a
// half-written KYC row can be re-submitted, but a duplicate subscriber has
// already been handed a username, a CAF number and a bill.
//
// FR: FR-CRM-002 | MDS §4.16
func (s *CRMStore) ConvertLead(ctx context.Context, leadID int, sub api.SubscriberRecord, passwordHash string) (*crm.Lead, *api.SubscriberRecord, error) {
	const claimLead = `
		UPDATE leads
		SET status = 'converted', converted_subscriber_id = $2, converted_at = NOW()
		WHERE id = $1 AND status <> 'converted'
		RETURNING id`

	const readLead = `SELECT ` + leadColumns + ` FROM leads WHERE id = $1`

	// The ctx CTE attributes the new connection to the converting operator for
	// migration 031's capture trigger, and marks it as originating from a lead
	// conversion rather than a direct signup — the two are different enough
	// for a growth report to want them apart.
	const insertSubscriber = `
		WITH ctx AS (
			SELECT set_config('app.actor', $9, true)                     AS actor,
			       set_config('app.change_reason', 'lead_conversion', true) AS reason
		), ins AS (
			INSERT INTO subscribers (
				caf_number, username, password_hash, mobile_number, email,
				plan_id, franchise_id, status, dunning_state, wallet_balance,
				registered_state, kyc_status, plan_expiry
			)
			-- pending_payment, exactly as a direct sign-up (subscriberRecordFrom):
			-- payment precedes authorisation. This was 'active' with a NULL
			-- plan_expiry, which migration 048's
			-- chk_authorised_subscriber_is_billable rejects — every conversion
			-- failed from 048 until this was changed — and which, before 048,
			-- put a converted lead on the network free with nothing to bill
			-- them. The auto-renewal scanner activates them once funded.
			SELECT $1,$2,$3,$4,NULLIF($5,''),$6,$7,'pending_payment','active',0.00,$8,'pending',NULL
			  FROM ctx WHERE ctx.actor IS NOT NULL
			RETURNING *
		)
		SELECT ` + apiSubscriberColumns + ` FROM ins s`

	var (
		lead    *crm.Lead
		created *api.SubscriberRecord
	)

	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The subscriber is inserted first so its id is available for the
		// lead's converted_subscriber_id — the FK and
		// chk_lead_converted_has_subscriber both require a real one. If the
		// claim below then finds the lead already converted, the whole
		// transaction rolls back and this insert never happened.
		rec, err := scanAPISubscriber(tx.QueryRow(ctx, insertSubscriber,
			sub.CAFNumber, sub.Username, passwordHash, sub.MobileNumber, sub.Email,
			sub.PlanID, sub.FranchiseID, sub.RegisteredState, actorFromContext(ctx)))
		if err != nil {
			// Surfaced verbatim so api.isUniqueViolation can classify a
			// duplicate caf_number or username as 409 rather than 500.
			return fmt.Errorf("db: convert lead %d: create subscriber: %w", leadID, err)
		}

		var claimedID int
		if err := tx.QueryRow(ctx, claimLead, leadID, rec.ID).Scan(&claimedID); err != nil {
			if isNoRows(err) {
				return errLeadNotClaimed
			}
			return fmt.Errorf("db: convert lead %d: claim: %w", leadID, err)
		}

		lead, err = scanLead(tx.QueryRow(ctx, readLead, leadID))
		if err != nil {
			return fmt.Errorf("db: convert lead %d: read back: %w", leadID, err)
		}
		created = rec
		return nil
	})
	if errors.Is(err, errLeadNotClaimed) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return lead, created, nil
}

// errLeadNotClaimed is an internal sentinel used to roll the conversion
// transaction back when the lead was already converted, without surfacing
// as a caller-visible error. Rolling back is the point: it is what undoes
// the subscriber insert the transaction had already made.
var errLeadNotClaimed = errors.New("db: lead already converted")

// GetFunnel answers FR-CRM-003: pipeline counts by source and stage, plus
// the conversion rate that follows from them.
//
// Computed on demand rather than snapshotted like revenue_snapshots: this
// is a handful of grouped counts over one indexed table, not a nightly
// reconciliation whose cost is worth memoizing.
func (s *CRMStore) GetFunnel(ctx context.Context, franchiseID *int) (*crm.FunnelReport, error) {
	const q = `
		SELECT source, status, COUNT(*)
		FROM leads
		WHERE ($1::int IS NULL OR franchise_id = $1)
		GROUP BY source, status
		ORDER BY source, status`

	rows, err := s.pool.Query(ctx, q, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: lead funnel: %w", err)
	}
	defer rows.Close()

	report := &crm.FunnelReport{Stages: []crm.FunnelStage{}}
	for rows.Next() {
		var st crm.FunnelStage
		if err := rows.Scan(&st.Source, &st.Status, &st.LeadCount); err != nil {
			return nil, fmt.Errorf("db: scan funnel stage: %w", err)
		}
		report.Stages = append(report.Stages, st)
		report.TotalLeads += st.LeadCount
		if st.Status == crm.StatusConverted {
			report.ConvertedLeads += st.LeadCount
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate funnel: %w", err)
	}

	// Decimal rather than float: this is the number that ends up on a
	// slide, and 33.33 should read the same every time it is computed.
	rate := decimal.Zero
	if report.TotalLeads > 0 {
		rate = decimal.NewFromInt(int64(report.ConvertedLeads)).
			Div(decimal.NewFromInt(int64(report.TotalLeads))).
			Mul(decimal.NewFromInt(100)).Round(2)
	}
	report.ConversionRatePct = rate.StringFixed(2)
	return report, nil
}
