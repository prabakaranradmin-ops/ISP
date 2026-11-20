package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Reusable bandwidth profiles — FR-CAT-002 | migration 065. The domain rules
// (rendering, validation) live in internal/nas/bandwidth.go; storage in
// internal/db/bandwidth.go.

// BandwidthProfileStore manages the profiles. Satisfied by *db.NASStore.
type BandwidthProfileStore interface {
	CreateBandwidthProfile(ctx context.Context, in nas.BandwidthProfile, by string) (*nas.BandwidthProfile, error)
	UpdateBandwidthProfile(ctx context.Context, id int, in nas.BandwidthProfile, by string) (*nas.BandwidthProfile, error)
	SetBandwidthProfileActive(ctx context.Context, id int, active bool, by string) error
	DeleteBandwidthProfile(ctx context.Context, id int) error
	ListBandwidthProfiles(ctx context.Context, includeRetired bool) ([]nas.BandwidthProfile, error)
	BandwidthProfileByID(ctx context.Context, id int) (*nas.BandwidthProfile, error)
	SetPlanBandwidthProfile(ctx context.Context, planID int, profileID *int, by string) error
}

// ErrBandwidthProfilesUnwired is returned when no profile store is configured.
var ErrBandwidthProfilesUnwired = errors.New("api: bandwidth profiles not configured")

// CreateBandwidthProfile adds a profile.
func (h *Handler) CreateBandwidthProfile(ctx context.Context, in nas.BandwidthProfile) (*nas.BandwidthProfile, error) {
	if h.bandwidth == nil {
		return nil, ErrBandwidthProfilesUnwired
	}
	p, err := h.bandwidth.CreateBandwidthProfile(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "bandwidth_profile.created", p.Name, auditDetail(p))
	return p, nil
}

// UpdateBandwidthProfile changes a profile in place.
//
// The audit detail carries the rendered attribute rather than only the
// numbers, because the rendered string is what a NAS actually receives — and
// "what speed was this customer getting last Tuesday" is answered from the
// trail, not from the current row.
func (h *Handler) UpdateBandwidthProfile(ctx context.Context, id int, in nas.BandwidthProfile) (*nas.BandwidthProfile, error) {
	if h.bandwidth == nil {
		return nil, ErrBandwidthProfilesUnwired
	}
	p, err := h.bandwidth.UpdateBandwidthProfile(ctx, id, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	detail := auditDetail(p)
	// How many subscribers this re-shapes is the consequence worth
	// recording: editing a profile changes what every plan on it delivers.
	detail["plans_affected"] = p.InUseByPlans
	middleware.Audit(ctx, "bandwidth_profile.updated", p.Name, detail)
	return p, nil
}

// SetBandwidthProfileActive retires or restores a profile.
func (h *Handler) SetBandwidthProfileActive(ctx context.Context, id int, active bool) error {
	if h.bandwidth == nil {
		return ErrBandwidthProfilesUnwired
	}
	if err := h.bandwidth.SetBandwidthProfileActive(ctx, id, active, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	action := "bandwidth_profile.retired"
	if active {
		action = "bandwidth_profile.restored"
	}
	middleware.Audit(ctx, action, strconv.Itoa(id), nil)
	return nil
}

// DeleteBandwidthProfile removes a profile no plan references.
func (h *Handler) DeleteBandwidthProfile(ctx context.Context, id int) error {
	if h.bandwidth == nil {
		return ErrBandwidthProfilesUnwired
	}
	if err := h.bandwidth.DeleteBandwidthProfile(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "bandwidth_profile.deleted", strconv.Itoa(id), nil)
	return nil
}

// ListBandwidthProfiles returns the profiles with their plan counts.
func (h *Handler) ListBandwidthProfiles(ctx context.Context, includeRetired bool) ([]nas.BandwidthProfile, error) {
	if h.bandwidth == nil {
		return nil, ErrBandwidthProfilesUnwired
	}
	return h.bandwidth.ListBandwidthProfiles(ctx, includeRetired)
}

// BandwidthProfileByID reads one profile.
func (h *Handler) BandwidthProfileByID(ctx context.Context, id int) (*nas.BandwidthProfile, error) {
	if h.bandwidth == nil {
		return nil, ErrBandwidthProfilesUnwired
	}
	return h.bandwidth.BandwidthProfileByID(ctx, id)
}

// SetPlanBandwidthProfile attaches a profile to a plan, or clears it with a
// nil profileID so the plan shapes from its own rate_limit_string again.
func (h *Handler) SetPlanBandwidthProfile(ctx context.Context, planID int, profileID *int) error {
	if h.bandwidth == nil {
		return ErrBandwidthProfilesUnwired
	}
	if err := h.bandwidth.SetPlanBandwidthProfile(ctx, planID, profileID, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	detail := map[string]any{"plan_id": planID}
	if profileID == nil {
		// Worth distinguishing in the trail: a cleared profile sends the
		// plan back to its legacy rate_limit_string, which is a different
		// speed from whatever the profile was rendering.
		detail["profile_id"] = nil
		middleware.Audit(ctx, "plan.bandwidth_profile_cleared", strconv.Itoa(planID), detail)
		return nil
	}
	detail["profile_id"] = *profileID
	middleware.Audit(ctx, "plan.bandwidth_profile_set", strconv.Itoa(planID), detail)
	return nil
}

// auditDetail records both the numbers and the attribute they render to.
func auditDetail(p *nas.BandwidthProfile) map[string]any {
	d := map[string]any{
		"profile_id":   p.ID,
		"max_down_bps": p.MaxDownBPS,
		"max_up_bps":   p.MaxUpBPS,
		"min_down_bps": p.MinDownBPS,
		"min_up_bps":   p.MinUpBPS,
		"priority":     p.Priority,
	}
	if p.HasBurst() {
		d["burst_down_bps"] = p.BurstDownBPS
		d["burst_up_bps"] = p.BurstUpBPS
		d["burst_threshold_down"] = p.BurstThresholdDown
		d["burst_threshold_up"] = p.BurstThresholdUp
		d["burst_time_seconds"] = p.BurstTimeSeconds
	}
	// A render failure is not worth failing an audit entry over — the write
	// already succeeded — but the trail should say so rather than omit the
	// field silently.
	if s, err := p.MikrotikRateLimit(); err == nil {
		d["rate_limit"] = s
	} else {
		d["rate_limit_error"] = err.Error()
	}
	return d
}
