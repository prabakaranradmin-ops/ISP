package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Named subscriber groups — FR-GRP-001 | migration 069.
// The domain rules live in internal/revenue/groups.go.

const groupColumns = `
	g.id, g.name, COALESCE(g.description, ''), g.active,
	COALESCE(g.created_by, ''), g.created_at, COALESCE(g.updated_by, ''), g.updated_at`

// memberCount counts a group's members excluding those who have left, so a
// group's size is the reach a message to it would actually have.
const memberCount = `
	(SELECT count(*) FROM subscriber_group_members m
	   JOIN subscribers s ON s.id = m.subscriber_id
	  WHERE m.group_id = g.id AND s.status <> 'terminated')`

func scanGroup(row pgx.Row, g *revenue.SubscriberGroup) error {
	return row.Scan(&g.ID, &g.Name, &g.Description, &g.Active,
		&g.CreatedBy, &g.CreatedAt, &g.UpdatedBy, &g.UpdatedAt, &g.Members)
}

func groupNameTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "uq_subscriber_group_name_active" {
		return revenue.ErrGroupNameTaken
	}
	return err
}

// CreateSubscriberGroup adds a group.
func (s *RevenueStore) CreateSubscriberGroup(ctx context.Context, in revenue.SubscriberGroup, by string) (*revenue.SubscriberGroup, error) {
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO subscriber_groups (name, description, created_by)
		VALUES ($1, NULLIF($2, ''), $3)
		RETURNING id, name, COALESCE(description, ''), active,
		          COALESCE(created_by, ''), created_at, COALESCE(updated_by, ''), updated_at, 0`

	var g revenue.SubscriberGroup
	if err := scanGroup(s.pool.QueryRow(ctx, q, in.Name, in.Description, by), &g); err != nil {
		return nil, fmt.Errorf("db: create subscriber group: %w", groupNameTaken(err))
	}
	return &g, nil
}

// RenameSubscriberGroup changes a group's name and description.
func (s *RevenueStore) RenameSubscriberGroup(ctx context.Context, id int, in revenue.SubscriberGroup, by string) error {
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscriber_groups
		   SET name = $2, description = NULLIF($3, ''), updated_by = $4, updated_at = NOW()
		 WHERE id = $1`, id, in.Name, in.Description, by)
	if err != nil {
		return fmt.Errorf("db: rename subscriber group %d: %w", id, groupNameTaken(err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: subscriber group %d not found", id)
	}
	return nil
}

// SetSubscriberGroupActive retires or restores a group.
//
// Retiring takes it out of the filters and the segment dropdown and leaves
// its memberships and any announcement that targets it alone — which is why
// this exists rather than only a delete.
func (s *RevenueStore) SetSubscriberGroupActive(ctx context.Context, id int, active bool, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscriber_groups SET active = $2, updated_by = $3, updated_at = NOW()
		 WHERE id = $1`, id, active, by)
	if err != nil {
		return fmt.Errorf("db: set subscriber group %d active=%v: %w", id, active, groupNameTaken(err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: subscriber group %d not found", id)
	}
	return nil
}

// DeleteSubscriberGroup removes a group no announcement targets.
//
// The count is read first only so the caller can be told to retire it
// instead of being handed a foreign-key violation; ON DELETE RESTRICT
// refuses it either way. Memberships go with the group (CASCADE) — they
// describe it and mean nothing without it.
func (s *RevenueStore) DeleteSubscriberGroup(ctx context.Context, id int) error {
	var targeted int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM announcements WHERE segment_group_id = $1`, id).Scan(&targeted); err != nil {
		return fmt.Errorf("db: count announcements targeting group %d: %w", id, err)
	}
	if targeted > 0 {
		return fmt.Errorf("%w: %d announcement(s)", revenue.ErrGroupInUse, targeted)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM subscriber_groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: delete subscriber group %d: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: subscriber group %d not found", id)
	}
	return nil
}

// ListSubscriberGroups returns the groups with their member counts.
func (s *RevenueStore) ListSubscriberGroups(ctx context.Context, includeRetired bool) ([]revenue.SubscriberGroup, error) {
	q := `SELECT ` + groupColumns + `, ` + memberCount + ` FROM subscriber_groups g`
	if !includeRetired {
		q += ` WHERE g.active`
	}
	q += ` ORDER BY g.active DESC, lower(btrim(g.name))`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list subscriber groups: %w", err)
	}
	defer rows.Close()

	var out []revenue.SubscriberGroup
	for rows.Next() {
		var g revenue.SubscriberGroup
		if err := scanGroup(rows, &g); err != nil {
			return nil, fmt.Errorf("db: scan subscriber group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddToGroup puts subscribers in a group, ignoring those already in it.
//
// Idempotent on the pair, which the primary key defines: adding somebody
// twice is the same membership, and a second row would double them in every
// count and every fan-out. Reported as the number actually added, so the
// console can say "3 added, 2 were already in it" rather than implying five
// changes.
//
// The SELECT from subscribers is what filters out ids that do not exist: an
// INSERT straight from the array would fail the whole batch on one bad id,
// and a paste of twenty ids with one typo should add the nineteen.
func (s *RevenueStore) AddToGroup(ctx context.Context, groupID int, subscriberIDs []int, by string) (int, error) {
	if len(subscriberIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO subscriber_group_members (group_id, subscriber_id, added_by)
		SELECT $1, id, $3 FROM subscribers WHERE id = ANY($2::int[])
		ON CONFLICT (group_id, subscriber_id) DO NOTHING`, groupID, subscriberIDs, by)
	if err != nil {
		return 0, fmt.Errorf("db: add %d subscriber(s) to group %d: %w", len(subscriberIDs), groupID, err)
	}
	return int(tag.RowsAffected()), nil
}

// RemoveFromGroup takes subscribers out of a group, reporting how many were
// actually in it.
func (s *RevenueStore) RemoveFromGroup(ctx context.Context, groupID int, subscriberIDs []int) (int, error) {
	if len(subscriberIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM subscriber_group_members
		 WHERE group_id = $1 AND subscriber_id = ANY($2::int[])`, groupID, subscriberIDs)
	if err != nil {
		return 0, fmt.Errorf("db: remove from group %d: %w", groupID, err)
	}
	return int(tag.RowsAffected()), nil
}

// GroupsForSubscriber returns the groups one subscriber is in, for their own
// page.
//
// Retired groups are included: the subscriber is still in them, and hiding
// that would make a membership look removed when it is not — which is the
// sort of discrepancy that gets noticed only when a message reaches somebody
// nobody expected.
func (s *RevenueStore) GroupsForSubscriber(ctx context.Context, subscriberID int) ([]revenue.SubscriberGroup, error) {
	const q = `
		SELECT ` + groupColumns + `, ` + memberCount + `
		  FROM subscriber_groups g
		  JOIN subscriber_group_members m ON m.group_id = g.id
		 WHERE m.subscriber_id = $1
		 ORDER BY g.active DESC, lower(btrim(g.name))`

	rows, err := s.pool.Query(ctx, q, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: groups for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	var out []revenue.SubscriberGroup
	for rows.Next() {
		var g revenue.SubscriberGroup
		if err := scanGroup(rows, &g); err != nil {
			return nil, fmt.Errorf("db: scan subscriber group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupMemberIDs returns the subscriber ids in a group, excluding those who
// have left. Used by the announcement fan-out and the list filter, which
// must agree on who is in a group.
func (s *RevenueStore) GroupMemberIDs(ctx context.Context, groupID int) ([]int, error) {
	const q = `
		SELECT m.subscriber_id
		  FROM subscriber_group_members m
		  JOIN subscribers s ON s.id = m.subscriber_id
		 WHERE m.group_id = $1 AND s.status <> 'terminated'
		 ORDER BY m.subscriber_id`

	rows, err := s.pool.Query(ctx, q, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: group %d members: %w", groupID, err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan group member: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
