package staffui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/rs/zerolog/log"
)

// ReportingStore backs the Reports screen — the report catalogue
// (FR-RPT-004), satisfied by *db.ReportingStore.
//
// This one embeds reporting.Querier rather than restating its methods, unlike
// every other store interface in this file. The convention is set aside
// deliberately: the catalogue holds fourteen reports and grows, and a
// hand-copied list of fourteen signatures is a list that drifts — a method
// added to the Querier and forgotten here fails at the call site with
// "does not implement", which is a confusing place to learn about it. The
// reason the other interfaces are redefined (keeping the console's needs
// narrower than a package's whole API) does not apply here, because the
// console serves the whole catalogue.
type ReportingStore interface {
	reporting.Querier
}

type reportsData struct {
	// Active is the report being shown, empty when the catalogue index is.
	Active string
	// Def is Active's catalogue entry, so the screen can show its title and
	// summary without restating either.
	Def reporting.ReportDef
	// Areas and Query drive the index: the cards grouped by area, and the
	// filter box's current text.
	Areas  []reportArea
	Query  string
	Months int
	Days   int
	Limit  int

	PlanMix          []reporting.PlanMixRow
	Growth           []reporting.GrowthRow
	TicketResolution []reporting.TicketResolutionRow
	Collection       []reporting.CollectionRow
	// Roster and the group fields belong to the subscribers report
	// (FR-GRP-001), one of the two with a row per subscriber and the only one
	// a group can filter.
	Roster  []reporting.SubscriberRosterRow
	Groups  []revenue.SubscriberGroup
	GroupID int
	// GroupFilterable tells the screen whether to offer the group selector at
	// all, read from reporting rather than restated here, so the screen and
	// the exporter cannot disagree about which reports accept one.
	GroupFilterable bool

	// FR-RPT-004.
	Expiring        []reporting.ExpiringRow
	PlanChanges     []reporting.PlanChangeRow
	PaymentMethods  []reporting.PaymentMethodRow
	StaffCollection []reporting.StaffCollectionRow
	InvoiceRegister []reporting.InvoiceRegisterRow
	Tax             []reporting.TaxRow
	Turnaround      []reporting.TurnaroundRow
	DataUsers       []reporting.DataUserRow
	FUPExceeded     []reporting.FUPExceededRow
}

// reportArea is one heading on the catalogue index with its reports.
type reportArea struct {
	Name    string
	Reports []reporting.ReportDef
}

// catalogueFor builds the index, keeping only the reports matching the filter
// box and dropping an area left with nothing — an empty heading reads as a
// section that failed to load.
func catalogueFor(query string) []reportArea {
	var out []reportArea
	for _, area := range reporting.Areas() {
		var keep []reporting.ReportDef
		for _, d := range reporting.InArea(area) {
			if d.Matches(query) {
				keep = append(keep, d)
			}
		}
		if len(keep) > 0 {
			out = append(out, reportArea{Name: area, Reports: keep})
		}
	}
	return out
}

// Reports shows the report catalogue, or one report — FR-RPT-004.
//
// ISP-wide only (no franchise filter in this pass — a franchise-scoped viewer
// would need their own restricted reach, tracked alongside CRD-EXP-005's
// other franchise-partner follow-up), and owner/billing only: this is
// financial and growth performance, the same reach Revenue already has.
//
// With no ?report= the catalogue index is shown rather than a default report.
// Fourteen reports is too many for a tab bar, and an index that opens on one
// of them makes the other thirteen look like they do not exist.
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

	q := r.URL.Query()
	report := q.Get("report")
	rd := reportsData{
		Query:  strings.TrimSpace(q.Get("q")),
		Months: reporting.NormaliseMonths(atoiOrZero(q.Get("months"))),
		Days:   reporting.NormaliseDays(atoiOrZero(q.Get("days"))),
		Limit:  reporting.NormaliseLimit(atoiOrZero(q.Get("limit"))),
	}
	rd.Areas = catalogueFor(rd.Query)

	// The index.
	if report == "" {
		if len(rd.Areas) == 0 && rd.Query != "" {
			d.Message = "No report matches " + rd.Query + "."
		}
		d.Data = rd
		h.render(w, "reports", d)
		return
	}

	def, known := reporting.Lookup(report)
	if !known {
		// Named rather than silently substituted. An earlier version of this
		// screen fell back to plan mix, which would show one report's figures
		// under another report's name on any typo or stale bookmark.
		h.renderError(w, r, s, http.StatusNotFound,
			"No report called "+report+". Pick one from the catalogue.")
		return
	}
	rd.Active, rd.Def = report, def
	rd.GroupFilterable = def.Group

	groupID := atoiOrZero(q.Get("group_id"))
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

	ctx := r.Context()
	months, days, limit := rd.Months, rd.Days, rd.Limit
	var err error
	switch report {
	case reporting.ReportPlanMix:
		rd.PlanMix, err = h.reporting.PlanMix(ctx, nil)
	case reporting.ReportGrowth:
		rd.Growth, err = h.reporting.GrowthMonthly(ctx, months, nil)
	case reporting.ReportTicketResolution:
		rd.TicketResolution, err = h.reporting.TicketResolution(ctx, months, nil)
	case reporting.ReportCollection:
		rd.Collection, err = h.reporting.FranchiseCollection(ctx, months, nil)
	case reporting.ReportSubscribers:
		var group *int
		if groupID > 0 {
			group = &groupID
		}
		rd.Roster, err = h.reporting.SubscriberRoster(ctx, group, nil)
	case reporting.ReportExpiring:
		rd.Expiring, err = h.reporting.ExpiringSubscribers(ctx, days, nil)
	case reporting.ReportPlanChanges:
		rd.PlanChanges, err = h.reporting.PlanChangeHistory(ctx, months, nil)
	case reporting.ReportPaymentsByMethod:
		rd.PaymentMethods, err = h.reporting.PaymentsByMethod(ctx, months, nil)
	case reporting.ReportCollectionByStaff:
		rd.StaffCollection, err = h.reporting.CollectionByStaff(ctx, months, nil)
	case reporting.ReportInvoiceRegister:
		rd.InvoiceRegister, err = h.reporting.InvoiceRegister(ctx, months, nil)
	case reporting.ReportTaxCharged:
		rd.Tax, err = h.reporting.TaxCharged(ctx, months, nil)
	case reporting.ReportTicketTurnaround:
		rd.Turnaround, err = h.reporting.TicketTurnaround(ctx, months, nil)
	case reporting.ReportTopDataUsers:
		rd.DataUsers, err = h.reporting.TopDataUsers(ctx, months, limit, nil)
	case reporting.ReportFUPExceeded:
		rd.FUPExceeded, err = h.reporting.FUPExceeded(ctx, nil)
	default:
		// Reachable only by adding a report to the catalogue and not here.
		// Refused rather than rendered empty: a report showing a blank table
		// is indistinguishable from one with nothing to show, and
		// TestFR_RPT_004_EveryCatalogueEntryHasAConsoleView fails on it.
		h.renderError(w, r, s, http.StatusNotImplemented,
			"The "+def.Title+" report has no console view yet. Download it as CSV instead.")
		return
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
