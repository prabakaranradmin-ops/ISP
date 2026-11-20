package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Multi-tier fair-use policies and data top-ups — FR-CAT-003 | migration 067.
// The domain rules live in internal/fup/policy.go; storage in
// internal/db/fuppolicy.go.

// FUPPolicyStore manages policies, their tiers and subscribers' top-ups.
// Satisfied by *db.FUPStore.
type FUPPolicyStore interface {
	SaveFUPPolicy(ctx context.Context, p fup.Policy, by string) (*fup.Policy, error)
	SetFUPPolicyActive(ctx context.Context, id int, active bool, by string) error
	DeleteFUPPolicy(ctx context.Context, id int) error
	ListFUPPolicies(ctx context.Context, includeRetired bool) ([]fup.Policy, error)
	FUPPolicyByID(ctx context.Context, id int) (*fup.Policy, error)
	SetPlanFUPPolicy(ctx context.Context, planID int, policyID *int, by string) error

	AddDataTopup(ctx context.Context, t fup.Topup, by string) (*fup.Topup, error)
	ListDataTopups(ctx context.Context, subscriberID int) ([]fup.Topup, error)
	ActiveTopupBytes(ctx context.Context, subscriberID int) (int64, error)
}

// ErrFUPPoliciesUnwired is returned when no policy store is configured.
var ErrFUPPoliciesUnwired = errors.New("api: fair-use policies not configured")

// SaveFUPPolicy creates or replaces a policy.
//
// The audit detail carries every tier, because the tiers are the change and
// the current row only says what the policy looks like now: "which policy
// change throttled these customers harder last Tuesday" is answered from the
// trail.
func (h *Handler) SaveFUPPolicy(ctx context.Context, p fup.Policy) (*fup.Policy, error) {
	if h.fupPolicies == nil {
		return nil, ErrFUPPoliciesUnwired
	}
	saved, err := h.fupPolicies.SaveFUPPolicy(ctx, p, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	action := "fup_policy.created"
	if p.ID != 0 {
		action = "fup_policy.updated"
	}
	middleware.Audit(ctx, action, saved.Name, map[string]any{
		"policy_id":      saved.ID,
		"tiers":          auditTiers(saved.Tiers),
		"plans_affected": saved.InUseByPlans,
	})
	return saved, nil
}

func auditTiers(tiers []fup.Tier) []map[string]any {
	out := make([]map[string]any, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, map[string]any{
			"threshold_bytes": t.ThresholdBytes,
			"throttle_rate":   t.ThrottleRate,
		})
	}
	return out
}

// SetFUPPolicyActive retires or restores a policy.
func (h *Handler) SetFUPPolicyActive(ctx context.Context, id int, active bool) error {
	if h.fupPolicies == nil {
		return ErrFUPPoliciesUnwired
	}
	if err := h.fupPolicies.SetFUPPolicyActive(ctx, id, active, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	action := "fup_policy.retired"
	if active {
		action = "fup_policy.restored"
	}
	middleware.Audit(ctx, action, strconv.Itoa(id), nil)
	return nil
}

// DeleteFUPPolicy removes a policy no plan references.
func (h *Handler) DeleteFUPPolicy(ctx context.Context, id int) error {
	if h.fupPolicies == nil {
		return ErrFUPPoliciesUnwired
	}
	if err := h.fupPolicies.DeleteFUPPolicy(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "fup_policy.deleted", strconv.Itoa(id), nil)
	return nil
}

// ListFUPPolicies returns the policies with their tiers and plan counts.
func (h *Handler) ListFUPPolicies(ctx context.Context, includeRetired bool) ([]fup.Policy, error) {
	if h.fupPolicies == nil {
		return nil, ErrFUPPoliciesUnwired
	}
	return h.fupPolicies.ListFUPPolicies(ctx, includeRetired)
}

// SetPlanFUPPolicy attaches a policy to a plan, or clears it with a nil
// policyID so the plan evaluates against its own threshold again.
func (h *Handler) SetPlanFUPPolicy(ctx context.Context, planID int, policyID *int) error {
	if h.fupPolicies == nil {
		return ErrFUPPoliciesUnwired
	}
	if err := h.fupPolicies.SetPlanFUPPolicy(ctx, planID, policyID, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	detail := map[string]any{"plan_id": planID}
	if policyID == nil {
		detail["policy_id"] = nil
		middleware.Audit(ctx, "plan.fup_policy_cleared", strconv.Itoa(planID), detail)
		return nil
	}
	detail["policy_id"] = *policyID
	middleware.Audit(ctx, "plan.fup_policy_set", strconv.Itoa(planID), detail)
	return nil
}

// GrantDataTopup records a data top-up for a subscriber.
//
// Deliberately named Grant rather than Sell: this records the allowance, and
// the caller is responsible for having taken the money and for passing the
// resulting ledger id. Separating them means a failed payment cannot leave a
// top-up behind, and a staff goodwill grant — which has no payment — goes
// through the same path rather than a second one that could drift from it.
//
// The scanner picks it up within its 10-second tick and issues the CoA that
// restores the subscriber's speed; nothing here touches the NAS.
func (h *Handler) GrantDataTopup(ctx context.Context, subscriberID int, bytes int64, expires time.Time, transactionID *int) (*fup.Topup, error) {
	if h.fupPolicies == nil {
		return nil, ErrFUPPoliciesUnwired
	}
	by := middleware.SubjectFromContext(ctx)
	if by == "" {
		// Recorded on every row, because "who gave this customer 50GB" is
		// the first question asked when one appears unexplained.
		return nil, fmt.Errorf("api: a data top-up must record who granted it")
	}

	t, err := h.fupPolicies.AddDataTopup(ctx, fup.Topup{
		SubscriberID:  subscriberID,
		Bytes:         bytes,
		ExpiresAt:     expires,
		TransactionID: transactionID,
	}, by)
	if err != nil {
		return nil, err
	}

	detail := map[string]any{
		"topup_id":   t.ID,
		"bytes":      t.Bytes,
		"expires_at": t.ExpiresAt,
		// Whether money changed hands is the difference between a sale and
		// a goodwill grant, and the trail should not make them look alike.
		"paid": t.TransactionID != nil,
	}
	if t.TransactionID != nil {
		detail["transaction_id"] = *t.TransactionID
	}
	middleware.Audit(ctx, "subscriber.data_topup_granted", strconv.Itoa(subscriberID), detail)
	return t, nil
}

// ListDataTopups returns a subscriber's top-ups, expired ones included.
func (h *Handler) ListDataTopups(ctx context.Context, subscriberID int) ([]fup.Topup, error) {
	if h.fupPolicies == nil {
		return nil, ErrFUPPoliciesUnwired
	}
	return h.fupPolicies.ListDataTopups(ctx, subscriberID)
}

// ActiveTopupBytes is a subscriber's unexpired top-up total.
func (h *Handler) ActiveTopupBytes(ctx context.Context, subscriberID int) (int64, error) {
	if h.fupPolicies == nil {
		return 0, ErrFUPPoliciesUnwired
	}
	return h.fupPolicies.ActiveTopupBytes(ctx, subscriberID)
}
