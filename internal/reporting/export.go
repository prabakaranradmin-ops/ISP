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

// GroupFilterable reports whether report honours Filter.GroupID.
func GroupFilterable(report string) bool { return report == ReportSubscribers }

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
