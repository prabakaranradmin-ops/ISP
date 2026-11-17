package staffui

import (
	"context"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/rs/zerolog/log"
)

// DiagnosticsRunner runs the billing checks (FR-BIL-019). Satisfied by
// *api.Handler.
type DiagnosticsRunner interface {
	BillingDiagnostics(ctx context.Context, limit int) (*billing.Diagnostics, error)
}

const diagnosticsLimit = 200

// BillingDiagnostics handles GET /staff/diagnostics. Each load runs the
// checks afresh; there is nothing to save.
func (h *Handler) BillingDiagnostics(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "diagnostics")
	if !ok {
		return
	}
	d := h.page(s, "Billing diagnostics", "diagnostics")
	if h.diagnostics == nil {
		d.Error = "Billing diagnostics are not configured on this deployment."
		h.render(w, "diagnostics", d)
		return
	}
	res, err := h.diagnostics.BillingDiagnostics(r.Context(), diagnosticsLimit)
	if err != nil {
		log.Error().Err(err).Msg("staffui: billing diagnostics failed")
		// Never an empty page that reads as "all clear".
		d.Error = "The checks could not be run, so nothing below can be trusted. Try again in a moment."
		h.render(w, "diagnostics", d)
		return
	}
	d.Data = res
	h.render(w, "diagnostics", d)
}
