//go:build integration

// Per-subscriber grace and configurable dunning — FR-BIL-016/017 | migration 055.
//
// Driven through the real scanner against the real store, so the SQL window,
// the grace column and the state machine are exercised together: a unit
// test of any one of them passes while the joins between them are wrong.
package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

func TestFR_BIL_016_017_GraceAndPolicyEndToEnd(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	t.Cleanup(func() {
		// billing_settings is a single reference row, not truncated between
		// tests; leave the shipped schedule behind for whoever runs next.
		_ = store.SetDunningPolicy(context.Background(), billing.DefaultDunningPolicy(), "")
	})
	if err := store.SetDunningPolicy(ctx, billing.DefaultDunningPolicy(), ""); err != nil {
		t.Fatal(err)
	}

	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	lapsed := time.Now().AddDate(0, 0, -20)
	inTen := time.Now().AddDate(0, 0, 10)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "lapsed", Status: "hard_suspended", DunningState: "hard_suspended", PlanExpiry: &lapsed})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "due_in_ten", PlanExpiry: &inTen, MobileNumber: "+919800000002"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "paid_up", MobileNumber: "+919800000003"})

	state := func(id int) string {
		t.Helper()
		return scanString(ctx, t, pool, `SELECT dunning_state || '/' || status FROM subscribers WHERE id = $1`, id)
	}
	scan := func() {
		t.Helper()
		if err := billing.NewDunningScanner(store, nil).Scan(ctx); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}

	t.Run("the shipped schedule is what a fresh install reads", func(t *testing.T) {
		p, err := store.DunningPolicy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		d := billing.DefaultDunningPolicy()
		if p.RemindFirstDays != d.RemindFirstDays || p.GraceDays != d.GraceDays || p.SoftSuspendDays != d.SoftSuspendDays {
			t.Errorf("got %+v", p)
		}
	})

	t.Run("grace cannot be set on someone who has paid", func(t *testing.T) {
		err := store.SetGraceExtension(ctx, 3, time.Now().AddDate(0, 0, 3), "x", "meena")
		if !errors.Is(err, billing.ErrGraceNotApplicable) {
			t.Errorf("want ErrGraceNotApplicable, got %v", err)
		}
	})

	t.Run("an extension without a reason is refused by the table itself", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE subscribers SET grace_until = now() + interval '2 days' WHERE id = 1`); err == nil {
			t.Error("chk_grace_has_reason must refuse an unexplained extension")
		}
	})

	t.Run("a grace extension lifts a hard suspension on the next pass", func(t *testing.T) {
		if err := store.SetGraceExtension(ctx, 1, time.Now().AddDate(0, 0, 4), "salary on Friday", "meena"); err != nil {
			t.Fatal(err)
		}
		if got := scanString(ctx, t, pool, `SELECT grace_reason || '/' || grace_set_by FROM subscribers WHERE id = 1`); got != "salary on Friday/meena" {
			t.Errorf("stored: %s", got)
		}
		// grace_period is a status RADIUS authorises (radius/authorisation.go):
		// service is back on.
		scan()
		if got := state(1); got != "grace_period/grace_period" {
			t.Errorf("want grace_period/grace_period, got %s", got)
		}
		scan() // and it holds: a second pass must not walk them back down
		if got := state(1); got != "grace_period/grace_period" {
			t.Errorf("second pass: want grace_period, got %s", got)
		}
	})

	t.Run("ending it hands them back to the schedule", func(t *testing.T) {
		if err := store.ClearGraceExtension(ctx, 1); err != nil {
			t.Fatal(err)
		}
		// Twenty days past expiry is hard suspension; the scanner walks
		// every stage in between within one pass, as it always has.
		scan()
		if got := state(1); got != "hard_suspended/hard_suspended" {
			t.Errorf("want hard_suspended, got %s", got)
		}
	})

	t.Run("a first reminder set further out reaches subscribers the old window missed", func(t *testing.T) {
		scan()
		if got := state(2); got != "active/active" {
			t.Fatalf("under the default 7 days, 10 days out is not due: %s", got)
		}
		p := billing.DunningPolicy{RemindFirstDays: 12, RemindSecondDays: 5, RemindLastDays: 1, GraceDays: 7, SoftSuspendDays: 5}
		if err := store.SetDunningPolicy(ctx, p, "meena"); err != nil {
			t.Fatal(err)
		}
		scan()
		if got := state(2); got != "remind_7d/active" {
			t.Errorf("12-day first reminder: want remind_7d, got %s", got)
		}
		if got := state(3); got != "active/active" {
			t.Errorf("30 days out is still not due: %s", got)
		}
		read, _ := store.DunningPolicy(ctx)
		if read.UpdatedBy != "meena" || read.UpdatedAt.IsZero() {
			t.Errorf("attribution: %+v", read)
		}
	})

	t.Run("the table refuses a schedule the ladder could not follow", func(t *testing.T) {
		for name, p := range map[string]billing.DunningPolicy{
			"out of order":   {RemindFirstDays: 3, RemindSecondDays: 7, RemindLastDays: 1, GraceDays: 3, SoftSuspendDays: 3},
			"after expiry":   {RemindFirstDays: 7, RemindSecondDays: 3, RemindLastDays: -1, GraceDays: 3, SoftSuspendDays: 3},
			"grace too long": {RemindFirstDays: 7, RemindSecondDays: 3, RemindLastDays: 1, GraceDays: 31, SoftSuspendDays: 3},
		} {
			if err := store.SetDunningPolicy(ctx, p, "meena"); err == nil {
				t.Errorf("%s: stored without complaint", name)
			}
		}
	})
}
