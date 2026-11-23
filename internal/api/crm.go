package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/crm"
)

// CRM lead pipeline endpoints — FR-CRM-001..003 | MDS §4.16.

// LeadQuerier is the persistence surface the lead pipeline needs.
// Satisfied by *db.CRMStore.
type LeadQuerier interface {
	CreateLead(ctx context.Context, l crm.Lead) (*crm.Lead, error)
	GetLead(ctx context.Context, id int) (*crm.Lead, error)
	ListLeads(ctx context.Context, status, source *string, franchiseID *int) ([]crm.Lead, error)
	UpdateLead(ctx context.Context, id int, status, assignedTo, notes, lostReason *string) (*crm.Lead, error)
	// ConvertLead must claim the lead and create the subscriber in one
	// transaction, returning (nil, nil, nil) when the lead was already
	// converted — that "did not land" signal is what stops two concurrent
	// conversions producing two subscribers.
	ConvertLead(ctx context.Context, leadID int, sub SubscriberRecord, passwordHash string) (*crm.Lead, *SubscriberRecord, error)
	GetFunnel(ctx context.Context, franchiseID *int) (*crm.FunnelReport, error)
}

type createLeadRequest struct {
	FullName     string `json:"full_name"`
	MobileNumber string `json:"mobile_number"`
	Email        string `json:"email"`
	Source       string `json:"source"`
	FranchiseID  *int   `json:"franchise_id"`
	AssignedTo   string `json:"assigned_to"`
	Notes        string `json:"notes"`
}

// CreateLead handles POST /api/v1/leads. Rules live in NewLead.
func (h *Handler) CreateLead(w http.ResponseWriter, r *http.Request) {
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	var req createLeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// A franchise-scoped caller's leads belong to their own franchise,
	// taken from the token rather than the body — the same rule the
	// franchise endpoints follow, so an LCO cannot file leads against a
	// competitor's pipeline.
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}
	created, err := h.NewLead(r.Context(), crm.Lead{
		FullName: req.FullName, MobileNumber: req.MobileNumber, Email: req.Email,
		Source: req.Source, FranchiseID: req.FranchiseID,
		AssignedTo: req.AssignedTo, Notes: req.Notes,
	}, scope)
	if reason := LeadInvalidReason(err); reason != "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", reason)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create lead failed")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// ListLeads handles GET /api/v1/leads?status=&source=.
func (h *Handler) ListLeads(w http.ResponseWriter, r *http.Request) {
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}

	var status, source *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}
	if v := r.URL.Query().Get("source"); v != "" {
		source = &v
	}

	list, err := h.leads.ListLeads(r.Context(), status, source, scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list leads failed")
		return
	}
	if list == nil {
		list = []crm.Lead{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetLead handles GET /api/v1/leads/{id}.
func (h *Handler) GetLead(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}

	lead, err := h.leads.GetLead(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lead lookup failed")
		return
	}
	if lead == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "lead not found")
		return
	}
	if !callerMaySeeLead(r, lead) {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
		return
	}
	writeJSON(w, http.StatusOK, lead)
}

// callerMaySeeLead enforces the same per-partner isolation the franchise
// endpoints do (CRD-FRN-001): a franchise-scoped caller reaches only their
// own leads, decided by their token rather than the id in the path.
func callerMaySeeLead(r *http.Request, lead *crm.Lead) bool {
	scope, ok := callerFranchiseScope(r)
	if !ok {
		return false
	}
	if scope == nil {
		return true // ISP-wide staff
	}
	return lead.FranchiseID != nil && *lead.FranchiseID == *scope
}

type updateLeadRequest struct {
	Status     *string `json:"status"`
	AssignedTo *string `json:"assigned_to"`
	Notes      *string `json:"notes"`
	LostReason *string `json:"lost_reason"`
}

// UpdateLead handles PATCH /api/v1/leads/{id} — pipeline movement short of
// conversion. Rules live in MoveLead.
func (h *Handler) UpdateLead(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	var req updateLeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
		return
	}
	updated, err := h.MoveLead(r.Context(), id, req.Status, req.AssignedTo, req.Notes, req.LostReason, scope)
	if !writeLeadError(w, err, "update lead failed") {
		writeJSON(w, http.StatusOK, updated)
	}
}

// writeLeadError maps a lead-operation error to its HTTP response and
// reports whether it wrote one. fallback names the action for a 500.
func writeLeadError(w http.ResponseWriter, err error, fallback string) bool {
	switch {
	case err == nil:
		return false
	case LeadInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", LeadInvalidReason(err))
	case errors.Is(err, ErrLeadsUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
	case errors.Is(err, ErrLeadNotFound):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "lead not found")
	case errors.Is(err, ErrLeadNotEntitled):
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
	case errors.Is(err, crm.ErrAlreadyConverted):
		writeError(w, http.StatusConflict, "ERR_ALREADY_CONVERTED", crm.ErrAlreadyConverted.Error())
	case errors.Is(err, crm.ErrLeadLost):
		writeError(w, http.StatusConflict, "ERR_LEAD_LOST", crm.ErrLeadLost.Error())
	case errors.Is(err, ErrLeadDuplicateLogin):
		writeError(w, http.StatusConflict, "ERR_CONFLICT", "CAF number or username already exists")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", fallback)
	}
	return true
}

type convertLeadRequest struct {
	// The fields a subscriber needs and a lead does not. Name, mobile and
	// email are carried over from the lead rather than accepted here, so
	// the subscriber provably came from that prospect.
	CAFNumber       string `json:"caf_number"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	PlanID          int    `json:"plan_id"`
	RegisteredState string `json:"registered_state"`
	Aadhaar         string `json:"aadhaar,omitempty"`
	PAN             string `json:"pan,omitempty"`
	// CPESerial optionally issues a device in the same onboarding moment
	// (FR-INV-002). A failure to issue never fails the conversion.
	CPESerial string `json:"cpe_serial,omitempty"`
}

// convertLeadResponse reports all three outcomes of the handoff: the
// subscriber that now exists, the lead that produced them, and what happened
// to the CPE — including the case where the subscriber was created but the
// device could not be issued.
type convertLeadResponse struct {
	Subscriber *SubscriberRecord `json:"subscriber"`
	Lead       *crm.Lead         `json:"lead"`
	CPEIssued  string            `json:"cpe_issued,omitempty"`
	CPEWarning string            `json:"cpe_warning,omitempty"`
}

// ConvertLead handles POST /api/v1/leads/{id}/convert. Rules live in
// ConvertLeadToSubscriber.
//
// FR: FR-CRM-002, FR-INV-002 | MDS §4.16
func (h *Handler) ConvertLead(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	var req convertLeadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this lead")
		return
	}
	// Field by field rather than a type conversion, for the reason given at
	// the same pattern in announcements.go: the wire type and the domain
	// input are deliberately separate and are expected to diverge.
	res, err := h.ConvertLeadToSubscriber(r.Context(), id, ConvertLeadInput{ //nolint:staticcheck // S1016: see above
		CAFNumber: req.CAFNumber, Username: req.Username, Password: req.Password,
		PlanID: req.PlanID, RegisteredState: req.RegisteredState,
		Aadhaar: req.Aadhaar, PAN: req.PAN, CPESerial: req.CPESerial,
	}, scope)
	if writeLeadError(w, err, "lead conversion failed") {
		return
	}
	writeJSON(w, http.StatusCreated, convertLeadResponse{
		Subscriber: res.Subscriber, Lead: res.Lead, CPEIssued: res.CPEIssued, CPEWarning: res.CPEWarning,
	})
}

// GetLeadFunnel handles GET /api/v1/leads/funnel (FR-CRM-003).
func (h *Handler) GetLeadFunnel(w http.ResponseWriter, r *http.Request) {
	if h.leads == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "lead store not configured")
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}

	report, err := h.leads.GetFunnel(r.Context(), scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "funnel report failed")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
