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

# PGBIN defaults to the bundled PostgreSQL this project installs on Windows,
# and falls back to whatever psql is on PATH — which is what CI and any Linux
# host have. Without the fallback this script is Windows-only, and the CI job
# that runs the browser suite would fail on a path that does not exist there.
PGBIN="${PGBIN:-/c/Program Files/ISP BSS/pgsql/bin}"
if [ ! -x "$PGBIN/psql" ] && [ ! -x "$PGBIN/psql.exe" ]; then
    if command -v psql >/dev/null 2>&1; then
        PGBIN="$(dirname "$(command -v psql)")"
    else
        echo "refresh_e2e_session: no psql in $PGBIN or on PATH" >&2
        exit 1
    fi
fi
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

# Fail if the refresh produced nothing.
#
# Every statement above is conditional on subscriber `test_user` existing, so
# against an unseeded or truncated database this script did its work, found
# nothing to work on, printed "live_sessions=0" and exited 0. The eight tests
# that assert the online state then fail together, which reads as eight broken
# screens rather than as a fixture that was never created — and in CI the
# seeding step before it would have looked like it passed.
rows="$("$PGBIN/psql" -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$PGDATABASE" -t -A -c \
  "SELECT count(*) FROM live_sessions")"
if [ "${rows:-0}" -eq 0 ]; then
    echo "refresh_e2e_session: no live session was created." >&2
    echo "  Everything here hangs off subscriber 'test_user'; seed the database" >&2
    echo "  (scripts/seed_local.sql) before running this." >&2
    exit 1
fi
