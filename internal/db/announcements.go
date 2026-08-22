package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// AnnouncementStore serves staff broadcasts. Satisfies api.AnnouncementQuerier.
type AnnouncementStore struct{ pool dbPool }

var _ api.AnnouncementQuerier = (*AnnouncementStore)(nil)

const announcementColumns = `
	id, title, body, channels, class,
	segment_franchise_id, segment_plan_id, segment_status,
	show_in_portal, status, recipient_count, created_by_username,
	created_at, sent_at`

func scanAnnouncement(row interface{ Scan(dest ...any) error }) (*notifications.Announcement, error) {
	var a notifications.Announcement
	err := row.Scan(
		&a.ID, &a.Title, &a.Body, &a.Channels, &a.Class,
		&a.SegmentFranchiseID, &a.SegmentPlanID, &a.SegmentStatus,
		&a.ShowInPortal, &a.Status, &a.RecipientCount, &a.CreatedBy,
		&a.CreatedAt, &a.SentAt,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateAnnouncement persists a draft broadcast, and — when a.SubscriberIDs
// is set — the explicit recipient list alongside it, in one transaction so
// an announcement never exists without the recipients it was created for.
func (s *AnnouncementStore) CreateAnnouncement(ctx context.Context, a notifications.Announcement) (*notifications.Announcement, error) {
	const insertQ = `
		WITH ins AS (
			INSERT INTO announcements (
				title, body, channels, class,
				segment_franchise_id, segment_plan_id, segment_status,
				show_in_portal, created_by_username
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING *
		)
		SELECT ` + announcementColumns + ` FROM ins`

	if len(a.SubscriberIDs) == 0 {
		created, err := scanAnnouncement(s.pool.QueryRow(ctx, insertQ,
			a.Title, a.Body, a.Channels, a.Class,
			a.SegmentFranchiseID, a.SegmentPlanID, a.SegmentStatus,
			a.ShowInPortal, a.CreatedBy))
		if err != nil {
			return nil, fmt.Errorf("db: create announcement: %w", err)
		}
		return created, nil
	}

	var created *notifications.Announcement
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		created, err = scanAnnouncement(tx.QueryRow(ctx, insertQ,
			a.Title, a.Body, a.Channels, a.Class,
			a.SegmentFranchiseID, a.SegmentPlanID, a.SegmentStatus,
			a.ShowInPortal, a.CreatedBy))
		if err != nil {
			return fmt.Errorf("insert announcement: %w", err)
		}
		for _, subscriberID := range a.SubscriberIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO announcement_recipients (announcement_id, subscriber_id) VALUES ($1,$2)`,
				created.ID, subscriberID); err != nil {
				return fmt.Errorf("insert recipient %d: %w", subscriberID, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("db: create announcement with recipients: %w", err)
	}
	return created, nil
}

// GetAnnouncement loads one broadcast. A missing row returns (nil, nil).
func (s *AnnouncementStore) GetAnnouncement(ctx context.Context, id int) (*notifications.Announcement, error) {
	const q = `SELECT ` + announcementColumns + ` FROM announcements WHERE id = $1`
	a, err := scanAnnouncement(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get announcement %d: %w", id, err)
	}
	return a, nil
}

// ListAnnouncements returns broadcasts newest first, optionally by status.
func (s *AnnouncementStore) ListAnnouncements(ctx context.Context, status *string) ([]notifications.Announcement, error) {
	const q = `
		SELECT ` + announcementColumns + `
		FROM announcements
		WHERE ($1::text IS NULL OR status = $1)
		ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, q, status)
	if err != nil {
		return nil, fmt.Errorf("db: list announcements: %w", err)
	}
	defer rows.Close()

	var out []notifications.Announcement
	for rows.Next() {
		a, err := scanAnnouncement(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan announcement: %w", err)
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate announcements: %w", err)
	}
	return out, nil
}

// ListPortalAnnouncements returns the banners a subscriber should see.
//
// Scoped to sent announcements whose segment the subscriber actually falls
// in — a banner aimed at one franchise's customers must not appear on
// everyone's dashboard, which is the same isolation the notification fan-out
// applies.
func (s *AnnouncementStore) ListPortalAnnouncements(ctx context.Context, subscriberID int) ([]notifications.Announcement, error) {
	const q = `
		SELECT ` + announcementColumns + `
		FROM announcements a
		WHERE a.show_in_portal
		  AND a.status = 'sent'
		  AND EXISTS (
		      SELECT 1 FROM subscribers s
		      WHERE s.id = $1
		        AND (a.segment_franchise_id IS NULL OR s.franchise_id = a.segment_franchise_id)
		        AND (a.segment_plan_id      IS NULL OR s.plan_id      = a.segment_plan_id)
		        AND (a.segment_status       IS NULL OR s.status       = a.segment_status)
		  )
		ORDER BY a.created_at DESC
		LIMIT 20`

	rows, err := s.pool.Query(ctx, q, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list portal announcements: %w", err)
	}
	defer rows.Close()

	var out []notifications.Announcement
	for rows.Next() {
		a, err := scanAnnouncement(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan portal announcement: %w", err)
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate portal announcements: %w", err)
	}
	return out, nil
}

// ClaimAnnouncementForSending atomically moves a draft to 'sending'.
//
// The `AND status='draft'` predicate is what stops a double-click
// broadcasting the same announcement twice to the same segment — the same
// conditional-claim pattern used for approval decisions (MDS §4.15) and lead
// conversion (§4.16). Returns (nil, nil) when the claim did not land.
func (s *AnnouncementStore) ClaimAnnouncementForSending(ctx context.Context, id int) (*notifications.Announcement, error) {
	const q = `
		WITH upd AS (
			UPDATE announcements SET status = 'sending'
			WHERE id = $1 AND status = 'draft'
			RETURNING *
		)
		SELECT ` + announcementColumns + ` FROM upd`

	a, err := scanAnnouncement(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: claim announcement %d: %w", id, err)
	}
	return a, nil
}

// FinishAnnouncement records the outcome of a fan-out.
func (s *AnnouncementStore) FinishAnnouncement(ctx context.Context, id int, status string, recipientCount int) error {
	const q = `
		UPDATE announcements
		SET status = $2, recipient_count = $3, sent_at = NOW()
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id, status, recipientCount)
	if err != nil {
		return fmt.Errorf("db: finish announcement %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: announcement %d: %w", id, ErrNotFound)
	}
	return nil
}

// ListSegmentSubscriberIDs resolves an announcement to the subscriber ids it
// targets: an explicit recipient list (announcement_recipients) if one was
// given at creation, otherwise the franchise/plan/status segment filters.
//
// Each NULL filter means "no restriction on this dimension", so a segment
// announcement with all three unset addresses the whole base. Terminated
// subscribers are excluded unconditionally from the segment path: they have
// left, and a marketing broadcast to a former customer is at best noise and
// at worst a compliance problem. An explicit recipient list is not filtered
// the same way — the console picked those subscribers by hand, so their
// current status is the operator's call, not this query's.
func (s *AnnouncementStore) ListSegmentSubscriberIDs(ctx context.Context, announcementID int, franchiseID, planID *int, status *string) ([]int, error) {
	explicit, err := s.listExplicitRecipients(ctx, announcementID)
	if err != nil {
		return nil, err
	}
	if explicit != nil {
		return explicit, nil
	}

	const q = `
		SELECT id FROM subscribers
		WHERE status <> 'terminated'
		  AND ($1::int  IS NULL OR franchise_id = $1)
		  AND ($2::int  IS NULL OR plan_id      = $2)
		  AND ($3::text IS NULL OR status       = $3)
		ORDER BY id`

	rows, err := s.pool.Query(ctx, q, franchiseID, planID, status)
	if err != nil {
		return nil, fmt.Errorf("db: resolve announcement segment: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan segment subscriber: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate segment subscribers: %w", err)
	}
	return out, nil
}

// listExplicitRecipients returns an announcement's console-picked recipient
// list, or nil (not an empty, non-nil slice) when it has none — the caller
// uses that nil-ness to decide whether to fall back to the segment filters.
func (s *AnnouncementStore) listExplicitRecipients(ctx context.Context, announcementID int) ([]int, error) {
	const q = `SELECT subscriber_id FROM announcement_recipients WHERE announcement_id = $1 ORDER BY subscriber_id`

	rows, err := s.pool.Query(ctx, q, announcementID)
	if err != nil {
		return nil, fmt.Errorf("db: list announcement recipients for %d: %w", announcementID, err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan announcement recipient: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate announcement recipients: %w", err)
	}
	return out, nil
}
