package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Named subscriber groups — FR-GRP-001 | migration 069. The domain rules live
// in internal/revenue/groups.go; storage in internal/db/groups.go.

// SubscriberGroupStore manages the groups. Satisfied by *db.RevenueStore.
type SubscriberGroupStore interface {
	CreateSubscriberGroup(ctx context.Context, in revenue.SubscriberGroup, by string) (*revenue.SubscriberGroup, error)
	RenameSubscriberGroup(ctx context.Context, id int, in revenue.SubscriberGroup, by string) error
	SetSubscriberGroupActive(ctx context.Context, id int, active bool, by string) error
	DeleteSubscriberGroup(ctx context.Context, id int) error
	ListSubscriberGroups(ctx context.Context, includeRetired bool) ([]revenue.SubscriberGroup, error)
	AddToGroup(ctx context.Context, groupID int, subscriberIDs []int, by string) (int, error)
	RemoveFromGroup(ctx context.Context, groupID int, subscriberIDs []int) (int, error)
	GroupsForSubscriber(ctx context.Context, subscriberID int) ([]revenue.SubscriberGroup, error)
}

// ErrSubscriberGroupsUnwired is returned when no group store is configured.
var ErrSubscriberGroupsUnwired = errors.New("api: subscriber groups not configured")

// CreateSubscriberGroup adds a group.
func (h *Handler) CreateSubscriberGroup(ctx context.Context, in revenue.SubscriberGroup) (*revenue.SubscriberGroup, error) {
	if h.groups == nil {
		return nil, ErrSubscriberGroupsUnwired
	}
	g, err := h.groups.CreateSubscriberGroup(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "subscriber_group.created", g.Name,
		map[string]any{"group_id": g.ID, "description": g.Description})
	return g, nil
}

// RenameSubscriberGroup changes a group's name and description.
//
// Audited with the new name as the subject and the id in the detail: a group
// can be the segment of an announcement that has already been sent, so
// "which group was 'Anna Nagar' when that went out" is a question the trail
// has to be able to answer.
func (h *Handler) RenameSubscriberGroup(ctx context.Context, id int, in revenue.SubscriberGroup) error {
	if h.groups == nil {
		return ErrSubscriberGroupsUnwired
	}
	if err := h.groups.RenameSubscriberGroup(ctx, id, in, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	middleware.Audit(ctx, "subscriber_group.renamed", in.Normalise().Name,
		map[string]any{"group_id": id})
	return nil
}

// SetSubscriberGroupActive retires or restores a group.
func (h *Handler) SetSubscriberGroupActive(ctx context.Context, id int, active bool) error {
	if h.groups == nil {
		return ErrSubscriberGroupsUnwired
	}
	if err := h.groups.SetSubscriberGroupActive(ctx, id, active, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	action := "subscriber_group.retired"
	if active {
		action = "subscriber_group.restored"
	}
	middleware.Audit(ctx, action, strconv.Itoa(id), nil)
	return nil
}

// DeleteSubscriberGroup removes a group no announcement targets.
func (h *Handler) DeleteSubscriberGroup(ctx context.Context, id int) error {
	if h.groups == nil {
		return ErrSubscriberGroupsUnwired
	}
	if err := h.groups.DeleteSubscriberGroup(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "subscriber_group.deleted", strconv.Itoa(id), nil)
	return nil
}

// ListSubscriberGroups returns the groups with their member counts.
func (h *Handler) ListSubscriberGroups(ctx context.Context, includeRetired bool) ([]revenue.SubscriberGroup, error) {
	if h.groups == nil {
		return nil, ErrSubscriberGroupsUnwired
	}
	return h.groups.ListSubscriberGroups(ctx, includeRetired)
}

// AddToGroup puts subscribers in a group, reporting how many were actually
// added.
//
// Membership changes are audited, not only the group's own lifecycle. A group
// is what an announcement is addressed to, so adding somebody to one changes
// who future messages reach — and the count, not the ids, is what makes an
// accidental bulk add visible in the trail.
func (h *Handler) AddToGroup(ctx context.Context, groupID int, subscriberIDs []int) (int, error) {
	if h.groups == nil {
		return 0, ErrSubscriberGroupsUnwired
	}
	added, err := h.groups.AddToGroup(ctx, groupID, subscriberIDs, middleware.SubjectFromContext(ctx))
	if err != nil {
		return 0, err
	}
	middleware.Audit(ctx, "subscriber_group.members_added", strconv.Itoa(groupID),
		map[string]any{"requested": len(subscriberIDs), "added": added})
	return added, nil
}

// RemoveFromGroup takes subscribers out of a group.
func (h *Handler) RemoveFromGroup(ctx context.Context, groupID int, subscriberIDs []int) (int, error) {
	if h.groups == nil {
		return 0, ErrSubscriberGroupsUnwired
	}
	removed, err := h.groups.RemoveFromGroup(ctx, groupID, subscriberIDs)
	if err != nil {
		return 0, err
	}
	middleware.Audit(ctx, "subscriber_group.members_removed", strconv.Itoa(groupID),
		map[string]any{"requested": len(subscriberIDs), "removed": removed})
	return removed, nil
}

// GroupsForSubscriber returns the groups one subscriber is in.
func (h *Handler) GroupsForSubscriber(ctx context.Context, subscriberID int) ([]revenue.SubscriberGroup, error) {
	if h.groups == nil {
		return nil, ErrSubscriberGroupsUnwired
	}
	return h.groups.GroupsForSubscriber(ctx, subscriberID)
}
