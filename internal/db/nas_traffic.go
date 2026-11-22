package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/maaransoft/isp-bss-oss/internal/nastraffic"
)

// NAS interface traffic — FR-NET-006 | migration 073.
// The rules about what a reading means live in internal/nastraffic.

var _ nastraffic.Store = (*NASStore)(nil)

// ── Polling ──────────────────────────────────────────────────────────────────

// ListPollableRouters returns the routers with SNMP polling turned on.
func (s *NASStore) ListPollableRouters(ctx context.Context) ([]nastraffic.Router, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, host(ip), snmp_port, snmp_community_encrypted
		  FROM nas_devices
		 WHERE snmp_enabled AND snmp_community_encrypted IS NOT NULL
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: list pollable routers: %w", err)
	}
	defer rows.Close()
	var out []nastraffic.Router
	for rows.Next() {
		var r nastraffic.Router
		if err := rows.Scan(&r.NASID, &r.Host, &r.Port, &r.CommunityEncrypted); err != nil {
			return nil, fmt.Errorf("db: scan pollable router: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterfaceBaselines returns each interface's last stored counters, by name.
//
// The counters are NUMERIC(20,0) — unsigned 64-bit does not fit BIGINT — so
// they travel as text and are parsed here.
func (s *NASStore) InterfaceBaselines(ctx context.Context, nasID int) (map[string]nastraffic.Previous, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, last_in_octets::text, last_out_octets::text, last_counter_at
		  FROM nas_interfaces
		 WHERE nas_id = $1 AND last_counter_at IS NOT NULL`, nasID)
	if err != nil {
		return nil, fmt.Errorf("db: read interface baselines: %w", err)
	}
	defer rows.Close()
	out := map[string]nastraffic.Previous{}
	for rows.Next() {
		var (
			name, in, outStr string
			at               time.Time
		)
		if err := rows.Scan(&name, &in, &outStr, &at); err != nil {
			return nil, fmt.Errorf("db: scan interface baseline: %w", err)
		}
		inV, err1 := strconv.ParseUint(in, 10, 64)
		outV, err2 := strconv.ParseUint(outStr, 10, 64)
		if err1 != nil || err2 != nil {
			// A baseline that cannot be parsed is treated as absent, so the
			// next poll re-baselines rather than computing from garbage.
			continue
		}
		out[name] = nastraffic.Previous{InOctets: inV, OutOctets: outV, At: at}
	}
	return out, rows.Err()
}

// SavePoll records one poll of one router in a single transaction: every
// interface's metadata and, where the reading advances it, its baseline; the
// samples whose baselines landed; and the router's last-success time.
//
// The baseline write is a compare-and-swap. It only lands if the interface's
// stored last_counter_at is still the one this poll measured against, so two
// pollers racing on the same router — two radiusd instances, say — cannot
// both record a sample for the same interval: the second finds the baseline
// already moved and records nothing. No lock is held across the SNMP round
// trips that precede this.
func (s *NASStore) SavePoll(ctx context.Context, nasID int, at time.Time, writes []nastraffic.Write) error {
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		for _, w := range writes {
			id, landed, err := upsertInterface(ctx, tx, nasID, at, w)
			if err != nil {
				return err
			}
			if w.Sample == nil || !landed {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO nas_interface_samples (interface_id, sampled_at, interval_seconds, in_bps, out_bps)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (interface_id, sampled_at) DO NOTHING`,
				id, w.Sample.At, w.Sample.IntervalSeconds, w.Sample.InBps, w.Sample.OutBps); err != nil {
				return fmt.Errorf("db: insert interface sample: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE nas_devices
			   SET snmp_last_polled_at = $2, snmp_last_ok_at = $2, snmp_last_error = NULL
			 WHERE id = $1`, nasID, at); err != nil {
			return fmt.Errorf("db: mark router polled: %w", err)
		}
		return nil
	})
}

// upsertInterface writes one interface and reports its id and whether a
// baseline-advancing write landed.
func upsertInterface(ctx context.Context, tx pgx.Tx, nasID int, at time.Time, w nastraffic.Write) (int, bool, error) {
	i := w.Iface
	var expectAt *time.Time
	if w.Expect != nil {
		e := w.Expect.At
		expectAt = &e
	}

	var id int
	if !w.AdvanceBaseline {
		// Metadata only: the stored counters are left exactly as they are.
		err := tx.QueryRow(ctx, `
			INSERT INTO nas_interfaces (nas_id, name, if_index, alias, if_type, speed_mbps, oper_up,
			                            has_hc_counters, last_seen_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (nas_id, name) DO UPDATE SET
			       if_index = EXCLUDED.if_index, alias = EXCLUDED.alias, if_type = EXCLUDED.if_type,
			       speed_mbps = EXCLUDED.speed_mbps, oper_up = EXCLUDED.oper_up,
			       has_hc_counters = EXCLUDED.has_hc_counters, last_seen_at = EXCLUDED.last_seen_at
			RETURNING id`,
			nasID, i.Name, i.Index, i.Alias, nullableType(i.Type), i.SpeedMbps, i.OperUp, i.HasHC, at).Scan(&id)
		if err != nil {
			return 0, false, fmt.Errorf("db: upsert interface %q: %w", i.Name, err)
		}
		return id, false, nil
	}

	err := tx.QueryRow(ctx, `
		INSERT INTO nas_interfaces (nas_id, name, if_index, alias, if_type, speed_mbps, oper_up,
		                            has_hc_counters, last_in_octets, last_out_octets, last_counter_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10::numeric, $11, $11)
		ON CONFLICT (nas_id, name) DO UPDATE SET
		       if_index = EXCLUDED.if_index, alias = EXCLUDED.alias, if_type = EXCLUDED.if_type,
		       speed_mbps = EXCLUDED.speed_mbps, oper_up = EXCLUDED.oper_up,
		       has_hc_counters = EXCLUDED.has_hc_counters,
		       last_in_octets = EXCLUDED.last_in_octets, last_out_octets = EXCLUDED.last_out_octets,
		       last_counter_at = EXCLUDED.last_counter_at, last_seen_at = EXCLUDED.last_seen_at
		 WHERE nas_interfaces.last_counter_at IS NOT DISTINCT FROM $12
		RETURNING id`,
		nasID, i.Name, i.Index, i.Alias, nullableType(i.Type), i.SpeedMbps, i.OperUp, i.HasHC,
		strconv.FormatUint(i.InOctets, 10), strconv.FormatUint(i.OutOctets, 10), at, expectAt).Scan(&id)
	if isNoRows(err) {
		// The compare-and-swap lost: another poller moved this interface's
		// baseline since we read it. Its sample covers this interval.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("db: upsert interface %q: %w", i.Name, err)
	}
	return id, true, nil
}

func nullableType(t int) *int {
	if t == 0 {
		return nil
	}
	return &t
}

// RecordPollFailure marks a failed attempt without touching the last success,
// so the console can say how long the router has been dark.
func (s *NASStore) RecordPollFailure(ctx context.Context, nasID int, at time.Time, reason string) error {
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE nas_devices SET snmp_last_polled_at = $2, snmp_last_error = $3 WHERE id = $1`,
		nasID, at, reason)
	if err != nil {
		return fmt.Errorf("db: record poll failure: %w", err)
	}
	return nil
}

// PurgeSamplesBefore deletes samples older than cutoff.
func (s *NASStore) PurgeSamplesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM nas_interface_samples WHERE sampled_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("db: purge interface samples: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ── Settings ─────────────────────────────────────────────────────────────────

// SetNASSNMP changes a router's SNMP settings. communityEncrypted nil keeps
// the stored community, so the form never has to show it to be resubmitted.
//
// Changing the community or port clears the last error: the error described
// the old settings, and leaving it up until the next poll would show a
// failure the operator has just fixed.
func (s *NASStore) SetNASSNMP(ctx context.Context, nasID int, enabled bool, communityEncrypted *string, port int) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE nas_devices
		   SET snmp_enabled = $2,
		       snmp_community_encrypted = COALESCE($3, snmp_community_encrypted),
		       snmp_port = $4,
		       snmp_last_error = CASE WHEN $3::text IS NOT NULL OR snmp_port <> $4
		                              THEN NULL ELSE snmp_last_error END
		 WHERE id = $1`, nasID, enabled, communityEncrypted, port)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "chk_nas_snmp_has_community" {
		return nastraffic.ErrSNMPNeedsCommunity
	}
	if err != nil {
		return fmt.Errorf("db: set NAS %d SNMP: %w", nasID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: NAS %d not found", nasID)
	}
	return nil
}

// ── Reading ──────────────────────────────────────────────────────────────────

// ListTrafficRouters returns every router with its polling state.
func (s *NASStore) ListTrafficRouters(ctx context.Context) ([]nastraffic.RouterStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT n.id, host(n.ip), n.vendor, COALESCE(n.description, ''), n.snmp_enabled,
		       n.snmp_community_encrypted IS NOT NULL, n.snmp_port,
		       n.snmp_last_polled_at, n.snmp_last_ok_at, COALESCE(n.snmp_last_error, ''),
		       (SELECT count(*) FROM nas_interfaces i WHERE i.nas_id = n.id)
		  FROM nas_devices n
		 ORDER BY n.id`)
	if err != nil {
		return nil, fmt.Errorf("db: list traffic routers: %w", err)
	}
	defer rows.Close()
	var out []nastraffic.RouterStatus
	for rows.Next() {
		var r nastraffic.RouterStatus
		if err := rows.Scan(&r.NASID, &r.IP, &r.Vendor, &r.Description, &r.Enabled, &r.HasCommunity,
			&r.Port, &r.LastPolledAt, &r.LastOKAt, &r.LastError, &r.Interfaces); err != nil {
			return nil, fmt.Errorf("db: scan traffic router: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListNASInterfaces returns a router's interfaces with their latest rate,
// busiest first — the interface an operator opens a router's traffic page to
// look at is almost always the uplink, and it is almost always the busiest.
func (s *NASStore) ListNASInterfaces(ctx context.Context, nasID int) ([]nastraffic.InterfaceStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.name, i.alias, i.if_index, i.speed_mbps, i.oper_up, i.has_hc_counters, i.last_seen_at,
		       l.sampled_at, COALESCE(l.in_bps, 0), COALESCE(l.out_bps, 0)
		  FROM nas_interfaces i
		  LEFT JOIN LATERAL (
		        SELECT sampled_at, in_bps, out_bps FROM nas_interface_samples s
		         WHERE s.interface_id = i.id ORDER BY sampled_at DESC LIMIT 1
		  ) l ON TRUE
		 WHERE i.nas_id = $1
		 ORDER BY COALESCE(l.in_bps, 0) + COALESCE(l.out_bps, 0) DESC, i.name`, nasID)
	if err != nil {
		return nil, fmt.Errorf("db: list NAS interfaces: %w", err)
	}
	defer rows.Close()
	var out []nastraffic.InterfaceStatus
	for rows.Next() {
		var i nastraffic.InterfaceStatus
		if err := rows.Scan(&i.ID, &i.Name, &i.Alias, &i.Index, &i.SpeedMbps, &i.OperUp, &i.HasHC,
			&i.LastSeenAt, &i.LatestAt, &i.LatestInBps, &i.LatestOutBps); err != nil {
			return nil, fmt.Errorf("db: scan NAS interface: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// TrafficSeries returns one interface's samples in [from, to), bucketed.
//
// The average is weighted by each sample's interval rather than a plain mean
// of rates, so a sample that covered ten minutes after a late poll counts for
// twice one that covered five. Buckets with no samples are absent, not zero:
// zero is a measurement of an idle link, and an unmeasured period must be
// drawn as a gap, not as silence.
func (s *NASStore) TrafficSeries(ctx context.Context, interfaceID int, from, to time.Time, bucket time.Duration) ([]nastraffic.Point, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT date_bin(make_interval(secs => $4), sampled_at, $2) AS bucket,
		       (sum(in_bps::numeric * interval_seconds) / sum(interval_seconds))::bigint,
		       (sum(out_bps::numeric * interval_seconds) / sum(interval_seconds))::bigint,
		       max(in_bps), max(out_bps)
		  FROM nas_interface_samples
		 WHERE interface_id = $1 AND sampled_at >= $2 AND sampled_at < $3
		 GROUP BY bucket
		 ORDER BY bucket`, interfaceID, from, to, bucket.Seconds())
	if err != nil {
		return nil, fmt.Errorf("db: traffic series: %w", err)
	}
	defer rows.Close()
	var out []nastraffic.Point
	for rows.Next() {
		var p nastraffic.Point
		if err := rows.Scan(&p.At, &p.InAvgBps, &p.OutAvgBps, &p.InPeakBps, &p.OutPeakBps); err != nil {
			return nil, fmt.Errorf("db: scan traffic point: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NASInterface returns one interface, for checking it belongs to the router a
// URL names.
func (s *NASStore) NASInterface(ctx context.Context, interfaceID int) (nasID int, name string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT nas_id, name FROM nas_interfaces WHERE id = $1`, interfaceID).Scan(&nasID, &name)
	if isNoRows(err) {
		return 0, "", nil
	}
	return nasID, name, err
}
