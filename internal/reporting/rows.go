package reporting

import (
	"time"

	"github.com/shopspring/decimal"
)

// Row types for the migration-032 reporting views — FR-RPT-001, FR-RPT-003.
//
// Money is decimal.Decimal throughout, never float64: these figures are read
// by an owner comparing them against a bank statement, and a rounding artefact
// in the third decimal place of an MRR total is the kind of discrepancy that
// costs an afternoon to explain.

// PlanMixRow is one plan's share of the base (FR-RPT-001).
type PlanMixRow struct {
	PlanID               int             `json:"plan_id"`
	PlanName             string          `json:"plan_name"`
	Price                decimal.Decimal `json:"price"`
	FranchiseID          *int            `json:"franchise_id,omitempty"`
	TotalSubscribers     int             `json:"total_subscribers"`
	ActiveSubscribers    int             `json:"active_subscribers"`
	SuspendedSubscribers int             `json:"suspended_subscribers"`
	// MRR counts active subscribers only — revenue from a suspended account
	// is revenue the business is not currently collecting.
	MRR decimal.Decimal `json:"mrr"`
}

// GrowthRow is one month's movement for a plan and franchise (FR-RPT-001).
type GrowthRow struct {
	Month          time.Time `json:"month"`
	FranchiseID    *int      `json:"franchise_id,omitempty"`
	PlanID         *int      `json:"plan_id,omitempty"`
	NewConnections int       `json:"new_connections"`
	Reactivations  int       `json:"reactivations"`
	Churned        int       `json:"churned"`
	// Suspended is reported beside churn, never inside it: a suspension is a
	// collections event and usually reverses.
	Suspended int `json:"suspended"`
	NetGrowth int `json:"net_growth"`
}

// TicketResolutionRow is one month's support performance (FR-RPT-001).
type TicketResolutionRow struct {
	Month       time.Time `json:"month"`
	Category    string    `json:"category"`
	Priority    string    `json:"priority"`
	FranchiseID *int      `json:"franchise_id,omitempty"`
	Raised      int       `json:"raised"`
	Resolved    int       `json:"resolved"`
	Reopens     int       `json:"reopens"`
	// Nil when nothing in the group was resolved. Rendering that as 0.0 hours
	// would report the fastest possible support for a month in which nobody
	// was helped at all.
	MedianResolutionHours *float64 `json:"median_resolution_hours"`
	ResolvedWithinSLA     int      `json:"resolved_within_sla"`
}

// CollectionRow is one franchise-month of collection performance (FR-RPT-003).
type CollectionRow struct {
	FranchiseID       int             `json:"franchise_id"`
	FranchiseName     string          `json:"franchise_name"`
	FranchiseStatus   string          `json:"franchise_status"`
	Month             time.Time       `json:"month"`
	Billed            decimal.Decimal `json:"billed"`
	InvoicesRaised    int             `json:"invoices_raised"`
	Collected         decimal.Decimal `json:"collected"`
	Commission        decimal.Decimal `json:"commission"`
	PayingSubscribers int             `json:"paying_subscribers"`
	// Nil when nothing was billed. A franchise that raised no invoices has no
	// collection rate, and reporting 0% would rank a new territory bottom of a
	// league table it has not joined yet.
	CollectionRatePct *float64 `json:"collection_rate_pct"`
}

// SubscriberRosterRow is one subscriber in a group roster (FR-GRP-001).
//
// The only report whose rows are subscribers rather than aggregates, which is
// what makes it the one a group can filter: the other four read pre-aggregated
// views in which no individual survives to be matched against a membership.
type SubscriberRosterRow struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	Status       string `json:"status"`
	PlanName     string `json:"plan_name"`
	FranchiseID  *int   `json:"franchise_id,omitempty"`
	MobileNumber string `json:"mobile_number"`
	// Groups names every group the subscriber is in, not only the one filtered
	// on. An operator exporting "Anna Nagar" to plan a cable replacement needs
	// to see that four of them are also in "Corporate" before scheduling the
	// outage.
	Groups    []string  `json:"groups"`
	CreatedAt time.Time `json:"created_at"`
}
