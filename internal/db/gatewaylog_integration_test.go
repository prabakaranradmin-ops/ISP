//go:build integration

// Payment-gateway order and callback log — FR-PAY-001 | migration 064.
//
// The claims only a real database settles: a callback naming a subscriber
// who does not exist is still stored (NULL, not refused), the status
// vocabulary is enforced, the table has nowhere to put a signature or a
// raw body, and it is append-only against the role the services run as.
package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func gatewayEvent(kind, status, paymentID string, subscriberID int, amount string) billing.GatewayEvent {
	e := billing.GatewayEvent{
		Gateway: billing.DefaultGateway, Kind: kind, Event: "payment.captured",
		GatewayPaymentID: paymentID, SubscriberID: subscriberID, Status: status,
	}
	if amount != "" {
		e.Amount = decimal.RequireFromString(amount)
	}
	return e
}

func TestFR_PAY_001_EventsAreRecordedAndReadBack(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.GatewayLog()
	seedPlan(ctx, t, pool, 1, "Home", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	events := []billing.GatewayEvent{
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayCredited, "pay_ok", 1, "1180.00"),
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayUnmatched, "pay_orphan", 0, "500.00"),
		gatewayEvent(billing.GatewayKindOrder, billing.GatewayCreated, "", 1, "799.00"),
	}
	for _, e := range events {
		if err := store.RecordGatewayEvent(ctx, e); err != nil {
			t.Fatalf("record %s: %v", e.Status, err)
		}
	}

	got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}

	byStatus := map[string]billing.GatewayLogEntry{}
	for _, e := range got {
		byStatus[e.Status] = e
	}

	credited := byStatus[billing.GatewayCredited]
	if credited.SubscriberID == nil || *credited.SubscriberID != 1 || credited.Username != "acme" {
		t.Errorf("the credited event reads %+v", credited)
	}
	if credited.Amount == nil || credited.Amount.StringFixed(2) != "1180.00" {
		t.Errorf("the credited amount is %v", credited.Amount)
	}
	if !credited.Credited() || credited.NeedsAttention() {
		t.Error("a credited event reports itself wrongly")
	}

	// The row most worth having: money arrived and matched nobody. The FK
	// must not refuse it over the very mismatch it documents.
	unmatched := byStatus[billing.GatewayUnmatched]
	if unmatched.SubscriberID != nil {
		t.Errorf("the unmatched event names subscriber %v", unmatched.SubscriberID)
	}
	if !unmatched.NeedsAttention() {
		t.Error("an unmatched event does not report itself as needing attention")
	}

	t.Run("a subscriber id that does not exist is stored as NULL, not refused", func(t *testing.T) {
		e := gatewayEvent(billing.GatewayKindCallback, billing.GatewayUnmatched, "pay_ghost", 999999, "100.00")
		if err := store.RecordGatewayEvent(ctx, e); err != nil {
			t.Fatalf("an event naming an unknown subscriber was refused: %v", err)
		}
		got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{PaymentID: "pay_ghost"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].SubscriberID != nil {
			t.Errorf("read back as %+v", got)
		}
	})

	t.Run("an event with no amount is fine", func(t *testing.T) {
		e := gatewayEvent(billing.GatewayKindCallback, billing.GatewayRejected, "", 0, "")
		e.Detail = "failed the signature check"
		if err := store.RecordGatewayEvent(ctx, e); err != nil {
			t.Fatalf("an amountless event was refused: %v", err)
		}
	})

	t.Run("an unknown kind or a missing status is refused by the store", func(t *testing.T) {
		if err := store.RecordGatewayEvent(ctx,
			gatewayEvent("sideways", billing.GatewayCredited, "x", 1, "1.00")); err == nil {
			t.Error("an unknown kind was accepted")
		}
		if err := store.RecordGatewayEvent(ctx,
			gatewayEvent(billing.GatewayKindCallback, "", "x", 1, "1.00")); err == nil {
			t.Error("an event with no status was accepted")
		}
	})

	t.Run("an over-long detail is truncated rather than refused", func(t *testing.T) {
		e := gatewayEvent(billing.GatewayKindCallback, billing.GatewayFailed, "pay_long", 1, "1.00")
		e.Detail = strings.Repeat("x", 900)
		if err := store.RecordGatewayEvent(ctx, e); err != nil {
			t.Fatalf("a long detail failed the write: %v", err)
		}
	})
}

// The table has nowhere to put a signature, a secret or a raw body.
// Asserted against the real schema, because a column could be added by a
// later migration without touching any Go type.
func TestFR_PAY_001_TheTableHasNoColumnForACredentialOrRawBody(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		 WHERE table_name = 'payment_gateway_log'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	forbidden := []string{"signature", "secret", "password", "raw", "body", "payload", "token"}
	var n int
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		n++
		lower := strings.ToLower(c)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("payment_gateway_log has a column %q: the gateway log keeps no signature, secret or raw body (FR-PAY-001)", c)
			}
		}
	}
	if n == 0 {
		t.Fatal("payment_gateway_log has no columns; migration 064 did not run")
	}
}

func TestFR_PAY_001_TheSchemaEnforcesTheStatusVocabulary(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	cases := []struct{ name, sql string }{
		{"an unknown status", `INSERT INTO payment_gateway_log (kind, status) VALUES ('callback', 'maybe')`},
		{"an unknown kind", `INSERT INTO payment_gateway_log (kind, status) VALUES ('telepathy', 'credited')`},
		{"a negative amount", `INSERT INTO payment_gateway_log (kind, status, amount) VALUES ('callback', 'credited', -1)`},
		// 'duplicate' was removed from the vocabulary because nothing can
		// produce it: Recharge is idempotent and gives the webhook no way
		// to tell a retried callback from a first one.
		{"the removed duplicate status", `INSERT INTO payment_gateway_log (kind, status) VALUES ('callback', 'duplicate')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tc.sql); err == nil {
				t.Error("the database accepted it")
			}
		})
	}
}

func TestFR_PAY_001_FilteringAndSummary(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.GatewayLog()
	seedPlan(ctx, t, pool, 1, "Home", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	for _, e := range []billing.GatewayEvent{
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayCredited, "p1", 1, "100.00"),
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayCredited, "p2", 1, "200.00"),
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayIgnored, "p3", 0, ""),
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayUnmatched, "p4", 0, "300.00"),
		gatewayEvent(billing.GatewayKindOrder, billing.GatewayCreated, "", 1, "799.00"),
	} {
		if err := store.RecordGatewayEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("by status", func(t *testing.T) {
		got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{Status: billing.GatewayCredited})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("got %d credited, want 2", len(got))
		}
	})

	t.Run("by kind", func(t *testing.T) {
		got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{Kind: billing.GatewayKindOrder})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("got %d orders, want 1", len(got))
		}
	})

	t.Run("by payment id, which is what support quotes", func(t *testing.T) {
		got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{PaymentID: "p4"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Status != billing.GatewayUnmatched {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("by subscriber", func(t *testing.T) {
		got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{SubscriberID: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("got %d for subscriber 1, want 3", len(got))
		}
	})

	// The reconciliation view: authentic money that did not reach a
	// wallet. 'ignored' and 'credited' are excluded because both are
	// working as intended and would drown the rows that are not.
	t.Run("problems only", func(t *testing.T) {
		got, err := store.ListGatewayEvents(ctx, billing.GatewayLogFilter{ProblemsOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Status != billing.GatewayUnmatched {
			t.Fatalf("problems-only returned %+v", got)
		}
		// And it agrees with the domain's own judgement, so the SQL and
		// NeedsAttention cannot drift apart.
		for _, e := range got {
			if !e.NeedsAttention() {
				t.Errorf("%s came back in the problems view but does not need attention", e.Status)
			}
		}
	})

	t.Run("the summary groups by status", func(t *testing.T) {
		since := time.Now().Add(-time.Hour)
		got, err := store.SummariseGatewayEvents(ctx, billing.GatewayLogFilter{From: &since})
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, c := range got {
			counts[c.Status] = c.Count
		}
		if counts[billing.GatewayCredited] != 2 || counts[billing.GatewayUnmatched] != 1 {
			t.Errorf("summary is %+v", counts)
		}
	})
}

// Append-only against the application role, like the audit trail.
//
// One transaction per statement, and each assertion checks the error text:
// a failed statement aborts a Postgres transaction, so sharing one would
// make every assertion after the first pass on "current transaction is
// aborted" rather than on a privilege denial.
func TestFR_PAY_001_TheLogIsAppendOnlyAgainstTheApplicationRole(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	if err := database.GatewayLog().RecordGatewayEvent(ctx,
		gatewayEvent(billing.GatewayKindCallback, billing.GatewayCredited, "pay_ok", 0, "100.00")); err != nil {
		t.Fatal(err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bss_app')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("bss_app does not exist in this database; migration 019 did not run")
	}

	asBssApp := func(t *testing.T, sql string) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE bss_app`); err != nil {
			t.Skipf("cannot assume the bss_app role here: %v", err)
		}
		_, err = tx.Exec(ctx, sql)
		return err
	}

	t.Run("it can read and append", func(t *testing.T) {
		if err := asBssApp(t, `SELECT COUNT(*) FROM payment_gateway_log`); err != nil {
			t.Errorf("bss_app cannot read the log: %v", err)
		}
		if err := asBssApp(t, `
			INSERT INTO payment_gateway_log (kind, status) VALUES ('callback', 'ignored')`); err != nil {
			t.Errorf("bss_app cannot append to the log: %v", err)
		}
	})

	for _, tc := range []struct{ name, sql string }{
		{"it cannot change an entry", `UPDATE payment_gateway_log SET status = 'credited'`},
		{"it cannot delete an entry", `DELETE FROM payment_gateway_log`},
		{"it cannot truncate the table", `TRUNCATE payment_gateway_log`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := asBssApp(t, tc.sql)
			if err == nil {
				t.Fatalf("bss_app succeeded; the log is not append-only")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				t.Errorf("it failed for the wrong reason: %v", err)
			}
		})
	}
}
