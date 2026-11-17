package staffui

import (
	"context"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/rs/zerolog/log"
)

// BillingRunReader reads the recurring-billing run history (FR-BIL-020).
// Satisfied by *api.Handler.
type BillingRunReader interface {
	ListBillingRuns(ctx context.Context, f billing.BillingRunFilter) ([]billing.BillingRun, error)
	GetBillingRun(ctx context.Context, id int) (*billing.BillingRun, error)
}

// stageAdvice says, for each step a renewal can fail at, what happened to
// the customer and what to do. Keyed by billing.Stage*.
var stageAdvice = map[string]string{
	billing.StagePrice:      "Nothing was charged. Usually no GST rate in force; the next pass retries once one is set.",
	billing.StageDebit:      "Nothing was charged. The wallet posting failed; the next pass retries.",
	billing.StageExpiry:     "Charged, but the plan was not extended. Extend their expiry by one cycle, or refund the charge.",
	billing.StageActivation: "Charged, but the first cycle was not activated. Activate the subscriber, or refund the charge.",
	billing.StageInvoice:    "Charged and renewed, but no invoice was written. Raise the invoice so the charge is declared.",
	billing.StageDunning:    "Renewed, but still marked as in dunning. The hourly dunning pass normally corrects this by itself.",
}

type billingRunsData struct {
	Runs         []billing.BillingRun
	IncludeIdle  bool
	ProblemsOnly bool
}

type billingRunData struct {
	Run    *billing.BillingRun
	Advice map[string]string
}

// BillingRuns handles GET /staff/billing-runs.
func (h *Handler) BillingRuns(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "billing-runs")
	if !ok {
		return
	}
	d := h.page(s, "Billing runs", "billing-runs")
	if h.billingRuns == nil {
		d.Error = "Billing run history is not configured on this deployment."
		h.render(w, "billing_runs", d)
		return
	}
	q := r.URL.Query()
	bd := billingRunsData{IncludeIdle: q.Get("idle") == "1", ProblemsOnly: q.Get("problems") == "1"}
	runs, err := h.billingRuns.ListBillingRuns(r.Context(), billing.BillingRunFilter{
		IncludeIdle: bd.IncludeIdle, ProblemsOnly: bd.ProblemsOnly, Limit: 200,
	})
	if err != nil {
		log.Error().Err(err).Msg("staffui: list billing runs failed")
		d.Error = "Could not load the billing runs."
	}
	bd.Runs = runs
	d.Data = bd
	h.render(w, "billing_runs", d)
}

// BillingRun handles GET /staff/billing-runs/{id}.
func (h *Handler) BillingRun(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "billing-runs")
	if !ok {
		return
	}
	if h.billingRuns == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Billing run history is not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid run number.")
		return
	}
	run, err := h.billingRuns.GetBillingRun(r.Context(), id)
	if err != nil {
		log.Error().Err(err).Int("run_id", id).Msg("staffui: get billing run failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not load that run.")
		return
	}
	if run == nil {
		h.renderError(w, r, s, http.StatusNotFound, "No billing run with that number.")
		return
	}
	d := h.page(s, "Billing run "+strconv.Itoa(id), "billing-runs")
	d.Data = billingRunData{Run: run, Advice: stageAdvice}
	h.render(w, "billing_run", d)
}
