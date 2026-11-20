package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004 | migration 066.
// The dictionary lives in internal/nas/dictionary.go; storage in
// internal/db/replyattrs.go.

// ReplyAttributeStore manages a plan's extra reply attributes. Satisfied by
// *db.NASStore.
type ReplyAttributeStore interface {
	SetPlanReplyAttribute(ctx context.Context, in nas.ReplyAttribute, by string) (*nas.ReplyAttribute, error)
	DeletePlanReplyAttribute(ctx context.Context, id int) error
	SetPlanReplyAttributeEnabled(ctx context.Context, id int, enabled bool, by string) error
	ListPlanReplyAttributes(ctx context.Context, planID int) ([]nas.ReplyAttribute, error)
}

// ErrReplyAttributesUnwired is returned when no store is configured.
var ErrReplyAttributesUnwired = errors.New("api: plan reply attributes not configured")

// SetPlanReplyAttribute adds or corrects one attribute on a plan.
//
// The audit detail carries the value, because the value is the change: "who
// put this plan's subscribers in the wrong address list" is answered from
// the trail, and the current row only says where they are now.
func (h *Handler) SetPlanReplyAttribute(ctx context.Context, in nas.ReplyAttribute) (*nas.ReplyAttribute, error) {
	if h.replyAttrs == nil {
		return nil, ErrReplyAttributesUnwired
	}
	a, err := h.replyAttrs.SetPlanReplyAttribute(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "plan.reply_attribute_set", a.Name, map[string]any{
		"plan_id":   a.PlanID,
		"vendor":    string(a.Vendor),
		"attribute": a.Name,
		"value":     a.Value,
		"enabled":   a.Enabled,
	})
	return a, nil
}

// DeletePlanReplyAttribute removes one attribute from a plan.
func (h *Handler) DeletePlanReplyAttribute(ctx context.Context, id int) error {
	if h.replyAttrs == nil {
		return ErrReplyAttributesUnwired
	}
	if err := h.replyAttrs.DeletePlanReplyAttribute(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "plan.reply_attribute_deleted", strconv.Itoa(id), nil)
	return nil
}

// SetPlanReplyAttributeEnabled turns one attribute on or off.
//
// A separate action from deleting it, and separately audited: turning an
// attribute off to see whether it is causing something is a different act
// from deciding a plan should not carry it, and the trail should not
// conflate the two.
func (h *Handler) SetPlanReplyAttributeEnabled(ctx context.Context, id int, enabled bool) error {
	if h.replyAttrs == nil {
		return ErrReplyAttributesUnwired
	}
	if err := h.replyAttrs.SetPlanReplyAttributeEnabled(ctx, id, enabled, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	action := "plan.reply_attribute_disabled"
	if enabled {
		action = "plan.reply_attribute_enabled"
	}
	middleware.Audit(ctx, action, strconv.Itoa(id), nil)
	return nil
}

// ListPlanReplyAttributes returns every attribute on a plan, enabled or not.
func (h *Handler) ListPlanReplyAttributes(ctx context.Context, planID int) ([]nas.ReplyAttribute, error) {
	if h.replyAttrs == nil {
		return nil, ErrReplyAttributesUnwired
	}
	return h.replyAttrs.ListPlanReplyAttributes(ctx, planID)
}
