package staffui

import (
	"context"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/reporting"
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
}

type reportsData struct {
	Active           string
	Months           int
	PlanMix          []reporting.PlanMixRow
	Growth           []reporting.GrowthRow
	TicketResolution []reporting.TicketResolutionRow
	Collection       []reporting.CollectionRow
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

	rd := reportsData{Active: report, Months: months}
	var err error
	switch report {
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
