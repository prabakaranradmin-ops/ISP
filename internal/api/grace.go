package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Per-subscriber grace (FR-BIL-016) and dunning timing (FR-BIL-017) —
// migration 055.

// DunningSettingsStore holds the ladder timing and grace extensions.
// Satisfied by *db.BillingStore.
type DunningSettingsStore interface {
	DunningPolicy(ctx context.Context) (billing.DunningPolicy, error)
	SetDunningPolicy(ctx context.Context, p billing.DunningPolicy, by string) error
	SetGraceExtension(ctx context.Context, subscriberID int, until time.Time, reason, by string) error
	ClearGraceExtension(ctx context.Context, subscriberID int) error
}

// MaxGraceExtension bounds a single extension. Longer than a month is no
// longer "a few more days to pay" but free service, which belongs to a
// different decision (an adjustment or a plan change), not this one.
const MaxGraceExtension = 30 * 24 * time.Hour

// ErrDunningUnwired is returned when the settings store is not configured.
var ErrDunningUnwired = errors.New("api: dunning settings not configured")

// ExtendGrace keeps a subscriber who is past expiry in grace — service on —
// until the given time, whatever the ladder would otherwise do. The dunning
// scanner applies it on its next pass.
func (h *Handler) ExtendGrace(ctx context.Context, subscriberID int, until time.Time, reason string) error {
	if h.dunning == nil {
		return ErrDunningUnwired
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: give a reason; it is kept with the extension", billing.ErrGraceInvalid)
	}
	now := time.Now()
	if !until.After(now) {
		return fmt.Errorf("%w: the extension must end in the future", billing.ErrGraceInvalid)
	}
	if until.Sub(now) > MaxGraceExtension {
		return fmt.Errorf("%w: an extension can last at most 30 days", billing.ErrGraceInvalid)
	}
	by := middleware.SubjectFromContext(ctx)
	if err := h.dunning.SetGraceExtension(ctx, subscriberID, until, reason, by); err != nil {
		return err
	}
	middleware.Audit(ctx, "subscriber.grace_extended", strconv.Itoa(subscriberID), map[string]any{
		"until": until.Format(time.RFC3339), "reason": reason,
	})
	return nil
}

// EndGrace removes a subscriber's grace extension.
func (h *Handler) EndGrace(ctx context.Context, subscriberID int) error {
	if h.dunning == nil {
		return ErrDunningUnwired
	}
	if err := h.dunning.ClearGraceExtension(ctx, subscriberID); err != nil {
		return err
	}
	middleware.Audit(ctx, "subscriber.grace_ended", strconv.Itoa(subscriberID), nil)
	return nil
}

// CurrentDunningPolicy returns the ladder timing in force.
func (h *Handler) CurrentDunningPolicy(ctx context.Context) (billing.DunningPolicy, error) {
	if h.dunning == nil {
		return billing.DunningPolicy{}, ErrDunningUnwired
	}
	return h.dunning.DunningPolicy(ctx)
}

// SaveDunningPolicy validates and stores new ladder timing. It applies from
// the scanner's next hourly pass.
func (h *Handler) SaveDunningPolicy(ctx context.Context, p billing.DunningPolicy) error {
	if h.dunning == nil {
		return ErrDunningUnwired
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if err := h.dunning.SetDunningPolicy(ctx, p, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	middleware.Audit(ctx, "billing.dunning_policy_changed", "billing_settings", map[string]any{
		"remind_first_days": p.RemindFirstDays, "remind_second_days": p.RemindSecondDays,
		"remind_last_days": p.RemindLastDays, "grace_days": p.GraceDays, "soft_suspend_days": p.SoftSuspendDays,
	})
	return nil
}

type graceRequest struct {
	Until  string `json:"until"` // RFC 3339, or YYYY-MM-DD meaning the end of that day
	Reason string `json:"reason"`
}

// parseGraceUntil accepts a timestamp or a date; a bare date means the end
// of that day in local time, which is what "grace until Friday" means.
func parseGraceUntil(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02", v, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: until must be a date (YYYY-MM-DD) or an RFC 3339 time", billing.ErrGraceInvalid)
	}
	return d.Add(24*time.Hour - time.Second), nil
}

// ExtendGraceHTTP handles POST /api/v1/subscribers/{id}/grace.
func (h *Handler) ExtendGraceHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var req graceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	until, err := parseGraceUntil(req.Until)
	if err == nil {
		err = h.ExtendGrace(r.Context(), id, until, req.Reason)
	}
	writeGraceResult(w, err, map[string]any{"subscriber_id": id, "grace_until": until})
}

// EndGraceHTTP handles DELETE /api/v1/subscribers/{id}/grace.
func (h *Handler) EndGraceHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	writeGraceResult(w, h.EndGrace(r.Context(), id), map[string]any{"subscriber_id": id, "grace_until": nil})
}

func writeGraceResult(w http.ResponseWriter, err error, ok map[string]any) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, ok)
	case billing.GraceInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", billing.GraceInvalidReason(err))
	case errors.Is(err, billing.ErrGraceNotApplicable):
		writeError(w, http.StatusConflict, "ERR_NOT_IN_DUNNING", billing.ErrGraceNotApplicable.Error())
	case errors.Is(err, ErrDunningUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "dunning settings not configured")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not change the grace extension")
	}
}

// GetDunningPolicyHTTP handles GET /api/v1/billing/dunning-policy.
func (h *Handler) GetDunningPolicyHTTP(w http.ResponseWriter, r *http.Request) {
	p, err := h.CurrentDunningPolicy(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "dunning settings not available")
		return
	}
	writeJSON(w, http.StatusOK, dunningPolicyJSON(p))
}

type dunningPolicyBody struct {
	RemindFirstDays  int `json:"remind_first_days"`
	RemindSecondDays int `json:"remind_second_days"`
	RemindLastDays   int `json:"remind_last_days"`
	GraceDays        int `json:"grace_days"`
	SoftSuspendDays  int `json:"soft_suspend_days"`
}

func dunningPolicyJSON(p billing.DunningPolicy) map[string]any {
	return map[string]any{
		"remind_first_days": p.RemindFirstDays, "remind_second_days": p.RemindSecondDays,
		"remind_last_days": p.RemindLastDays, "grace_days": p.GraceDays,
		"soft_suspend_days": p.SoftSuspendDays, "updated_by": p.UpdatedBy, "updated_at": p.UpdatedAt,
	}
}

// PutDunningPolicyHTTP handles PUT /api/v1/billing/dunning-policy.
func (h *Handler) PutDunningPolicyHTTP(w http.ResponseWriter, r *http.Request) {
	var b dunningPolicyBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	p := billing.DunningPolicy{RemindFirstDays: b.RemindFirstDays, RemindSecondDays: b.RemindSecondDays,
		RemindLastDays: b.RemindLastDays, GraceDays: b.GraceDays, SoftSuspendDays: b.SoftSuspendDays}
	err := h.SaveDunningPolicy(r.Context(), p)
	switch {
	case errors.Is(err, billing.ErrPolicyInvalid):
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", strings.TrimPrefix(err.Error(), billing.ErrPolicyInvalid.Error()+": "))
	case errors.Is(err, ErrDunningUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "dunning settings not configured")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not save the dunning settings")
	default:
		writeJSON(w, http.StatusOK, dunningPolicyJSON(p))
	}
}
