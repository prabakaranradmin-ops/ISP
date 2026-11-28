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
// Soft suspension has the same shape now that it means something. RADIUS
// walls a soft-suspended login in (internal/nas/walledgarden.go), but only at
// login: a session up from before keeps full service unless it is ended, and
// one walled in stays walled in after the subscriber pays unless it is ended
// again. radius.LiveSessionMustEnd decides which moves those are.
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
	// And their username, so the handler clears radiusd's cache at the
	// moment it sends.
	if p.Username != "ravi" {
		t.Errorf("disconnect payload username %q, want ravi", p.Username)
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

// A move disconnects exactly when it changes what RADIUS would accept the
// subscriber with. Reminders and grace leave the session alone; entering
// soft or hard suspension ends it, and so does leaving soft suspension,
// whether by paying or by a grace extension. Someone already in a stage is
// not disconnected again on every hourly pass, and someone leaving hard
// suspension has no session to end: RADIUS was refusing them.
func TestFR_BIL_004_OnlyAChangeOfTermsDisconnects(t *testing.T) {
	extended := day(2)
	for name, tc := range map[string]struct {
		from       billing.DunningState
		expiry     float64
		graceUntil *time.Time
		want       int
	}{
		"into a reminder":                       {billing.DunningActive, 5, nil, 0},
		"into grace":                            {billing.DunningRemind1d, 0, nil, 0},
		"into soft suspension":                  {billing.DunningGracePeriod, -4, nil, 1},
		"already soft suspended":                {billing.DunningSoftSuspended, -5, nil, 0},
		"already hard suspended":                {billing.DunningHardSuspended, -30, nil, 0},
		"paid while soft suspended":             {billing.DunningSoftSuspended, 30, nil, 1},
		"grace extended during soft suspension": {billing.DunningSoftSuspended, -4, &extended, 1},
		"paid while hard suspended":             {billing.DunningHardSuspended, 30, nil, 0},
		"grace extended during hard suspension": {billing.DunningHardSuspended, -10, &extended, 0},
	} {
		t.Run(name, func(t *testing.T) {
			db := &scanDB{
				states: map[int]billing.DunningState{6: tc.from},
				candidates: []billing.DunningCandidate{{SubscriberID: 6, Username: "x", State: tc.from,
					PlanExpiry: day(tc.expiry), GraceUntil: tc.graceUntil}},
			}
			s, q, c := cutoffScanner(db)
			if err := s.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := len(q.ofType(fup.TaskTypePoD)); n != tc.want {
				t.Errorf("%s (now %s) queued %d disconnects, want %d", name, db.states[6], n, tc.want)
			}
			if tc.want > 0 && len(c.invalid) != 1 {
				t.Errorf("%s: cache invalidations %v", name, c.invalid)
			}
		})
	}
}

// A grace extension that lifts a soft suspension and then runs out puts the
// subscriber back into soft suspension in the same billing cycle. The second
// entry has to disconnect again, or the session the extension let out keeps
// full service until the line drops.
func TestFR_BIL_004_ReenteringSoftSuspensionInOneCycleWallsInAgain(t *testing.T) {
	until := day(1)
	db := &scanDB{
		states: map[int]billing.DunningState{7: billing.DunningSoftSuspended},
		candidates: []billing.DunningCandidate{{SubscriberID: 7, Username: "kavya",
			State: billing.DunningSoftSuspended, PlanExpiry: day(-4), GraceUntil: &until}},
	}
	q := &recordingQueue{}
	s := billing.NewDunningScanner(db, nil)
	s.SetTaskEnqueuer(q)
	billing.SetScannerClock(s, func() time.Time { return scanNow })
	if err := s.Scan(context.Background()); err != nil || db.states[7] != billing.DunningGracePeriod {
		t.Fatalf("extension: %v, %s", err, db.states[7])
	}

	// A day and a half later the extension has run out.
	db.candidates[0].State = billing.DunningGracePeriod
	later := scanNow.Add(36 * time.Hour)
	billing.SetScannerClock(s, func() time.Time { return later })
	if err := s.Scan(context.Background()); err != nil || db.states[7] != billing.DunningSoftSuspended {
		t.Fatalf("expiry of the extension: %v, %s", err, db.states[7])
	}
	if n := len(q.ofType(fup.TaskTypePoD)); n != 2 {
		t.Errorf("%d disconnects; want one letting the session out and one walling it in again", n)
	}
	// This queue keeps everything; the real one drops a task whose id it
	// has seen. The re-entry's id differs from an entry earlier the same
	// cycle, so the real queue keeps it too.
	if billing.DunningDisconnectTaskID(7, billing.DunningSoftSuspended, day(-4), scanNow.Add(-3*time.Hour)) ==
		billing.DunningDisconnectTaskID(7, billing.DunningSoftSuspended, day(-4), later) {
		t.Error("a re-entry into soft suspension reuses an earlier entry's id, and the queue would drop it")
	}
}

// The disconnect is keyed so a rerun or an overlapping pass queues one, and
// nothing legitimately separate is suppressed: another subscriber, another
// stage, another cycle, or a later re-entry.
func TestFR_BIL_004_TheDisconnectIsIdempotentPerTransition(t *testing.T) {
	id := func(sub int, st billing.DunningState, expiry float64, at time.Time) string {
		return billing.DunningDisconnectTaskID(sub, st, day(expiry), at)
	}
	first := id(4, billing.DunningHardSuspended, -7, scanNow)
	if first == "" || first != id(4, billing.DunningHardSuspended, -7, scanNow.Add(20*time.Minute)) {
		t.Error("two passes in the same hour must produce one id")
	}
	for name, other := range map[string]string{
		"a later cycle":      id(4, billing.DunningHardSuspended, -37, scanNow),
		"another subscriber": id(5, billing.DunningHardSuspended, -7, scanNow),
		"another stage":      id(4, billing.DunningSoftSuspended, -7, scanNow),
		"a later re-entry":   id(4, billing.DunningHardSuspended, -7, scanNow.Add(26*time.Hour)),
		"the stage's notice": billing.DunningNoticeTaskID(4, billing.DunningHardSuspended, day(-7)),
	} {
		if other == first {
			t.Errorf("%s shares the disconnect's id, and one of the two would be dropped", name)
		}
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
