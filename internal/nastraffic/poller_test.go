package nastraffic

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeClient struct {
	mu      sync.Mutex
	results map[string]Result
	errs    map[string]error
	// block, when set, holds every poll until the context ends — a router
	// that answers nothing.
	block    map[string]bool
	inFlight int
	maxSeen  int
	targets  []Target
}

func (c *fakeClient) Poll(ctx context.Context, t Target) (Result, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.maxSeen {
		c.maxSeen = c.inFlight
	}
	c.targets = append(c.targets, t)
	blocked := c.block[t.Host]
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.inFlight--; c.mu.Unlock() }()

	if blocked {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	time.Sleep(5 * time.Millisecond) // long enough for polls to overlap
	if err := c.errs[t.Host]; err != nil {
		return Result{}, err
	}
	return c.results[t.Host], nil
}

type fakeStore struct {
	mu        sync.Mutex
	routers   []Router
	baselines map[int]map[string]Previous
	saved     map[int][]Write
	failures  map[int]string
	purges    []time.Time
}

func newFakeStore(routers ...Router) *fakeStore {
	return &fakeStore{routers: routers, baselines: map[int]map[string]Previous{},
		saved: map[int][]Write{}, failures: map[int]string{}}
}

func (s *fakeStore) ListPollableRouters(context.Context) ([]Router, error) { return s.routers, nil }

func (s *fakeStore) InterfaceBaselines(_ context.Context, nasID int) (map[string]Previous, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Previous{}
	for k, v := range s.baselines[nasID] {
		out[k] = v
	}
	return out, nil
}

// SavePoll applies baselines the way the real store does, so a sequence of
// polls through the fake behaves like a sequence through the database.
func (s *fakeStore) SavePoll(_ context.Context, nasID int, at time.Time, writes []Write) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved[nasID] = writes
	if s.baselines[nasID] == nil {
		s.baselines[nasID] = map[string]Previous{}
	}
	for _, w := range writes {
		if w.AdvanceBaseline {
			s.baselines[nasID][w.Iface.Name] = Previous{InOctets: w.Iface.InOctets, OutOctets: w.Iface.OutOctets, At: at}
		}
	}
	delete(s.failures, nasID)
	return nil
}

func (s *fakeStore) RecordPollFailure(_ context.Context, nasID int, _ time.Time, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[nasID] = reason
	return nil
}

func (s *fakeStore) PurgeSamplesBefore(_ context.Context, cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purges = append(s.purges, cutoff)
	return 0, nil
}

func plainDecrypt(c string) (string, error) { return strings.TrimPrefix(c, "enc:"), nil }

func oneInterface(in, out uint64) Result {
	return Result{Interfaces: []Interface{{Index: 1, Name: "ether1", HasHC: true, SpeedMbps: i64(1000), InOctets: in, OutOctets: out}}}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// The second poll measures against the first; the first only sets a
// baseline. This is the whole pipeline from counters to a stored rate.
func TestFR_NET_006_TheSecondPollRecordsARate(t *testing.T) {
	store := newFakeStore(Router{NASID: 1, Host: "10.0.0.1", Port: 161, CommunityEncrypted: "enc:public"})
	client := &fakeClient{results: map[string]Result{"10.0.0.1": oneInterface(1_000_000, 2_000_000)}}
	p := NewPoller(store, client, plainDecrypt)
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	p.PollOnce(context.Background())
	if w := store.saved[1]; len(w) != 1 || w[0].Sample != nil || !w[0].AdvanceBaseline || w[0].Expect != nil {
		t.Fatalf("first poll wrote %+v, want a baseline with no sample", w)
	}
	if client.targets[0].Community != "public" || client.targets[0].Port != 161 {
		t.Errorf("polled with %+v — the community must be the decrypted one", client.targets[0])
	}

	now = now.Add(5 * time.Minute)
	client.results["10.0.0.1"] = oneInterface(1_000_000+37_500_000, 2_000_000+75_000_000)
	p.PollOnce(context.Background())
	w := store.saved[1]
	if len(w) != 1 || w[0].Sample == nil {
		t.Fatalf("second poll wrote %+v, want a sample", w)
	}
	if w[0].Sample.InBps != 1_000_000 || w[0].Sample.OutBps != 2_000_000 {
		t.Errorf("sample %+v, want 1 and 2 Mbit/s", *w[0].Sample)
	}
	// The write carries the baseline it measured against, for the store's
	// compare-and-swap.
	if w[0].Expect == nil || !w[0].Expect.At.Equal(now.Add(-5*time.Minute)) {
		t.Errorf("the write does not carry the baseline it measured against: %+v", w[0].Expect)
	}
}

// One router that never answers does not hold up the others, and is
// recorded as failed once its own time runs out.
func TestFR_NET_006_ADeadRouterDoesNotHoldUpTheRest(t *testing.T) {
	store := newFakeStore(
		Router{NASID: 1, Host: "dead", Port: 161, CommunityEncrypted: "enc:x"},
		Router{NASID: 2, Host: "alive", Port: 161, CommunityEncrypted: "enc:x"},
	)
	client := &fakeClient{
		results: map[string]Result{"alive": oneInterface(1, 1)},
		block:   map[string]bool{"dead": true},
	}
	p := NewPoller(store, client, plainDecrypt)
	p.routerTimeout = 100 * time.Millisecond

	start := time.Now()
	p.PollOnce(context.Background())
	if time.Since(start) > 2*time.Second {
		t.Fatal("the cycle waited far longer than one router's timeout")
	}
	if _, ok := store.saved[2]; !ok {
		t.Error("the live router was not recorded")
	}
	if store.failures[1] == "" {
		t.Error("the dead router's failure was not recorded")
	}
	if !strings.Contains(store.failures[1], "No answer") {
		t.Errorf("the failure reads %q", store.failures[1])
	}
}

func TestFR_NET_006_PollsRunConcurrentlyButBounded(t *testing.T) {
	var routers []Router
	results := map[string]Result{}
	for i := 1; i <= 30; i++ {
		host := "r" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		routers = append(routers, Router{NASID: i, Host: host, Port: 161, CommunityEncrypted: "enc:x"})
		results[host] = oneInterface(1, 1)
	}
	client := &fakeClient{results: results}
	p := NewPoller(newFakeStore(routers...), client, plainDecrypt)
	p.PollOnce(context.Background())
	if client.maxSeen < 2 {
		t.Error("routers were polled one at a time; a slow router would hold up the whole network")
	}
	if client.maxSeen > p.concurrency {
		t.Errorf("%d polls in flight, above the bound of %d", client.maxSeen, p.concurrency)
	}
}

// A community that cannot be decrypted is a failure the operator can fix, and
// the router is never polled with garbage.
func TestFR_NET_006_AnUndecryptableCommunityIsReportedNotUsed(t *testing.T) {
	store := newFakeStore(Router{NASID: 1, Host: "10.0.0.1", Port: 161, CommunityEncrypted: "broken"})
	client := &fakeClient{results: map[string]Result{"10.0.0.1": oneInterface(1, 1)}}
	p := NewPoller(store, client, func(string) (string, error) { return "", errors.New("bad key") })
	p.PollOnce(context.Background())
	if len(client.targets) != 0 {
		t.Error("the router was polled with a community that could not be decrypted")
	}
	if !strings.Contains(store.failures[1], "set it again") {
		t.Errorf("the failure reads %q", store.failures[1])
	}
}

// Expired samples are purged at most once an hour, not on every five-minute
// cycle, and the cutoff is the retention period back from now.
func TestFR_NET_006_SamplesArePurgedHourlyAtTheRetentionCutoff(t *testing.T) {
	store := newFakeStore()
	p := NewPoller(store, &fakeClient{}, plainDecrypt)
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	p.PollOnce(context.Background())
	now = now.Add(5 * time.Minute)
	p.PollOnce(context.Background())
	if len(store.purges) != 1 {
		t.Fatalf("%d purges in two cycles five minutes apart, want 1", len(store.purges))
	}
	if !store.purges[0].Equal(now.Add(-5 * time.Minute).Add(-Retention)) {
		t.Errorf("purged before %v", store.purges[0])
	}
	now = now.Add(time.Hour)
	p.PollOnce(context.Background())
	if len(store.purges) != 2 {
		t.Error("no purge after an hour")
	}
}
