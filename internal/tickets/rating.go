package tickets

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Subscriber ratings on resolved tickets — FR-SUP-004 | migration 070.
//
// The rules live here rather than in the store or the portal handler so the
// portal API, the portal UI and any later channel all refuse the same things
// for the same reasons.

// Rating bounds. One to five, never zero: a zero would be indistinguishable
// from "not rated" in every average, and an unrated ticket is already NULL.
const (
	MinRating = 1
	MaxRating = 5
)

// MaxRatingComment caps the free text.
//
// Long enough for a sentence or two of useful detail, short enough that the
// column is not a place to paste a conversation. A comment that is too long
// is refused rather than truncated: silently cutting somebody's complaint in
// half is worse than asking them to shorten it.
const MaxRatingComment = 500

// Errors a caller distinguishes. Each maps to a different response, so they
// are separate values rather than one "invalid rating".
var (
	// ErrRatingOutOfRange is a score outside 1–5.
	ErrRatingOutOfRange = errors.New("tickets: a rating must be from 1 to 5")
	// ErrCommentTooLong is a comment over MaxRatingComment characters.
	ErrCommentTooLong = fmt.Errorf("tickets: a rating comment may be at most %d characters", MaxRatingComment)
	// ErrTicketNotResolved is an attempt to rate work that has not been
	// finished. Rating an open ticket would be rating a resolution that has
	// not happened.
	ErrTicketNotResolved = errors.New("tickets: only a resolved ticket can be rated")
	// ErrNotYourTicket is an attempt to rate somebody else's ticket. Kept
	// distinct from "not found" at the store boundary and deliberately
	// collapsed into it at the API boundary — see RateTicket's own comment.
	ErrNotYourTicket = errors.New("tickets: that ticket belongs to another subscriber")
	// ErrTicketNotFound is a ticket id that does not exist.
	ErrTicketNotFound = errors.New("tickets: no ticket with that id")
)

// RatableStatuses are the statuses a subscriber may rate from.
//
// Closed as well as resolved: a ticket a CSR closed after the customer
// stopped replying is still work the customer can have an opinion about, and
// refusing the rating there would silently lose the feedback most worth
// having.
func RatableStatuses() []string { return []string{"resolved", "closed"} }

// Ratable reports whether a ticket in this status may be rated.
func Ratable(status string) bool {
	for _, s := range RatableStatuses() {
		if s == status {
			return true
		}
	}
	return false
}

// Rating is a subscriber's score and optional comment.
type Rating struct {
	Score   int
	Comment string
}

// Normalise trims the comment so what Validate checks is what gets stored.
func (r Rating) Normalise() Rating {
	r.Comment = strings.TrimSpace(r.Comment)
	return r
}

// Validate checks the score and comment.
func (r Rating) Validate() error {
	if r.Score < MinRating || r.Score > MaxRating {
		return ErrRatingOutOfRange
	}
	// Characters, not bytes: a Tamil or Hindi comment is three bytes per
	// character, and a byte limit would refuse a short complaint in the
	// customer's own language while accepting a long one in English.
	if utf8.RuneCountInString(r.Comment) > MaxRatingComment {
		return ErrCommentTooLong
	}
	return nil
}

// RatingSummary is how recent support has been rated.
type RatingSummary struct {
	// Rated and Ratable count tickets, so a flattering average over three
	// responses cannot be read as a verdict on three hundred tickets.
	Rated   int
	Ratable int
	// Average is nil when nothing has been rated. Zero would print as the
	// worst possible score for a helpdesk nobody has rated yet.
	Average *float64
	// Distribution counts each score, index 0 being one star. The shape
	// matters more than the mean: twenty 5s and five 1s averages the same as
	// twenty-five 4s and is a completely different support operation.
	Distribution [MaxRating]int
}

// ResponseRate is the share of ratable tickets that were rated, nil when
// there were none to rate.
//
// Reported because an average over 4% of tickets is a different claim from
// the same average over 70%, and the figure that makes that visible should
// not have to be worked out by the reader.
func (s RatingSummary) ResponseRate() *float64 {
	if s.Ratable <= 0 {
		return nil
	}
	pct := float64(s.Rated) / float64(s.Ratable) * 100
	return &pct
}

// Negative counts one- and two-star ratings — the ones worth a follow-up
// call. Grouped because a two is a complaint, not a middling review.
func (s RatingSummary) Negative() int {
	return s.Distribution[0] + s.Distribution[1]
}
