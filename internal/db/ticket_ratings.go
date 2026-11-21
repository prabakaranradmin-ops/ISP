package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// Subscriber ratings on resolved tickets — FR-SUP-004 | migration 070.
// The rules live in internal/tickets/rating.go.

// RateTicket records a subscriber's rating of their own resolved ticket.
//
// Ownership and status are both in the UPDATE's WHERE clause, so the check
// and the write are one statement and cannot be separated by a status change
// in between — a ticket reopened a moment after being read would otherwise
// be rated as resolved.
//
// When nothing is updated the row is read back to say *why*, because the
// three reasons need different answers: a ticket that does not exist, one
// belonging to somebody else, and one not yet resolved are a 404, a 404 (see
// below) and a 409. That second read is only on the failure path, so the
// normal case stays a single statement.
//
// Re-rating is allowed and overwrites. Somebody who mis-taps one star should
// be able to correct it, and the alternative — one irrevocable chance —
// makes the first tap frightening. What is deliberately *not* allowed is
// staff rating on a subscriber's behalf: this store method takes the
// subscriber id from the authenticated portal session, and there is no
// console path to it at all.
func (s *PortalStore) RateTicket(ctx context.Context, ticketID, subscriberID int, r tickets.Rating) error {
	r = r.Normalise()
	if err := r.Validate(); err != nil {
		return err
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE tickets
		   SET rating = $3, rating_comment = NULLIF($4, ''), rated_at = NOW(),
		       updated_at = NOW()
		 WHERE id = $1
		   AND subscriber_id = $2
		   AND status = ANY($5::text[])`,
		ticketID, subscriberID, r.Score, r.Comment, tickets.RatableStatuses())
	if err != nil {
		return fmt.Errorf("db: rate ticket %d: %w", ticketID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// Nothing updated. Work out which precondition failed.
	var owner int
	var status string
	err = s.pool.QueryRow(ctx,
		`SELECT subscriber_id, status FROM tickets WHERE id = $1`, ticketID).Scan(&owner, &status)
	if isNoRows(err) {
		return tickets.ErrTicketNotFound
	}
	if err != nil {
		return fmt.Errorf("db: read ticket %d after failed rating: %w", ticketID, err)
	}
	if owner != subscriberID {
		return tickets.ErrNotYourTicket
	}
	if !tickets.Ratable(status) {
		return tickets.ErrTicketNotResolved
	}
	// Every precondition holds and the update still did nothing, which
	// should be impossible. Reported rather than silently treated as
	// success, because a rating the subscriber believes was recorded and was
	// not is worse than an error they can retry.
	return fmt.Errorf("db: rating ticket %d changed no row despite passing every check", ticketID)
}

// There is deliberately no second query for the rating figures. They are
// computed by ConsoleStore.HelpdeskSummary, from the same first_close CTE
// that produces Closed30d, so the response rate has a denominator that
// describes exactly the tickets the rest of the strip describes. A separate
// "rating summary" query with its own window would drift from it, and the
// first symptom would be a response rate over 100%.
