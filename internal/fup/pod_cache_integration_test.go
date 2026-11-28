//go:build integration

package fup

import (
	"context"
	"encoding/json"
	"testing"

	"layeh.com/radius"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// A disconnect clears radiusd's auth cache before it goes out — FR-BIL-004,
// FR-NET-002.
//
// A disconnect after a status change is how the change reaches a live
// session, and the redial is answered from radiusd's auth cache, which only
// radiusd can clear. The API cannot. So the handler that sends the
// disconnect, which runs inside radiusd, clears the subscriber first.

// sequencedCache records each invalidation and how many disconnects the NAS
// had already received at that moment.
type sequencedCache struct {
	nasSaw  <-chan struct{}
	cleared []string
	before  []int
}

func (c *sequencedCache) InvalidateSubscriber(_ context.Context, username string) error {
	c.cleared = append(c.cleared, username)
	c.before = append(c.before, len(c.nasSaw))
	return nil
}

func TestFR_BIL_004_TheDisconnectClearsTheAuthCacheFirst(t *testing.T) {
	secret := []byte("pod-secret")
	port, received := itStubNAS(t, secret, radius.CodeDisconnectACK)
	cache := &sequencedCache{nasSaw: received}

	h := NewPoDHandler(&itCoADB{nasIP: "127.0.0.1", sessionID: "sess-wg-1"}, secret)
	h.SetPort(port)
	h.SetSubscriberCache(cache)

	payload, _ := json.Marshal(PoDPayload{SubscriberID: 30, Username: "walled@isp"})
	if err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypePoD, payload)); err != nil {
		t.Fatalf("PoD: %v", err)
	}
	if len(cache.cleared) != 1 || cache.cleared[0] != "walled@isp" {
		t.Fatalf("cache cleared %v, want [walled@isp]", cache.cleared)
	}
	if cache.before[0] != 0 {
		t.Error("the NAS had the disconnect before the cache was cleared; the redial could beat it")
	}
	if len(received) != 1 {
		t.Errorf("the NAS received %d disconnects", len(received))
	}
}

// With nobody online the cache is still cleared: a redial already in flight
// should read the new status, and the task is still not retried.
func TestFR_BIL_004_NoLiveSessionStillClearsTheCache(t *testing.T) {
	cache := &sequencedCache{nasSaw: make(chan struct{})}
	h := NewPoDHandler(&itNoSessionDB{}, []byte("pod-secret"))
	h.SetSubscriberCache(cache)

	payload, _ := json.Marshal(PoDPayload{SubscriberID: 31, Username: "offline@isp"})
	err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypePoD, payload))
	if !errorIsSkipRetry(err) {
		t.Errorf("no session: %v, want a skip-retry", err)
	}
	if len(cache.cleared) != 1 {
		t.Errorf("cache cleared %v", cache.cleared)
	}
}

// Older payloads carry no username, and voucher sessions have no subscriber:
// both still disconnect, with nothing to clear.
func TestFR_BIL_004_ADisconnectWithoutAUsernameStillGoesOut(t *testing.T) {
	secret := []byte("pod-secret")
	port, received := itStubNAS(t, secret, radius.CodeDisconnectACK)
	cache := &sequencedCache{nasSaw: received}
	h := NewPoDHandler(&itCoADB{nasIP: "127.0.0.1", sessionID: "sess-wg-2"}, secret)
	h.SetPort(port)
	h.SetSubscriberCache(cache)

	for _, p := range []PoDPayload{{SubscriberID: 32}, {NasIP: "127.0.0.1", SessionID: "voucher-1"}} {
		payload, _ := json.Marshal(p)
		if err := h.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypePoD, payload)); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	if len(cache.cleared) != 0 {
		t.Errorf("cleared %v with no username to clear", cache.cleared)
	}
	if len(received) != 2 {
		t.Errorf("the NAS received %d disconnects, want 2", len(received))
	}
}
