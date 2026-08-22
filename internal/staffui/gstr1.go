package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/rs/zerolog/log"
)

// GSTR-1 export — FR-BIL-006 | CRD §1.3.
//
// Served from the Billing section as a file download rather than a screen:
// the output is a month of invoices for an accountant to open in a
// spreadsheet, not something to read in a browser.

// GSTR1Store supplies the invoices a return is built from.
type GSTR1Store interface {
	ListInvoicesForGSTR1(ctx context.Context, year int, month time.Month) ([]billing.InvoiceRow, error)
}

// GSTR1Export handles GET /staff/billing/gstr1?period=YYYY-MM.
func (h *Handler) GSTR1Export(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "billing")
	if !ok {
		return
	}
	if h.gstr1 == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"GST reporting is not configured on this deployment.")
		return
	}

	year, month, err := parsePeriod(r.URL.Query().Get("period"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, err.Error())
		return
	}

	rows, err := h.gstr1.ListInvoicesForGSTR1(r.Context(), year, month)
	if err != nil {
		log.Error().Err(err).Int("year", year).Str("month", month.String()).
			Msg("staffui: GSTR-1 invoice load failed")
		h.renderError(w, r, s, http.StatusInternalServerError,
			"Could not read invoices for that period.")
		return
	}

	ret := billing.BuildReturn(
		billing.Period{Year: year, Month: month}, h.gstSupplier, rows)

	// Headers before the body: once WriteAccountantCSV starts writing, the
	// status is already 200 and an error can no longer be reported as one.
	filename := fmt.Sprintf("gstr1-%04d-%02d.csv", year, int(month))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	if err := billing.WriteAccountantCSV(w, ret); err != nil {
		// Nothing useful can be sent to the browser at this point - the
		// response is part-written - so this exists to make a truncated
		// download diagnosable rather than silent.
		log.Error().Err(err).Str("period", ret.Period.String()).
			Msg("staffui: GSTR-1 CSV write failed mid-response; the download will be truncated")
	}
}

// parsePeriod reads a YYYY-MM period, defaulting to last month.
//
// Last month rather than this one: a return is filed for a completed
// period, and an export of a month still in progress is a partial return
// that looks like a whole one.
func parsePeriod(raw string) (int, time.Month, error) {
	if strings.TrimSpace(raw) == "" {
		prev := time.Now().AddDate(0, -1, 0)
		return prev.Year(), prev.Month(), nil
	}
	parts := strings.SplitN(strings.TrimSpace(raw), "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("period must be in YYYY-MM form, for example 2026-01")
	}
	year, err := strconv.Atoi(parts[0])
	if err != nil || year < 2000 || year > 2200 {
		return 0, 0, fmt.Errorf("period must be in YYYY-MM form, for example 2026-01")
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 1 || m > 12 {
		return 0, 0, fmt.Errorf("period month must be 01 to 12")
	}
	return year, time.Month(m), nil
}
