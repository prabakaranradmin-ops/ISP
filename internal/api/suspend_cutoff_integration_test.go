//go:build integration

// A manual status change reaches the live session, like the dunning one does
// — FR-BIL-004, FR-SUB-005.
//
// PATCH /api/v1/subscribers/{id} and the console's bulk status change both
// set statuses RADIUS treats differently at login, and neither touched the
// login already in progress. A subscriber an operator hard-suspended by hand
// kept their session — and with it full service — until the line dropped,
// which on always-on PPPoE can be weeks. Terminating went through the
// approval path and did disconnect; suspending did not.
//
// Soft suspension has the same shape in both directions since it walls a
// session in at login: a session up before it keeps full service unless it
// is ended, and one walled in before a restore stays walled in.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/health"
)

// namedDB keeps each subscriber's status and returns rows with a username,
// as the real store does; the shared stubDB answers "active" for everyone
// and leaves the username blank.
type namedDB struct {
	stubDB
	mu      sync.Mutex
	status  map[int]string
	readErr error
}

func newNamedDB(before string) *namedDB {
	return &namedDB{status: map[int]string{3: before, 4: before, 5: before, 9: before}}
}

func (d *namedDB) row(id int) *api.SubscriberRecord {
	return &api.SubscriberRecord{ID: id, Username: "sub" + strconv.Itoa(id), Status: d.status[id]}
}

func (d *namedDB) GetSubscriberByID(_ context.Context, id int) (*api.SubscriberRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.readErr != nil {
		return nil, d.readErr
	}
	return d.row(id), nil
}

func (d *namedDB) UpdateSubscriber(_ context.Context, id int, _ *int, status *string, _ *time.Time) (*api.SubscriberRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if status != nil {
		d.status[id] = *status
	}
	return d.row(id), nil
}

func suspendMux(t *testing.T, online bool, db *namedDB) (*http.ServeMux, *api.Handler, *stubTaskEnqueuer, *stubSubCache) {
	t.Helper()
	sessions := &stubSessionReader{}
	if online {
		sessions.session = &health.SessionSummary{SessionID: "sess-1"}
	}
	tasks, cache := &stubTaskEnqueuer{}, &stubSubCache{}
	h := api.NewHandler(api.HandlerDeps{DB: db, KYC: &stubKYC{},
		Wallet:   billing.NewWalletService(&stubWallet{}),
		Sessions: sessions, Tasks: tasks, SubCache: cache})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux, h, tasks, cache
}

func disconnects(t *testing.T, tasks *stubTaskEnqueuer) []fup.PoDPayload {
	t.Helper()
	var out []fup.PoDPayload
	for _, task := range tasks.snapshot() {
		if task.Type() != fup.TaskTypePoD {
			continue
		}
		var p fup.PoDPayload
		if err := json.Unmarshal(task.Payload(), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestFR_BIL_004_AManualHardSuspensionEndsTheLiveSession(t *testing.T) {
	mux, _, tasks, cache := suspendMux(t, true, newNamedDB("active"))
	rec := httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"hard_suspended"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}
	got := disconnects(t, tasks)
	if len(got) != 1 || got[0].SubscriberID != 5 {
		t.Fatalf("disconnects queued %+v; the hard-suspended subscriber's live session must end", got)
	}
	// The username rides along so radiusd clears its own cache before the
	// redial; this process cannot reach that cache.
	if got[0].Username != "sub5" {
		t.Errorf("disconnect username %q, want sub5", got[0].Username)
	}
	if len(cache.invalidated) != 1 || cache.invalidated[0] != "sub5" {
		t.Errorf("auth cache invalidated %v, want [sub5]", cache.invalidated)
	}
}

// A PATCH disconnects exactly when the change alters what RADIUS would
// accept the subscriber with; radius.LiveSessionMustEnd decides, so the API
// and the login path cannot disagree.
func TestFR_BIL_004_OnlyAChangeOfTermsDisconnects(t *testing.T) {
	for _, tc := range []struct {
		before, to string
		want       bool
	}{
		{"active", "hard_suspended", true},
		{"active", "terminated", true},
		{"active", "soft_suspended", true},
		{"grace_period", "soft_suspended", true},
		{"soft_suspended", "active", true},
		{"soft_suspended", "grace_period", true},
		{"soft_suspended", "hard_suspended", true},
		{"active", "grace_period", false},
		{"grace_period", "active", false},
		{"active", "active", false},
		{"soft_suspended", "soft_suspended", false},
		{"hard_suspended", "active", false},
	} {
		mux, _, tasks, _ := suspendMux(t, true, newNamedDB(tc.before))
		rec := httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"`+tc.to+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s → %s: %d", tc.before, tc.to, rec.Code)
		}
		if got := len(disconnects(t, tasks)) == 1; got != tc.want {
			t.Errorf("PATCH %s → %s: disconnect=%v, want %v", tc.before, tc.to, got, tc.want)
		}
	}

	// A plan-only edit is not a status change at all.
	mux, _, tasks, _ := suspendMux(t, true, newNamedDB("soft_suspended"))
	httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"plan_id":2}`)
	if len(disconnects(t, tasks)) != 0 {
		t.Error("a plan-only edit disconnected the subscriber")
	}
	// Nobody online, nothing to disconnect.
	mux, _, tasks, _ = suspendMux(t, false, newNamedDB("active"))
	httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"hard_suspended"}`)
	if len(disconnects(t, tasks)) != 0 {
		t.Error("a disconnect was queued for a subscriber with no live session")
	}
}

// If the status before the change cannot be read, the change still lands
// and counts as a change of terms: a reconnect costs seconds, a missed one
// leaves a paying subscriber walled in.
func TestFR_BIL_004_AnUnreadablePreviousStatusCountsAsAChange(t *testing.T) {
	db := newNamedDB("soft_suspended")
	db.readErr = errors.New("db: connection reset")
	mux, _, tasks, _ := suspendMux(t, true, db)
	rec := httptestJSON(t, mux, http.MethodPatch, "/api/v1/subscribers/5", "billing_admin", `{"status":"active"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d; a failed read before the change must not refuse it", rec.Code)
	}
	if len(disconnects(t, tasks)) != 1 {
		t.Error("restoring a subscriber whose previous status was unreadable left their session alone")
	}
}

// The console's bulk status change goes through UpdateStatusForMany.
func TestFR_BIL_004_ABulkStatusChangeReachesEverySession(t *testing.T) {
	for _, tc := range []struct {
		before, to string
		want       int
	}{
		{"active", "hard_suspended", 3},
		{"active", "soft_suspended", 3},
		{"soft_suspended", "active", 3},
		{"active", "active", 0},
		{"soft_suspended", "soft_suspended", 0},
	} {
		_, h, tasks, _ := suspendMux(t, true, newNamedDB(tc.before))
		for _, r := range h.UpdateStatusForMany(t.Context(), []int{3, 4, 9}, tc.to) {
			if !r.OK {
				t.Fatalf("bulk result %+v", r)
			}
		}
		if got := len(disconnects(t, tasks)); got != tc.want {
			t.Errorf("bulk %s → %s: %d disconnects, want %d", tc.before, tc.to, got, tc.want)
		}
	}
}
