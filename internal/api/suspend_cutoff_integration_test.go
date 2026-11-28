//go:build integration

// A manual suspension ends the live session, like the dunning one does —
// FR-BIL-004, FR-SUB-005.
//
// PATCH /api/v1/subscribers/{id} and the console's bulk "Suspend" both set a
// status that RADIUS refuses on the next login, and neither ended the login
// already in progress. A subscriber an operator hard-suspended by hand kept
// their session — and with it full service — until the line dropped, which on
// always-on PPPoE can be weeks. Terminating went through the approval path
// and did disconnect; suspending did not.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/health"
)

// namedDB returns the updated row with its username, as the real store does;
// the shared stubDB leaves it blank.
type namedDB struct{ stubDB }

func (d *namedDB) UpdateSubscriber(_ context.Context, id int, _ *int, status *string, _ *time.Time) (*api.SubscriberRecord, error) {
	st := "active"
	if status != nil {
		st = *status
	}
	return &api.SubscriberRecord{ID: id, Username: "sub" + strconv.Itoa(id), Status: st}, nil
}

func suspendMux(t *testing.T, online bool) (*http.ServeMux, *api.Handler, *stubTaskEnqueuer, *stubSubCache) {
	t.Helper()
	sessions := &stubSessionReader{}
	if online {
		sessions.session = &health.SessionSummary{SessionID: "sess-1"}
	}
	tasks, cache := &stubTaskEnqueuer{}, &stubSubCache{}
	h := api.NewHandler(api.HandlerDeps{DB: &namedDB{}, KYC: &stubKYC{},
		Wallet:   billing.NewWalletService(&stubWallet{}),
		Sessions: sessions, Tasks: tasks, SubCache: cache})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux, h, tasks, cache
}

func disconnectsFor(t *testing.T, tasks *stubTaskEnqueuer) []int {
	t.Helper()
	var ids []int
	for _, task := range tasks.snapshot() {
		if task.Type() != fup.TaskTypePoD {
			continue
		}
		var p fup.PoDPayload
		if err := json.Unmarshal(task.Payload(), &p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.SubscriberID)
	}
	return ids
}

func TestFR_BIL_004_AManualHardSuspensionEndsTheLiveSession(t *testing.T) {
	mux, _, tasks, cache := suspendMux(t, true)
	rec := httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"hard_suspended"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}
	if got := disconnectsFor(t, tasks); len(got) != 1 || got[0] != 5 {
		t.Errorf("disconnects queued for %v; the hard-suspended subscriber's live session must end", got)
	}
	if len(cache.invalidated) != 1 || cache.invalidated[0] != "sub5" {
		t.Errorf("auth cache invalidated %v, want [sub5]", cache.invalidated)
	}
}

// Only a status RADIUS refuses ends the session: the same allowlist decides
// both, so they cannot disagree.
func TestFR_BIL_004_OnlyARefusedStatusDisconnects(t *testing.T) {
	for status, want := range map[string]bool{
		"hard_suspended": true, "terminated": true,
		"active": false, "grace_period": false, "soft_suspended": false,
	} {
		mux, _, tasks, _ := suspendMux(t, true)
		httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"`+status+`"}`)
		if got := len(disconnectsFor(t, tasks)) == 1; got != want {
			t.Errorf("PATCH to %s: disconnect=%v, want %v", status, got, want)
		}
	}
	// A plan-only edit is not a status change at all.
	mux, _, tasks, _ := suspendMux(t, true)
	httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"plan_id":2}`)
	if len(disconnectsFor(t, tasks)) != 0 {
		t.Error("a plan-only edit disconnected the subscriber")
	}
	// Nobody online, nothing to disconnect.
	mux, _, tasks, _ = suspendMux(t, false)
	httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"hard_suspended"}`)
	if len(disconnectsFor(t, tasks)) != 0 {
		t.Error("a disconnect was queued for a subscriber with no live session")
	}
}

// The console's bulk "Suspend" goes through UpdateStatusForMany.
func TestFR_BIL_004_ABulkHardSuspensionEndsEverySession(t *testing.T) {
	_, h, tasks, _ := suspendMux(t, true)
	results := h.UpdateStatusForMany(t.Context(), []int{3, 4, 9}, "hard_suspended")
	for _, r := range results {
		if !r.OK {
			t.Fatalf("bulk result %+v", r)
		}
	}
	if got := disconnectsFor(t, tasks); len(got) != 3 {
		t.Errorf("disconnects for %v, want all three", got)
	}

	_, h, tasks, _ = suspendMux(t, true)
	h.UpdateStatusForMany(t.Context(), []int{3, 4}, "soft_suspended")
	if got := disconnectsFor(t, tasks); len(got) != 0 {
		t.Errorf("a bulk soft suspension disconnected %v", got)
	}
}
