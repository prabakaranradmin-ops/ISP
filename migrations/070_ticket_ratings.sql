-- +goose Up
-- Subscriber ratings on resolved tickets — FR-SUP-004.
--
-- Columns on tickets rather than a table of their own. A rating is one per
-- ticket, written by one person, read everywhere the ticket is read: the
-- queue, the subscriber's own list and the helpdesk summary all want it
-- beside the row they are already fetching, and a separate table would add a
-- join to every one of them to store at most one row per ticket.
--
-- The cost of that choice is that a re-rating overwrites rather than
-- appending, so there is no history of what somebody rated before they
-- changed their mind. That is acceptable here: the rating is current
-- feedback, not a record the business needs to audit, and the staff trail
-- (FR-OBS-006) records that a rating changed because the portal action is
-- audited.
ALTER TABLE tickets ADD COLUMN rating SMALLINT;
ALTER TABLE tickets ADD COLUMN rated_at TIMESTAMPTZ;
ALTER TABLE tickets ADD COLUMN rating_comment TEXT;

-- 1 to 5. Deliberately not 0 to 5: a zero would be indistinguishable from
-- "not rated" in every average, and an unrated ticket is already NULL.
ALTER TABLE tickets ADD CONSTRAINT chk_ticket_rating_range
    CHECK (rating IS NULL OR rating BETWEEN 1 AND 5);

-- A rating and its timestamp travel together, so "rated 4" can never exist
-- without the when, and a date can never exist without the score.
ALTER TABLE tickets ADD CONSTRAINT chk_ticket_rating_has_date
    CHECK ((rating IS NULL) = (rated_at IS NULL));

-- A comment is feedback about a rating, so it cannot exist alone.
ALTER TABLE tickets ADD CONSTRAINT chk_ticket_rating_comment_needs_rating
    CHECK (rating IS NOT NULL OR rating_comment IS NULL);

-- Deliberately NOT a constraint tying rating to status.
--
-- Only a resolved ticket may be rated, and that rule is enforced in the
-- write path (RateTicket's WHERE clause) rather than here. A CHECK would
-- also forbid the reverse: a rated ticket being reopened, which moves status
-- back to 'open' while the rating stays. Reopening is a supported action, so
-- a constraint of the obvious shape would make the reopen fail with a
-- constraint violation on a table the operator was not editing — and the
-- rating would be the thing blocking a customer's problem from being looked
-- at again.
--
-- What the rating then means is "how the previous resolution was rated",
-- which is also why it is not cleared on reopen: deleting the one piece of
-- negative feedback at the moment it is most deserved would be the wrong
-- behaviour, and the reopen itself is already counted against the support
-- figures (migration 032's reopen_count).

-- The helpdesk summary averages recent ratings, and the queue sorts by them.
-- Partial, because most tickets are never rated and an index over the NULLs
-- would be mostly dead weight.
CREATE INDEX idx_tickets_rated ON tickets (rated_at DESC, rating)
    WHERE rating IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_tickets_rated;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS chk_ticket_rating_comment_needs_rating;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS chk_ticket_rating_has_date;
ALTER TABLE tickets DROP CONSTRAINT IF EXISTS chk_ticket_rating_range;
ALTER TABLE tickets DROP COLUMN IF EXISTS rating_comment;
ALTER TABLE tickets DROP COLUMN IF EXISTS rated_at;
ALTER TABLE tickets DROP COLUMN IF EXISTS rating;
