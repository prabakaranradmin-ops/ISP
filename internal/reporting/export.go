package reporting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Report generation shared by both delivery paths — FR-RPT-002 | MDS §4.8.
//
// One place that turns (report, filters) into bytes, so the CSV a user
// downloads now and the CSV a scheduled export delivers tonight are produced
// by the same code. Two generators would drift, and the drift would surface as
// an operator insisting the numbers changed.

// DefaultMonths bounds a time-series report when the caller names no window.
// Twelve gives a year-on-year comparison without scanning the whole history on
// every dashboard load.
const DefaultMonths = 12

// MaxMonths caps the window. Beyond this the response stops being a report and
// becomes a data export that should go through the asynchronous path.
const MaxMonths = 120

// Querier reads the reporting views. Satisfied by *db.ReportingStore.
type Querier interface {
	PlanMix(ctx context.Context, franchiseID *int) ([]PlanMixRow, error)
	GrowthMonthly(ctx context.Context, months int, franchiseID *int) ([]GrowthRow, error)
	TicketResolution(ctx context.Context, months int, franchiseID *int) ([]TicketResolutionRow, error)
	FranchiseCollection(ctx context.Context, months int, franchiseID *int) ([]CollectionRow, error)
	SubscriberRoster(ctx context.Context, groupID, franchiseID *int) ([]SubscriberRosterRow, error)

	// FR-RPT-004. Each takes the window its catalogue entry declares — days
	// for the forward-looking expiry list, months for the trailing ones, and
	// neither for the two point-in-time snapshots.
	ExpiringSubscribers(ctx context.Context, days int, franchiseID *int) ([]ExpiringRow, error)
	PlanChangeHistory(ctx context.Context, months int, franchiseID *int) ([]PlanChangeRow, error)
	PaymentsByMethod(ctx context.Context, months int, franchiseID *int) ([]PaymentMethodRow, error)
	CollectionByStaff(ctx context.Context, months int, franchiseID *int) ([]StaffCollectionRow, error)
	InvoiceRegister(ctx context.Context, months int, franchiseID *int) ([]InvoiceRegisterRow, error)
	TaxCharged(ctx context.Context, months int, franchiseID *int) ([]TaxRow, error)
	TicketTurnaround(ctx context.Context, months int, franchiseID *int) ([]TurnaroundRow, error)
	TopDataUsers(ctx context.Context, months, limit int, franchiseID *int) ([]DataUserRow, error)
	FUPExceeded(ctx context.Context, franchiseID *int) ([]FUPExceededRow, error)
}

// ErrGroupFilterUnsupported is returned when a group filter is asked of a
// report that cannot honour it — FR-GRP-001.
//
// Refused rather than ignored. The four aggregate reports read pre-aggregated
// views (and mv_ticket_resolution is materialised), so no individual
// subscriber survives in them to be matched against a membership. Dropping the
// filter silently would hand an operator a figure for the whole base under a
// heading naming one group, which is the kind of number that gets quoted in a
// meeting. An error makes the limit visible at the moment it bites.
var ErrGroupFilterUnsupported = errors.New(
	"reporting: this report cannot be filtered by subscriber group")

// GroupFilterable now lives in catalogue.go, derived from the report
// catalogue rather than restated here (FR-RPT-004).

// Filter narrows a report.
//
// FranchiseID is a pointer because nil and zero mean different things: nil is
// "every franchise", which only ISP-wide staff may ask for. Callers derive it
// from the token, never from a query parameter.
//
// GroupID is a pointer for the same reason, and only ReportSubscribers honours
// it — see ErrGroupFilterUnsupported.
type Filter struct {
	Months      int
	FranchiseID *int
	GroupID     *int
	// Days is the forward-looking window for the expiring-subscribers report
	// (FR-RPT-004), where "the next 7 days" is the question a renewal call
	// list answers and a trailing month window means nothing.
	Days int
	// Limit caps the top-data-users report. A "top users" list is a ranking,
	// so it needs a cut-off; without one it is just the whole base sorted.
	Limit int
}

// NormaliseMonths clamps a requested window into range.
func NormaliseMonths(months int) int {
	if months <= 0 {
		return DefaultMonths
	}
	if months > MaxMonths {
		return MaxMonths
	}
	return months
}

// WriteCSV renders report to w.
//
// The rows are fetched whole and then written, rather than streamed row by row
// from the database. The store's methods already return slices, and reshaping
// them into cursors for a report bounded at ten years of monthly aggregates —
// thousands of rows, not millions — would be complexity bought for nothing.
func WriteCSV(ctx context.Context, q Querier, report string, f Filter, w io.Writer) error {
	months := NormaliseMonths(f.Months)

	// Checked before the switch so an unsupported combination costs no query,
	// and so adding a report cannot accidentally inherit a silent drop.
	if f.GroupID != nil && !GroupFilterable(report) {
		return fmt.Errorf("%w: %s", ErrGroupFilterUnsupported, report)
	}

	switch report {
	case ReportSubscribers:
		// A roster is a point-in-time list of the base, like plan mix, so it
		// takes no month window.
		rows, err := q.SubscriberRoster(ctx, f.GroupID, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteSubscriberRosterCSV(w, rows)

	case ReportPlanMix:
		// Plan mix is a point-in-time snapshot of the base, so it takes no
		// month window — passing one would imply a history it does not have.
		rows, err := q.PlanMix(ctx, f.FranchiseID)
		if err != nil {
			return err
		}
		return WritePlanMixCSV(w, rows)

	case ReportGrowth:
		rows, err := q.GrowthMonthly(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteGrowthCSV(w, rows)

	case ReportTicketResolution:
		rows, err := q.TicketResolution(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteTicketResolutionCSV(w, rows)

	case ReportCollection:
		rows, err := q.FranchiseCollection(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteCollectionCSV(w, rows)

	// ── FR-RPT-004 ──────────────────────────────────────────────────────

	case ReportExpiring:
		rows, err := q.ExpiringSubscribers(ctx, NormaliseDays(f.Days), f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteExpiringCSV(w, rows)

	case ReportPlanChanges:
		rows, err := q.PlanChangeHistory(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WritePlanChangesCSV(w, rows)

	case ReportPaymentsByMethod:
		rows, err := q.PaymentsByMethod(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WritePaymentsByMethodCSV(w, rows)

	case ReportCollectionByStaff:
		rows, err := q.CollectionByStaff(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteCollectionByStaffCSV(w, rows)

	case ReportInvoiceRegister:
		rows, err := q.InvoiceRegister(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteInvoiceRegisterCSV(w, rows)

	case ReportTaxCharged:
		rows, err := q.TaxCharged(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteTaxChargedCSV(w, rows)

	case ReportTicketTurnaround:
		rows, err := q.TicketTurnaround(ctx, months, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteTurnaroundCSV(w, rows)

	case ReportTopDataUsers:
		rows, err := q.TopDataUsers(ctx, months, NormaliseLimit(f.Limit), f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteTopDataUsersCSV(w, rows)

	case ReportFUPExceeded:
		rows, err := q.FUPExceeded(ctx, f.FranchiseID)
		if err != nil {
			return err
		}
		return WriteFUPExceededCSV(w, rows)

	default:
		return fmt.Errorf("reporting: unknown report %q", report)
	}
}

// Filename builds the download name for a report.
//
// Dated, and scoped when the caller is franchise-bound, so a folder of
// downloads stays identifiable: "growth.csv" three times over is a filing
// problem the browser solves by appending (1) and (2).
func Filename(report string, f Filter, at time.Time) string {
	name := report + "-" + at.UTC().Format("2006-01-02")
	if f.FranchiseID != nil {
		name = fmt.Sprintf("%s-franchise-%d", name, *f.FranchiseID)
	}
	// The group too, so a folder of rosters for different groups does not
	// become five files called subscribers-2026-11-21(1..5).csv.
	if f.GroupID != nil {
		name = fmt.Sprintf("%s-group-%d", name, *f.GroupID)
	}
	return name + ".csv"
}

// Day-window bounds for the expiring-subscribers report.
const (
	// DefaultDays is a week: long enough to work a call list through, short
	// enough that the list is still the people about to lapse.
	DefaultDays = 7
	// MaxDays caps it at a quarter. Past that an "expiring" list is the
	// renewal schedule of the whole base.
	MaxDays = 90
)

// Top-users bounds.
const (
	DefaultLimit = 50
	MaxLimit     = 1000
)

// NormaliseDays clamps a requested forward window into range.
//
// Zero or negative falls back to the default rather than returning nobody: a
// hand-edited URL showing the usual week is recoverable, where an empty list
// reads as "nothing is expiring" and would stop a day's calls being made.
func NormaliseDays(days int) int {
	if days <= 0 {
		return DefaultDays
	}
	if days > MaxDays {
		return MaxDays
	}
	return days
}

// NormaliseLimit clamps a requested ranking size into range.
func NormaliseLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}
