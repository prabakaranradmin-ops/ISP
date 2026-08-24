package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// Hotspot administration — FR-HSP-001..002 | MDS §4.23.
//
// The staff half of the hotspot. The captive portal itself (internal/hotspot)
// is unauthenticated by necessity and can only redeem what was issued here;
// issuing, listing, voiding and MAC registration all sit behind the ordinary
// staff authorisation, because each of them creates or revokes network access.

// HotspotQuerier is the persistence surface for hotspot administration.
// Satisfied by *db.HotspotStore.
type HotspotQuerier interface {
	CreateVoucher(ctx context.Context, v hotspot.NewVoucher) (int, error)
	ListVouchers(ctx context.Context, f hotspot.VoucherFilter) ([]hotspot.Voucher, error)
	VoidVoucher(ctx context.Context, voucherID int) (bool, error)
	RegisterDevice(ctx context.Context, mac string, subscriberID int, label string, nasID *int) (int, error)
	DeactivateDevice(ctx context.Context, mac string) (bool, error)
	// GetVoucherCommissionSummary backs CRD-EXP-010's settlement reporting —
	// see internal/db/hotspot.go's own doc comment on why this lives
	// alongside vouchers rather than in the franchise P&L query.
	GetVoucherCommissionSummary(ctx context.Context, franchiseID int) (*hotspot.VoucherCommissionSummary, error)
}

// maxVoucherBatch caps one generation request.
//
// Bounded because each voucher is a row insert and the whole batch's plaintext
// is held in memory to be returned exactly once; an unbounded count would let a
// single request print a hundred thousand codes nobody asked for and could not
// distribute.
const maxVoucherBatch = 500

type createVoucherBatchRequest struct {
	PlanID          int `json:"plan_id"`
	Count           int `json:"count"`
	DurationMinutes int `json:"duration_minutes"`
	// Volume allowance in bytes; 0 means unlimited. Enforced by the quota
	// scanner (migration 035), which ends the session on exhaustion rather than
	// throttling it — a voucher is prepaid for a fixed volume, and a crawl
	// after it runs out reads as a broken network rather than a spent voucher.
	DataCapBytes int64  `json:"data_cap_bytes,omitempty"`
	FranchiseID  *int   `json:"franchise_id,omitempty"`
	ValidForDays *int   `json:"valid_for_days,omitempty"`
	BatchRef     string `json:"batch_ref,omitempty"`
	// SaleAmount is what each voucher in this batch sells for — a decimal
	// string, omitted or "0" for a free voucher. Only meaningful alongside
	// FranchiseID: a commission is credited at redemption (CRD-EXP-010)
	// only when both a franchise and a positive sale amount are present.
	SaleAmount string `json:"sale_amount,omitempty"`
}

// CreateVoucherBatch handles POST /api/v1/hotspot/vouchers.
//
// Returns every plaintext code once, in the response body and nowhere else —
// storage holds only the hash. An operator prints this response; if they lose
// it, the batch is voided and reissued rather than recovered.
func (h *Handler) CreateVoucherBatch(w http.ResponseWriter, r *http.Request) {
	if h.hotspot == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "hotspot is not configured")
		return
	}

	var req createVoucherBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := validateVoucherBatch(req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	var expiresAt *time.Time
	if req.ValidForDays != nil && *req.ValidForDays > 0 {
		t := time.Now().AddDate(0, 0, *req.ValidForDays)
		expiresAt = &t
	}
	batchRef := req.BatchRef
	if batchRef == "" {
		// Generated rather than left empty so a batch is always reconcilable:
		// "which sheet did this voucher come from" is the first question asked
		// when a printed stack goes missing.
		batchRef = "batch-" + time.Now().UTC().Format("20060102T150405")
	}
	createdBy := middleware.SubjectFromContext(r.Context())

	codes := make([]string, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		generated, err := hotspot.GenerateCode()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not generate voucher codes")
			return
		}
		_, err = h.hotspot.CreateVoucher(r.Context(), hotspot.NewVoucher{
			CodeHash:        generated.Hash,
			CodePrefix:      generated.Prefix,
			PlanID:          req.PlanID,
			FranchiseID:     req.FranchiseID,
			DurationMinutes: req.DurationMinutes,
			DataCapBytes:    req.DataCapBytes,
			ExpiresAt:       expiresAt,
			BatchRef:        batchRef,
			CreatedBy:       createdBy,
			SaleAmount:      req.SaleAmount,
		})
		if err != nil {
			// Partial batches are reported rather than rolled back: the codes
			// already stored are valid and will be redeemed, so pretending the
			// request failed entirely would leave live vouchers the operator
			// never saw and cannot account for.
			writeError(w, http.StatusInternalServerError, "ERR_INTERNAL",
				fmt.Sprintf("generated %d of %d vouchers before failing; batch_ref %q", len(codes), req.Count, batchRef))
			return
		}
		codes = append(codes, generated.Plaintext)
	}

	middleware.Audit(r.Context(), "hotspot.vouchers_created", batchRef, map[string]any{
		"count": req.Count, "plan_id": req.PlanID, "duration_minutes": req.DurationMinutes,
	})

	writeJSON(w, http.StatusCreated, map[string]any{
		"batch_ref":  batchRef,
		"count":      len(codes),
		"codes":      codes,
		"expires_at": expiresAt,
		"warning":    "This is the only time these codes are shown. Print or export them now; they cannot be recovered.",
	})
}

func validateVoucherBatch(req createVoucherBatchRequest) error {
	if req.PlanID <= 0 {
		return fmt.Errorf("plan_id is required")
	}
	if req.Count <= 0 || req.Count > maxVoucherBatch {
		return fmt.Errorf("count must be between 1 and %d", maxVoucherBatch)
	}
	if req.DurationMinutes <= 0 {
		return fmt.Errorf("duration_minutes must be greater than zero")
	}
	if req.DataCapBytes < 0 {
		return fmt.Errorf("data_cap_bytes cannot be negative")
	}
	if req.SaleAmount != "" {
		amount, err := decimal.NewFromString(req.SaleAmount)
		if err != nil || amount.IsNegative() {
			return fmt.Errorf("sale_amount must be a non-negative decimal")
		}
	}
	return nil
}

// ListVouchers handles GET /api/v1/hotspot/vouchers.
func (h *Handler) ListVouchers(w http.ResponseWriter, r *http.Request) {
	if h.hotspot == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "hotspot is not configured")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) //nolint:errcheck // absent/invalid falls back to the store's default
	vouchers, err := h.hotspot.ListVouchers(r.Context(), hotspot.VoucherFilter{
		BatchRef: r.URL.Query().Get("batch_ref"),
		Status:   r.URL.Query().Get("status"),
		Limit:    limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list vouchers failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vouchers": vouchers})
}

// VoidVoucher handles DELETE /api/v1/hotspot/vouchers/{id}.
func (h *Handler) VoidVoucher(w http.ResponseWriter, r *http.Request) {
	if h.hotspot == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "hotspot is not configured")
		return
	}
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "id must be numeric")
		return
	}

	voided, err := h.hotspot.VoidVoucher(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "void failed")
		return
	}
	if !voided {
		writeError(w, http.StatusConflict, "ERR_CONFLICT",
			"no unused voucher with that id — an already-redeemed voucher is revoked by revoking its grant")
		return
	}

	middleware.Audit(r.Context(), "hotspot.voucher_voided", strconv.Itoa(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"voided": true, "voucher_id": id})
}

// ── MAC registration (FR-HSP-002) ───────────────────────────────────────────

type registerDeviceRequest struct {
	MACAddress   string `json:"mac_address"`
	SubscriberID int    `json:"subscriber_id"`
	Label        string `json:"label,omitempty"`
	NASID        *int   `json:"nas_id,omitempty"`
}

// RegisterHotspotDevice handles POST /api/v1/hotspot/devices — binding a MAC
// to a subscriber so MAC Auth Bypass will authenticate it.
func (h *Handler) RegisterHotspotDevice(w http.ResponseWriter, r *http.Request) {
	if h.hotspot == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "hotspot is not configured")
		return
	}

	var req registerDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// Normalised with the same function the RADIUS daemon uses on the incoming
	// User-Name. If these two ever disagreed, an operator could register a MAC
	// in a spelling the authenticator never looks up, and the device would be
	// refused for reasons invisible from the admin UI.
	mac, ok := radius.NormaliseMAC(req.MACAddress)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"mac_address must be a MAC address (any common separator)")
		return
	}
	if req.SubscriberID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id is required")
		return
	}

	id, err := h.hotspot.RegisterDevice(r.Context(), mac, req.SubscriberID, req.Label, req.NASID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not register the device")
		return
	}

	middleware.Audit(r.Context(), "hotspot.device_registered", mac, map[string]any{
		"subscriber_id": req.SubscriberID, "nas_id": req.NASID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "mac_address": mac, "subscriber_id": req.SubscriberID,
	})
}

// DeactivateHotspotDevice handles DELETE /api/v1/hotspot/devices/{mac} — a
// lost or stolen phone that must stop authenticating.
func (h *Handler) DeactivateHotspotDevice(w http.ResponseWriter, r *http.Request) {
	if h.hotspot == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "hotspot is not configured")
		return
	}
	mac, ok := radius.NormaliseMAC(r.PathValue("mac"))
	if !ok {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid MAC address")
		return
	}

	deactivated, err := h.hotspot.DeactivateDevice(r.Context(), mac)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "deactivate failed")
		return
	}
	if !deactivated {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no active device with that MAC")
		return
	}

	middleware.Audit(r.Context(), "hotspot.device_deactivated", mac, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deactivated": true, "mac_address": mac})
}

// GetVoucherCommissions handles GET /api/v1/hotspot/vouchers/commissions?franchise_id=.
// ISP-wide staff only (mounted behind admin, matching Franchises' own
// owner/billing_admin gate) — a franchise partner's own view of this same
// data is served directly by the console (internal/staffui/franchise.go's
// MyPnL), not through this route.
func (h *Handler) GetVoucherCommissions(w http.ResponseWriter, r *http.Request) {
	if h.hotspot == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "hotspot is not configured")
		return
	}
	franchiseID, err := strconv.Atoi(r.URL.Query().Get("franchise_id"))
	if err != nil || franchiseID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "franchise_id is required")
		return
	}
	summary, err := h.hotspot.GetVoucherCommissionSummary(r.Context(), franchiseID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "voucher commission summary failed")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
