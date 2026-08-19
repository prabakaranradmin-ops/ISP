-- +goose Up
-- Session DB-037 | FR-FUP-002, FR-API-003, FR-RPT-002 | MDS §4.17, §4.22
--
-- The background task queue, replacing Asynq (and with it the last thing
-- that required a Redis at all — see internal/localcache and migration 036
-- for the caches and session state that moved off it earlier).
--
-- Everything Asynq was actually used for maps onto ordinary SQL:
--
--   asynq.TaskID + ErrTaskIDConflict  -> task_id UNIQUE + ON CONFLICT
--   asynq.MaxRetry / backoff          -> retry_count, max_retry, run_after
--   asynq.Timeout                     -> timeout_seconds
--   asynq.Retention                   -> retention_until
--   the archived (dead-letter) queue  -> status = 'dead'
--   weighted multi-queue dequeue      -> queue column + SKIP LOCKED
--
-- task_id is NULLable and UNIQUE together on purpose. PostgreSQL treats
-- NULLs as distinct in a unique index, so the three task types that supply
-- an idempotency key (dunning notices, FUP warnings, announcement fan-out)
-- get exactly-once enqueueing, while every other task inserts freely
-- without needing a synthetic unique value.

CREATE TABLE IF NOT EXISTS jobqueue_tasks (
    id               BIGSERIAL     PRIMARY KEY,
    task_id          TEXT          UNIQUE,
    queue            TEXT          NOT NULL,
    task_type        TEXT          NOT NULL,
    -- The exact bytes the producer marshalled. BYTEA rather than JSONB
    -- because the queue never inspects a payload — only the handler that
    -- registered for the type does — and re-encoding through a JSON type
    -- would risk changing what the handler receives.
    payload          BYTEA         NOT NULL,
    status           TEXT          NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','processing','completed','dead')),
    max_retry        SMALLINT      NOT NULL DEFAULT 0,
    retry_count      SMALLINT      NOT NULL DEFAULT 0,
    timeout_seconds  INTEGER,
    -- When this task next becomes eligible. Set forward on each retry to
    -- implement backoff, so a failing webhook does not spin the pool.
    run_after        TIMESTAMPTZ   NOT NULL DEFAULT now(),
    -- Who holds it and until when. A worker that dies mid-task leaves the
    -- lease to expire rather than the row stuck in 'processing' forever.
    locked_by        TEXT,
    lease_expires_at TIMESTAMPTZ,
    last_error       TEXT,
    -- How long a finished row stays inspectable. NULL means delete on
    -- completion, matching Asynq's default of not retaining a task.
    retention_until  TIMESTAMPTZ,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);

-- The dequeue path: pending rows for one queue that are due now, oldest
-- first. Partial on status so the index stays small as completed rows
-- accumulate — the table is mostly finished work at any given moment.
CREATE INDEX IF NOT EXISTS idx_jobqueue_dequeue
    ON jobqueue_tasks (queue, run_after, id) WHERE status = 'pending';

-- The lease reaper's scan.
CREATE INDEX IF NOT EXISTS idx_jobqueue_lease
    ON jobqueue_tasks (lease_expires_at) WHERE status = 'processing';

-- The dead-letter monitor's count, per queue (internal/fup's
-- DeadLetterMonitor, which alerted on Asynq's archived-task count).
CREATE INDEX IF NOT EXISTS idx_jobqueue_dead
    ON jobqueue_tasks (queue) WHERE status = 'dead';

-- The retention sweep.
CREATE INDEX IF NOT EXISTS idx_jobqueue_retention
    ON jobqueue_tasks (retention_until) WHERE retention_until IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS jobqueue_tasks;
