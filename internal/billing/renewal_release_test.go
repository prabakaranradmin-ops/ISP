package billing_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
)

// Paying lets a soft-suspended subscriber out of the walled garden —
// FR-BIL-004, FR-NOTIF-006.
//
// A soft-suspended subscriber is online, behind the walled garden their
// session was accepted into. The renewal scanner restores them and sends
// "your service has been restored", but the session it restores is the one
// RADIUS walled in, and the router does not ask again. Unless the session is
// ended, they are told they are back while reaching nothing but the payment
// page, until their line happens to drop.

func renewOne(t *testing.T, state billing.DunningState) (*recordingQueue, *orderedCache, *fakeRenewalScanQuerier) {
	t.Helper()
	db := newFakeRenewalScanQuerier()
	db.states = map[int]billing.DunningState{1: state}
	db.candidates = []billing.RenewalCandidate{{
		SubscriberID: 1, Username: "paid@isp", RegisteredState: "TN",
		PlanName: "Basic", PlanPrice: decFromString(t, "500.00"), PlanValidityDays: 30,
		PlanExpiry: scanDay(-5), DunningState: state,
	}}
	scanner := billing.NewRecurringBillingScanner(db, billing.NewWalletService(newFakeWalletQuerier("600.00")))
	billing.SetRecurringBillingScannerClock(scanner, func() time.Time { return renewalNow })
	q := &recordingQueue{}
	c := &orderedCache{q: q}
	scanner.SetTaskEnqueuer(q)
	scanner.SetSubscriberCache(c)
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if db.states[1] != billing.DunningActive {
		t.Fatalf("not restored: %s", db.states[1])
	}
	return q, c, db
}

func TestFR_BIL_004_PayingLetsASoftSuspendedSubscriberOut(t *testing.T) {
	q, c, _ := renewOne(t, billing.DunningSoftSuspended)

	pods := q.ofType(fup.TaskTypePoD)
	if len(pods) != 1 {
		t.Fatalf("%d disconnects for a renewed soft-suspended subscriber, want 1: "+
			"without it they stay walled in after paying", len(pods))
	}
	var p fup.PoDPayload
	if err := json.Unmarshal(pods[0].Payload(), &p); err != nil || p.SubscriberID != 1 || p.Username != "paid@isp" {
		t.Errorf("disconnect payload %s (%v)", pods[0].Payload(), err)
	}
	if len(c.invalid) == 0 || c.invalid[0] != "paid@isp" || c.queued[0] != 0 {
		t.Errorf("cache invalidations %v with %v disconnects already queued; the cache must go first", c.invalid, c.queued)
	}
	// And they are still told, now truthfully.
	if len(q.ofType(billing.TaskTypeServiceRestored)) != 1 {
		t.Error("the restoration notice was not sent")
	}
}

// Nobody else is disconnected by a renewal. A hard-suspended subscriber has no
// session (RADIUS was refusing them) and reconnects on their own; one in a
// reminder stage or grace was never restricted.
func TestFR_BIL_004_ARenewalDisconnectsNobodyElse(t *testing.T) {
	for _, state := range []billing.DunningState{
		billing.DunningHardSuspended, billing.DunningGracePeriod, billing.DunningRemind1d, billing.DunningActive,
	} {
		q, _, _ := renewOne(t, state)
		if n := len(q.ofType(fup.TaskTypePoD)); n != 0 {
			t.Errorf("renewing out of %s queued %d disconnects", state, n)
		}
	}
}
