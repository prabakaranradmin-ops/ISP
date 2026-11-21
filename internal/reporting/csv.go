package reporting

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// CSV export of the migration-032 reporting views — FR-RPT-002 | MDS §4.8.
//
// Written straight to an io.Writer rather than built into a string: a growth
// report over a few years of a large base is tens of thousands of rows, and
// buffering it whole costs memory on the API process for no benefit when the
// client is a browser download that starts rendering immediately.
//
// Every formatter here is shared by both delivery paths — the synchronous
// endpoint and the scheduled export worker produce byte-identical files,
// because a scheduled report that differs from the one on screen is a support
// ticket waiting to happen.

// Report names the exportable views. These strings appear in URLs and in
// stored export filenames, so they are kebab-case rather than the raw view
// names.
const (
	ReportPlanMix          = "plan-mix"
	ReportGrowth           = "growth"
	ReportTicketResolution = "ticket-resolution"
	ReportCollection       = "collection"
	// ReportSubscribers is the subscriber roster — FR-GRP-001. The only
	// report with one row per subscriber, and so the only one a group can
	// filter.
	ReportSubscribers = "subscribers"

	// The reports added by FR-RPT-004. Descriptions, areas and which filters
	// apply live in catalogue.go; these are only the names on the wire.
	ReportExpiring          = "expiring-subscribers"
	ReportPlanChanges       = "plan-change-history"
	ReportPaymentsByMethod  = "payments-by-method"
	ReportCollectionByStaff = "collection-by-staff"
	ReportInvoiceRegister   = "invoice-register"
	ReportTaxCharged        = "tax-charged"
	ReportTicketTurnaround  = "ticket-turnaround"
	ReportTopDataUsers      = "top-data-users"
	ReportFUPExceeded       = "fup-exceeded"
)

// ── Formatting helpers ──────────────────────────────────────────────────────

// month renders a reporting period as YYYY-MM.
//
// Deliberately not the full timestamp the view returns: these rows are the
// first of a month at midnight UTC, and printing "2026-08-01T00:00:00Z" in a
// spreadsheet column invites the reader to wonder what happened at midnight.
func month(t time.Time) string { return t.UTC().Format("2006-01") }

// optInt renders a nullable franchise or plan id. Empty, not "0" — a zero id
// would read as a real franchise and sort alongside them.
func optInt(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

// optFloat renders a nullable rate or duration to two decimals.
//
// Empty for nil, for the reason the row types already document: a median
// resolution time of "0.0" for a month in which nothing was resolved claims
// the fastest possible support, and a collection rate of "0.0" ranks a
// territory that raised no invoices at the bottom of a league table it has not
// joined.
func optFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

// ── Encoders ────────────────────────────────────────────────────────────────

// WritePlanMixCSV streams the plan-mix report.
func WritePlanMixCSV(w io.Writer, rows []PlanMixRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"plan_id", "plan_name", "price", "franchise_id",
		"total_subscribers", "active_subscribers", "suspended_subscribers", "mrr",
	}); err != nil {
		return fmt.Errorf("reporting: write plan mix header: %w", err)
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			strconv.Itoa(r.PlanID), r.PlanName, r.Price.StringFixed(2), optInt(r.FranchiseID),
			strconv.Itoa(r.TotalSubscribers), strconv.Itoa(r.ActiveSubscribers),
			strconv.Itoa(r.SuspendedSubscribers), r.MRR.StringFixed(2),
		}); err != nil {
			return fmt.Errorf("reporting: write plan mix row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteGrowthCSV streams the subscriber-growth report.
func WriteGrowthCSV(w io.Writer, rows []GrowthRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"month", "franchise_id", "plan_id",
		"new_connections", "reactivations", "churned", "suspended", "net_growth",
	}); err != nil {
		return fmt.Errorf("reporting: write growth header: %w", err)
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			month(r.Month), optInt(r.FranchiseID), optInt(r.PlanID),
			strconv.Itoa(r.NewConnections), strconv.Itoa(r.Reactivations),
			strconv.Itoa(r.Churned), strconv.Itoa(r.Suspended), strconv.Itoa(r.NetGrowth),
		}); err != nil {
			return fmt.Errorf("reporting: write growth row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteTicketResolutionCSV streams the support-performance report.
func WriteTicketResolutionCSV(w io.Writer, rows []TicketResolutionRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"month", "category", "priority", "franchise_id",
		"raised", "resolved", "reopens", "median_resolution_hours", "resolved_within_sla",
	}); err != nil {
		return fmt.Errorf("reporting: write ticket resolution header: %w", err)
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			month(r.Month), r.Category, r.Priority, optInt(r.FranchiseID),
			strconv.Itoa(r.Raised), strconv.Itoa(r.Resolved), strconv.Itoa(r.Reopens),
			optFloat(r.MedianResolutionHours), strconv.Itoa(r.ResolvedWithinSLA),
		}); err != nil {
			return fmt.Errorf("reporting: write ticket resolution row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteCollectionCSV streams the franchise-collection report.
func WriteCollectionCSV(w io.Writer, rows []CollectionRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"franchise_id", "franchise_name", "franchise_status", "month",
		"billed", "invoices_raised", "collected", "commission",
		"paying_subscribers", "collection_rate_pct",
	}); err != nil {
		return fmt.Errorf("reporting: write collection header: %w", err)
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			strconv.Itoa(r.FranchiseID), r.FranchiseName, r.FranchiseStatus, month(r.Month),
			r.Billed.StringFixed(2), strconv.Itoa(r.InvoicesRaised),
			r.Collected.StringFixed(2), r.Commission.StringFixed(2),
			strconv.Itoa(r.PayingSubscribers), optFloat(r.CollectionRatePct),
		}); err != nil {
			return fmt.Errorf("reporting: write collection row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteSubscriberRosterCSV streams the subscriber roster — FR-GRP-001.
//
// Mobile number but no email or address: a roster is used to work a list — ring
// a block of customers before a cable replacement, check who a promotion
// reached — and one contact column serves that. Widening it would turn a
// routine export into a bulk extract of contact details (DPDP
// purpose-limitation), which the subscriber's own page already serves one at a
// time.
func WriteSubscriberRosterCSV(w io.Writer, rows []SubscriberRosterRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"subscriber_id", "username", "status", "plan_name", "franchise_id",
		"mobile_number", "groups", "created_at",
	}); err != nil {
		return fmt.Errorf("reporting: write subscriber roster header: %w", err)
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			strconv.Itoa(r.SubscriberID), r.Username, r.Status, r.PlanName,
			optInt(r.FranchiseID), r.MobileNumber,
			// Semicolons, not commas: a comma inside a field is legal CSV but
			// the quoting it forces is what spreadsheets most often get wrong
			// on import, and a group list splitting into columns silently
			// shifts every column after it.
			strings.Join(r.Groups, "; "),
			r.CreatedAt.UTC().Format(time.RFC3339),
		}); err != nil {
			return fmt.Errorf("reporting: write subscriber roster row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}
