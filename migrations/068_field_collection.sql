-- +goose Up
-- Field payment collection — FR-COL-001.
--
-- Doorstep cash collection is how a large share of Indian retail broadband
-- is actually paid for, and the platform had no notion of it: a collector
-- had no list of who to visit, and money they took was recorded — if at all
-- — from the subscriber's page by whoever was at a desk, so it was
-- attributable to the wrong person.
--
-- Deliberately small. Payments themselves already carry their method and who
-- recorded them (FR-BIL-013, migration 054), so this adds no second payment
-- path: a collector records cash through the same wallet credit as everyone
-- else, and `payment_details.recorded_by` is what makes the takings theirs.
-- What was missing is only the assignment and the list.
ALTER TABLE subscribers ADD COLUMN collector_id INTEGER
    REFERENCES staff_users(id) ON DELETE SET NULL;

-- ON DELETE SET NULL, not RESTRICT: a collector who leaves must be
-- removable, and the subscribers they covered should fall back to
-- unassigned rather than block the deletion. Their past collections are
-- unaffected — those live in payment_details.recorded_by, which is text and
-- survives the staff row being gone, precisely so a cash trail cannot be
-- erased by deleting an account.
COMMENT ON COLUMN subscribers.collector_id IS
    'Staff member who collects payment at the door (FR-COL-001). NULL = not on a collection round.';

-- The collector's own list: their subscribers, soonest expiry first. Partial
-- because the overwhelming majority of subscribers are not on a collection
-- round at all, and a full index would be mostly NULLs.
CREATE INDEX idx_subscribers_collector ON subscribers (collector_id, plan_expiry)
    WHERE collector_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_subscribers_collector;
ALTER TABLE subscribers DROP COLUMN IF EXISTS collector_id;
