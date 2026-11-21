package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/rs/zerolog/log"
)

// ReportingStore backs the Reports screen — the four general-analytics
// reports (plan mix, growth/churn, ticket resolution, franchise collection)
// that have been routed and CSV-exportable at /api/v1/reports/{report}
// since they were built, with no console screen to view them without
// calling the API directly.
//
// Redefined per package rather than sharing reporting.Querier, matching
// every other store interface in this file. Satisfied by *db.ReportingStore.
type ReportingStore interface {
	PlanMix(ctx context.Context, franchiseID *int) ([]reporting.PlanMixRow, error)
	GrowthMonthly(ctx context.Context, months int, franchiseID *int) ([]reporting.GrowthRow, error)
	TicketResolution(ctx context.Context, months int, franchiseID *int) ([]reporting.TicketResolutionRow, error)
	FranchiseCollection(ctx context.Context, months int, franchiseID *int) ([]reporting.CollectionRow, error)
	SubscriberRoster(ctx context.Context, groupID, franchiseID *int) ([]reporting.SubscriberRosterRow, error)
}

type reportsData struct {
	Active           string
	Months           int
	PlanMix          []reporting.PlanMixRow
	Growth           []reporting.GrowthRow
	TicketResolution []reporting.TicketResolutionRow
	Collection       []reporting.CollectionRow
	// Roster and the group fields belong to the subscribers report
	// (FR-GRP-001), the only one of the five with a row per subscriber and so
	// the only one a group can filter.
	Roster  []reporting.SubscriberRosterRow
	Groups  []revenue.SubscriberGroup
	GroupID int
	// GroupFilterable tells the screen whether to offer the group selector at
	// all, read from reporting rather than restated here, so the screen and
	// the exporter cannot disagree about which reports accept one.
	GroupFilterable bool
}

// Reports shows the four general-analytics reports. ISP-wide only (no
// franchise filter in this first pass — a franchise-scoped viewer would need
// their own restricted reach, tracked alongside CRD-EXP-005's other
// franchise-partner follow-up), and owner/billing only: this is financial
// and growth performance, the same reach Revenue already has.
func (h *Handler) Reports(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "reports")
	if !ok {
		return
	}
	d := h.page(s, "Reports", "reports")

	if h.reporting == nil {
		d.Error = "Reporting is not configured on this deployment."
		h.render(w, "reports", d)
		return
	}

	report := r.URL.Query().Get("report")
	if report == "" {
		report = reporting.ReportPlanMix
	}
	months := reporting.DefaultMonths
	if v := r.URL.Query().Get("months"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			months = reporting.NormaliseMonths(n)
		}
	}

	groupID := atoiOrZero(r.URL.Query().Get("group_id"))
	rd := reportsData{
		Active:          report,
		Months:          months,
		GroupFilterable: reporting.GroupFilterable(report),
	}
	if h.groups != nil {
		if gs, gerr := h.groups.ListSubscriberGroups(r.Context(), false); gerr == nil {
			rd.Groups = gs
		} else {
			log.Error().Err(gerr).Msg("staffui: list groups for reports failed")
		}
	}
	// Dropped rather than carried into a report that cannot honour it, and
	// said so. The exporter refuses the same combination outright; here the
	// operator is mid-navigation between reports, so keeping the chosen
	// report and losing the filter is the less surprising of the two.
	if groupID > 0 && !rd.GroupFilterable {
		d.Error = "That report reads pre-aggregated figures, in which individual subscribers " +
			"do not appear, so it cannot be filtered by group. The Subscribers report can."
		groupID = 0
	}
	rd.GroupID = groupID

	var err error
	switch report {
	case reporting.ReportSubscribers:
		var group *int
		if groupID > 0 {
			group = &groupID
		}
		rd.Roster, err = h.reporting.SubscriberRoster(r.Context(), group, nil)
	case reporting.ReportGrowth:
		rd.Growth, err = h.reporting.GrowthMonthly(r.Context(), months, nil)
	case reporting.ReportTicketResolution:
		rd.TicketResolution, err = h.reporting.TicketResolution(r.Context(), months, nil)
	case reporting.ReportCollection:
		rd.Collection, err = h.reporting.FranchiseCollection(r.Context(), months, nil)
	default:
		rd.Active = reporting.ReportPlanMix
		rd.PlanMix, err = h.reporting.PlanMix(r.Context(), nil)
	}
	if err != nil {
		log.Error().Err(err).Str("report", report).Msg("staffui: load report failed")
		d.Error = "Could not load that report."
		h.render(w, "reports", d)
		return
	}

	d.Data = rd
	h.render(w, "reports", d)
}

// ExportReportCSV handles GET /staff/reports/{report}.csv — FR-GRP-001.
//
// A console-side download, not a link to /api/v1/reports/{report}?format=csv.
// The API authenticates a Bearer JWT; a console operator has a session cookie,
// so the obvious link hands them a 401 — which is what the browser test that
// found this asserted, and what no unit test could have caught, since every
// layer below here was exercised with its own fake.
//
// The bytes come from reporting.WriteCSV, the same function the API and the
// scheduled exporter call, so the file an operator downloads here and the one
// a scheduled export delivers tonight stay identical rather than drifting
// into two formatters.
func (h *Handler) ExportReportCSV(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "reports")
	if !ok {
		return
	}
	if h.reporting == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Reporting is not configured on this deployment.")
		return
	}

	report := strings.TrimSuffix(r.PathValue("report"), ".csv")
	if !reporting.ValidReport(report) {
		h.renderError(w, r, s, http.StatusNotFound,
			"No such report. Available: "+strings.Join(reporting.Reports(), ", ")+".")
		return
	}

	f := reporting.Filter{Months: atoiOrZero(r.URL.Query().Get("months"))}
	if groupID := atoiOrZero(r.URL.Query().Get("group_id")); groupID > 0 {
		// Refused rather than dropped, matching the API and the screen — and
		// refused here, before any header is written, because once the 200
		// and the Content-Disposition are out the operator has a file named
		// after a group holding figures for the whole base.
		if !reporting.GroupFilterable(report) {
			h.renderError(w, r, s, http.StatusUnprocessableEntity,
				"That report reads pre-aggregated figures, in which individual subscribers do not "+
					"appear, so it cannot be filtered by group. The Subscribers report can.")
			return
		}
		f.GroupID = &groupID
	}

	// Headers before the body: once anything is written the status is fixed,
	// so a failure after this point cannot become an error page.
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", reporting.Filename(report, f, time.Now())))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	if err := reporting.WriteCSV(r.Context(), h.reporting, report, f, w); err != nil {
		// Mid-stream: the status is already sent, so this can only be logged.
		log.Error().Err(err).Str("report", report).Msg("staffui: report CSV stream failed mid-response")
	}
}
