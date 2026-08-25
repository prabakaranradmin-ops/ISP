package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/procurement"
)

// General procurement endpoints — CRD-EXP-007 | MDS §4.16's own note that
// internal/inventory's Purchase type is CPE-restocking specific, not a
// general purchase-order system.
//
// Everything here is owner/billing_admin only, matching internal/staffui's
// own reasoning for why inventory purchases and device-type changes are
// gated that way: a spend decision is procurement, not day-to-day
// operations, whatever it is being spent on.

// ProcurementQuerier is the persistence surface purchase orders need.
// Satisfied by *db.ProcurementStore.
type ProcurementQuerier interface {
	CreatePurchaseOrder(ctx context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error)
	ListPurchaseOrders(ctx context.Context, status *string) ([]procurement.PurchaseOrder, error)
	GetPurchaseOrder(ctx context.Context, id int) (*procurement.PurchaseOrder, error)
	DecidePurchaseOrder(ctx context.Context, id int, approve bool, decidedBy, reason string) (*procurement.PurchaseOrder, error)
	UpdateFulfilment(ctx context.Context, id int, status, actor string) (*procurement.PurchaseOrder, error)
}

type createPurchaseOrderRequest struct {
	Description string `json:"description"`
	Vendor      string `json:"vendor"`
	Category    string `json:"category"`
	Amount      string `json:"amount"`
}

// CreatePurchaseOrder handles POST /api/v1/procurement/orders.
func (h *Handler) CreatePurchaseOrder(w http.ResponseWriter, r *http.Request) {
	if h.procurement == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "procurement is not configured")
		return
	}

	var req createPurchaseOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Description == "" || req.Vendor == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "description and vendor are required")
		return
	}
	if !procurement.ValidCategory(req.Category) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "category must be one of: hardware, services, other")
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.IsNegative() {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "amount must be a non-negative decimal")
		return
	}

	created, err := h.procurement.CreatePurchaseOrder(r.Context(), procurement.NewPurchaseOrder{
		Description: req.Description, Vendor: req.Vendor, Category: req.Category,
		Amount: amount.StringFixed(2), RequestedBy: middleware.SubjectFromContext(r.Context()),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create purchase order failed")
		return
	}

	middleware.Audit(r.Context(), "procurement.request", strconv.Itoa(created.ID), map[string]any{
		"vendor": created.Vendor, "amount": created.Amount,
	})
	writeJSON(w, http.StatusCreated, created)
}

// ListPurchaseOrders handles GET /api/v1/procurement/orders?status=.
func (h *Handler) ListPurchaseOrders(w http.ResponseWriter, r *http.Request) {
	if h.procurement == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "procurement is not configured")
		return
	}
	var status *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}
	list, err := h.procurement.ListPurchaseOrders(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list purchase orders failed")
		return
	}
	if list == nil {
		list = []procurement.PurchaseOrder{}
	}
	writeJSON(w, http.StatusOK, list)
}

type decidePurchaseOrderRequest struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason"`
}

// DecidePurchaseOrder handles POST /api/v1/procurement/orders/{id}/decide.
//
// The distinct-approver guarantee is enforced by chk_po_distinct_approver at
// the schema — checked here too, against the caller's own identity, so a
// self-approval is refused with a clear 403 rather than surfacing as an
// opaque constraint-violation 500.
func (h *Handler) DecidePurchaseOrder(w http.ResponseWriter, r *http.Request) {
	if h.procurement == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "procurement is not configured")
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}

	var req decidePurchaseOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if !req.Approve && req.Reason == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "reason is required to reject a request")
		return
	}

	existing, err := h.procurement.GetPurchaseOrder(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "purchase order lookup failed")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "purchase order not found")
		return
	}
	actor := middleware.SubjectFromContext(r.Context())
	if existing.RequestedBy == actor {
		writeError(w, http.StatusForbidden, "ERR_SELF_APPROVAL", "the requester cannot decide their own purchase order")
		return
	}

	decided, err := h.procurement.DecidePurchaseOrder(r.Context(), id, req.Approve, actor, req.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "decide purchase order failed")
		return
	}
	if decided == nil {
		writeError(w, http.StatusConflict, "ERR_ALREADY_DECIDED", "this purchase order was already decided")
		return
	}

	middleware.Audit(r.Context(), "procurement.decide", strconv.Itoa(id), map[string]any{
		"approve": req.Approve, "decided_by": actor,
	})
	writeJSON(w, http.StatusOK, decided)
}

type updateFulfilmentRequest struct {
	Status string `json:"status"`
}

// UpdateFulfilmentStatus handles POST /api/v1/procurement/orders/{id}/fulfilment.
func (h *Handler) UpdateFulfilmentStatus(w http.ResponseWriter, r *http.Request) {
	if h.procurement == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "procurement is not configured")
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var req updateFulfilmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if !procurement.ValidFulfilmentStatus(req.Status) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "status must be one of: ordered, received, cancelled")
		return
	}

	updated, err := h.procurement.UpdateFulfilment(r.Context(), id, req.Status, middleware.SubjectFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update purchase order failed")
		return
	}
	if updated == nil {
		writeError(w, http.StatusConflict, "ERR_CONFLICT", "no purchase order in a state this transition applies to")
		return
	}

	middleware.Audit(r.Context(), "procurement.fulfilment", strconv.Itoa(id), map[string]any{"status": req.Status})
	writeJSON(w, http.StatusOK, updated)
}
