package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// FUPStore serves the FUP scanner, the CoA/PoD senders, admin session control
// and LEA lookups — all of it reads or writes subscriber_session_history, so
// keeping them on one store avoids splitting closely related queries across
// types for no structural reason.
// Satisfies fup.FUPQuerier, fup.CoAQuerier, api.SessionController,
// api.LEAQuerier and api.LEAAuditRecorder.
type FUPStore struct{ pool dbPool }

var (
	_ fup.FUPQuerier         = (*FUPStore)(nil)
	_ fup.CoAQuerier         = (*FUPStore)(nil)
	_ api.SessionController  = (*FUPStore)(nil)
	_ api.LEAQuerier         = (*FUPStore)(nil)
	_ api.LEAAuditRecorder   = (*FUPStore)(nil)
	_ radius.AccountingStore = (*FUPStore)(nil)
)

// liveSessionUsage aggregates octets across every open session a subscriber has,
// since a reconnect opens a new row while the quota is per billing cycle.
const liveSessionUsage = `
	SELECT h.subscriber_id,
	       MAX(host(h.nas_ip_address)) AS nas_ip,
	       SUM(h.input_octets + h.output_octets) AS bytes_used
	FROM subscriber_session_history h
	WHERE h.stop_time IS NULL
	GROUP BY h.subscriber_id`

// GetActiveSessionsAboveFUP returns online subscribers whose usage has reached
// their plan's FUP threshold and who are not already throttled.
//
// Filtering fup_active here rather than in Go keeps the scanner's 10s tick from
// re-issuing CoA for subscribers it throttled on a previous pass.
func (s *FUPStore) GetActiveSessionsAboveFUP(ctx context.Context) ([]fup.SessionStats, error) {
	const q = `
		WITH usage AS (` + liveSessionUsage + `)
		SELECT s.id, s.username, COALESCE(u.nas_ip, ''),
		       p.fup_threshold_bytes, COALESCE(u.bytes_used, 0), s.fup_active,
		       COALESCE(p.fup_throttle_string, '')
		FROM usage u
		JOIN subscribers s ON s.id = u.subscriber_id
		JOIN plans p       ON p.id = s.plan_id
		WHERE p.fup_threshold_bytes > 0
		  AND s.fup_active = FALSE
		  AND u.bytes_used >= p.fup_threshold_bytes
		  AND s.status IN ('active','grace_period')`

	return s.scanSessionStats(ctx, q)
}

// GetSessionsAtWarning returns online subscribers who have crossed pct of their
// quota but have not yet breached it.
//
// The upper bound matters: a subscriber past 100% belongs to the breach path,
// and warning them about approaching a limit they have already hit would be wrong.
func (s *FUPStore) GetSessionsAtWarning(ctx context.Context, pct int) ([]fup.SessionStats, error) {
	if pct <= 0 || pct >= 100 {
		pct = fup.FUPWarningPct
	}
	const q = `
		WITH usage AS (` + liveSessionUsage + `)
		SELECT s.id, s.username, COALESCE(u.nas_ip, ''),
		       p.fup_threshold_bytes, COALESCE(u.bytes_used, 0), s.fup_active,
		       COALESCE(p.fup_throttle_string, '')
		FROM usage u
		JOIN subscribers s ON s.id = u.subscriber_id
		JOIN plans p       ON p.id = s.plan_id
		WHERE p.fup_threshold_bytes > 0
		  AND s.fup_active = FALSE
		  AND u.bytes_used >= (p.fup_threshold_bytes * $1) / 100
		  AND u.bytes_used <  p.fup_threshold_bytes
		  AND s.status IN ('active','grace_period')`

	return s.scanSessionStats(ctx, q, pct)
}

func (s *FUPStore) scanSessionStats(ctx context.Context, q string, args ...any) ([]fup.SessionStats, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query FUP sessions: %w", err)
	}
	defer rows.Close()

	stats := make([]fup.SessionStats, 0, 16)
	for rows.Next() {
		var st fup.SessionStats
		if err := rows.Scan(&st.SubscriberID, &st.Username, &st.NasIP,
			&st.FUPThreshold, &st.BytesUsed, &st.FUPActive, &st.FUPThrottle); err != nil {
			return nil, fmt.Errorf("db: scan FUP session row: %w", err)
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate FUP sessions: %w", err)
	}
	return stats, nil
}

// SetFUPActive records that a subscriber is (or is no longer) throttled.
func (s *FUPStore) SetFUPActive(ctx context.Context, subscriberID int, active bool) error {
	const q = `UPDATE subscribers SET fup_active = $2 WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, subscriberID, active)
	if err != nil {
		return fmt.Errorf("db: set fup_active for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	return nil
}

// SetSpeedOverride records an owner-triggered temporary rate for a
// subscriber, independent of their billed plan. expiresAt nil means "until
// manually cleared" — GetSubscriberByUsername and GetSubscriberNASSession
// both treat a NULL expiry as never-expired.
func (s *FUPStore) SetSpeedOverride(ctx context.Context, subscriberID int, rateLimit string, expiresAt *time.Time) error {
	const q = `UPDATE subscribers SET speed_override_rate_limit = $2, speed_override_expires_at = $3 WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, subscriberID, rateLimit, expiresAt)
	if err != nil {
		return fmt.Errorf("db: set speed override for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	return nil
}

// ClearSpeedOverride removes a subscriber's temporary rate, restoring their
// plan (or FUP throttle) rate on the next Access-Accept or CoA.
func (s *FUPStore) ClearSpeedOverride(ctx context.Context, subscriberID int) error {
	const q = `UPDATE subscribers SET speed_override_rate_limit = NULL, speed_override_expires_at = NULL WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, subscriberID)
	if err != nil {
		return fmt.Errorf("db: clear speed override for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	return nil
}

// ListExpiredSpeedOverrides returns subscriber IDs whose override has
// passed its expiry and still needs clearing — the FUP scanner's expiry
// sweep uses this so it only touches rows that actually need reverting.
func (s *FUPStore) ListExpiredSpeedOverrides(ctx context.Context) ([]int, error) {
	const q = `
		SELECT id FROM subscribers
		WHERE speed_override_expires_at IS NOT NULL AND speed_override_expires_at <= NOW()`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list expired speed overrides: %w", err)
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan expired speed override id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetSubscriberNASSession returns the NAS address, RADIUS session id, the
// rate limit to apply, and the subscriber's plan ID, for building a
// CoA-Request. planID resolves a policy-reference vendor's QoS profile name
// (FR-NAS-001, MDS §4.11) the same way it does for Access-Accept.
//
// The returned rate limit is the throttled profile when the subscriber is
// flagged fup_active, which is what makes the CoA actually reduce their speed.
func (s *FUPStore) GetSubscriberNASSession(ctx context.Context, subscriberID int) (nasIP, sessionID, rateLimit string, planID int, err error) {
	const q = `
		SELECT host(h.nas_ip_address), h.session_id,
		       CASE WHEN s.speed_override_rate_limit IS NOT NULL
		                 AND (s.speed_override_expires_at IS NULL OR s.speed_override_expires_at > NOW())
		            THEN s.speed_override_rate_limit
		            WHEN s.fup_active AND COALESCE(p.fup_throttle_string,'') <> ''
		            THEN p.fup_throttle_string
		            ELSE p.rate_limit_string
		       END,
		       s.plan_id
		FROM subscriber_session_history h
		JOIN subscribers s ON s.id = h.subscriber_id
		JOIN plans p       ON p.id = s.plan_id
		WHERE h.subscriber_id = $1 AND h.stop_time IS NULL
		ORDER BY h.start_time DESC
		LIMIT 1`

	err = s.pool.QueryRow(ctx, q, subscriberID).Scan(&nasIP, &sessionID, &rateLimit, &planID)
	if isNoRows(err) {
		// No live session: the subscriber disconnected before the CoA task ran.
		// Retrying cannot help, so this is reported as not-found rather than a
		// transient failure.
		return "", "", "", 0, fmt.Errorf("db: no active session for subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return "", "", "", 0, fmt.Errorf("db: get NAS session for subscriber %d: %w", subscriberID, err)
	}
	return nasIP, sessionID, rateLimit, planID, nil
}

// StartSession records a new RADIUS session at Accounting-Start.
//
// Idempotent on session_id: a NAS retransmits an Accounting-Start it never saw
// acknowledged, and a second open row for one session would be counted twice by
// the FUP scanner, which sums octets across every open session a subscriber
// has. That would throttle a subscriber at half their real quota.
//
// Enforced with WHERE NOT EXISTS rather than a unique index because the table is
// partitioned by start_time (migration 010), and PostgreSQL requires the
// partition key in any unique constraint — session_id alone cannot carry one.
// Two Starts racing at the same instant could still both insert; the daemon's
// Redis dedup covers the retransmit window where that is actually possible.
// ipv6Prefix is the CIDR the NAS delegated for this session, empty for an
// IPv4-only one. Stored because an IPv6 subscriber's traffic appears from
// that prefix and nothing else recorded here identifies them: a
// law-enforcement request naming an IPv6 address could not be answered
// against assigned_ipv4 at all. The column has existed since migration
// 010; until now nothing wrote it (FR-NET-003, DBD §6.2).
func (s *FUPStore) StartSession(ctx context.Context, subscriberID int, sessionID, nasIP, assignedIP, ipv6Prefix string) error {
	const q = `
		INSERT INTO subscriber_session_history (
			subscriber_id, session_id, nas_ip_address, assigned_ipv4,
			assigned_ipv6_prefix, start_time
		)
		-- $2 is cast explicitly because it appears both in the SELECT list, where
		-- nothing constrains its type, and in the comparison below against a
		-- VARCHAR column. Without the cast PostgreSQL deduces two different types
		-- for one parameter and rejects the statement (SQLSTATE 42P08).
		SELECT $1, $2::varchar, $3::inet, NULLIF($4,'')::inet, NULLIF($5,'')::cidr, NOW()
		WHERE NOT EXISTS (
			SELECT 1 FROM subscriber_session_history
			 WHERE session_id = $2::varchar AND stop_time IS NULL
		)`

	if _, err := s.pool.Exec(ctx, q, subscriberID, sessionID, nasIP, assignedIP, ipv6Prefix); err != nil {
		return fmt.Errorf("db: start session for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// CloseSupersededSessions closes session-history rows left open by an
// Accounting-Stop that never arrived, and reports how many it closed.
//
// This is a correctness fix, not housekeeping. liveSessionUsage (this file,
// top) sums octets across *every* open session a subscriber has, because a
// reconnect legitimately opens a new row mid-cycle and the quota is
// per-cycle. That is right when the old rows get closed and badly wrong when
// they do not: each abandoned row keeps contributing its octets forever, so
// the FUP scanner sees more usage than the subscriber has had and throttles
// them early. Observed on this deployment at 7680 MB counted against 4096 MB
// actually used — an 87% over-count from five abandoned rows, which on a
// plan with a FUP threshold means throttling at roughly half the real quota.
//
// Nothing closed these before. A lost Accounting-Stop is not an edge case —
// a NAS reboot, a power cut, or one dropped UDP datagram all produce it — so
// in production these accumulate indefinitely and the over-count grows.
//
// The rule is deliberately conservative: a session is closed only when the
// same subscriber has a *newer* session on the *same NAS*, which means the
// older one demonstrably ended (a device cannot hold two concurrent PPPoE
// sessions on one NAS). stop_time is set to the successor's start_time — the
// latest moment the old session could still have been alive, so the closure
// never claims more precision than is actually known.
//
// Deliberately NOT closed: an open session with no successor. It may be a
// genuinely long-running one, and there is no updated_at on this table to
// distinguish "connected for three weeks" from "abandoned three weeks ago".
// Closing those needs a signal this table does not carry, so they are left
// for an operator rather than guessed at.
func (s *FUPStore) CloseSupersededSessions(ctx context.Context) (int64, error) {
	// The correlated subquery finds the earliest later session for the same
	// subscriber on the same NAS. Matching on nas_ip_address as well as
	// subscriber keeps a subscriber with two genuine concurrent lines on
	// different NAS devices from having one closed by the other.
	const q = `
		UPDATE subscriber_session_history h
		SET stop_time = successor.start_time,
		    terminate_cause = COALESCE(h.terminate_cause, 'superseded-no-acct-stop')
		FROM (
			SELECT a.id, a.start_time AS row_start,
			       MIN(b.start_time) AS start_time
			  FROM subscriber_session_history a
			  JOIN subscriber_session_history b
			    ON b.subscriber_id  = a.subscriber_id
			   AND b.nas_ip_address = a.nas_ip_address
			   AND b.start_time     > a.start_time
			 WHERE a.stop_time IS NULL
			 GROUP BY a.id, a.start_time
		) AS successor
		WHERE h.id = successor.id AND h.start_time = successor.row_start
		  AND h.stop_time IS NULL`

	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("db: close superseded sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// UpdateSessionOctets applies an Interim-Update counter to the open session,
// reporting whether one was found.
//
// A miss is not an error: the daemon may have been down when the session
// started, or the NAS may be accounting for a session this system never
// authorised. The caller counts those rather than failing the NAS, but it has
// to be able to tell — silently affecting zero rows is how usage goes missing
// without anyone noticing.
func (s *FUPStore) UpdateSessionOctets(ctx context.Context, sessionID string, inputOctets, outputOctets int64) (bool, error) {
	const q = `
		UPDATE subscriber_session_history
		SET input_octets = $2, output_octets = $3
		WHERE session_id = $1 AND stop_time IS NULL`

	tag, err := s.pool.Exec(ctx, q, sessionID, inputOctets, outputOctets)
	if err != nil {
		return false, fmt.Errorf("db: update session %s octets: %w", sessionID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// StopSession closes a session at Accounting-Stop, reporting whether an open
// session was found. See UpdateSessionOctets for why a miss is reported rather
// than raised.
func (s *FUPStore) StopSession(ctx context.Context, sessionID string, inputOctets, outputOctets int64, cause string) (bool, error) {
	const q = `
		UPDATE subscriber_session_history
		SET stop_time = NOW(), input_octets = $2, output_octets = $3, terminate_cause = NULLIF($4,'')
		WHERE session_id = $1 AND stop_time IS NULL`

	tag, err := s.pool.Exec(ctx, q, sessionID, inputOctets, outputOctets, cause)
	if err != nil {
		return false, fmt.Errorf("db: stop session %s: %w", sessionID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ── Admin session control (API-004) ─────────────────────────────────────────

// ResolveSessionSubscriber finds the subscriber and NAS address owning a live,
// NAS-issued session_id — what the admin disconnect and FUP-override actions
// need before they can enqueue a PoD or CoA task.
//
// Uses idx_session_lookup (migration 017): without it this sequential-scans
// the current month's partition on every call.
func (s *FUPStore) ResolveSessionSubscriber(ctx context.Context, sessionID string) (subscriberID int, nasIP string, err error) {
	const q = `
		SELECT subscriber_id, host(nas_ip_address)
		FROM subscriber_session_history
		WHERE session_id = $1 AND stop_time IS NULL
		LIMIT 1`

	err = s.pool.QueryRow(ctx, q, sessionID).Scan(&subscriberID, &nasIP)
	if isNoRows(err) {
		return 0, "", fmt.Errorf("db: session %q: %w", sessionID, ErrNotFound)
	}
	if err != nil {
		return 0, "", fmt.Errorf("db: resolve session %q: %w", sessionID, err)
	}
	return subscriberID, nasIP, nil
}

// ── LEA lookup (API-004, FR-OBS-003) ─────────────────────────────────────────

// LookupByPublicIP resolves the subscriber holding publicIP (and port, for
// CGNAT deployments) at instant at.
//
// Two paths, selected by whether the caller supplied a port: directly-assigned
// IPs are looked up in subscriber_session_history (idx_lea_ipv4_time), shared
// CGNAT port blocks in cgnat_allocations (idx_cgnat_lea). Both indexes are
// built for exactly this query shape — most recent row starting at or before
// `at` for a given address — so the ORDER BY ... LIMIT 1 pattern is an
// index-only lookup, not a scan.
func (s *FUPStore) LookupByPublicIP(ctx context.Context, publicIP string, port *int, at time.Time) (*api.LEAResult, error) {
	if port != nil {
		return s.lookupCGNAT(ctx, publicIP, *port, at)
	}
	return s.lookupDirectIP(ctx, publicIP, at)
}

func (s *FUPStore) lookupDirectIP(ctx context.Context, publicIP string, at time.Time) (*api.LEAResult, error) {
	const q = `
		SELECT h.subscriber_id, h.session_id, h.start_time, h.stop_time,
		       s.caf_number, s.username, s.mobile_number, s.registered_state
		FROM subscriber_session_history h
		JOIN subscribers s ON s.id = h.subscriber_id
		WHERE h.assigned_ipv4 = $1::inet AND h.start_time <= $2
		ORDER BY h.start_time DESC
		LIMIT 1`

	var (
		r         api.LEAResult
		startTime time.Time
		stopTime  *time.Time
	)
	err := s.pool.QueryRow(ctx, q, publicIP, at).Scan(
		&r.SubscriberID, &r.SessionID, &startTime, &stopTime,
		&r.CAFNumber, &r.Username, &r.MobileNumber, &r.RegisteredState,
	)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: LEA direct-IP lookup: %w", err)
	}
	// The index gives the most recent session that started at or before `at`;
	// it must still have been active at `at` to actually be a match — an older
	// session that had already ended is not the right answer.
	if stopTime != nil && stopTime.Before(at) {
		return nil, nil
	}
	r.SessionStart = startTime
	r.SessionStop = stopTime
	r.Source = "direct_ip"
	return &r, nil
}

func (s *FUPStore) lookupCGNAT(ctx context.Context, publicIP string, port int, at time.Time) (*api.LEAResult, error) {
	const q = `
		SELECT c.subscriber_id, c.allocated_at, c.released_at,
		       s.caf_number, s.username, s.mobile_number, s.registered_state
		FROM cgnat_allocations c
		JOIN subscribers s ON s.id = c.subscriber_id
		WHERE c.public_ip = $1::inet AND c.port_start <= $2 AND c.port_end >= $2 AND c.allocated_at <= $3
		ORDER BY c.allocated_at DESC
		LIMIT 1`

	var (
		r           api.LEAResult
		allocatedAt time.Time
		releasedAt  *time.Time
	)
	err := s.pool.QueryRow(ctx, q, publicIP, port, at).Scan(
		&r.SubscriberID, &allocatedAt, &releasedAt,
		&r.CAFNumber, &r.Username, &r.MobileNumber, &r.RegisteredState,
	)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: LEA CGNAT lookup: %w", err)
	}
	if releasedAt != nil && releasedAt.Before(at) {
		return nil, nil
	}
	r.SessionStart = allocatedAt
	r.SessionStop = releasedAt
	r.Source = "cgnat"
	return &r, nil
}

// RecordLEAAudit writes the append-only audit row FR-OBS-003 requires for
// every LEA lookup, whether or not it found a match.
//
// The table's RLS policy (migration 014) permits INSERT for any role, which is
// the only operation this method performs, so it needs no elevated privilege.
// The append-only guarantee, however, depends on the application connecting as
// a role that is not the table owner: PostgreSQL exempts owners and
// superusers from row-level security, and this deployment's DSN currently
// connects as the postgres superuser — so RLS is not actually enforced against
// this connection today. Enforcing it requires a distinct low-privilege
// application role, which is a deployment change outside this package.
func (s *FUPStore) RecordLEAAudit(ctx context.Context, entry api.LEAAuditEntry) error {
	const q = `
		INSERT INTO lea_audit_log (
			accessor_identity, accessor_role, queried_public_ip, queried_port,
			queried_timestamp, result_subscriber_id, result_row_count
		) VALUES ($1, $2, $3::inet, $4, $5, $6, $7)`

	if _, err := s.pool.Exec(ctx, q,
		entry.AccessorIdentity, entry.AccessorRole, entry.QueriedPublicIP, entry.QueriedPort,
		entry.QueriedTimestamp, entry.ResultSubscriberID, entry.ResultRowCount,
	); err != nil {
		return fmt.Errorf("db: record LEA audit: %w", err)
	}
	return nil
}
