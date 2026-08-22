-- +goose Up
-- Bulk notification (console multi-select "Send to these subscribers")
-- needs a way to target an arbitrary, console-picked list rather than only
-- the existing franchise/plan/status segment filters. A join table rather
-- than an array column on announcements: ListSegmentSubscriberIDs already
-- returns a row set, and this lets that stay a plain SELECT instead of an
-- unnest() special case.
CREATE TABLE IF NOT EXISTS announcement_recipients (
    announcement_id INTEGER NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    subscriber_id   INTEGER NOT NULL REFERENCES subscribers(id)   ON DELETE CASCADE,
    PRIMARY KEY (announcement_id, subscriber_id)
);

-- +goose Down
DROP TABLE IF EXISTS announcement_recipients;
