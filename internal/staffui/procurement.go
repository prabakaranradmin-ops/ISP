package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/procurement"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ProcurementStore backs the Procurement screen — general purchase orders
// (CRD-EXP-007), the console counterpart to internal/api/procurement.go.
// Not wired to any ledger: see that package's own doc comment on why.
//
// Redefined per package rather than sharing api.ProcurementQuerier, matching
// every other store interface in this file. Satisfied by
// *db.ProcurementStore.
type ProcurementStore interface {
	CreatePurchaseOrder(ctx context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error)
	ListPurchaseOrders(ctx context.Context, status *string) ([]procurement.PurchaseOrder, error)
	GetPurchaseOrder(ctx context.Context, id int) (*procurement.PurchaseOrder, error)
	DecidePurchaseOrder(ctx context.Context, id int, approve bool, decidedBy, reason string) (*procurement.PurchaseOrder, error)
	UpdateFulfilment(ctx context.Context, id int, status, actor string) (*procurement.PurchaseOrder, error)
}

type procurementData struct {
	Orders       []procurement.PurchaseOrder
	StatusFilter string
	// Vendors are the suppliers an order may name (FR-INV-004). The form
	// showed a text box before; it is now a dropdown off the shared master,
	// and an empty list is what the template uses to say a vendor has to
	// exist first.
	Vendors []vendors.Vendor
}

// Procurement lists purchase orders and hosts the request form. Owner/
// billing_admin only, matching the API's own gate: a spend decision is
// procurement, whatever it is being spent on.
func (h *Handler) Procurement(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "procurement")
	if !ok {
		return
	}
	h.renderProcurement(w, r, s, r.URL.Query().Get("status"), "", "")
}

func (h *Handler) renderProcurement(w http.ResponseWriter, r *http.Request, s Session, statusFilter, message, errMsg string) {
	d := h.page(s, "Procurement", "procurement")
	d.Message, d.Error = message, errMsg

	if h.procurement == nil {
		d.Error = "Procurement is not configured on this deployment."
		h.render(w, "procurement", d)
		return
	}

	var statusPtr *string
	if statusFilter != "" {
		statusPtr = &statusFilter
	}
	orders, err := h.procurement.ListPurchaseOrders(r.Context(), statusPtr)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list purchase orders failed")
		d.Error = "Could not load purchase orders."
		h.render(w, "procurement", d)
		return
	}

	d.Data = procurementData{
		Orders:       orders,
		StatusFilter: statusFilter,
		Vendors:      h.pickableVendors(r.Context()),
	}
	h.render(w, "procurement", d)
}

// CreatePurchaseOrderForm files a new request.
func (h *Handler) CreatePurchaseOrderForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "procurement")
	if !ok {
		return
	}
	if h.procurement == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Procurement is not configured.")
		return
	}

	description := strings.TrimSpace(r.PostFormValue("description"))
	category := r.PostFormValue("category")
	vendorID, vErr := strconv.Atoi(strings.TrimSpace(r.PostFormValue("vendor_id")))
	if description == "" {
		h.renderProcurement(w, r, s, "", "", "A description is required.")
		return
	}
	if vErr != nil || vendorID <= 0 {
		h.renderProcurement(w, r, s, "", "",
			"Choose a vendor. If the supplier is not in the list, add it on the Vendors screen first.")
		return
	}
	if !procurement.ValidCategory(category) {
		h.renderProcurement(w, r, s, "", "", "Category must be one of: hardware, services, other.")
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("amount")))
	if err != nil || amount.IsNegative() {
		h.renderProcurement(w, r, s, "", "", "Amount must be a non-negative number.")
		return
	}

	created, err := h.procurement.CreatePurchaseOrder(r.Context(), procurement.NewPurchaseOrder{
		Description: description, VendorID: vendorID, Category: category,
		Amount: amount.StringFixed(2), RequestedBy: s.Username,
	})
	if err != nil {
		// A vendor retired between the page loading and the form posting is
		// told apart from a storage failure: the first is something the
		// operator can act on, the second is not.
		if msg, ok := vendorProblem(err); ok {
			h.renderProcurement(w, r, s, "", "", msg)
			return
		}
		log.Error().Err(err).Msg("staffui: create purchase order failed")
		h.renderProcurement(w, r, s, "", "", "Could not file that purchase order.")
		return
	}
	h.renderProcurement(w, r, s, "", fmt.Sprintf("Purchase order #%d filed for approval.", created.ID), "")
}

// DecidePurchaseOrderForm approves or rejects a pending request. The
// distinct-approver rule is re-checked here (not just left to
// chk_po_distinct_approver) so a self-approval attempt gets a clear message
// instead of a raw constraint-violation failure.
func (h *Handler) DecidePurchaseOrderForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "procurement")
	if !ok {
		return
	}
	if h.procurement == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Procurement is not configured.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderProcurement(w, r, s, "", "", "Invalid purchase order id.")
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if !approve && reason == "" {
		h.renderProcurement(w, r, s, "", "", "A reason is required to reject a purchase order.")
		return
	}

	existing, err := h.procurement.GetPurchaseOrder(r.Context(), id)
	if err != nil || existing == nil {
		h.renderProcurement(w, r, s, "", "", "Purchase order not found.")
		return
	}
	if existing.RequestedBy == s.Username {
		h.renderProcurement(w, r, s, "", "", "You cannot decide a purchase order you filed yourself.")
		return
	}

	decided, err := h.procurement.DecidePurchaseOrder(r.Context(), id, approve, s.Username, reason)
	if err != nil {
		log.Error().Err(err).Int("id", id).Msg("staffui: decide purchase order failed")
		h.renderProcurement(w, r, s, "", "", "Could not decide that purchase order.")
		return
	}
	if decided == nil {
		h.renderProcurement(w, r, s, "", "", "This purchase order was already decided.")
		return
	}
	h.renderProcurement(w, r, s, "", fmt.Sprintf("Purchase order #%d %s.", id, decided.Status), "")
}

// UpdateFulfilmentForm moves an approved order to ordered/received, or
// cancels it.
func (h *Handler) UpdateFulfilmentForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "procurement")
	if !ok {
		return
	}
	if h.procurement == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Procurement is not configured.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderProcurement(w, r, s, "", "", "Invalid purchase order id.")
		return
	}
	status := r.PostFormValue("status")
	if !procurement.ValidFulfilmentStatus(status) {
		h.renderProcurement(w, r, s, "", "", "Status must be one of: ordered, received, cancelled.")
		return
	}

	updated, err := h.procurement.UpdateFulfilment(r.Context(), id, status, s.Username)
	if err != nil {
		log.Error().Err(err).Int("id", id).Msg("staffui: update purchase order fulfilment failed")
		h.renderProcurement(w, r, s, "", "", "Could not update that purchase order.")
		return
	}
	if updated == nil {
		h.renderProcurement(w, r, s, "", "", "No purchase order in a state this update applies to.")
		return
	}
	h.renderProcurement(w, r, s, "", fmt.Sprintf("Purchase order #%d marked %s.", id, status), "")
}
