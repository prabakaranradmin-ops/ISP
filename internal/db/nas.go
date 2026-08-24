package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// NASStore serves the multi-vendor NAS attribute engine: the registered
// device inventory (nas.Resolver's cache source) and per-plan vendor
// profile mappings for policy-reference vendors.
//
// Satisfies nas.DeviceStore.
type NASStore struct{ pool dbPool }

var _ nas.DeviceStore = (*NASStore)(nil)

// ListNASDevices returns every registered NAS, secret still encrypted —
// nas.Resolver decrypts on load, keeping plaintext secrets out of this
// package entirely (FR-SEC-002's PII-encryption discipline extended to the
// RADIUS shared secret, which is exactly as sensitive).
func (s *NASStore) ListNASDevices(ctx context.Context) ([]nas.DeviceRow, error) {
	const q = `
		SELECT id, host(ip), vendor, radius_secret_encrypted, coa_port, pod_port, allow_mab
		FROM nas_devices`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list nas_devices: %w", err)
	}
	defer rows.Close()

	var out []nas.DeviceRow
	for rows.Next() {
		var row nas.DeviceRow
		if err := rows.Scan(&row.ID, &row.IP, &row.Vendor, &row.RadiusSecretEncrypted, &row.CoAPort, &row.PoDPort, &row.AllowMAB); err != nil {
			return nil, fmt.Errorf("db: scan nas_devices row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate nas_devices: %w", err)
	}
	return out, nil
}

// ── Management (FR-NAS-001 | MDS §4.11) ─────────────────────────────────────
//
// Registering a NAS and turning MAB on for it were direct SQL until now, which
// made the one prerequisite a hotspot deployment cannot skip
// (nas_devices.allow_mab) reachable only by someone with a psql prompt.

// CreateNASDevice registers a NAS and returns the operator-facing summary.
func (s *NASStore) CreateNASDevice(ctx context.Context, d nas.NewNASDevice) (*nas.DeviceSummary, error) {
	const q = `
		INSERT INTO nas_devices (
			ip, vendor, description, radius_secret_encrypted, key_version_id,
			coa_port, pod_port, allow_mab)
		VALUES ($1::inet, $2, NULLIF($3,''), $4, $5, $6, $7, $8)
		RETURNING id, host(ip), vendor, COALESCE(description,''), coa_port, pod_port,
		          allow_mab, created_at, updated_at`

	var out nas.DeviceSummary
	err := s.pool.QueryRow(ctx, q, d.IP, d.Vendor, d.Description, d.SecretEncrypted,
		d.KeyVersion, d.CoAPort, d.PoDPort, d.AllowMAB).
		Scan(&out.ID, &out.IP, &out.Vendor, &out.Description, &out.CoAPort, &out.PoDPort,
			&out.AllowMAB, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("db: create nas device %s: %w", d.IP, err)
	}
	return &out, nil
}

// UpdateNASDevice applies a partial update, returning nil when no such device.
//
// The secret and its key version move together or not at all: storing
// ciphertext against the wrong key version makes it undecryptable, which would
// take the NAS offline at the next resolver refresh rather than at the moment
// of the mistake.
func (s *NASStore) UpdateNASDevice(ctx context.Context, id int, u nas.NASDeviceUpdate) (*nas.DeviceSummary, error) {
	const q = `
		UPDATE nas_devices SET
			vendor                  = COALESCE($2, vendor),
			description             = COALESCE($3, description),
			coa_port                = COALESCE($4, coa_port),
			pod_port                = COALESCE($5, pod_port),
			allow_mab               = COALESCE($6, allow_mab),
			radius_secret_encrypted = COALESCE($7, radius_secret_encrypted),
			key_version_id          = COALESCE($8, key_version_id),
			updated_at              = NOW()
		WHERE id = $1
		RETURNING id, host(ip), vendor, COALESCE(description,''), coa_port, pod_port,
		          allow_mab, created_at, updated_at`

	var out nas.DeviceSummary
	err := s.pool.QueryRow(ctx, q, id, u.Vendor, u.Description, u.CoAPort, u.PoDPort,
		u.AllowMAB, u.SecretEncrypted, u.KeyVersion).
		Scan(&out.ID, &out.IP, &out.Vendor, &out.Description, &out.CoAPort, &out.PoDPort,
			&out.AllowMAB, &out.CreatedAt, &out.UpdatedAt)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update nas device %d: %w", id, err)
	}
	return &out, nil
}

// ListNASDeviceSummaries returns the registered NAS inventory without secrets.
//
// A separate query from ListNASDevices rather than a filter over it: the
// ciphertext is never selected, so it cannot reach a response by accident.
func (s *NASStore) ListNASDeviceSummaries(ctx context.Context) ([]nas.DeviceSummary, error) {
	const q = `
		SELECT id, host(ip), vendor, COALESCE(description,''), coa_port, pod_port,
		       allow_mab, created_at, updated_at
		  FROM nas_devices
		 ORDER BY id`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list nas device summaries: %w", err)
	}
	defer rows.Close()

	out := make([]nas.DeviceSummary, 0, 16)
	for rows.Next() {
		var d nas.DeviceSummary
		if err := rows.Scan(&d.ID, &d.IP, &d.Vendor, &d.Description, &d.CoAPort,
			&d.PoDPort, &d.AllowMAB, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan nas device summary: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate nas device summaries: %w", err)
	}
	return out, nil
}

// GetNetworkHealth reports every registered NAS device alongside how many
// sessions are active on it right now (CRD-EXP-008). LEFT JOIN so a device
// with zero active sessions still gets a row — the point is to see the
// whole registered estate at a glance, not just the busy half of it. Reads
// subscriber_session_history the same way the health panel already does
// (stop_time IS NULL means still connected); no new table, no migration.
func (s *NASStore) GetNetworkHealth(ctx context.Context) ([]nas.NetworkHealthRow, error) {
	const q = `
		SELECT n.id, host(n.ip), n.vendor, COALESCE(n.description, ''),
		       COUNT(h.id) FILTER (WHERE h.stop_time IS NULL)
		  FROM nas_devices n
		  LEFT JOIN subscriber_session_history h ON h.nas_ip_address = n.ip AND h.stop_time IS NULL
		 GROUP BY n.id, n.ip, n.vendor, n.description
		 ORDER BY n.id`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: network health: %w", err)
	}
	defer rows.Close()

	out := make([]nas.NetworkHealthRow, 0, 16)
	for rows.Next() {
		var r nas.NetworkHealthRow
		if err := rows.Scan(&r.ID, &r.IP, &r.Vendor, &r.Description, &r.ActiveSessions); err != nil {
			return nil, fmt.Errorf("db: scan network health row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate network health: %w", err)
	}
	return out, nil
}

// ListPlanNASProfiles returns every plan-to-vendor-profile mapping, for
// nas.Resolver's cache (the same small-dataset, refresh-on-interval
// reasoning as ListNASDevices — a handful of plans times a handful of
// vendors, not a per-subscriber-scale table).
func (s *NASStore) ListPlanNASProfiles(ctx context.Context) ([]nas.PlanProfileRow, error) {
	const q = `SELECT plan_id, vendor, profile_name FROM plan_nas_profiles`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list plan_nas_profiles: %w", err)
	}
	defer rows.Close()

	var out []nas.PlanProfileRow
	for rows.Next() {
		var row nas.PlanProfileRow
		if err := rows.Scan(&row.PlanID, &row.Vendor, &row.ProfileName); err != nil {
			return nil, fmt.Errorf("db: scan plan_nas_profiles row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate plan_nas_profiles: %w", err)
	}
	return out, nil
}

// GetPlanNASProfile returns the pre-provisioned QoS profile/role name a
// plan maps to for a policy-reference vendor (FR-NAS-001). Returns "" with
// no error when no mapping exists — the caller (a vendor AttributeBuilder)
// is responsible for treating an empty profile name as a build error, so
// the nas_attribute_build_errors_total metric fires exactly once, at the
// point that actually knows it's a problem.
func (s *NASStore) GetPlanNASProfile(ctx context.Context, planID int, vendor string) (string, error) {
	const q = `
		SELECT profile_name FROM plan_nas_profiles
		WHERE plan_id = $1 AND vendor = $2`

	var profileName string
	err := s.pool.QueryRow(ctx, q, planID, vendor).Scan(&profileName)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: get plan_nas_profile for plan %d vendor %s: %w", planID, vendor, err)
	}
	return profileName, nil
}
