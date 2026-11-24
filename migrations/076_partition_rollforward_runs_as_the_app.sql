-- +goose Up
-- The monthly partition roll-forward has to be callable by the service, and
-- it was not.
--
-- create_monthly_partitions() (migration 010) creates the current month plus
-- three. Its own COMMENT says it "must be called from the monthly maintenance
-- job (OPS §12) so partitions always exist ahead of writes: an insert with no
-- matching partition fails outright". Nothing called it. It ran once per
-- deployment, when the migration applied, and three partitioned tables —
-- subscriber_session_history (010), cgnat_allocations (011) and flow_records
-- (052) — were therefore good for about four months and then not.
--
-- What that failure looks like: PostgreSQL refuses the INSERT with "no
-- partition of relation ... found for row". RADIUS stops recording session
-- history, CGNAT port-block allocations stop being recorded — which is the
-- LEA trail required by FR-NET-001 and a licence condition, not a
-- convenience — and NetFlow flow records stop. Authentication still works,
-- so the first symptom is a regulatory request that cannot be answered.
--
-- Adding a caller was not enough on its own. The services connect as
-- bss_app (migration 019), which holds DML and nothing else: CREATE TABLE
-- ... PARTITION OF requires CREATE on the schema *and* ownership of the
-- parent, and bss_app has neither. A scanner calling this function as the
-- application would have failed on every pass with "must be owner of table",
-- which is a worse outcome than no scanner — it looks like it works.
--
-- So the function becomes SECURITY DEFINER, running as its owner (postgres,
-- which applies the migrations and owns the parents), with EXECUTE granted
-- to bss_app. search_path is pinned, because a SECURITY DEFINER function
-- that resolves names through the caller's search_path is the standard
-- privilege-escalation hole: a caller who can create a schema could shadow
-- any function this one calls.
--
-- The widened privilege is bounded by what the body can do: it creates
-- partitions of an existing partitioned table and nothing else. CREATE TABLE
-- ... PARTITION OF fails if the named parent is not partitioned, so the
-- argument cannot be turned into "create me an arbitrary table".

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_monthly_partitions(
    parent_table TEXT,
    months_ahead INT DEFAULT 3
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    first_month  DATE := date_trunc('month', NOW())::date;
    offset_month INT;
    range_start  DATE;
    range_end    DATE;
    part_name    TEXT;
BEGIN
    IF months_ahead < 0 OR months_ahead > 60 THEN
        RAISE EXCEPTION 'months_ahead must be between 0 and 60, got %', months_ahead;
    END IF;

    -- Only a partitioned table in public. Checked explicitly rather than
    -- left to the CREATE to reject: this function now runs with the owner's
    -- privileges, so it should refuse an argument it was not meant to take
    -- instead of finding out from an error message.
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = parent_table
          AND n.nspname = 'public'
          AND c.relkind = 'p'
    ) THEN
        RAISE EXCEPTION 'public.% is not a partitioned table', parent_table;
    END IF;

    FOR offset_month IN 0..months_ahead LOOP
        range_start := (first_month + (offset_month     || ' month')::interval)::date;
        range_end   := (first_month + (offset_month + 1 || ' month')::interval)::date;
        part_name   := parent_table || '_' || to_char(range_start, 'YYYY_MM');

        IF NOT EXISTS (
            SELECT 1 FROM pg_class c
            JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE c.relname = part_name AND n.nspname = 'public'
        ) THEN
            EXECUTE format(
                'CREATE TABLE public.%I PARTITION OF public.%I FOR VALUES FROM (%L) TO (%L)',
                part_name, parent_table, range_start, range_end
            );
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON FUNCTION create_monthly_partitions(TEXT, INT) IS
    'Creates monthly RANGE partitions for the current month plus months_ahead. Idempotent, and refuses anything that is not a partitioned table in public. SECURITY DEFINER so the bss_app role can call it without owning the parents; called daily by the partition roll-forward in radiusd (internal/db.PartitionScanner), not by hand. An insert with no matching partition fails outright, so coverage is monitored: db_partition_months_ahead.';
-- +goose StatementEnd

-- PUBLIC gets nothing. A SECURITY DEFINER function is granted to PUBLIC by
-- default, which would make it callable by any role that can connect.
REVOKE ALL ON FUNCTION create_monthly_partitions(TEXT, INT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION create_monthly_partitions(TEXT, INT) TO bss_app;

-- Catch up anything already behind, now, as part of the upgrade: a database
-- that has been running for four months needs this before the scanner's
-- first pass, and a deployment that is mid-outage needs it before radiusd
-- starts.
SELECT create_monthly_partitions('subscriber_session_history', 3);
SELECT create_monthly_partitions('cgnat_allocations', 3);
SELECT create_monthly_partitions('flow_records', 3);

-- +goose Down
REVOKE EXECUTE ON FUNCTION create_monthly_partitions(TEXT, INT) FROM bss_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_monthly_partitions(
    parent_table TEXT,
    months_ahead INT DEFAULT 3
) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    first_month  DATE := date_trunc('month', NOW())::date;
    offset_month INT;
    range_start  DATE;
    range_end    DATE;
    part_name    TEXT;
BEGIN
    FOR offset_month IN 0..months_ahead LOOP
        range_start := (first_month + (offset_month     || ' month')::interval)::date;
        range_end   := (first_month + (offset_month + 1 || ' month')::interval)::date;
        part_name   := parent_table || '_' || to_char(range_start, 'YYYY_MM');

        IF NOT EXISTS (
            SELECT 1 FROM pg_class c
            JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE c.relname = part_name AND n.nspname = 'public'
        ) THEN
            EXECUTE format(
                'CREATE TABLE public.%I PARTITION OF public.%I FOR VALUES FROM (%L) TO (%L)',
                part_name, parent_table, range_start, range_end
            );
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd
