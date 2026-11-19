//go:build integration

// Plan editing and retirement — FR-CAT-001 | migration 061.
//
// The property that matters most is the one a unit test cannot show: a
// retired plan goes on renewing the subscribers already on it, at its
// price. Retiring a tariff must change what can be sold, not what anybody
// is paying.
package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
	"github.com/shopspring/decimal"
)

func newPlan(name, price string, validity int) staffui.Plan {
	return staffui.Plan{
		Name: name, RateLimitString: "100M/100M", VolumeGB: 3300,
		Price: decimal.RequireFromString(price), ValidityDays: validity,
	}
}

func TestFR_CAT_001_PlanLifecycle(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Catalogue()

	created, err := store.CreatePlan(ctx, newPlan("Fibre 100", "799.00", 30))
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if !created.Active {
		t.Error("a new plan is not offered")
	}
	if created.Subscribers != 0 {
		t.Errorf("a new plan claims %d subscribers", created.Subscribers)
	}

	t.Run("two offered plans cannot share a name, case-insensitively", func(t *testing.T) {
		_, err := store.CreatePlan(ctx, newPlan("  fibre 100  ", "899.00", 30))
		if !errors.Is(err, staffui.ErrPlanNameTaken) {
			t.Fatalf("want ErrPlanNameTaken, got %v", err)
		}
	})

	t.Run("editing changes the stored tariff and is attributed", func(t *testing.T) {
		edited := *created
		edited.Price = decimal.RequireFromString("999.00")
		edited.RateLimitString = "200M/200M"
		edited.ValidityDays = 60
		got, err := store.UpdatePlan(ctx, edited, "meena")
		if err != nil {
			t.Fatalf("update plan: %v", err)
		}
		if !got.Price.Equal(decimal.RequireFromString("999.00")) || got.RateLimitString != "200M/200M" {
			t.Errorf("stored plan reads %+v", got)
		}
		if got.UpdatedBy != "meena" || got.UpdatedAt == nil {
			t.Error("the edit was not attributed")
		}
	})

	t.Run("retiring hides it from the offered list but keeps it listed", func(t *testing.T) {
		if err := store.SetPlanActive(ctx, created.ID, false, "meena"); err != nil {
			t.Fatalf("retire: %v", err)
		}
		offered, err := store.ListActivePlans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range offered {
			if p.ID == created.ID {
				t.Error("a retired plan is still offered")
			}
		}
		all, err := store.ListPlans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, p := range all {
			if p.ID == created.ID {
				found, _ = true, p
				if p.Active {
					t.Error("the retired plan still reads as active")
				}
			}
		}
		if !found {
			t.Error("a retired plan vanished from the full listing; a subscriber on it would be unexplainable")
		}
	})

	// The usual way a tariff is re-priced: retire the old one, create its
	// replacement under the same name, leave existing subscribers alone.
	t.Run("a retired plan's name can be reused by its replacement", func(t *testing.T) {
		replacement, err := store.CreatePlan(ctx, newPlan("Fibre 100", "999.00", 30))
		if err != nil {
			t.Fatalf("reusing a retired plan's name: %v", err)
		}
		if replacement.ID == created.ID {
			t.Error("the retired row was reused")
		}

		// And the retired one cannot be offered again while its name is
		// taken — explained rather than a constraint error.
		if err := store.SetPlanActive(ctx, created.ID, true, "meena"); !errors.Is(err, staffui.ErrPlanNameTaken) {
			t.Fatalf("want ErrPlanNameTaken, got %v", err)
		}
		// Clear the way and it reinstates.
		if err := store.SetPlanActive(ctx, replacement.ID, false, "meena"); err != nil {
			t.Fatal(err)
		}
		if err := store.SetPlanActive(ctx, created.ID, true, "meena"); err != nil {
			t.Fatalf("reinstating once the name is free: %v", err)
		}
	})

	t.Run("editing a plan that does not exist is not-found", func(t *testing.T) {
		missing := newPlan("Nowhere", "1.00", 1)
		missing.ID = 999999
		if _, err := store.UpdatePlan(ctx, missing, "meena"); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		if err := store.SetPlanActive(ctx, 999999, false, "meena"); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("the subscriber count is real", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 1, seedOpts{
			Username: "onplan", Balance: "0.00", MobileNumber: "+919800000001", PlanID: created.ID})
		got, err := store.GetPlan(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Subscribers != 1 {
			t.Errorf("the plan reports %d subscribers, want 1", got.Subscribers)
		}
	})
}

// A retired plan must go on renewing the subscribers already on it, at its
// price. This is the whole meaning of "kept for existing subscribers", and
// it lives in the renewal candidate query — which joins plans without
// caring whether they are still offered.
func TestFR_CAT_001_RetiredPlansStillRenewTheirSubscribers(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	catalogue := database.Catalogue()
	billingStore := database.Billing()

	plan, err := catalogue.CreatePlan(ctx, newPlan("Legacy Fibre", "500.00", 30))
	if err != nil {
		t.Fatal(err)
	}
	seedGstRate(ctx, t, pool, 1)
	// Funded and expired: a renewal candidate.
	expired := time.Now().UTC().AddDate(0, 0, -1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{
		Username: "legacy", Balance: "5000.00", MobileNumber: "+919800000001",
		PlanID: plan.ID, PlanExpiry: &expired})

	before, err := billingStore.ListRenewalCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("want 1 renewal candidate before retiring, got %d", len(before))
	}

	if err := catalogue.SetPlanActive(ctx, plan.ID, false, "meena"); err != nil {
		t.Fatal(err)
	}

	after, err := billingStore.ListRenewalCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("a retired plan stopped renewing its subscribers: %d candidates", len(after))
	}
	if !after[0].PlanPrice.Equal(decimal.RequireFromString("500.00")) {
		t.Errorf("the renewal is priced %s, want the retired plan's own 500.00", after[0].PlanPrice)
	}
	if after[0].PlanName != "Legacy Fibre" {
		t.Errorf("the renewal names plan %q", after[0].PlanName)
	}
}

// A scheduled plan change (FR-BIL-023) onto a retired plan: the dropdown
// does not offer one, and the store does not refuse one either — recorded
// here as the behaviour that exists, so a later decision to refuse it is a
// deliberate change rather than a surprise.
func TestFR_CAT_001_SchedulingOntoARetiredPlanIsNotRefusedByTheStore(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	catalogue := database.Catalogue()
	billingStore := database.Billing()

	current, err := catalogue.CreatePlan(ctx, newPlan("Current", "500.00", 30))
	if err != nil {
		t.Fatal(err)
	}
	retired, err := catalogue.CreatePlan(ctx, newPlan("Retired", "300.00", 30))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogue.SetPlanActive(ctx, retired.ID, false, "meena"); err != nil {
		t.Fatal(err)
	}
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{
		Username: "mover", Balance: "0.00", MobileNumber: "+919800000001", PlanID: current.ID})

	_, err = billingStore.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: retired.ID, EffectiveOn: futureDay(10),
	}, "meena")
	if err != nil {
		t.Fatalf("the store refused a retired plan, which this test says it does not: %v", err)
	}
	// Documented consequence: the console never offers it, so reaching
	// this needs a hand-built request. Moving a customer onto a retired
	// tariff on purpose is a legitimate thing for an operator to do —
	// honouring a grandfathered price, for instance.
}
