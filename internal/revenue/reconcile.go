// Package revenue implements reconciliation jobs, collections forecast, and
// franchise commission calculation.
//
// FR: FR-REV-001..004, FR-FRN-001..002 | DDS §5.10 | DBD §6.2
package revenue

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/shopspring/decimal"
)

var (
	revenueReconcileLastRun = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "revenue_reconcile_last_run_timestamp",
		Help: "Unix timestamp of the last revenue reconciliation run",
	})
	ledgerVarianceAlert = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "revenue_ledger_variance",
		Help: "Current ledger variance amount (should be 0.00)",
	})
)

// RevenueSnapshot holds one day's revenue health metrics.
type RevenueSnapshot struct {
	SnapshotDate            time.Time
	UnbilledSubscriberCount int
	LedgerVariance          decimal.Decimal
	TotalWalletBalance      decimal.Decimal
}

// CollectionsForecast is a 30-day forward-looking revenue estimate.
type CollectionsForecast struct {
	ForecastDate     time.Time
	ForecastForDate  time.Time
	ExpectedRenewals int
	AtRiskRenewals   int
	ExpectedRevenue  decimal.Decimal
	AtRiskRevenue    decimal.Decimal
}

// RevenueQuerier is the DB interface for reconciliation jobs.
type RevenueQuerier interface {
	GetUnbilledActiveSubscribers(ctx context.Context) (int, error)
	GetLedgerVariance(ctx context.Context) (decimal.Decimal, error)
	GetTotalWalletBalance(ctx context.Context) (decimal.Decimal, error)
	UpsertRevenueSnapshot(ctx context.Context, snap RevenueSnapshot) error
	BuildCollectionsForecast(ctx context.Context, days int) ([]CollectionsForecast, error)
	UpsertCollectionsForecast(ctx context.Context, forecasts []CollectionsForecast) error
}

// Alerter fires alerts when thresholds are breached.
type Alerter interface {
	Trigger(event string, detail any)
}

// ReconcileJob runs nightly at 02:00 IST on a nightly schedule.
//
// FR: FR-REV-001..004 | DDS §5.10
type ReconcileJob struct {
	db      RevenueQuerier
	alerter Alerter
}

// NewReconcileJob constructs a ReconcileJob.
func NewReconcileJob(db RevenueQuerier, alerter Alerter) *ReconcileJob {
	return &ReconcileJob{db: db, alerter: alerter}
}

// Run executes the full nightly reconciliation:
// 1. Unbilled subscriber count → revenue_snapshots
// 2. Ledger variance check → alert if ABS > 0.01
// 3. 30-day collections forecast → collections_forecast
func (j *ReconcileJob) Run(ctx context.Context) error {
	// 1. Unbilled subscriber count
	unbilledCount, err := j.db.GetUnbilledActiveSubscribers(ctx)
	if err != nil {
		return fmt.Errorf("revenue: get unbilled subscribers: %w", err)
	}

	// 2. Ledger variance
	variance, err := j.db.GetLedgerVariance(ctx)
	if err != nil {
		return fmt.Errorf("revenue: get ledger variance: %w", err)
	}
	ledgerVarianceAlert.Set(variance.InexactFloat64())
	if variance.Abs().GreaterThan(decimal.NewFromFloat(0.01)) {
		j.alerter.Trigger("ledger_variance_detected", variance)
	}

	// 3. Total wallet balance
	totalBalance, err := j.db.GetTotalWalletBalance(ctx)
	if err != nil {
		return fmt.Errorf("revenue: get total wallet balance: %w", err)
	}

	snap := RevenueSnapshot{
		SnapshotDate:            time.Now().UTC().Truncate(24 * time.Hour),
		UnbilledSubscriberCount: unbilledCount,
		LedgerVariance:          variance,
		TotalWalletBalance:      totalBalance,
	}
	if err := j.db.UpsertRevenueSnapshot(ctx, snap); err != nil {
		return fmt.Errorf("revenue: upsert snapshot: %w", err)
	}

	// 4. Collections forecast
	forecasts, err := j.db.BuildCollectionsForecast(ctx, 30)
	if err != nil {
		return fmt.Errorf("revenue: build forecast: %w", err)
	}
	if err := j.db.UpsertCollectionsForecast(ctx, forecasts); err != nil {
		return fmt.Errorf("revenue: upsert forecast: %w", err)
	}

	revenueReconcileLastRun.SetToCurrentTime()
	return nil
}
