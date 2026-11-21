package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/reporting"
)

// Reporting queries — FR-RPT-001, FR-RPT-003 | migration 032 | MDS §4.8.
//
// These read the views rather than rebuilding their logic in Go. Keeping the
// aggregation in SQL means the numbers a scheduled export produces and the
// numbers the dashboard shows come from one definition, which is the whole
// reason for having views at all.

// ReportingStore reads the reporting views.
type ReportingStore struct{ pool dbPool }

var _ reporting.Refresher = (*ReportingStore)(nil)

// RefreshTicketResolution rebuilds mv_ticket_resolution.
//
// Goes through refresh_reporting_views() rather than issuing REFRESH directly:
// PostgreSQL requires *ownership* of a materialised view to refresh it and has
// no narrower privilege, so the alternative would be making bss_app the owner
// and thereby also letting it drop the view. The SECURITY DEFINER function
// (migration 032) grants exactly the refresh and nothing else.
//
// The function refreshes CONCURRENTLY, which is what keeps a refresh from
// taking an ACCESS EXCLUSIVE lock and blocking every dashboard reading the
// view — that would surface as the reporting page hanging for the duration,
// precisely when somebody is looking at it.
func (s *ReportingStore) RefreshTicketResolution(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `SELECT refresh_reporting_views()`); err != nil {
		return fmt.Errorf("db: refresh reporting views: %w", err)
	}
	return nil
}

// PlanMix returns the current plan distribution (FR-RPT-001).
func (s *ReportingStore) PlanMix(ctx context.Context, franchiseID *int) ([]reporting.PlanMixRow, error) {
	const q = `
		SELECT plan_id, plan_name, price::text, franchise_id,
		       total_subscribers, active_subscribers, suspended_subscribers, mrr::text
		  FROM v_plan_mix
		 WHERE ($1::int IS NULL OR franchise_id = $1)
		 ORDER BY active_subscribers DESC, plan_name`

	rows, err := s.pool.Query(ctx, q, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: plan mix: %w", err)
	}
	defer rows.Close()

	out := []reporting.PlanMixRow{}
	for rows.Next() {
		var r reporting.PlanMixRow
		var price, mrr string
		if err := rows.Scan(&r.PlanID, &r.PlanName, &price, &r.FranchiseID,
			&r.TotalSubscribers, &r.ActiveSubscribers, &r.SuspendedSubscribers, &mrr); err != nil {
			return nil, fmt.Errorf("db: scan plan mix: %w", err)
		}
		if r.Price, err = parseDecimal(price); err != nil {
			return nil, err
		}
		if r.MRR, err = parseDecimal(mrr); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GrowthMonthly returns monthly growth and churn (FR-RPT-001).
func (s *ReportingStore) GrowthMonthly(ctx context.Context, months int, franchiseID *int) ([]reporting.GrowthRow, error) {
	const q = `
		SELECT month, franchise_id, plan_id,
		       new_connections, reactivations, churned, suspended, net_growth
		  FROM v_subscriber_growth_monthly
		 WHERE month >= date_trunc('month', NOW()) - ($1 * INTERVAL '1 month')
		   AND ($2::int IS NULL OR franchise_id = $2)
		 ORDER BY month DESC, plan_id`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: growth monthly: %w", err)
	}
	defer rows.Close()

	out := []reporting.GrowthRow{}
	for rows.Next() {
		var r reporting.GrowthRow
		if err := rows.Scan(&r.Month, &r.FranchiseID, &r.PlanID,
			&r.NewConnections, &r.Reactivations, &r.Churned, &r.Suspended, &r.NetGrowth); err != nil {
			return nil, fmt.Errorf("db: scan growth: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TicketResolution returns resolution metrics (FR-RPT-001).
func (s *ReportingStore) TicketResolution(ctx context.Context, months int, franchiseID *int) ([]reporting.TicketResolutionRow, error) {
	const q = `
		SELECT month, category, priority, franchise_id,
		       raised, resolved, reopens, median_resolution_hours, resolved_within_sla
		  FROM mv_ticket_resolution
		 WHERE month >= date_trunc('month', NOW()) - ($1 * INTERVAL '1 month')
		   AND ($2::int IS NULL OR franchise_id = $2)
		 ORDER BY month DESC, category, priority`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: ticket resolution: %w", err)
	}
	defer rows.Close()

	out := []reporting.TicketResolutionRow{}
	for rows.Next() {
		var r reporting.TicketResolutionRow
		if err := rows.Scan(&r.Month, &r.Category, &r.Priority, &r.FranchiseID,
			&r.Raised, &r.Resolved, &r.Reopens, &r.MedianResolutionHours, &r.ResolvedWithinSLA); err != nil {
			return nil, fmt.Errorf("db: scan ticket resolution: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FranchiseCollection returns per-franchise collection performance
// (FR-RPT-003; franchise is the reporting area — see MDS §4.8).
func (s *ReportingStore) FranchiseCollection(ctx context.Context, months int, franchiseID *int) ([]reporting.CollectionRow, error) {
	const q = `
		SELECT franchise_id, franchise_name, franchise_status, month,
		       billed::text, invoices_raised, collected::text, commission::text,
		       paying_subscribers, collection_rate_pct
		  FROM v_franchise_collection
		 WHERE month >= date_trunc('month', NOW()) - ($1 * INTERVAL '1 month')
		   AND ($2::int IS NULL OR franchise_id = $2)
		 ORDER BY month DESC, franchise_name`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: franchise collection: %w", err)
	}
	defer rows.Close()

	out := []reporting.CollectionRow{}
	for rows.Next() {
		var r reporting.CollectionRow
		var billed, collected, commission string
		if err := rows.Scan(&r.FranchiseID, &r.FranchiseName, &r.FranchiseStatus, &r.Month,
			&billed, &r.InvoicesRaised, &collected, &commission,
			&r.PayingSubscribers, &r.CollectionRatePct); err != nil {
			return nil, fmt.Errorf("db: scan franchise collection: %w", err)
		}
		var err error
		if r.Billed, err = parseDecimal(billed); err != nil {
			return nil, err
		}
		if r.Collected, err = parseDecimal(collected); err != nil {
			return nil, err
		}
		if r.Commission, err = parseDecimal(commission); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SubscriberRoster lists the subscribers in a group, or the whole base when
// groupID is nil — FR-GRP-001.
//
// The one report that reads the subscriber table rather than an aggregate
// view, which is what lets a group filter apply at all.
//
// Terminated subscribers are excluded, matching memberCount in groups.go and
// GroupMemberIDs. That agreement is the point: the console shows a group's
// size, the announcement fan-out reaches a set of people, and this export is
// the list an operator works from. If the three counted differently, whichever
// one an operator checked first would make the other two look broken.
func (s *ReportingStore) SubscriberRoster(ctx context.Context, groupID, franchiseID *int) ([]reporting.SubscriberRosterRow, error) {
	const q = `
		SELECT sub.id, sub.username, sub.status, p.name, sub.franchise_id,
		       sub.mobile_number, sub.created_at,
		       COALESCE(ARRAY(
		           SELECT g.name FROM subscriber_group_members m
		             JOIN subscriber_groups g ON g.id = m.group_id
		            WHERE m.subscriber_id = sub.id
		            ORDER BY lower(btrim(g.name))
		       ), '{}') AS groups
		  FROM subscribers sub
		  JOIN plans p ON p.id = sub.plan_id
		 WHERE sub.status <> 'terminated'
		   AND ($1::int IS NULL OR EXISTS (
		           SELECT 1 FROM subscriber_group_members m
		            WHERE m.subscriber_id = sub.id AND m.group_id = $1))
		   AND ($2::int IS NULL OR sub.franchise_id = $2)
		 ORDER BY sub.id`

	rows, err := s.pool.Query(ctx, q, groupID, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: subscriber roster: %w", err)
	}
	defer rows.Close()

	out := []reporting.SubscriberRosterRow{}
	for rows.Next() {
		var r reporting.SubscriberRosterRow
		if err := rows.Scan(&r.SubscriberID, &r.Username, &r.Status, &r.PlanName,
			&r.FranchiseID, &r.MobileNumber, &r.CreatedAt, &r.Groups); err != nil {
			return nil, fmt.Errorf("db: scan subscriber roster: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
