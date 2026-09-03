package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Bulk subscriber operations — console multi-select actions (plan change,
// suspend/resume, wallet credit). Each is exactly the existing single-
// subscriber action, called once per id: none of the underlying safeguards
// (proration math, the wallet-credit approval gate, audit entries) change
// shape for a batch. What changes is that one bad id in a batch of fifty
// must not silently swallow the other forty-nine, so every core method here
// reports a per-id result rather than a single pass/fail for the request.
//
// The *ForMany methods are exported and take a context rather than an
// http.Request so internal/staffui — a direct, non-HTTP caller, the same
// relationship ProvisionSubscriber already has with the console — can run
// the identical logic the HTTP bulk endpoints below use, rather than a
// second implementation that could drift.
//
// maxBulkSubscribers bounds a single request's blast radius and keeps a
// request body deserializing 100,000 ids from tying up a request goroutine.
const maxBulkSubscribers = 500

// BulkResult reports one subscriber's outcome within a bulk action.
type BulkResult struct {
	SubscriberID int    `json:"subscriber_id"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
}

// validateBulkIDs applies the one check every bulk HTTP endpoint shares.
func validateBulkIDs(w http.ResponseWriter, ids []int) bool {
	if len(ids) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_ids must not be empty")
		return false
	}
	if len(ids) > maxBulkSubscribers {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			fmt.Sprintf("subscriber_ids must not exceed %d per request", maxBulkSubscribers))
		return false
	}
	return true
}

// ── Bulk plan change ─────────────────────────────────────────────────────────

type bulkPlanChangeRequest struct {
	SubscriberIDs []int `json:"subscriber_ids"`
	NewPlanID     int   `json:"new_plan_id"`
}

// BulkChangeSubscriberPlan handles POST /api/v1/subscribers/bulk/plan-change.
func (h *Handler) BulkChangeSubscriberPlan(w http.ResponseWriter, r *http.Request) {
	if h.lifecycle == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lifecycle store not configured")
		return
	}
	var req bulkPlanChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if !validateBulkIDs(w, req.SubscriberIDs) {
		return
	}
	if req.NewPlanID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "new_plan_id is required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": h.ChangePlanForMany(r.Context(), req.SubscriberIDs, req.NewPlanID),
	})
}

// ChangePlanForMany runs changeSubscriberPlan once per id.
func (h *Handler) ChangePlanForMany(ctx context.Context, ids []int, newPlanID int) []BulkResult {
	results := make([]BulkResult, 0, len(ids))
	for _, id := range ids {
		if _, err := h.changeSubscriberPlan(ctx, id, newPlanID); err != nil {
			results = append(results, BulkResult{SubscriberID: id, Error: err.Error()})
			continue
		}
		results = append(results, BulkResult{SubscriberID: id, OK: true})
	}
	return results
}

// ── Bulk status change (suspend/resume) ─────────────────────────────────────

// BulkAllowedStatuses excludes "terminated" deliberately: termination stays
// a single-subscriber, approval-gated action (TerminateSubscriber). Bulk
// must not become a way to mass-terminate without the second-approver check
// that action alone requires. Exported so internal/staffui's bulk-action
// form can offer exactly these choices rather than a hand-kept second list.
var BulkAllowedStatuses = map[string]bool{
	"active":         true,
	"soft_suspended": true,
	"hard_suspended": true,
}

type bulkStatusRequest struct {
	SubscriberIDs []int  `json:"subscriber_ids"`
	Status        string `json:"status"`
}

// BulkUpdateStatus handles POST /api/v1/subscribers/bulk/status.
func (h *Handler) BulkUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "subscriber store not configured")
		return
	}
	var req bulkStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if !validateBulkIDs(w, req.SubscriberIDs) {
		return
	}
	if !BulkAllowedStatuses[req.Status] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"status must be one of: active, soft_suspended, hard_suspended")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": h.UpdateStatusForMany(r.Context(), req.SubscriberIDs, req.Status),
	})
}

// UpdateStatusForMany runs the same status change UpdateSubscriber (PATCH)
// applies, once per id. Callers must restrict status to BulkAllowedStatuses
// themselves — this does not re-check it, matching how UpdateSubscriber
// itself trusts the DB's CHECK constraint rather than duplicating the list.
func (h *Handler) UpdateStatusForMany(ctx context.Context, ids []int, status string) []BulkResult {
	results := make([]BulkResult, 0, len(ids))
	for _, id := range ids {
		// nil plan_expiry: a bulk suspend/resume changes status only and must
		// never move anyone's billed period as a side effect.
		updated, err := h.db.UpdateSubscriber(ctx, id, nil, &status, nil)
		if err != nil {
			results = append(results, BulkResult{SubscriberID: id, Error: err.Error()})
			continue
		}
		if updated == nil {
			results = append(results, BulkResult{SubscriberID: id, Error: ErrSubscriberNotFound.Error()})
			continue
		}
		middleware.Audit(ctx, "subscriber.update", strconv.Itoa(id), map[string]any{"status": status})
		h.emit(ctx, partner.EventSubscriberStatusChanged, id)
		results = append(results, BulkResult{SubscriberID: id, OK: true})
	}
	return results
}

// ── Bulk wallet credit ───────────────────────────────────────────────────────

type bulkCreditRequest struct {
	SubscriberIDs []int  `json:"subscriber_ids"`
	Amount        string `json:"amount"`
	Reason        string `json:"reason"`
}

// BulkCreditResult reports one subscriber's approval-request outcome —
// separate from BulkResult because success here means "a pending approval
// was created", carried in ApprovalID, not "the credit was applied".
type BulkCreditResult struct {
	SubscriberID int    `json:"subscriber_id"`
	OK           bool   `json:"ok"`
	ApprovalID   int    `json:"approval_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

// BulkWalletCredit handles POST /api/v1/subscribers/bulk/credit.
//
// Unlike plan change and status, this does not credit anyone directly: a
// wallet credit is gated behind two-person approval with no amount
// threshold (see CreateAdjustment), and bulk must not be a way around that.
// What this creates is N pending approval requests — one per subscriber,
// each still needing a second staff member's sign-off — not N instant
// credits. The response makes that explicit rather than implying money has
// already moved.
func (h *Handler) BulkWalletCredit(w http.ResponseWriter, r *http.Request) {
	if h.approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "approval store not configured")
		return
	}
	var req bulkCreditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if !validateBulkIDs(w, req.SubscriberIDs) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "reason is required")
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "amount must be a positive decimal")
		return
	}

	results := h.RequestCreditForMany(r.Context(), middleware.SubjectFromContext(r.Context()),
		req.SubscriberIDs, amount, req.Reason)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"note":    "each id below has a pending approval request, not an applied credit",
		"results": results,
	})
}

// RequestCreditForMany files one wallet-credit approval request per id.
// requestedBy identifies who filed the batch — the HTTP handler reads it
// from the caller's own auth context; a non-HTTP caller (the console)
// supplies the signed-in operator's username directly.
func (h *Handler) RequestCreditForMany(ctx context.Context, requestedBy string, ids []int, amount decimal.Decimal, reason string) []BulkCreditResult {
	results := make([]BulkCreditResult, 0, len(ids))
	for _, id := range ids {
		created, err := h.createApprovalRequest(ctx, requestedBy, workflow.ApprovalRequest{
			ActionType:   workflow.ActionWalletCredit,
			SubscriberID: id,
			Amount:       amountPtr(amount),
			// "(bulk)" lets a second approver recognise these as one batch
			// without a schema change to carry a separate batch id.
			Reason: reason + " (bulk)",
		})
		if err != nil {
			log.Error().Err(err).Int("subscriber_id", id).Msg("api: bulk credit approval request failed")
			results = append(results, BulkCreditResult{SubscriberID: id, Error: err.Error()})
			continue
		}
		results = append(results, BulkCreditResult{SubscriberID: id, OK: true, ApprovalID: created.ID})
	}
	return results
}
