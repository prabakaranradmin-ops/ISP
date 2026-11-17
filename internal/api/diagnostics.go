package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Billing diagnostics — FR-BIL-019. Read-only.

// DiagnosticsStore runs the billing checks. Satisfied by *db.BillingStore.
type DiagnosticsStore interface {
	BillingDiagnostics(ctx context.Context, limit int) (*billing.Diagnostics, error)
}

// ErrDiagnosticsUnwired is returned when no store is configured.
var ErrDiagnosticsUnwired = errors.New("api: billing diagnostics not configured")

// BillingDiagnostics runs the three checks, each list capped at limit.
func (h *Handler) BillingDiagnostics(ctx context.Context, limit int) (*billing.Diagnostics, error) {
	if h.diagnostics == nil {
		return nil, ErrDiagnosticsUnwired
	}
	return h.diagnostics.BillingDiagnostics(ctx, limit)
}

// GetBillingDiagnosticsHTTP handles GET /api/v1/billing/diagnostics?limit=.
func (h *Handler) GetBillingDiagnosticsHTTP(w http.ResponseWriter, r *http.Request) {
	d, err := h.BillingDiagnostics(r.Context(), parseLimit(r.URL.Query(), 200, 1000))
	switch {
	case errors.Is(err, ErrDiagnosticsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "billing diagnostics not configured")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "billing diagnostics failed")
		return
	}

	opt := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.Format(time.RFC3339)
	}
	un := make([]map[string]any, 0, len(d.Uninvoiced))
	for _, u := range d.Uninvoiced {
		un = append(un, map[string]any{
			"subscriber_id": u.SubscriberID, "username": u.Username, "status": u.Status,
			"plan": u.PlanName, "plan_price": u.PlanPrice.StringFixed(2),
			"cycle_start": u.CycleStart.Format(time.RFC3339), "plan_expiry": u.PlanExpiry.Format(time.RFC3339),
			"last_invoice_at": opt(u.LastInvoice),
		})
	}
	mm := make([]map[string]any, 0, len(d.Mismatches))
	for _, m := range d.Mismatches {
		var after any
		if m.LastAfter != nil {
			after = m.LastAfter.StringFixed(2)
		}
		mm = append(mm, map[string]any{
			"subscriber_id": m.SubscriberID, "username": m.Username,
			"stored_balance": m.Stored.StringFixed(2), "ledger_sum": m.LedgerSum.StringFixed(2),
			"last_balance_after": after, "ledger_entries": m.Entries, "difference": m.Difference().StringFixed(2),
		})
	}
	dr := make([]map[string]any, 0, len(d.Drifts))
	for _, x := range d.Drifts {
		dr = append(dr, map[string]any{
			"subscriber_id": x.SubscriberID, "username": x.Username, "plan": x.PlanName,
			"catalogue_price": x.CataloguePrice.StringFixed(2), "billed_base": x.BilledBase.StringFixed(2),
			"difference": x.Difference().StringFixed(2), "invoice_number": billing.InvoiceNumber(x.InvoiceID),
			"invoiced_at": x.InvoicedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ran_at": d.RanAt.Format(time.RFC3339), "clean": d.Clean(),
		"uninvoiced_cycles":  map[string]any{"count": d.UninvoicedCount, "items": un},
		"balance_mismatches": map[string]any{"count": d.MismatchCount, "items": mm},
		"price_drift":        map[string]any{"count": d.DriftCount, "items": dr},
	})
}
