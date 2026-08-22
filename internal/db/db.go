// Package db is the PostgreSQL persistence layer. It provides the concrete
// implementations of the storage interfaces each domain package declares.
//
// Structure: every consumer gets its own small store type, all sharing one
// connection pool. This is deliberate rather than one large God-store — several
// consumers declare the same method name with different return types
// (GetSubscriberByUsername returns radius.Subscriber, api.SubscriberRecord and
// portal.SubscriberAuth to three different callers), which a single Go type
// cannot satisfy.
//
// Money: NUMERIC columns are read as text and parsed with decimal.NewFromString,
// and written as text with an explicit ::numeric cast. Money never passes
// through float64 at any point — binary floating point cannot represent 71.91,
// and GST totals must reconcile to the paisa (FR-BIL-002, DoD L0-007).
//
// DDS §5 | DBD §6.2
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// ErrNotFound is returned when a lookup matches no row. Callers that treat a
// missing row as a non-error condition should check with errors.Is.
var ErrNotFound = errors.New("db: not found")

// DB owns the connection pool and hands out per-domain stores.
type DB struct {
	pool *haPool
}

// Config carries the pool tuning knobs. IDD §8.3 sizes the pool at 25
// connections per service instance.
type Config struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// DefaultConfig returns the pool settings from IDD §8.3 for a given DSN.
func DefaultConfig(dsn string) Config {
	return Config{
		DSN:             dsn,
		MaxConns:        25,
		MinConns:        5,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: 30 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

// Connect opens the pool and verifies it with a ping, so a bad DSN or an
// unreachable database fails at startup rather than on the first query.
func Connect(ctx context.Context, cfg Config) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: parse DSN: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &DB{pool: newHAPool(pool)}, nil
}

// New wraps an already-configured pool. Used by tests and by callers that
// manage the pool lifecycle themselves.
func New(pool *pgxpool.Pool) *DB {
	return &DB{pool: newHAPool(pool)}
}

// Close releases every pooled connection.
func (d *DB) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Pool exposes the underlying pool for health checks and migrations. Note
// this is the raw *pgxpool.Pool, not the SQLSTATE-25006-aware wrapper (see
// hapool.go) every store actually uses — callers that run queries directly
// against it (rather than through a store) do not get the automatic
// failover-detection Reset(). None do today.
func (d *DB) Pool() *pgxpool.Pool { return d.pool.Pool }

// Ping reports whether the database is reachable.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// ── Store accessors ─────────────────────────────────────────────────────────

// Radius returns the store satisfying radius.DBQuerier.
func (d *DB) Radius() *RadiusStore { return &RadiusStore{pool: d.pool} }

// API returns the store satisfying api.SubscriberQuerier and api.KYCQuerier.
func (d *DB) API() *APIStore { return &APIStore{pool: d.pool} }

// Portal returns the store satisfying the portal's subscriber, notification and
// ticket queriers.
func (d *DB) Portal() *PortalStore { return &PortalStore{pool: d.pool} }

// Billing returns the store satisfying billing.WalletQuerier and
// billing.DunningQuerier.
func (d *DB) Billing() *BillingStore { return &BillingStore{pool: d.pool} }

// Notifications returns the store satisfying notifications.NotifQuerier.
func (d *DB) Notifications() *NotificationStore { return &NotificationStore{pool: d.pool} }

// FUP returns the store satisfying fup.FUPQuerier and fup.CoAQuerier.
func (d *DB) FUP() *FUPStore { return &FUPStore{pool: d.pool} }

// Revenue returns the store satisfying revenue.RevenueQuerier,
// revenue.FranchiseQuerier and revenue.SubscriberLister.
func (d *DB) Revenue() *RevenueStore { return &RevenueStore{pool: d.pool} }

// Health returns the store satisfying health.DBQuerier.
func (d *DB) Health() *HealthStore { return &HealthStore{pool: d.pool} }

// Tickets returns the store satisfying api.TicketAdminQuerier.
func (d *DB) Tickets() *TicketStore { return &TicketStore{pool: d.pool} }

// NAS returns the store satisfying nas.DeviceStore.
func (d *DB) NAS() *NASStore { return &NASStore{pool: d.pool} }

// Workflow returns the store satisfying api.ApprovalQuerier and
// api.FieldTaskQuerier.
func (d *DB) Workflow() *WorkflowStore { return &WorkflowStore{pool: d.pool} }

// CRM returns the store satisfying api.LeadQuerier.
func (d *DB) CRM() *CRMStore { return &CRMStore{pool: d.pool} }

// Inventory returns the store satisfying api.InventoryQuerier.
func (d *DB) Inventory() *InventoryStore { return &InventoryStore{pool: d.pool} }

// Announcements returns the store satisfying api.AnnouncementQuerier.
func (d *DB) Announcements() *AnnouncementStore { return &AnnouncementStore{pool: d.pool} }

// TR069 returns the store satisfying tr069.Store and api.CPEControlQuerier.
func (d *DB) TR069() *TR069Store { return &TR069Store{pool: d.pool} }

// Reporting returns the store satisfying reporting.Refresher and the
// api reporting queriers.
func (d *DB) Reporting() *ReportingStore { return &ReportingStore{pool: d.pool} }

// ── Money helpers ───────────────────────────────────────────────────────────

// parseDecimal converts a NUMERIC-as-text column into a decimal.
//
// Every money column is selected as `col::text` and parsed here. Scanning into
// float64 would round 71.91 and break the ledger reconciliation that FR-REV-002
// asserts to the paisa.
func parseDecimal(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("db: parse numeric %q: %w", s, err)
	}
	return d, nil
}

// decimalFromInt lifts a count into a decimal so it can multiply money
// without the result ever passing through float64.
func decimalFromInt(n int) decimal.Decimal { return decimal.NewFromInt(int64(n)) }

// ── Query helpers ───────────────────────────────────────────────────────────

// isNoRows reports whether err is pgx's no-rows sentinel.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// inTx runs fn inside a transaction, committing on success and rolling back on
// error or panic. pool is the dbPool interface (hapool.go), not the
// concrete *pgxpool.Pool, so a write inside the transaction gets the same
// SQLSTATE 25006 detection an ordinary Exec/QueryRow call does.
func inTx(ctx context.Context, pool dbPool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin transaction: %w", err)
	}

	committed := false
	//nolint:contextcheck // the fresh context is the point; see below
	defer func() {
		if !committed {
			// Deliberately not derived from ctx: the usual reason we reach this
			// path is that ctx was cancelled, and a rollback issued on a dead
			// context is dropped, returning the connection to the pool with an
			// open transaction still on it.
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx) //nolint:errcheck // nothing useful to do if rollback fails
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit transaction: %w", err)
	}
	committed = true
	return nil
}

// Partner returns the store satisfying api.PartnerQuerier,
// middleware.APIKeyAuthenticator and partner.DeliveryStore.
func (d *DB) Partner() *PartnerStore { return &PartnerStore{pool: d.pool} }

// Hotspot returns the store satisfying radius.MABQuerier and the hotspot
// captive-portal queriers.
func (d *DB) Hotspot() *HotspotStore { return &HotspotStore{pool: d.pool} }

// Archive returns the store backing document archival and the retention purge
// sweep (FR-DOC-001).
func (d *DB) Archive() *ArchiveStore { return &ArchiveStore{pool: d.pool} }

// Demo returns the store backing the console's "Demo Data" panel.
func (d *DB) Demo() *DemoStore { return NewDemoStore(d.pool) }
