package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/rs/zerolog/log"
)

// Report export and scheduling — FR-RPT-002 | MDS §4.8.
//
// The views from migration 032 (FR-RPT-001, FR-RPT-003) had no HTTP surface at
// all: the numbers existed and nothing served them. This adds both ways to get
// at them — a synchronous CSV or JSON download for a dashboard, and a queued
// export for the reports too slow or too large to hold a connection open for.
//
// Every read is franchise-scoped from the caller's own token, never from a
// query parameter, so a partner cannot widen their view by editing a URL.

// ReportQuerier reads the reporting views. Satisfied by *db.ReportingStore.
type ReportQuerier interface {
	reporting.Querier
}

// GetReport handles GET /api/v1/reports/{report}.
//
// format=csv streams a download; anything else returns JSON. CSV is the
// default for an explicit ?format=csv rather than by content negotiation,
// because the common caller is a browser link and an Accept header is not
// something an operator can put in one.
func (h *Handler) GetReport(w http.ResponseWriter, r *http.Request) {
	if h.reports == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "reporting is not configured")
		return
	}

	report := r.PathValue("report")
	if !reporting.ValidReport(report) {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND",
			"unknown report; available: "+strings.Join(reporting.Reports(), ", "))
		return
	}

	filter, ok := h.reportFilter(w, r)
	if !ok {
		return
	}

	// Refused here, before any header is written. WriteCSV would refuse it too,
	// but by then the 200 is committed and the caller would receive an empty
	// attachment — so the check has to happen on this side of the headers.
	if filter.GroupID != nil && !reporting.GroupFilterable(report) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			report+" cannot be filtered by subscriber group; it reads pre-aggregated "+
				"figures in which individual subscribers do not appear. Use the "+
				reporting.ReportSubscribers+" report for a group roster.")
		return
	}

	if r.URL.Query().Get("format") != "csv" {
		h.writeReportJSON(w, r, report, filter)
		return
	}

	// Headers before the first row: once WriteCSV starts writing, the status is
	// committed and a mid-stream failure can only be logged, not reported. That
	// is inherent to streaming, and the alternative — buffering a large report
	// to keep the option of a clean 500 — costs more than it saves here.
	filename := reporting.Filename(report, filter, time.Now())
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	if err := reporting.WriteCSV(r.Context(), h.reports, report, filter, w); err != nil {
		log.Error().Err(err).Str("report", report).Msg("api: report CSV stream failed mid-response")
	}
}

// writeReportJSON serves the same data as the CSV path, for API clients.
func (h *Handler) writeReportJSON(w http.ResponseWriter, r *http.Request, report string, f reporting.Filter) {
	months := reporting.NormaliseMonths(f.Months)
	ctx := r.Context()

	var (
		rows any
		err  error
	)
	switch report {
	case reporting.ReportPlanMix:
		rows, err = h.reports.PlanMix(ctx, f.FranchiseID)
	case reporting.ReportGrowth:
		rows, err = h.reports.GrowthMonthly(ctx, months, f.FranchiseID)
	case reporting.ReportTicketResolution:
		rows, err = h.reports.TicketResolution(ctx, months, f.FranchiseID)
	case reporting.ReportCollection:
		rows, err = h.reports.FranchiseCollection(ctx, months, f.FranchiseID)
	case reporting.ReportSubscribers:
		rows, err = h.reports.SubscriberRoster(ctx, f.GroupID, f.FranchiseID)

	// FR-RPT-004. Every catalogue entry needs a case here as well as in
	// WriteCSV; TestFR_RPT_004_EveryCatalogueEntryIsServedByEveryPath holds
	// the two lists together.
	case reporting.ReportExpiring:
		rows, err = h.reports.ExpiringSubscribers(ctx, reporting.NormaliseDays(f.Days), f.FranchiseID)
	case reporting.ReportPlanChanges:
		rows, err = h.reports.PlanChangeHistory(ctx, months, f.FranchiseID)
	case reporting.ReportPaymentsByMethod:
		rows, err = h.reports.PaymentsByMethod(ctx, months, f.FranchiseID)
	case reporting.ReportCollectionByStaff:
		rows, err = h.reports.CollectionByStaff(ctx, months, f.FranchiseID)
	case reporting.ReportInvoiceRegister:
		rows, err = h.reports.InvoiceRegister(ctx, months, f.FranchiseID)
	case reporting.ReportTaxCharged:
		rows, err = h.reports.TaxCharged(ctx, months, f.FranchiseID)
	case reporting.ReportTicketTurnaround:
		rows, err = h.reports.TicketTurnaround(ctx, months, f.FranchiseID)
	case reporting.ReportTopDataUsers:
		rows, err = h.reports.TopDataUsers(ctx, months, reporting.NormaliseLimit(f.Limit), f.FranchiseID)
	case reporting.ReportFUPExceeded:
		rows, err = h.reports.FUPExceeded(ctx, f.FranchiseID)

	default:
		// Unreachable while ValidReport guards the route, and here anyway: the
		// original switch had no default, so adding a report to the catalogue
		// without a case here would have served {"rows": null} with a 200 — a
		// report that silently returns nothing is worse than one that 404s.
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND",
			"report "+report+" has no JSON representation")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "report query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report, "rows": rows})
}

// reportFilter builds the filter from the query string and the caller's scope,
// writing the error response itself when the caller may not be served.
func (h *Handler) reportFilter(w http.ResponseWriter, r *http.Request) (reporting.Filter, bool) {
	scope, ok := callerFranchiseScope(r)
	if !ok {
		// A franchise-scoped role whose token carries no franchise_id. Refused
		// rather than treated as ISP-wide, matching every other scoped route:
		// defaulting a missing claim to "everything" turns a misissued token
		// into a cross-partner data leak.
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise scope")
		return reporting.Filter{}, false
	}

	months, err := strconv.Atoi(r.URL.Query().Get("months"))
	if err != nil {
		months = 0 // absent or unparseable falls back to the default window
	}

	// ISP-wide staff may narrow to one franchise explicitly. A franchise-scoped
	// caller cannot: their scope already came from the token, and honouring the
	// parameter would let them ask about somebody else.
	if scope == nil {
		if raw := r.URL.Query().Get("franchise_id"); raw != "" {
			id, convErr := strconv.Atoi(raw)
			if convErr != nil || id <= 0 {
				writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "franchise_id must be a positive integer")
				return reporting.Filter{}, false
			}
			scope = &id
		}
	}

	// A group narrows the roster report — FR-GRP-001. Unlike franchise_id this
	// is open to every reporting role: a group is a label an operator put on
	// their own customers, not a partition between partners, and a
	// franchise-scoped caller still only sees their own rows because the
	// franchise predicate applies as well.
	var groupID *int
	if raw := r.URL.Query().Get("group_id"); raw != "" {
		id, convErr := strconv.Atoi(raw)
		if convErr != nil || id <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "group_id must be a positive integer")
			return reporting.Filter{}, false
		}
		groupID = &id
	}

	// The two windows that belong to one report each — FR-RPT-004. Both are
	// clamped rather than refused: an out-of-range window is a hand-edited
	// URL, and the usual week of expiries is more useful than an error.
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil {
		days = 0
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil {
		limit = 0
	}

	return reporting.Filter{
		Months: months, FranchiseID: scope, GroupID: groupID,
		Days: days, Limit: limit,
	}, true
}

// ── Scheduled export ────────────────────────────────────────────────────────

type exportRequest struct {
	Months int `json:"months,omitempty"`
	// FranchiseID is accepted only from ISP-wide staff; a scoped caller's own
	// franchise always wins, whatever the body says.
	FranchiseID *int `json:"franchise_id,omitempty"`
	// Days, Limit and GroupID belong to one report each (FR-RPT-004,
	// FR-GRP-001). Accepted here so a schedule can reproduce what the
	// synchronous download does; without them "export the next 30 days of
	// expiries" would quietly deliver seven.
	Days    int  `json:"days,omitempty"`
	Limit   int  `json:"limit,omitempty"`
	GroupID *int `json:"group_id,omitempty"`
}

// RequestReportExport handles POST /api/v1/reports/{report}/export.
//
// Returns 202 with an export id. The work happens on the reports queue and is
// delivered into archival storage, so the result outlives the request and
// carries a checksum and a retention date rather than being a file someone has
// to remember to clean up.
func (h *Handler) RequestReportExport(w http.ResponseWriter, r *http.Request) {
	if h.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "the export queue is not configured")
		return
	}

	report := r.PathValue("report")
	if !reporting.ValidReport(report) {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND",
			"unknown report; available: "+strings.Join(reporting.Reports(), ", "))
		return
	}

	var req exportRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
			return
		}
	}

	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise scope")
		return
	}
	if scope == nil && req.FranchiseID != nil {
		if *req.FranchiseID <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "franchise_id must be a positive integer")
			return
		}
		scope = req.FranchiseID
	}

	// The export id doubles as the archive row's entity_id. Time-based rather
	// than a sequence because exports have no table of their own; seconds
	// resolution is enough for a human-initiated action and keeps the id short
	// enough to quote over the phone.
	exportID := int(time.Now().Unix())

	// Refused here as well as on the worker, so the caller is told now rather
	// than finding an export that never arrived.
	if req.GroupID != nil {
		if *req.GroupID <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				"group_id must be a positive integer")
			return
		}
		if !reporting.GroupFilterable(report) {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				report+" cannot be filtered by subscriber group; use the "+
					reporting.ReportSubscribers+" report")
			return
		}
	}

	task, err := reporting.NewExportTask(reporting.ExportPayload{
		Report:      report,
		Months:      req.Months,
		Days:        req.Days,
		Limit:       req.Limit,
		GroupID:     req.GroupID,
		FranchiseID: scope,
		RequestedBy: middleware.SubjectFromContext(r.Context()),
		EntityID:    exportID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not queue the export")
		return
	}
	if _, err := h.tasks.Enqueue(task); err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not queue the export")
		return
	}

	middleware.Audit(r.Context(), "report.export_requested", report, map[string]any{
		"export_id": exportID, "months": req.Months, "franchise_id": scope,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"export_id": exportID,
		"report":    report,
		"status":    "queued",
		"poll":      fmt.Sprintf("/api/v1/reports/exports/%d", exportID),
	})
}

// ArchiveLookup finds a delivered export. Satisfied by *db.ArchiveStore.
type ArchiveLookup interface {
	GetArchive(ctx context.Context, docKind string, entityID int) (*archive.Record, error)
}

// GetReportExport handles GET /api/v1/reports/exports/{id} — where a queued
// export ended up, once it has.
func (h *Handler) GetReportExport(w http.ResponseWriter, r *http.Request) {
	if h.archives == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "archival storage is not configured")
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "id must be numeric")
		return
	}

	rec, err := h.archives.GetArchive(r.Context(), archive.KindReport, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "export lookup failed")
		return
	}
	if rec == nil {
		// Still running, failed, or never existed — indistinguishable from here,
		// and saying so is more honest than inventing a status. The queue's own
		// dead-letter alerting covers the failure case.
		writeJSON(w, http.StatusOK, map[string]any{
			"export_id": id,
			"status":    "pending",
			"detail":    "not yet delivered; a failed export is reported through the dead-letter queue",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"export_id": id,
		"status":    "delivered",
		"archive":   rec,
	})
}
