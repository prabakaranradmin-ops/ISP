package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
)

// Partner API and webhook persistence — FR-API-001..003 | migration 033 | MDS §4.22.

// PartnerStore reads and writes api_keys, webhook_endpoints and
// webhook_deliveries.
type PartnerStore struct{ pool dbPool }

// apiKeyColumns is a SQL projection, not a credential. gosec's G101 flags
// the "key_hash" substring as a possible hardcoded secret; the column is
// deliberately absent from this list precisely so a routine key listing
// cannot leak it.
//
//nolint:gosec // G101 false positive: column names, no secret material
const apiKeyColumns = `
	k.id, k.partner_name, k.key_prefix, k.scopes, k.active,
	k.last_used_at, k.expires_at, k.created_by, k.created_at, k.revoked_at`

func scanAPIKey(row interface{ Scan(dest ...any) error }) (*partner.APIKey, error) {
	var k partner.APIKey
	err := row.Scan(&k.ID, &k.PartnerName, &k.KeyPrefix, &k.Scopes, &k.Active,
		&k.LastUsedAt, &k.ExpiresAt, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// CreateAPIKey stores a new key. The plaintext never reaches this layer.
func (s *PartnerStore) CreateAPIKey(ctx context.Context, partnerName, prefix, hash string,
	scopes []string, expiresAt *time.Time, createdBy string,
) (*partner.APIKey, error) {
	const q = `
		WITH ins AS (
			INSERT INTO api_keys (partner_name, key_prefix, key_hash, scopes, expires_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING *
		)
		SELECT ` + apiKeyColumns + ` FROM ins k`

	k, err := scanAPIKey(s.pool.QueryRow(ctx, q, partnerName, prefix, hash, scopes, expiresAt, createdBy))
	if err != nil {
		return nil, fmt.Errorf("db: create api key for %q: %w", partnerName, err)
	}
	return k, nil
}

// AuthenticateKey resolves a presented key to its record.
//
// Returns (nil, nil) for every failure a caller must not distinguish: unknown
// prefix, wrong secret, revoked, expired. The endpoint answers 401 for all of
// them, because telling an attacker which part of a credential was wrong is
// telling them what to fix.
//
// The hash comparison happens in Go, not SQL: a `WHERE key_hash = $2` would
// compare in the database with no constant-time guarantee and would put the
// hash in the query log.
func (s *PartnerStore) AuthenticateKey(ctx context.Context, presented string) (*partner.APIKey, error) {
	prefix, ok := partner.ParsePrefix(presented)
	if !ok {
		return nil, nil
	}

	const q = `SELECT ` + apiKeyColumns + `, k.key_hash FROM api_keys k WHERE k.key_prefix = $1 AND k.active`

	var k partner.APIKey
	var storedHash string
	err := s.pool.QueryRow(ctx, q, prefix).Scan(
		&k.ID, &k.PartnerName, &k.KeyPrefix, &k.Scopes, &k.Active,
		&k.LastUsedAt, &k.ExpiresAt, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt, &storedHash)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: authenticate api key: %w", err)
	}

	if !partner.VerifyKey(presented, storedHash) {
		return nil, nil
	}
	if !k.Usable(time.Now()) {
		return nil, nil
	}
	return &k, nil
}

// TouchKeyUsage records that a key was used.
//
// Best-effort and deliberately not in the request's critical path error
// handling: failing a partner's API call because a bookkeeping column could
// not be written would trade a real outage for a cosmetic loss.
func (s *PartnerStore) TouchKeyUsage(ctx context.Context, keyID int) {
	_, _ = s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`, keyID) //nolint:errcheck
}

// ListAPIKeys returns every key, newest first.
func (s *PartnerStore) ListAPIKeys(ctx context.Context) ([]partner.APIKey, error) {
	const q = `SELECT ` + apiKeyColumns + ` FROM api_keys k ORDER BY k.created_at DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list api keys: %w", err)
	}
	defer rows.Close()

	out := []partner.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan api key: %w", err)
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// RevokeAPIKey deactivates a key. Returns false if it was already revoked.
//
// The `active` predicate makes this an atomic conditional claim, the same
// pattern used for approvals and CPE tasks: a second revoke reports honestly
// that it changed nothing rather than silently overwriting revoked_at and
// losing when the key actually stopped working.
func (s *PartnerStore) RevokeAPIKey(ctx context.Context, keyID int) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = NOW() WHERE id = $1 AND active`, keyID)
	if err != nil {
		return false, fmt.Errorf("db: revoke api key %d: %w", keyID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ── Webhook endpoints ───────────────────────────────────────────────────────

const webhookEndpointColumns = `
	e.id, e.api_key_id, e.url, e.events, e.active, e.description, e.created_at`

// CreateWebhookEndpoint registers a partner callback.
func (s *PartnerStore) CreateWebhookEndpoint(ctx context.Context, apiKeyID int, url,
	secretEncrypted, keyVersion string, events []string, description string,
) (*partner.WebhookEndpoint, error) {
	const q = `
		WITH ins AS (
			INSERT INTO webhook_endpoints (api_key_id, url, secret_encrypted, key_version_id, events, description)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
			RETURNING *
		)
		SELECT ` + webhookEndpointColumns + ` FROM ins e`

	var e partner.WebhookEndpoint
	err := s.pool.QueryRow(ctx, q, apiKeyID, url, secretEncrypted, keyVersion, events, description).
		Scan(&e.ID, &e.APIKeyID, &e.URL, &e.Events, &e.Active, &e.Description, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("db: create webhook endpoint: %w", err)
	}
	return &e, nil
}

// ListWebhookEndpoints returns a partner's endpoints.
func (s *PartnerStore) ListWebhookEndpoints(ctx context.Context, apiKeyID int) ([]partner.WebhookEndpoint, error) {
	const q = `SELECT ` + webhookEndpointColumns + `
		FROM webhook_endpoints e WHERE e.api_key_id = $1 ORDER BY e.id`

	rows, err := s.pool.Query(ctx, q, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("db: list webhook endpoints: %w", err)
	}
	defer rows.Close()

	out := []partner.WebhookEndpoint{}
	for rows.Next() {
		var e partner.WebhookEndpoint
		if err := rows.Scan(&e.ID, &e.APIKeyID, &e.URL, &e.Events, &e.Active, &e.Description, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan webhook endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeactivateWebhookEndpoint stops deliveries to an endpoint.
func (s *PartnerStore) DeactivateWebhookEndpoint(ctx context.Context, endpointID, apiKeyID int) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE webhook_endpoints SET active = FALSE WHERE id = $1 AND api_key_id = $2 AND active`,
		endpointID, apiKeyID)
	if err != nil {
		return false, fmt.Errorf("db: deactivate webhook endpoint %d: %w", endpointID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// SubscribersFor returns the active endpoints subscribed to an event type,
// with the secret still encrypted — decryption happens in the sender, which is
// the only component that needs the plaintext.
func (s *PartnerStore) SubscribersFor(ctx context.Context, eventType string) ([]partner.EndpointSecret, error) {
	// The api_keys join is what stops a revoked partner from continuing to
	// receive events: deactivating a key must silence its webhooks too, or
	// revocation is only half a revocation.
	const q = `
		SELECT e.id, e.url, e.secret_encrypted, e.key_version_id
		  FROM webhook_endpoints e
		  JOIN api_keys k ON k.id = e.api_key_id
		 WHERE e.active AND k.active AND $1 = ANY(e.events)
		   AND (k.expires_at IS NULL OR k.expires_at > NOW())
		 ORDER BY e.id`

	rows, err := s.pool.Query(ctx, q, eventType)
	if err != nil {
		return nil, fmt.Errorf("db: webhook subscribers for %q: %w", eventType, err)
	}
	defer rows.Close()

	out := []partner.EndpointSecret{}
	for rows.Next() {
		var e partner.EndpointSecret
		if err := rows.Scan(&e.EndpointID, &e.URL, &e.SecretEncrypted, &e.KeyVersion); err != nil {
			return nil, fmt.Errorf("db: scan webhook subscriber: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── Delivery log ────────────────────────────────────────────────────────────

// RecordDeliveryAttempt creates or updates the audit row for one delivery.
//
// ON CONFLICT on (endpoint_id, event_id) makes a retry update the existing row
// rather than inserting a second one. Without it a queue retry after a
// mid-write crash would double-log, and the attempt count — the number used to
// spot a flapping partner — would be meaningless.
func (s *PartnerStore) RecordDeliveryAttempt(ctx context.Context, endpointID int, ev partner.Event,
	status string, responseStatus *int, responseExcerpt, lastError string, nextAttempt *time.Time,
) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("db: marshal webhook payload: %w", err)
	}

	const q = `
		INSERT INTO webhook_deliveries (
			endpoint_id, event_id, event_type, payload, status, attempts,
			response_status, response_excerpt, last_error, next_attempt_at, delivered_at)
		-- $5 is cast explicitly because it appears both as a value and inside a
		-- comparison; without the cast Postgres deduces two different types for
		-- the same parameter and rejects the statement (SQLSTATE 42P08).
		VALUES ($1, $2, $3, $4, $5::varchar, 1, $6, NULLIF($7,''), NULLIF($8,''), $9,
		        CASE WHEN $5::text = 'delivered' THEN NOW() END)
		ON CONFLICT (endpoint_id, event_id) DO UPDATE SET
			attempts         = webhook_deliveries.attempts + 1,
			status           = EXCLUDED.status,
			response_status  = EXCLUDED.response_status,
			response_excerpt = EXCLUDED.response_excerpt,
			last_error       = EXCLUDED.last_error,
			next_attempt_at  = EXCLUDED.next_attempt_at,
			delivered_at     = COALESCE(webhook_deliveries.delivered_at, EXCLUDED.delivered_at)`

	_, err = s.pool.Exec(ctx, q, endpointID, ev.EventID, ev.EventType, payload,
		status, responseStatus, responseExcerpt, lastError, nextAttempt)
	if err != nil {
		return fmt.Errorf("db: record webhook delivery: %w", err)
	}
	return nil
}

// ListDeliveries returns recent deliveries for an endpoint (FR-API-003).
func (s *PartnerStore) ListDeliveries(ctx context.Context, endpointID, limit int) ([]partner.Delivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
		SELECT id, endpoint_id, event_id, event_type, status, attempts,
		       response_status, response_excerpt, last_error, created_at, delivered_at
		  FROM webhook_deliveries
		 WHERE endpoint_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`

	rows, err := s.pool.Query(ctx, q, endpointID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list webhook deliveries: %w", err)
	}
	defer rows.Close()

	out := []partner.Delivery{}
	for rows.Next() {
		var d partner.Delivery
		var eventID uuid.UUID
		if err := rows.Scan(&d.ID, &d.EndpointID, &eventID, &d.EventType, &d.Status, &d.Attempts,
			&d.ResponseStatus, &d.ResponseExcerpt, &d.LastError, &d.CreatedAt, &d.DeliveredAt); err != nil {
			return nil, fmt.Errorf("db: scan webhook delivery: %w", err)
		}
		d.EventID = eventID
		out = append(out, d)
	}
	return out, rows.Err()
}
