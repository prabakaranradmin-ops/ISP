package billing_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// Hard suspension cuts the line it says it cuts — FR-BIL-004.
//
// The dunning screen tells the operator "Hard suspension: Disconnected", and
// RADIUS refuses a hard-suspended subscriber's next login. Nothing ended the
// login they already had. The scanner changed the stored status and sent the
// notice, and a PPPoE session that was up stayed up — for as long as the line
// held, which on an always-on connection is days or weeks — so a subscriber
// suspended for not paying kept full service.
//
// The disconnect has to come after the auth cache forgets them. A router that
// is disconnected reconnects within seconds, and radiusd answers from a cache
// that holds the subscriber for a minute: without the invalidation the
// reconnect is accepted on the old status and the session is back.

type recordingQueue struct{ tasks []*jobqueue.Task }

func (q *recordingQueue) EnqueueContext(_ context.Context, t *jobqueue.Task, _ ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	q.tasks = append(q.tasks, t)
	return &jobqueue.TaskInfo{ID: int64(len(q.tasks))}, nil
}

func (q *recordingQueue) ofType(typ string) []*jobqueue.Task {
	var out []*jobqueue.Task
	for _, t := range q.tasks {
		if t.Type() == typ {
			out = append(out, t)
		}
	}
	return out
}

// orderedCache records invalidations, and the queue length at the moment of
// each, so a test can prove the cache was cleared before the disconnect.
type orderedCache struct {
	q       *recordingQueue
	invalid []string
	queued  []int
}

func (c *orderedCache) InvalidateSubscriber(_ context.Context, username string) error {
	c.invalid = append(c.invalid, username)
	c.queued = append(c.queued, len(c.q.ofType(fup.TaskTypePoD)))
	return nil
}

func cutoffScanner(db *scanDB) (*billing.DunningScanner, *recordingQueue, *orderedCache) {
	q := &recordingQueue{}
	c := &orderedCache{q: q}
	s := billing.NewDunningScanner(db, nil)
	s.SetTaskEnqueuer(q)
	s.SetSubscriberCache(c)
	billing.SetScannerClock(s, func() time.Time { return scanNow })
	return s, q, c
}

func TestFR_BIL_004_HardSuspensionDisconnectsTheLiveSession(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{4: billing.DunningSoftSuspended},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 4, Username: "ravi", State: billing.DunningSoftSuspended, PlanExpiry: day(-7)},
		},
	}
	s, q, c := cutoffScanner(db)
	if err := s.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if db.states[4] != billing.DunningHardSuspended {
		t.Fatalf("not hard suspended: %s", db.states[4])
	}

	pods := q.ofType(fup.TaskTypePoD)
	if len(pods) != 1 {
		t.Fatalf("queued %d disconnects for a newly hard-suspended subscriber, want 1", len(pods))
	}
	var p fup.PoDPayload
	if err := json.Unmarshal(pods[0].Payload(), &p); err != nil || p.SubscriberID != 4 {
		t.Errorf("disconnect payload %s (%v): it must name the subscriber so the handler finds their live session", pods[0].Payload(), err)
	}
	// The auth cache forgot them first, or the reconnect is accepted on the
	// old status.
	if len(c.invalid) != 1 || c.invalid[0] != "ravi" || c.queued[0] != 0 {
		t.Errorf("cache invalidations %v with %v disconnects already queued; the cache must be cleared before the disconnect", c.invalid, c.queued)
	}
	// And the notice still goes.
	if len(q.ofType(billing.TaskTypeDunningNotice)) != 1 {
		t.Error("the hard-suspension notice was not sent")
	}
}

// A subscriber who jumps several stages at once (the first run after a
// deployment) is still disconnected once, for the stage they land on.
func TestFR_BIL_004_CatchingUpToHardSuspensionDisconnectsOnce(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{5: billing.DunningActive},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 5, Username: "meena", State: billing.DunningActive, PlanExpiry: day(-40)},
		},
	}
	s, q, _ := cutoffScanner(db)
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(q.ofType(fup.TaskTypePoD)); n != 1 {
		t.Errorf("queued %d disconnects, want 1", n)
	}
}

// Only the move into hard suspension cuts the line. Reminders, grace and
// soft suspension leave the subscriber connected, and someone already hard
// suspended is not disconnected again on every hourly pass.
func TestFR_BIL_004_NothingElseDisconnects(t *testing.T) {
	for name, tc := range map[string]struct {
		from   billing.DunningState
		expiry float64
	}{
		"into a reminder":        {billing.DunningActive, 5},
		"into grace":             {billing.DunningRemind1d, 0},
		"into soft suspension":   {billing.DunningGracePeriod, -4},
		"already hard suspended": {billing.DunningHardSuspended, -30},
		"paid, back to active":   {billing.DunningHardSuspended, 30},
	} {
		t.Run(name, func(t *testing.T) {
			db := &scanDB{
				states:     map[int]billing.DunningState{6: tc.from},
				candidates: []billing.DunningCandidate{{SubscriberID: 6, Username: "x", State: tc.from, PlanExpiry: day(tc.expiry)}},
			}
			s, q, _ := cutoffScanner(db)
			if err := s.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := len(q.ofType(fup.TaskTypePoD)); n != 0 {
				t.Errorf("%s queued %d disconnects", name, n)
			}
		})
	}
}

// The disconnect is keyed per subscriber per billing cycle, so a rerun or an
// overlapping pass cannot queue two; a later cycle gets its own.
func TestFR_BIL_004_TheDisconnectIsIdempotentPerCycle(t *testing.T) {
	first := billing.DunningCutoffTaskID(4, day(-7))
	if first == "" || first != billing.DunningCutoffTaskID(4, day(-7)) {
		t.Error("the same subscriber and cycle must produce one stable id")
	}
	if first == billing.DunningCutoffTaskID(4, day(-37)) {
		t.Error("a later cycle reuses the earlier cycle's disconnect id, and would be suppressed")
	}
	if first == billing.DunningCutoffTaskID(5, day(-7)) {
		t.Error("two subscribers share a disconnect id")
	}
	if first == billing.DunningNoticeTaskID(4, billing.DunningHardSuspended, day(-7)) {
		t.Error("the disconnect shares the notice's id, so whichever is queued second is dropped")
	}
}

// Without a queue (the unit tests' NewDunningScanner(db, nil)) the scanner
// still advances; without a cache it still disconnects.
func TestFR_BIL_004_TheCutOffDegradesGracefully(t *testing.T) {
	db := &scanDB{
		states:     map[int]billing.DunningState{4: billing.DunningSoftSuspended},
		candidates: []billing.DunningCandidate{{SubscriberID: 4, Username: "ravi", State: billing.DunningSoftSuspended, PlanExpiry: day(-7)}},
	}
	s := billing.NewDunningScanner(db, nil)
	billing.SetScannerClock(s, func() time.Time { return scanNow })
	if err := s.Scan(context.Background()); err != nil || db.states[4] != billing.DunningHardSuspended {
		t.Errorf("no queue: %v, %s", err, db.states[4])
	}

	db.states[4] = billing.DunningSoftSuspended
	q := &recordingQueue{}
	s = billing.NewDunningScanner(db, nil)
	s.SetTaskEnqueuer(q)
	billing.SetScannerClock(s, func() time.Time { return scanNow })
	if err := s.Scan(context.Background()); err != nil || len(q.ofType(fup.TaskTypePoD)) != 1 {
		t.Errorf("no cache: %v, %d disconnects", err, len(q.ofType(fup.TaskTypePoD)))
	}
}
