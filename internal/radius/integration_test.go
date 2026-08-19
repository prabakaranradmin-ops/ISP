//go:build integration

// Integration tests for the RADIUS AAA module.
//
// Covers INT-AAA-001 .. INT-AAA-005 from the Integration Tests tracker sheet.
// The caches these exercise (brute-force counter, fast-verifier cache,
// accounting dedup) used to live in Redis and were tested against a real
// in-process server (miniredis) so key formats, TTLs and SetNX semantics
// were exercised rather than mocked. They are now in-process maps
// (internal/localcache) reached directly through the daemon's own fields —
// still the real implementation, with no fake in between.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/radius -Tags integration
package radius

import (
	"context"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"golang.org/x/crypto/bcrypt"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
)

var itSecret = []byte("testing123")
var itVerifierSecret = []byte("test-verifier-cache-secret-32-bytes-min")

// ── Test doubles ────────────────────────────────────────────────────────────

// itResponseWriter captures the packets a handler writes.
type itResponseWriter struct {
	mu      sync.Mutex
	packets []*radius.Packet
}

func (w *itResponseWriter) Write(p *radius.Packet) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.packets = append(w.packets, p)
	return nil
}

func (w *itResponseWriter) last() *radius.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.packets) == 0 {
		return nil
	}
	return w.packets[len(w.packets)-1]
}

// itSubscriberDB is an in-memory DBQuerier.
type itSubscriberDB struct {
	subs map[string]*Subscriber
}

func (db *itSubscriberDB) GetSubscriberByUsername(_ context.Context, username string) (*Subscriber, error) {
	sub, ok := db.subs[username]
	if !ok {
		return nil, nil
	}
	return sub, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func itNewDaemon(t *testing.T, subs map[string]*Subscriber) *RadiusDaemon {
	t.Helper()
	return NewRadiusDaemon(":0", itSecret, &itSubscriberDB{subs: subs}, itVerifierSecret)
}

// itCachedEntries counts everything the daemon currently holds across its
// three in-process caches. Replaces the old miniredis Keys() assertions,
// which could see every key in one place because they all shared one Redis.
func itCachedEntries(d *RadiusDaemon) int {
	return d.acctDedup.Len() + d.verifierCache.store.Len() + d.guard.counter.Len()
}

func itHashPassword(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	return string(h)
}

func itAccessRequest(t *testing.T, username, password string) *radius.Request {
	t.Helper()
	pkt := radius.New(radius.CodeAccessRequest, itSecret)
	if err := rfc2865.UserName_SetString(pkt, username); err != nil {
		t.Fatalf("set User-Name: %v", err)
	}
	if err := rfc2865.UserPassword_SetString(pkt, password); err != nil {
		t.Fatalf("set User-Password: %v", err)
	}
	return &radius.Request{Packet: pkt}
}

// itAccountingRequest builds an Interim-Update.
//
// It previously set NAS-Identifier as the session key, matching a handler that
// read the session id from that attribute — a per-device string, not a
// per-session one. Both were wrong together, so this test passed while
// exercising the defect. It now builds the attributes RFC 2866 actually
// specifies.
func itAccountingRequest(t *testing.T, sessionID string, inputOctets uint32) *radius.Request {
	t.Helper()
	pkt := radius.New(radius.CodeAccountingRequest, itSecret)
	if err := rfc2866.AcctStatusType_Set(pkt, rfc2866.AcctStatusType_Value_InterimUpdate); err != nil {
		t.Fatalf("set Acct-Status-Type: %v", err)
	}
	if err := rfc2866.AcctSessionID_SetString(pkt, sessionID); err != nil {
		t.Fatalf("set Acct-Session-Id: %v", err)
	}
	if err := rfc2866.AcctInputOctets_Set(pkt, rfc2866.AcctInputOctets(inputOctets)); err != nil {
		t.Fatalf("set Acct-Input-Octets: %v", err)
	}
	return &radius.Request{
		Packet:     pkt,
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 41000},
	}
}

// itCounterValue reads the current value of a counter for delta assertions.
func itCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// itHistogramCount reads the observation count of a histogram.
func itHistogramCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	var m dto.Metric
	collector, ok := h.(prometheus.Metric)
	if !ok {
		t.Fatalf("histogram does not implement prometheus.Metric")
	}
	if err := collector.Write(&m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// ── INT-AAA-001 ─────────────────────────────────────────────────────────────

// TestFR_AAA_002_HandleAuth_ActiveSubscriberAccepted verifies an active subscriber with
// correct credentials receives Access-Accept and that the latency histogram
// records the request.
//
// INT-AAA-001 | FR-AAA-002
func TestFR_AAA_002_HandleAuth_ActiveSubscriberAccepted(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"alice@isp": {
			ID:           1,
			Username:     "alice@isp",
			PasswordHash: itHashPassword(t, "correct-horse"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})

	beforeAccept := itCounterValue(t, radiusAuthAccept)
	beforeLatency := itHistogramCount(t, radiusAuthDuration)

	w := &itResponseWriter{}
	d.handleAuth(context.Background(), w, itAccessRequest(t, "alice@isp", "correct-horse"))

	resp := w.last()
	if resp == nil {
		t.Fatal("handler wrote no response packet")
	}
	if resp.Code != radius.CodeAccessAccept {
		t.Errorf("want Access-Accept, got %v", resp.Code)
	}
	if got := itCounterValue(t, radiusAuthAccept); got != beforeAccept+1 {
		t.Errorf("radius_auth_accept_total: want +1, got %v", got-beforeAccept)
	}
	if got := itHistogramCount(t, radiusAuthDuration); got != beforeLatency+1 {
		t.Errorf("latency metric not emitted: sample count went %d -> %d", beforeLatency, got)
	}

	// The Accept must carry the subscriber's rate limit as a MikroTik VSA.
	if vsa := resp.Get(radius.Type(26)); vsa == nil {
		t.Error("expected Vendor-Specific rate-limit attribute on Access-Accept")
	}
}

// ── INT-AAA-002 ─────────────────────────────────────────────────────────────

// TestFR_AAA_002_HandleAuth_InvalidPassword verifies a wrong password yields Access-Reject.
//
// INT-AAA-002 | FR-AAA-002
func TestFR_AAA_002_HandleAuth_InvalidPassword(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"bob@isp": {
			ID:           2,
			Username:     "bob@isp",
			PasswordHash: itHashPassword(t, "right-password"),
			Status:       "active",
			RateLimitStr: "50M/50M",
		},
	})

	beforeReject := itCounterValue(t, radiusAuthReject)

	w := &itResponseWriter{}
	d.handleAuth(context.Background(), w, itAccessRequest(t, "bob@isp", "wrong-password"))

	resp := w.last()
	if resp == nil {
		t.Fatal("handler wrote no response packet")
	}
	if resp.Code != radius.CodeAccessReject {
		t.Errorf("want Access-Reject, got %v", resp.Code)
	}
	if got := itCounterValue(t, radiusAuthReject); got != beforeReject+1 {
		t.Errorf("radius_auth_reject_total: want +1, got %v", got-beforeReject)
	}
}

// ── INT-AAA-003 ─────────────────────────────────────────────────────────────

// TestFR_AAA_002_HandleAuth_SuspendedSubscriber verifies hard-suspended and terminated
// subscribers are rejected even with correct credentials, and that no session
// state is cached for them.
//
// INT-AAA-003 | FR-AAA-002
func TestFR_AAA_002_HandleAuth_SuspendedSubscriber(t *testing.T) {
	for _, status := range []string{"hard_suspended", "terminated"} {
		t.Run(status, func(t *testing.T) {
			d := itNewDaemon(t, map[string]*Subscriber{
				"carol@isp": {
					ID:           3,
					Username:     "carol@isp",
					PasswordHash: itHashPassword(t, "valid-pass"),
					Status:       status,
					RateLimitStr: "100M/100M",
				},
			})

			w := &itResponseWriter{}
			d.handleAuth(context.Background(), w, itAccessRequest(t, "carol@isp", "valid-pass"))

			resp := w.last()
			if resp == nil {
				t.Fatal("handler wrote no response packet")
			}
			if resp.Code != radius.CodeAccessReject {
				t.Errorf("status=%s: want Access-Reject, got %v", status, resp.Code)
			}
			// Nothing may be cached for a rejected subscriber — no verifier
			// entry in particular, which would let the next attempt skip
			// bcrypt for an account that is not allowed to authenticate.
			if n := itCachedEntries(d); n != 0 {
				t.Errorf("status=%s: expected no cached entries after reject, got %d", status, n)
			}
		})
	}
}

// ── INT-AAA-004 ─────────────────────────────────────────────────────────────

// TestFR_SEC_001_BruteForce_BlocksAt10Failures verifies that after MaxFailedAttempts
// consecutive failures the next attempt is rejected by the guard, and that the
// ban key carries the 15-minute lockout TTL.
//
// INT-AAA-004 | FR-SEC-001
func TestFR_SEC_001_BruteForce_BlocksAt10Failures(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"dave@isp": {
			ID:           4,
			Username:     "dave@isp",
			PasswordHash: itHashPassword(t, "the-real-password"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	// 10 failed attempts with the wrong password.
	for i := 1; i <= MaxFailedAttempts; i++ {
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, "dave@isp", "guess"))
		if got := w.last(); got == nil || got.Code != radius.CodeAccessReject {
			t.Fatalf("attempt %d: want Access-Reject, got %v", i, got)
		}
	}

	key := BruteForceKey("dave@isp")
	count, exists := d.guard.counter.Get(key)
	if !exists {
		t.Fatalf("expected brute-force counter at key %q", key)
	}
	if count != MaxFailedAttempts {
		t.Errorf("counter: want %d, got %d", MaxFailedAttempts, count)
	}
	// The TTL is armed on every failure, so it reads just under the full
	// window rather than exactly equal to it — the elapsed time since the
	// tenth failure. An exact equality check was possible against
	// miniredis's simulated clock; against a real one it would be flaky.
	ttl := d.guard.counter.TTL(key)
	if ttl <= 0 || ttl > LockoutDuration {
		t.Errorf("lockout TTL: want (0, %v], got %v", LockoutDuration, ttl)
	}

	// The 11th attempt is blocked even though the password is now correct.
	beforeBlocked := itCounterValue(t, bruteForceBlocked)
	w := &itResponseWriter{}
	d.handleAuth(ctx, w, itAccessRequest(t, "dave@isp", "the-real-password"))

	resp := w.last()
	if resp == nil {
		t.Fatal("11th attempt wrote no response packet")
	}
	if resp.Code != radius.CodeAccessReject {
		t.Errorf("11th attempt: want Access-Reject while banned, got %v", resp.Code)
	}
	if got := itCounterValue(t, bruteForceBlocked); got != beforeBlocked+1 {
		t.Errorf("radius_bruteforce_blocked_total: want +1, got %v", got-beforeBlocked)
	}

	// Once the lockout expires the correct password works again. Forcing the
	// entry expired stands in for miniredis's FastForward — an in-process
	// counter has no simulated clock, and sleeping out a real 15 minutes is
	// not a test.
	d.guard.counter.ExpireNow(key)
	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itAccessRequest(t, "dave@isp", "the-real-password"))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Errorf("after lockout expiry: want Access-Accept, got %v", got)
	}
}

// TestFR_SEC_001_BruteForce_ResetOnSuccessfulAuth verifies a successful login clears the
// counter so unrelated later typos do not inherit old attempts.
//
// INT-AAA-004 (supporting) | FR-SEC-001
func TestFR_SEC_001_BruteForce_ResetOnSuccessfulAuth(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"erin@isp": {
			ID:           5,
			Username:     "erin@isp",
			PasswordHash: itHashPassword(t, "s3cret"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d.handleAuth(ctx, &itResponseWriter{}, itAccessRequest(t, "erin@isp", "nope"))
	}
	if got, _ := d.guard.counter.Get(BruteForceKey("erin@isp")); got != 3 {
		t.Fatalf("counter before success: want 3, got %d", got)
	}

	d.handleAuth(ctx, &itResponseWriter{}, itAccessRequest(t, "erin@isp", "s3cret"))

	if _, exists := d.guard.counter.Get(BruteForceKey("erin@isp")); exists {
		t.Error("expected brute-force counter to be cleared after successful auth")
	}
}

// ── INT-AAA-005 ─────────────────────────────────────────────────────────────

// TestFR_AAA_003_Dedup_DuplicateInterimSkipped verifies a replayed Interim-Update with the
// same session and octet count is counted once only.
//
// Run with -count=3 per the tracker; each run gets a fresh miniredis and the
// assertions are on deltas, so repeats are independent.
//
// INT-AAA-005 | FR-AAA-003
func TestFR_AAA_003_Dedup_DuplicateInterimSkipped(t *testing.T) {
	d := itNewDaemon(t, nil)
	ctx := context.Background()

	const sessionID = "sess-abc123"
	const octets = uint32(1234567890)

	beforeSkipped := itCounterValue(t, radiusDedupSkipped)

	// First Interim-Update — accepted and recorded.
	w1 := &itResponseWriter{}
	d.handleAccounting(ctx, w1, itAccountingRequest(t, sessionID, octets))
	if got := w1.last(); got == nil || got.Code != radius.CodeAccountingResponse {
		t.Fatalf("first update: want Accounting-Response, got %v", got)
	}
	if got := itCounterValue(t, radiusDedupSkipped); got != beforeSkipped {
		t.Errorf("first update must not be counted as a duplicate (delta %v)", got-beforeSkipped)
	}

	// Exact replay — acknowledged but not double-counted.
	w2 := &itResponseWriter{}
	d.handleAccounting(ctx, w2, itAccountingRequest(t, sessionID, octets))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccountingResponse {
		t.Fatalf("replay: want Accounting-Response, got %v", got)
	}
	if got := itCounterValue(t, radiusDedupSkipped); got != beforeSkipped+1 {
		t.Errorf("radius_acct_dedup_skipped_total: want +1 on replay, got %v", got-beforeSkipped)
	}

	// Exactly one dedup key exists for this session/octet pair.
	keys := d.acctDedup.Keys()
	if len(keys) != 1 {
		t.Fatalf("want exactly 1 dedup key, got %d: %v", len(keys), keys)
	}
	// Session, record type, and both counters: everything that distinguishes a
	// genuine record from a retransmission of the same one.
	wantKey := "acct_dedup:" + sessionID + ":interim:1234567890:0"
	if keys[0] != wantKey {
		t.Errorf("dedup key: want %q, got %q", wantKey, keys[0])
	}

	// A genuine counter advance (new octet total) is a distinct key, not a dup.
	w3 := &itResponseWriter{}
	d.handleAccounting(ctx, w3, itAccountingRequest(t, sessionID, octets+5000))
	if got := itCounterValue(t, radiusDedupSkipped); got != beforeSkipped+1 {
		t.Errorf("advanced counter must not be deduped (delta %v)", got-beforeSkipped)
	}
	if n := d.acctDedup.Len(); n != 2 {
		t.Errorf("want 2 dedup keys after counter advance, got %d", n)
	}
}

// ── Fast-verifier cache (DoD Phase 1 Step 3, NFR-PERF-001) ─────────────────

// TestVerifierCache_SecondAuthUsesFastPath verifies a second authentication
// with the same correct password is served by the fast-verifier cache
// (radius_verifier_cache_hit_total increments) rather than bcrypt, while
// still producing an ordinary Access-Accept.
func TestVerifierCache_SecondAuthUsesFastPath(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"carol@isp": {
			ID:           3,
			Username:     "carol@isp",
			PasswordHash: itHashPassword(t, "carol-password"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	// First auth: cache miss, pays full bcrypt, populates the cache.
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itAccessRequest(t, "carol@isp", "carol-password"))
	if got := w1.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("first auth: want Access-Accept, got %v", got)
	}

	beforeHits := itCounterValue(t, radiusVerifierCacheHit)

	// Second auth, same password: should hit the fast path.
	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itAccessRequest(t, "carol@isp", "carol-password"))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("second auth: want Access-Accept, got %v", got)
	}
	if got := itCounterValue(t, radiusVerifierCacheHit); got != beforeHits+1 {
		t.Errorf("radius_verifier_cache_hit_total: want +1 on the second auth, got %v", got-beforeHits)
	}
}

// TestVerifierCache_WrongPasswordAfterCachedEntry_StillRejected verifies that
// once a verifier is cached for the correct password, a *different*
// (incorrect) password on a later request still falls through to bcrypt and
// is still rejected — a cache miss/mismatch must never be treated as a
// rejection on its own.
func TestVerifierCache_WrongPasswordAfterCachedEntry_StillRejected(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"dave@isp": {
			ID:           4,
			Username:     "dave@isp",
			PasswordHash: itHashPassword(t, "dave-real-password"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	// Populate the cache with the correct password.
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itAccessRequest(t, "dave@isp", "dave-real-password"))
	if got := w1.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("setup auth: want Access-Accept, got %v", got)
	}

	// A guess with the wrong password must still be rejected.
	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itAccessRequest(t, "dave@isp", "a-guess"))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccessReject {
		t.Fatalf("wrong password after a cached entry exists: want Access-Reject, got %v", got)
	}
}

// TestVerifierCache_ExpiredEntryFallsBackToBcrypt verifies an entry past its
// TTL is treated as a miss (falls through to bcrypt) rather than as a
// rejection or a stale accept.
func TestVerifierCache_ExpiredEntryFallsBackToBcrypt(t *testing.T) {
	d := itNewDaemon(t, map[string]*Subscriber{
		"erin@isp": {
			ID:           5,
			Username:     "erin@isp",
			PasswordHash: itHashPassword(t, "erin-password"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itAccessRequest(t, "erin@isp", "erin-password"))
	if got := w1.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("setup auth: want Access-Accept, got %v", got)
	}

	// Stands in for miniredis's FastForward — see the note in
	// TestFR_SEC_001_BruteForce_BlocksAt10Failures.
	d.verifierCache.store.ExpireNow(verifierKey("erin@isp"))

	beforeHits := itCounterValue(t, radiusVerifierCacheHit)
	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itAccessRequest(t, "erin@isp", "erin-password"))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("auth after TTL expiry: want Access-Accept (via bcrypt fallback), got %v", got)
	}
	if got := itCounterValue(t, radiusVerifierCacheHit); got != beforeHits {
		t.Errorf("expired entry must not count as a fast-path hit: delta %v", got-beforeHits)
	}
}

// TestVerifierCache_PasswordChange_OldRejectedNewAccepted verifies that when
// a subscriber's password changes after a verifier was cached under the old
// one, the old password is rejected and the new password succeeds (via the
// bcrypt fallback, which re-populates the cache under the new password).
func TestVerifierCache_PasswordChange_OldRejectedNewAccepted(t *testing.T) {
	sub := &Subscriber{
		ID:           6,
		Username:     "frank@isp",
		PasswordHash: itHashPassword(t, "old-password"),
		Status:       "active",
		RateLimitStr: "100M/100M",
	}
	d := itNewDaemon(t, map[string]*Subscriber{"frank@isp": sub})
	ctx := context.Background()

	// Cache a verifier under the old password.
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itAccessRequest(t, "frank@isp", "old-password"))
	if got := w1.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("setup auth: want Access-Accept, got %v", got)
	}

	// Simulate a password change (an admin/portal flow rehashing PasswordHash).
	sub.PasswordHash = itHashPassword(t, "new-password")

	t.Run("the old password is now rejected", func(t *testing.T) {
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, "frank@isp", "old-password"))
		if got := w.last(); got == nil || got.Code != radius.CodeAccessReject {
			t.Errorf("want Access-Reject for the old password, got %v", got)
		}
	})

	t.Run("the new password is accepted", func(t *testing.T) {
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, "frank@isp", "new-password"))
		if got := w.last(); got == nil || got.Code != radius.CodeAccessAccept {
			t.Errorf("want Access-Accept for the new password, got %v", got)
		}
	})
}

// TestHandleAuth_RepeatAuthMeetsP99Budget is the direct verification of
// NFR-PERF-001/NFR-SCAL-001: with a *real* bcrypt cost=12 hash (not
// bcrypt.MinCost, which every other test in this file uses to stay fast),
// the first authentication pays full bcrypt cost, but 200 subsequent
// authentications with the same password — the fast-verifier-cache path —
// must have a p99 latency under 15ms.
func TestHandleAuth_RepeatAuthMeetsP99Budget(t *testing.T) {
	if testing.Short() {
		t.Skip("real bcrypt cost=12 hashing is slow; skipped in -short mode")
	}

	const cost12Password = "p99-budget-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(cost12Password), 12)
	if err != nil {
		t.Fatalf("bcrypt hash at cost=12: %v", err)
	}

	d := itNewDaemon(t, map[string]*Subscriber{
		"p99@isp": {
			ID:           7,
			Username:     "p99@isp",
			PasswordHash: string(hash),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	// First request: cache miss, pays the full ~280ms bcrypt cost. Confirm it
	// really is slow, so the later assertion is proving something — if this
	// environment's bcrypt happened to be fast, a passing p99 below would be
	// meaningless.
	first := time.Now()
	w0 := &itResponseWriter{}
	d.handleAuth(ctx, w0, itAccessRequest(t, "p99@isp", cost12Password))
	firstLatency := time.Since(first)
	if got := w0.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Fatalf("first auth: want Access-Accept, got %v", got)
	}
	if firstLatency < 15*time.Millisecond {
		t.Skipf("bcrypt cost=12 completed in %v on this machine — too fast to demonstrate the fast path's benefit here", firstLatency)
	}

	const n = 200
	latencies := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, "p99@isp", cost12Password))
		latencies[i] = time.Since(start)
		if got := w.last(); got == nil || got.Code != radius.CodeAccessAccept {
			t.Fatalf("repeat auth %d: want Access-Accept, got %v", i, got)
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p99 := latencies[int(float64(n)*0.99)]
	t.Logf("first (cold, bcrypt) auth: %v | repeat (cached) auth p99: %v, max: %v", firstLatency, p99, latencies[n-1])

	if p99 >= 15*time.Millisecond {
		t.Errorf("repeat-auth p99 = %v, want < 15ms (NFR-PERF-001)", p99)
	}
}
