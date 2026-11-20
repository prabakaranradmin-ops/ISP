package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Payment-gateway order and callback log — FR-PAY-001 | migration 064.
//
// Append-only against the role the services run as: bss_app holds INSERT
// and SELECT with UPDATE and DELETE revoked, so this store offers no
// update or delete method — the database would refuse one. Rows here are
// evidence in a payment dispute.
//
// There is no column for the webhook signature, the gateway secret or the
// raw body, and nothing here writes one.

// GatewayLogStore records and reads gateway events.
type GatewayLogStore struct{ pool dbPool }

// NewGatewayLogStore constructs a GatewayLogStore.
func NewGatewayLogStore(pool dbPool) *GatewayLogStore { return &GatewayLogStore{pool: pool} }

// GatewayLog returns the gateway event store.
func (d *DB) GatewayLog() *GatewayLogStore { return NewGatewayLogStore(d.pool) }

var _ billing.GatewayLogger = (*GatewayLogStore)(nil)

// RecordGatewayEvent appends one order or callback.
//
// A subscriber id that does not exist is stored as NULL rather than
// failing the write: the row exists to say money arrived and could not be
// matched, and refusing it over the very mismatch it documents would lose
// exactly the record worth keeping.
func (s *GatewayLogStore) RecordGatewayEvent(ctx context.Context, e billing.GatewayEvent) error {
	gateway := strings.TrimSpace(e.Gateway)
	if gateway == "" {
		gateway = billing.DefaultGateway
	}
	if e.Kind != billing.GatewayKindOrder && e.Kind != billing.GatewayKindCallback {
		return fmt.Errorf("db: gateway event has an unknown kind %q", e.Kind)
	}
	if strings.TrimSpace(e.Status) == "" {
		return fmt.Errorf("db: gateway event has no status")
	}

	detail := e.Detail
	if len(detail) > 500 {
		detail = detail[:500]
	}
	orderID, paymentID := truncate(e.GatewayOrderID, 100), truncate(e.GatewayPaymentID, 100)

	var amount any
	if e.Amount.IsPositive() {
		amount = e.Amount.StringFixed(2)
	}

	// The subscriber is resolved to NULL when it names nobody, so the FK
	// cannot reject the row. A LEFT JOIN-ish guard rather than a lookup
	// first: one statement, and no window in which the subscriber could
	// go away between the check and the insert.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO payment_gateway_log
			(gateway, kind, event, gateway_order_id, gateway_payment_id,
			 subscriber_id, amount, status, detail)
		VALUES ($1, $2, $3, $4, $5,
		        (SELECT id FROM subscribers WHERE id = $6), $7::numeric, $8, $9)`,
		gateway, e.Kind, e.Event, orderID, paymentID,
		e.SubscriberID, amount, e.Status, detail)
	if err != nil {
		return fmt.Errorf("db: record gateway %s %q: %w", e.Kind, e.Status, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const gatewayLogColumns = `
	g.id, g.gateway, g.kind, g.event, g.gateway_order_id, g.gateway_payment_id,
	g.subscriber_id, COALESCE(s.username, ''), g.amount::text, g.status,
	g.detail, g.created_at`

// ListGatewayEvents returns events newest first.
func (s *GatewayLogStore) ListGatewayEvents(ctx context.Context, f billing.GatewayLogFilter) ([]billing.GatewayLogEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	// The problems-only view mirrors GatewayLogEntry.NeedsAttention:
	// authentic money that did not reach a wallet. 'ignored' is excluded
	// there for the same reason — it is working as intended and would
	// drown the rows that are not.
	const q = `
		SELECT ` + gatewayLogColumns + `
		  FROM payment_gateway_log g
		  LEFT JOIN subscribers s ON s.id = g.subscriber_id
		 WHERE ($1 = '' OR g.status = $1)
		   AND ($2 = '' OR g.kind = $2)
		   AND ($3 = '' OR g.gateway_payment_id = $3)
		   AND ($4 = 0 OR g.subscriber_id = $4)
		   AND (NOT $5::boolean OR g.status IN ('unmatched', 'failed', 'rejected', 'unconfigured'))
		   AND ($6::timestamptz IS NULL OR g.created_at >= $6)
		   AND ($7::timestamptz IS NULL OR g.created_at < $7)
		 ORDER BY g.created_at DESC, g.id DESC
		 LIMIT $8`

	rows, err := s.pool.Query(ctx, q,
		strings.TrimSpace(f.Status), strings.TrimSpace(f.Kind),
		strings.TrimSpace(f.PaymentID), f.SubscriberID, f.ProblemsOnly,
		f.From, f.To, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list gateway events: %w", err)
	}
	defer rows.Close()

	out := make([]billing.GatewayLogEntry, 0, f.Limit)
	for rows.Next() {
		var (
			e      billing.GatewayLogEntry
			amount *string
		)
		if err := rows.Scan(&e.ID, &e.Gateway, &e.Kind, &e.Event,
			&e.GatewayOrderID, &e.GatewayPaymentID, &e.SubscriberID, &e.Username,
			&amount, &e.Status, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan gateway event: %w", err)
		}
		if amount != nil {
			v, err := parseDecimal(*amount)
			if err != nil {
				return nil, err
			}
			e.Amount = &v
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate gateway events: %w", err)
	}
	return out, nil
}

// SummariseGatewayEvents groups events in a window by status, so the
// screen can lead with what is happening rather than with a list.
func (s *GatewayLogStore) SummariseGatewayEvents(ctx context.Context, f billing.GatewayLogFilter) ([]billing.GatewayStatusCount, error) {
	const q = `
		SELECT status, COUNT(*)
		  FROM payment_gateway_log
		 WHERE ($1::timestamptz IS NULL OR created_at >= $1)
		   AND ($2::timestamptz IS NULL OR created_at < $2)
		 GROUP BY status
		 ORDER BY COUNT(*) DESC`

	rows, err := s.pool.Query(ctx, q, f.From, f.To)
	if err != nil {
		return nil, fmt.Errorf("db: summarise gateway events: %w", err)
	}
	defer rows.Close()

	out := make([]billing.GatewayStatusCount, 0, 8)
	for rows.Next() {
		var c billing.GatewayStatusCount
		if err := rows.Scan(&c.Status, &c.Count); err != nil {
			return nil, fmt.Errorf("db: scan gateway status count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
