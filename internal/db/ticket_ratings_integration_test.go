//go:build integration

// Subscriber ratings on resolved tickets — FR-SUP-004 | migration 070.
//
// What only a real database can show: that ownership and status are checked
// in the same statement as the write (so a ticket reopened a moment earlier
// cannot be rated as resolved), that the table's constraints hold the
// rating, its timestamp and its comment together, and that a rated ticket
// can still be reopened — which a constraint of the obvious shape would
// forbid, making the rating block a customer's problem from being looked at
// again.
package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// seedRatableTicket files a ticket and moves it to status, returning its id.
func seedRatableTicket(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, status string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(ctx, `
		INSERT INTO tickets (subscriber_id, category, description, status, priority)
		VALUES ($1, 'billing', 'help me', $2, 'medium')
		RETURNING id`, subscriberID, status).Scan(&id); err != nil {
		t.Fatalf("seed ticket for %d: %v", subscriberID, err)
	}
	return id
}

func TestFR_SUP_004_ASubscriberRatesTheirOwnResolvedTicket(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Portal()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})

	id := seedRatableTicket(ctx, t, pool, 1, "resolved")

	if err := store.RateTicket(ctx, id, 1, tickets.Rating{Score: 4, Comment: "  slow but sorted  "}); err != nil {
		t.Fatalf("rate: %v", err)
	}

	var rating int
	var comment string
	var ratedAt *string
	if err := pool.QueryRow(ctx, `
		SELECT rating, COALESCE(rating_comment, ''), rated_at::text
		  FROM tickets WHERE id = $1`, id).Scan(&rating, &comment, &ratedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rating != 4 {
		t.Errorf("rating = %d, want 4", rating)
	}
	// Trimmed by Normalise before the insert, so the stored text is what
	// Validate checked.
	if comment != "slow but sorted" {
		t.Errorf("comment = %q", comment)
	}
	if ratedAt == nil {
		t.Error("rated_at is NULL, which chk_ticket_rating_has_date should have refused")
	}

	// Re-rating overwrites: somebody who mis-taps one star must be able to
	// correct it.
	if err := store.RateTicket(ctx, id, 1, tickets.Rating{Score: 5}); err != nil {
		t.Fatalf("re-rate: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT rating, COALESCE(rating_comment, '') FROM tickets WHERE id = $1`,
		id).Scan(&rating, &comment); err != nil {
		t.Fatalf("read back after re-rating: %v", err)
	}
	if rating != 5 {
		t.Errorf("rating after re-rating = %d, want 5", rating)
	}
	// The comment was not resupplied, so it is cleared rather than left
	// describing a score that no longer exists.
	if comment != "" {
		t.Errorf("a stale comment %q survived a re-rating that supplied none", comment)
	}
}

// A closed ticket is ratable too: one a CSR closed after the customer stopped
// replying is still work they can have an opinion about.
func TestFR_SUP_004_AClosedTicketCanAlsoBeRated(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Portal()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})

	id := seedRatableTicket(ctx, t, pool, 1, "closed")
	if err := store.RateTicket(ctx, id, 1, tickets.Rating{Score: 2}); err != nil {
		t.Errorf("a closed ticket could not be rated: %v", err)
	}
}

// The three refusals, each distinguishable — the caller answers them
// differently.
func TestFR_SUP_004_RatingIsRefusedWithTheReason(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Portal()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "theirs@isp"})

	mine := seedRatableTicket(ctx, t, pool, 1, "resolved")
	theirs := seedRatableTicket(ctx, t, pool, 2, "resolved")
	open := seedRatableTicket(ctx, t, pool, 1, "open")

	// Somebody else's ticket.
	if err := store.RateTicket(ctx, theirs, 1, tickets.Rating{Score: 5}); !errors.Is(err, tickets.ErrNotYourTicket) {
		t.Errorf("rating another subscriber's ticket returned %v, want ErrNotYourTicket", err)
	}
	// And it was not written.
	var rating *int
	if err := pool.QueryRow(ctx, `SELECT rating FROM tickets WHERE id = $1`, theirs).Scan(&rating); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rating != nil {
		t.Errorf("a rating was written to another subscriber's ticket: %d", *rating)
	}

	// A ticket that is not finished.
	if err := store.RateTicket(ctx, open, 1, tickets.Rating{Score: 5}); !errors.Is(err, tickets.ErrTicketNotResolved) {
		t.Errorf("rating an open ticket returned %v, want ErrTicketNotResolved", err)
	}

	// A ticket that does not exist.
	if err := store.RateTicket(ctx, 999999, 1, tickets.Rating{Score: 5}); !errors.Is(err, tickets.ErrTicketNotFound) {
		t.Errorf("rating a missing ticket returned %v, want ErrTicketNotFound", err)
	}

	// And a score the domain refuses never reaches the table at all.
	if err := store.RateTicket(ctx, mine, 1, tickets.Rating{Score: 0}); !errors.Is(err, tickets.ErrRatingOutOfRange) {
		t.Errorf("a zero score returned %v, want ErrRatingOutOfRange", err)
	}
}

// The constraints, exercised directly. These are the rules the store's own
// code cannot be the only guard for, because a later writer — a migration, a
// fix-up script — would bypass it.
func TestFR_SUP_004_TheTableRefusesAnIncoherentRating(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	id := seedRatableTicket(ctx, t, pool, 1, "resolved")

	cases := []struct {
		name string
		sql  string
	}{
		{"a score of zero", `UPDATE tickets SET rating = 0, rated_at = NOW() WHERE id = $1`},
		{"a score of six", `UPDATE tickets SET rating = 6, rated_at = NOW() WHERE id = $1`},
		{"a rating with no date", `UPDATE tickets SET rating = 4 WHERE id = $1`},
		{"a date with no rating", `UPDATE tickets SET rated_at = NOW() WHERE id = $1`},
		{"a comment with no rating", `UPDATE tickets SET rating_comment = 'orphan' WHERE id = $1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tc.sql, id); err == nil {
				t.Errorf("the table accepted %s", tc.name)
			}
		})
	}
}

// The one a constraint of the obvious shape would have broken. Reopening a
// rated ticket must work: tying rating to status in a CHECK would make the
// reopen fail on a column the operator was not editing, and the rating would
// be the thing blocking a customer's problem from being looked at again.
func TestFR_SUP_004_ARatedTicketCanStillBeReopened(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Portal()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	id := seedRatableTicket(ctx, t, pool, 1, "resolved")

	if err := store.RateTicket(ctx, id, 1, tickets.Rating{Score: 1, Comment: "not fixed"}); err != nil {
		t.Fatalf("rate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tickets SET status = 'open' WHERE id = $1`, id); err != nil {
		t.Fatalf("reopening a rated ticket was refused: %v", err)
	}

	// The rating survives the reopen. Clearing it would delete the one piece
	// of negative feedback at the moment it is most deserved.
	var rating *int
	if err := pool.QueryRow(ctx, `SELECT rating FROM tickets WHERE id = $1`, id).Scan(&rating); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rating == nil || *rating != 1 {
		t.Errorf("the rating did not survive the reopen: %v", rating)
	}

	// And it cannot be rated again until it is resolved again, because the
	// resolution it would be rating has not happened.
	if err := store.RateTicket(ctx, id, 1, tickets.Rating{Score: 5}); !errors.Is(err, tickets.ErrTicketNotResolved) {
		t.Errorf("a reopened ticket could be re-rated: %v", err)
	}
}

// ── The helpdesk summary ────────────────────────────────────────────────────

// The figures come from the same query and the same 30-day set as Closed30d,
// so the response rate divides by the number printed beside it.
func TestFR_SUP_004_TheHelpdeskSummaryRatesTheSameTicketsItCounts(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	portalStore := database.Portal()
	console := database.Console()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	seedStaffUser(ctx, t, pool, 1, "csr_user", "csr")

	// Four tickets resolved inside the window, three of them rated.
	var ids []int
	for i := 0; i < 4; i++ {
		id := seedRatableTicket(ctx, t, pool, 1, "resolved")
		ids = append(ids, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO ticket_status_history (ticket_id, old_status, new_status, changed_by, occurred_at)
			VALUES ($1, 'open', 'resolved', 'csr_user', NOW() - interval '2 days')`, id); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	for i, score := range []int{1, 2, 5} {
		if err := portalStore.RateTicket(ctx, ids[i], 1, tickets.Rating{Score: score}); err != nil {
			t.Fatalf("rate %d: %v", ids[i], err)
		}
	}

	sum, err := console.HelpdeskSummary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}

	if sum.Closed30d != 4 {
		t.Fatalf("Closed30d = %d, want 4", sum.Closed30d)
	}
	// Ratable is the same set as Closed30d, which is the whole point: the
	// response rate's denominator is the figure beside it on the screen.
	if sum.Ratings.Ratable != sum.Closed30d {
		t.Errorf("Ratable = %d but Closed30d = %d; the response rate would divide by a set "+
			"the reader cannot see", sum.Ratings.Ratable, sum.Closed30d)
	}
	if sum.Ratings.Rated != 3 {
		t.Errorf("Rated = %d, want 3", sum.Ratings.Rated)
	}
	if sum.Ratings.Average == nil {
		t.Fatal("Average is nil despite three ratings")
	}
	// (1 + 2 + 5) / 3
	if *sum.Ratings.Average < 2.6 || *sum.Ratings.Average > 2.7 {
		t.Errorf("Average = %.2f, want ~2.67", *sum.Ratings.Average)
	}
	if sum.Ratings.Negative() != 2 {
		t.Errorf("Negative = %d, want 2 (the 1 and the 2)", sum.Ratings.Negative())
	}
	if rate := sum.Ratings.ResponseRate(); rate == nil || *rate < 74 || *rate > 76 {
		t.Errorf("ResponseRate = %v, want ~75", rate)
	}
	if sum.Ratings.Distribution[0] != 1 || sum.Ratings.Distribution[1] != 1 ||
		sum.Ratings.Distribution[4] != 1 {
		t.Errorf("Distribution = %v, want one each at 1, 2 and 5", sum.Ratings.Distribution)
	}
}

func TestFR_SUP_004_AnUnratedHelpdeskReportsNoAverageRatherThanZero(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)

	sum, err := database.Console().HelpdeskSummary(ctx)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// Zero would print as the worst possible score for a helpdesk nobody has
	// rated yet.
	if sum.Ratings.Average != nil {
		t.Errorf("Average = %v with no ratings at all, want nil", *sum.Ratings.Average)
	}
	if sum.Ratings.ResponseRate() != nil {
		t.Error("ResponseRate is reported with nothing to rate; 0% claims nobody replied " +
			"when the truth is that nobody was asked")
	}
}

// ── The lists ───────────────────────────────────────────────────────────────

// The rating travels on the rows both lists already fetch, so neither needs a
// second query and the two cannot disagree.
func TestFR_SUP_004_BothTicketListsCarryTheRating(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	portalStore := database.Portal()
	console := database.Console()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	id := seedRatableTicket(ctx, t, pool, 1, "resolved")

	if err := portalStore.RateTicket(ctx, id, 1, tickets.Rating{Score: 3, Comment: "ok"}); err != nil {
		t.Fatalf("rate: %v", err)
	}

	mine, err := portalStore.ListTickets(ctx, 1)
	if err != nil || len(mine) != 1 {
		t.Fatalf("portal list: %v, %d rows", err, len(mine))
	}
	if mine[0].Rating == nil || *mine[0].Rating != 3 {
		t.Errorf("the subscriber's own list does not carry the rating: %v", mine[0].Rating)
	}
	if mine[0].RatingComment != "ok" {
		t.Errorf("comment = %q", mine[0].RatingComment)
	}
	if !mine[0].Rated() || !mine[0].CanRate() {
		t.Errorf("Rated = %v, CanRate = %v for a rated resolved ticket",
			mine[0].Rated(), mine[0].CanRate())
	}

	queue, err := console.ListTicketQueue(ctx, staffui.TicketFilter{View: "all"})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	var found bool
	for _, q := range queue {
		if q.ID == id {
			found = true
			if q.Rating == nil || *q.Rating != 3 {
				t.Errorf("the console queue does not carry the rating: %v", q.Rating)
			}
			if q.RatingComment != "ok" {
				t.Errorf("console comment = %q", q.RatingComment)
			}
		}
	}
	if !found {
		t.Errorf("ticket %d is not in the queue at all", id)
	}
}
