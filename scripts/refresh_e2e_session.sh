#!/usr/bin/env bash
# Refresh the seeded live session the Playwright suites depend on.
#
# Eight tests assert the "online" state — the portal's live-usage panel, the
# console's Online tile, and the Sessions screen including its disconnect.
# All of them read live_sessions, whose reads filter updated_at against a
# 30-minute TTL, so the row goes stale and those tests fail with
# "element(s) not found" that reads exactly like a broken page.
#
# Two ways it goes stale:
#   - time passing, especially this machine suspending mid-session
#   - the disconnect test itself, which closes the accounting row it
#     disconnected and leaves nothing for the next run to show
#
# Both are environment, not defects. Run this before a browser suite rather
# than re-deriving the SQL, and whenever the online-state cluster fails
# together.
set -euo pipefail

PGBIN="${PGBIN:-/c/Program Files/ISP BSS/pgsql/bin}"
PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-55432}"
PGUSER="${PGUSER:-postgres}"
PGDATABASE="${PGDATABASE:-isp_bss_oss}"
export PGPASSWORD="${PGPASSWORD:-postgres}"
export MSYS_NO_PATHCONV=1

"$PGBIN/psql" -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -v ON_ERROR_STOP=1 -q <<'SQL'
-- The live row the dashboard and portal read. updated_at is what the TTL
-- filters on; started_at only makes the session look established, and is set
-- three hours back so the usage figures look like a real session rather than
-- one that began this instant.
INSERT INTO live_sessions (subscriber_id, session_id, nas_ip, assigned_ip,
                           bytes_in, bytes_out, bytes_total,
                           speed_profile, fup_throttled, started_at, updated_at)
SELECT id, 'demo-live-001', '10.10.0.1', '100.64.0.7',
       1800000000000, 600000000000, 3543348019200,
       '100M/100M', false, now() - interval '3 hours', now()
  FROM subscribers WHERE username = 'test_user'
ON CONFLICT (subscriber_id) DO UPDATE
   SET started_at = now() - interval '3 hours',
       updated_at = now();

-- The open accounting row a real Accounting-Start writes alongside it. The
-- Sessions screen resolves a disconnect through this table, so without an
-- open row the session can be seen and never disconnected. Reopened rather
-- than inserted again, because the previous run's disconnect test closed
-- this very row.
UPDATE subscriber_session_history
   SET stop_time = NULL, terminate_cause = NULL
 WHERE session_id = 'demo-live-001';

INSERT INTO subscriber_session_history (subscriber_id, session_id, nas_ip_address,
                                        assigned_ipv4, start_time)
SELECT id, 'demo-live-001', '10.10.0.1'::inet, '100.64.0.7'::inet,
       -- Clamped to the month start so it routes to an existing partition
       -- (migration 010 partitions this table by start_time).
       GREATEST(date_trunc('month', now()), now() - interval '3 hours')
  FROM subscribers WHERE username = 'test_user'
   AND NOT EXISTS (SELECT 1 FROM subscriber_session_history
                    WHERE session_id = 'demo-live-001');
SQL

"$PGBIN/psql" -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -t -A -c \
  "SELECT 'live_sessions=' || count(*) ||
          ', open accounting rows=' || (SELECT count(*) FROM subscriber_session_history
                                         WHERE stop_time IS NULL) ||
          ', age=' || COALESCE(age(now(), max(updated_at))::text, 'n/a')
     FROM live_sessions"
