// Tests for the worker queue's overload behaviour.
//
// An internal test package (not radius_test, unlike most of this package's
// tests) because tryEnqueue and packetQueue are unexported and the whole
// point here is the behaviour at the boundary between them — the same
// reasoning internal/cache's own tests give for reaching inside.
package radius

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testDaemon builds a daemon suitable for queue tests: never started, so no
// socket is bound and no worker ever drains what these tests enqueue.
func testDaemon() *RadiusDaemon {
	return NewRadiusDaemon(
		"127.0.0.1:0",
		[]byte("test-shared-secret"),
		nil, // no DBQuerier: nothing here reaches the auth path
		[]byte("test-verifier-secret-at-least-32-bytes-long"),
	)
}

// TestTryEnqueue_ShedsRatherThanBlocksWhenSaturated is the regression test
// for the crash loop described in tryEnqueue's own comment.
//
// The failure this guards against does not look like a failing test — it
// looks like a hung one, which is exactly why it needs a timeout rather than
// a plain assertion. Before the `default` arm existed, this call parked
// forever on a full queue. In production that meant an unbounded pile of
// goroutines (layeh spawns one per packet and never waits), then an OOM
// kill, then a restart into a cold verifier cache that made the next minute
// of authentication far more expensive and refilled the queue faster than
// before.
func TestTryEnqueue_ShedsRatherThanBlocksWhenSaturated(t *testing.T) {
	d := testDaemon()
	ctx := context.Background()

	// Fill to exactly capacity. Every one of these must be accepted: a
	// premature drop would mean the daemon sheds load it could have served.
	for i := 0; i < cap(d.packetQueue); i++ {
		if !d.tryEnqueue(ctx, "auth", nil, nil) {
			t.Fatalf("packet %d of %d was shed while the queue still had room",
				i, cap(d.packetQueue))
		}
	}

	before := testutil.ToFloat64(radiusPacketsDropped.WithLabelValues("auth"))

	// The saturated call runs in a goroutine so a regression fails the test
	// instead of hanging the whole package until `go test` times out.
	returned := make(chan bool, 1)
	go func() { returned <- d.tryEnqueue(ctx, "auth", nil, nil) }()

	select {
	case accepted := <-returned:
		if accepted {
			t.Error("a packet was accepted into a queue already at capacity")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tryEnqueue blocked on a full queue. This is the OOM-then-crash-loop " +
			"path: layeh's PacketServer spawns a goroutine per packet with no bound, " +
			"so blocking here retains memory instead of shedding load.")
	}

	if after := testutil.ToFloat64(radiusPacketsDropped.WithLabelValues("auth")); after != before+1 {
		t.Errorf("radius_packets_dropped_total{listener=auth}: want %v, got %v — "+
			"a shed packet that is not counted is an outage with no signal", before+1, after)
	}
}

// TestTryEnqueue_DropsAreAttributedToTheirListener covers the label, which
// is what makes the counter actionable: an accounting flood and an
// authentication storm have different causes and different responses, and a
// single unlabelled total cannot tell an operator which one is happening.
func TestTryEnqueue_DropsAreAttributedToTheirListener(t *testing.T) {
	d := testDaemon()
	ctx := context.Background()

	for i := 0; i < cap(d.packetQueue); i++ {
		d.tryEnqueue(ctx, "auth", nil, nil)
	}

	authBefore := testutil.ToFloat64(radiusPacketsDropped.WithLabelValues("auth"))
	acctBefore := testutil.ToFloat64(radiusPacketsDropped.WithLabelValues("acct"))

	d.tryEnqueue(ctx, "acct", nil, nil)

	if got := testutil.ToFloat64(radiusPacketsDropped.WithLabelValues("acct")); got != acctBefore+1 {
		t.Errorf("acct drops: want %v, got %v", acctBefore+1, got)
	}
	if got := testutil.ToFloat64(radiusPacketsDropped.WithLabelValues("auth")); got != authBefore {
		t.Errorf("an accounting drop was attributed to the auth listener: want %v, got %v",
			authBefore, got)
	}
}

// TestTryEnqueue_QueueDepthTracksOccupancy covers the gauge OPS §12.3.4 has
// referenced for some time while it did not exist. Depth is the leading
// indicator — it rises before anything is shed — so it is what an operator
// can actually alert on ahead of subscriber impact.
func TestTryEnqueue_QueueDepthTracksOccupancy(t *testing.T) {
	d := testDaemon()
	ctx := context.Background()

	if got := testutil.ToFloat64(radiusQueueCapacity); got != float64(cap(d.packetQueue)) {
		t.Errorf("radius_worker_queue_capacity: want %d, got %v", cap(d.packetQueue), got)
	}

	const enqueued = 10
	for i := 0; i < enqueued; i++ {
		d.tryEnqueue(ctx, "auth", nil, nil)
	}

	if got := testutil.ToFloat64(radiusQueueDepth); got != enqueued {
		t.Errorf("radius_worker_queue_depth after %d packets: want %d, got %v",
			enqueued, enqueued, got)
	}
}

// TestTryEnqueue_StopsAcceptingOnceContextIsCancelled — a shutdown in
// progress must not keep pulling packets in behind the drain, or the
// graceful-shutdown window is spent serving work that arrived after the
// decision to stop.
func TestTryEnqueue_StopsAcceptingOnceContextIsCancelled(t *testing.T) {
	d := testDaemon()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The queue is empty, so the send case is ready. With ctx already
	// cancelled, both arms are ready and Go picks pseudo-randomly — so this
	// asserts the property that actually matters (it returns promptly and
	// never blocks), not a specific arm, which would be a flaky test.
	returned := make(chan bool, 1)
	go func() { returned <- d.tryEnqueue(ctx, "auth", nil, nil) }()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("tryEnqueue blocked after context cancellation — shutdown would hang")
	}
}
