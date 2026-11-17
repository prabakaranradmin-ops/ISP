package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Recurring-billing run history — FR-BIL-020. Read-only.

// BillingRunStore reads recorded runs. Satisfied by *db.BillingStore.
type BillingRunStore interface {
	ListBillingRuns(ctx context.Context, f billing.BillingRunFilter) ([]billing.BillingRun, error)
	GetBillingRun(ctx context.Context, id int) (*billing.BillingRun, error)
}

// ErrBillingRunsUnwired is returned when no store is configured.
var ErrBillingRunsUnwired = errors.New("api: billing run history not configured")

// ListBillingRuns lists recorded runs, newest first.
func (h *Handler) ListBillingRuns(ctx context.Context, f billing.BillingRunFilter) ([]billing.BillingRun, error) {
	if h.billingRuns == nil {
		return nil, ErrBillingRunsUnwired
	}
	return h.billingRuns.ListBillingRuns(ctx, f)
}

// GetBillingRun loads one run with its failures; nil if there is none.
func (h *Handler) GetBillingRun(ctx context.Context, id int) (*billing.BillingRun, error) {
	if h.billingRuns == nil {
		return nil, ErrBillingRunsUnwired
	}
	return h.billingRuns.GetBillingRun(ctx, id)
}

func billingRunJSON(r billing.BillingRun) map[string]any {
	var finished any
	if r.FinishedAt != nil {
		finished = r.FinishedAt.Format(time.RFC3339)
	}
	out := map[string]any{
		"id": r.ID, "kind": r.Kind, "status": r.Status,
		"started_at": r.StartedAt.Format(time.RFC3339), "finished_at": finished,
		"duration_seconds": r.Duration().Seconds(),
		"candidates":       r.Candidates, "renewed": r.Renewed, "activated": r.Activated,
		"insufficient_balance": r.InsufficientBalance, "failed": r.Failed,
		"invoices_raised": r.InvoicesRaised, "invoice_failures": r.InvoiceFailures,
		"amount_charged": r.AmountCharged.StringFixed(2),
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	return out
}

// ListBillingRunsHTTP handles GET /api/v1/billing/runs?include_idle=&problems=&limit=.
func (h *Handler) ListBillingRunsHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runs, err := h.ListBillingRuns(r.Context(), billing.BillingRunFilter{
		IncludeIdle:  q.Get("include_idle") == "true",
		ProblemsOnly: q.Get("problems") == "true",
		Limit:        parseLimit(q, 100, 500),
	})
	switch {
	case errors.Is(err, ErrBillingRunsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "billing run history not configured")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "billing run lookup failed")
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, billingRunJSON(run))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetBillingRunHTTP handles GET /api/v1/billing/runs/{id}.
func (h *Handler) GetBillingRunHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	run, err := h.GetBillingRun(r.Context(), id)
	switch {
	case errors.Is(err, ErrBillingRunsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "billing run history not configured")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "billing run lookup failed")
		return
	case run == nil:
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no such billing run")
		return
	}
	out := billingRunJSON(*run)
	failures := make([]map[string]any, 0, len(run.Failures))
	for _, f := range run.Failures {
		failures = append(failures, map[string]any{
			"subscriber_id": f.SubscriberID, "username": f.Username, "stage": f.Stage,
			"message": f.Message, "charged": f.Charged,
		})
	}
	out["failures"] = failures
	writeJSON(w, http.StatusOK, out)
}
