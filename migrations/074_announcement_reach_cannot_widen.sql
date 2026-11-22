-- +goose Up
-- An announcement's reach can no longer widen after it is composed.
--
-- A NULL segment means "no restriction on this dimension" (migration 028), so
-- anything that leaves an announcement with every segment NULL and no
-- recipient list turns it into a message to the whole base. Two routes led
-- there without anybody touching the announcement:
--
-- 1. A list announcement — the console's bulk "Notify these", or the API with
--    subscriber_ids — was recognised only by having announcement_recipients
--    rows. Those rows cascade when a subscriber is deleted (migration 041), so
--    once its recipients were gone it read as a segment announcement with no
--    filters. A draft would then send to everybody, and a portal banner showed
--    on everybody's portal. The portal read never consulted the list at all,
--    so a list banner showed on every portal from the moment it was sent.
--    Whether an announcement is a list is now a fact on its own row.
--
-- 2. Deleting the plan or franchise a segment named nulled the reference
--    (ON DELETE SET NULL), including on announcements already sent. Removing
--    demo data deletes demo plans, so this was reachable from the console.
--    RESTRICT now, as segment_group_id has been since migration 069.
--    Plans and franchises are retired, not deleted, everywhere else; demo
--    removal deletes the announcements addressed to its demo plans itself.
--
-- Not recoverable here: an announcement already widened by either route
-- before this migration looks exactly like one addressed to everyone, and
-- nothing records which it was.

ALTER TABLE announcements
    ADD COLUMN addressed_to_list BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE announcements a
   SET addressed_to_list = TRUE
 WHERE EXISTS (SELECT 1 FROM announcement_recipients r WHERE r.announcement_id = a.id);

-- A list and a segment together would leave reach ambiguous. The API has
-- refused the combination since migration 041, so existing rows satisfy this;
-- NOT VALID all the same, so a deployment holding a row written some other
-- way still migrates — new and updated rows are checked either way.
ALTER TABLE announcements
    ADD CONSTRAINT chk_announcement_list_has_no_segment CHECK (
        NOT addressed_to_list OR (
            segment_franchise_id IS NULL AND segment_plan_id IS NULL AND
            segment_status IS NULL AND segment_group_id IS NULL
        )
    ) NOT VALID;

ALTER TABLE announcements
    DROP CONSTRAINT announcements_segment_plan_id_fkey,
    ADD CONSTRAINT announcements_segment_plan_id_fkey
        FOREIGN KEY (segment_plan_id) REFERENCES plans(id) ON DELETE RESTRICT;

ALTER TABLE announcements
    DROP CONSTRAINT announcements_segment_franchise_id_fkey,
    ADD CONSTRAINT announcements_segment_franchise_id_fkey
        FOREIGN KEY (segment_franchise_id) REFERENCES franchises(id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE announcements
    DROP CONSTRAINT announcements_segment_franchise_id_fkey,
    ADD CONSTRAINT announcements_segment_franchise_id_fkey
        FOREIGN KEY (segment_franchise_id) REFERENCES franchises(id) ON DELETE SET NULL;

ALTER TABLE announcements
    DROP CONSTRAINT announcements_segment_plan_id_fkey,
    ADD CONSTRAINT announcements_segment_plan_id_fkey
        FOREIGN KEY (segment_plan_id) REFERENCES plans(id) ON DELETE SET NULL;

ALTER TABLE announcements DROP CONSTRAINT IF EXISTS chk_announcement_list_has_no_segment;
ALTER TABLE announcements DROP COLUMN IF EXISTS addressed_to_list;
