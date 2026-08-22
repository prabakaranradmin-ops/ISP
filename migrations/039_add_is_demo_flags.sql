-- +goose Up
-- Console "Demo Data" panel — lets an owner load and remove presentable
-- sample data for a client walkthrough with no SQL/command-line access.
-- is_demo marks exactly the rows the panel created, so removal is a plain
-- WHERE clause rather than tracking IDs elsewhere.

ALTER TABLE subscribers ADD COLUMN is_demo BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE plans       ADD COLUMN is_demo BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE nas_devices ADD COLUMN is_demo BOOLEAN NOT NULL DEFAULT false;

-- Partial indexes rather than plain ones: is_demo is false for essentially
-- every row in a live deployment, so a full index would carry a large
-- number of entries the "is demo data loaded / remove it" queries never
-- look at.
CREATE INDEX idx_subscribers_is_demo ON subscribers(id) WHERE is_demo;
CREATE INDEX idx_plans_is_demo       ON plans(id)       WHERE is_demo;
CREATE INDEX idx_nas_devices_is_demo ON nas_devices(id) WHERE is_demo;

-- +goose Down
DROP INDEX IF EXISTS idx_subscribers_is_demo;
DROP INDEX IF EXISTS idx_plans_is_demo;
DROP INDEX IF EXISTS idx_nas_devices_is_demo;
ALTER TABLE subscribers DROP COLUMN is_demo;
ALTER TABLE plans       DROP COLUMN is_demo;
ALTER TABLE nas_devices DROP COLUMN is_demo;
