package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/maaransoft/isp-bss-oss/pkg/validate"
)

// Franchise / LCO endpoints — FR-FRN-003..006 | MDS §4.10 | API §7.
//
// internal/revenue/franchise.go has carried the commission engine, the
// franchise-scoping middleware and a subscriber-listing handler since v2.0
// with no route registered anywhere: none of it was reachable. These are the
// routes that make it reachable, and the scope enforcement that makes it
// safe to expose.

// FranchiseQuerier is the persistence surface the franchise endpoints need.
// Satisfied by *db.RevenueStore.
type FranchiseQuerier interface {
	ListFranchises(ctx context.Context, franchiseID *int) ([]revenue.FranchiseRecord, error)
	CreateFranchise(ctx context.Context, req revenue.CreateFranchiseRequest) (*revenue.FranchiseRecord, error)
	GetFranchisePnL(ctx context.Context, franchiseID int, from, to *time.Time) (*revenue.FranchisePnL, error)
	ListConsolidatedPnL(ctx context.Context, from, to *time.Time) (*revenue.ConsolidatedPnL, error)
}

// callerFranchiseScope returns the franchise a caller is confined to, or nil
// for ISP-wide staff.
//
// Derived from the caller's own token, never from a request parameter — a
// franchise-scoped caller cannot widen their own visibility by asking for a
// different id, because they never get to say which id applies to them.
//
// A franchise-scoped role whose token carries no franchise_id is refused
// rather than treated as unscoped: defaulting to ISP-wide on a missing claim
// would turn a misissued token into a cross-partner data leak. This mirrors
// revenue.FranchiseMiddleware's own rule.
func callerFranchiseScope(r *http.Request) (scope *int, ok bool) {
	role := middleware.RoleFromContext(r.Context())
	if !franchiseScopedRole(role) {
		return nil, true
	}
	id := middleware.FranchiseIDFromContext(r.Context())
	if id == 0 {
		return nil, false
	}
	return &id, true
}

// franchiseScopedRole mirrors revenue.franchiseScopedRoles, which is
// unexported. Kept in step with migration 024's chk_staff_franchise_binding,
// which enforces the same three roles at the database.
func franchiseScopedRole(role string) bool {
	switch role {
	case "lco", "franchise_admin", "franchise_staff":
		return true
	default:
		return false
	}
}

// ListFranchises handles GET /api/v1/franchises.
//
// FR: FR-FRN-001, FR-FRN-004
func (h *Handler) ListFranchises(w http.ResponseWriter, r *http.Request) {
	if h.franchises == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "franchise store not configured")
		return
	}
	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}

	list, err := h.franchises.ListFranchises(r.Context(), scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list franchises failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// CreateFranchise handles POST /api/v1/franchises — partner onboarding.
//
// FR: FR-FRN-006. This is the API half; the staff-console screen the
// requirement also asks for is separate work.
func (h *Handler) CreateFranchise(w http.ResponseWriter, r *http.Request) {
	if h.franchises == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "franchise store not configured")
		return
	}

	var req revenue.CreateFranchiseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Name == "" || req.OwnerName == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "name and owner_name are required")
		return
	}
	if req.MobileNumber == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "mobile_number is required")
		return
	}
	// Validated here rather than left to chk_franchises_mobile_e164
	// (migration 020): the same reasoning as CreateLead's own check
	// (internal/api/crm.go) — a bad format would otherwise surface as a raw
	// 500 from a CHECK constraint the caller cannot see, on a value most
	// callers will type in local (non-E.164) format by default.
	if !validate.E164(req.MobileNumber) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"mobile_number must be E.164 format (e.g. +919876543210)")
		return
	}

	// Validated here rather than left to the NUMERIC(5,2) column: a bad rate
	// would otherwise surface as a raw cast error, and a rate outside 0–100
	// is accepted by the column but is not a commission percentage. A 150%
	// commission would pay a partner more than the subscriber paid.
	rate, err := decimal.NewFromString(req.CommissionRatePct)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "commission_rate_pct must be a decimal number")
		return
	}
	if rate.IsNegative() || rate.GreaterThan(decimal.NewFromInt(100)) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "commission_rate_pct must be between 0 and 100")
		return
	}

	created, err := h.franchises.CreateFranchise(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create franchise failed")
		return
	}

	middleware.Audit(r.Context(), "franchise.create", strconv.Itoa(created.ID), map[string]any{
		"name": created.Name, "commission_rate_pct": created.CommissionRatePct,
	})
	writeJSON(w, http.StatusCreated, created)
}

// GetFranchisePnL handles GET /api/v1/franchises/{franchise_id}/pnl.
//
// FR: FR-FRN-003, FR-FRN-004
func (h *Handler) GetFranchisePnL(w http.ResponseWriter, r *http.Request) {
	if h.franchises == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "franchise store not configured")
		return
	}
	franchiseID, err := pathInt(r, "franchise_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid franchise_id")
		return
	}

	scope, ok := callerFranchiseScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "token has no franchise binding")
		return
	}
	// The core isolation rule (CRD-FRN-001: "LCO partners cannot see other
	// LCOs' subscribers"). 403, not 404: the caller is authenticated and the
	// franchise may well exist — they are simply not entitled to it, and
	// saying so is more honest than pretending it is missing.
	if scope != nil && *scope != franchiseID {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "not entitled to this franchise")
		return
	}

	from, to, err := parseDateWindow(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	pnl, err := h.franchises.GetFranchisePnL(r.Context(), franchiseID, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "franchise P&L failed")
		return
	}
	if pnl == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "franchise not found")
		return
	}
	writeJSON(w, http.StatusOK, pnl)
}

// GetConsolidatedPnL handles GET /api/v1/franchises/consolidated-pnl.
//
// Deliberately not reachable by a franchise-scoped role at all, rather than
// scoped down to one partner: a "consolidated P&L across all partners"
// containing exactly one partner is a misleading answer to the question
// asked. Route registration enforces this (ISP-wide roles only); this check
// is the second line, in case that registration is ever loosened.
//
// FR: FR-FRN-003, FR-FRN-004
func (h *Handler) GetConsolidatedPnL(w http.ResponseWriter, r *http.Request) {
	if h.franchises == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "franchise store not configured")
		return
	}
	if franchiseScopedRole(middleware.RoleFromContext(r.Context())) {
		writeError(w, http.StatusForbidden, "ERR_FORBIDDEN", "consolidated P&L is ISP-wide only")
		return
	}

	from, to, err := parseDateWindow(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	pnl, err := h.franchises.ListConsolidatedPnL(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "consolidated P&L failed")
		return
	}
	writeJSON(w, http.StatusOK, pnl)
}

// parseDateWindow reads the optional ?from= / ?to= date filters.
//
// A malformed date is rejected rather than ignored: silently dropping an
// unparseable filter would report the whole history as though it were the
// requested window, which reads as a correct answer to a different question.
func parseDateWindow(r *http.Request) (from, to *time.Time, err error) {
	const layout = "2006-01-02"
	if v := r.URL.Query().Get("from"); v != "" {
		t, parseErr := time.Parse(layout, v)
		if parseErr != nil {
			return nil, nil, errors.New("from must be a YYYY-MM-DD date")
		}
		from = &t
	}
	if v := r.URL.Query().Get("to"); v != "" {
		t, parseErr := time.Parse(layout, v)
		if parseErr != nil {
			return nil, nil, errors.New("to must be a YYYY-MM-DD date")
		}
		// The SQL filters created_at < to, so an inclusive end date has to be
		// advanced a day — otherwise ?to=2026-08-13 silently excludes
		// everything that happened on the 13th.
		end := t.AddDate(0, 0, 1)
		to = &end
	}
	if from != nil && to != nil && to.Before(*from) {
		return nil, nil, errors.New("to must not be earlier than from")
	}
	return from, to, nil
}
