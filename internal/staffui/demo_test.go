package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDemoStore fails Remove with whatever the test hands it.
type fakeDemoStore struct {
	status    DemoStatus
	removeErr error
	removed   bool
}

func (f *fakeDemoStore) MarkSubscriberDemo(context.Context, int) error { return nil }
func (f *fakeDemoStore) MarkPlanDemo(context.Context, int) error       { return nil }
func (f *fakeDemoStore) MarkNASDemo(context.Context, int) error        { return nil }

func (f *fakeDemoStore) Status(context.Context) (DemoStatus, error) { return f.status, nil }

func (f *fakeDemoStore) Remove(context.Context) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = true
	return nil
}

func serveRemoveDemo(h *Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/staff/demo/remove", nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey,
		Session{Username: "owner_user", Role: "isp_owner"}))
	w := httptest.NewRecorder()
	h.RemoveDemoData(w, r)
	return w
}

// A removal blocked by real data has an obvious remedy, so the console says
// what it is. Everything else keeps pointing at the log, because there is
// nothing useful to say about an arbitrary foreign-key violation.
//
// Worth a test because the useful message is the whole point of the
// ErrDemoDataInUse sentinel, and "check the server log" is what an operator
// got for every failure until 2026-11-23 — including the one case where the
// fix is a single click away, and including on installs where they have no
// access to the log at all.
func TestRemoveDemoData_SaysWhatToDoWhenRealDataBlocksIt(t *testing.T) {
	t.Run("a real subscriber on a demo plan", func(t *testing.T) {
		store := &fakeDemoStore{
			status: DemoStatus{Subscribers: 5, Plans: 2, NASDevices: 1},
			removeErr: fmt.Errorf("db: remove demo data: %w: 1 real subscriber(s) are on a demo plan; "+
				"move them to a real plan first", ErrDemoDataInUse),
		}
		h := &Handler{demo: store}
		body := serveRemoveDemo(h).Body.String()

		if !strings.Contains(body, "a real subscriber is on a demo plan") {
			t.Errorf("the console must name the reason, got:\n%s", body)
		}
		if !strings.Contains(body, "Move them to a real plan") {
			t.Errorf("the console must say what to do about it, got:\n%s", body)
		}
		if strings.Contains(body, "check the server log") {
			t.Error("this failure has a remedy, so it must not be reported as a log-diving exercise")
		}
	})

	t.Run("any other failure still points at the log", func(t *testing.T) {
		store := &fakeDemoStore{
			status:    DemoStatus{Subscribers: 5},
			removeErr: errors.New("db: remove demo data: connection refused"),
		}
		h := &Handler{demo: store}
		body := serveRemoveDemo(h).Body.String()

		if !strings.Contains(body, "check the server log") {
			t.Errorf("an unexplained failure should still send the operator to the log, got:\n%s", body)
		}
		if strings.Contains(body, "demo plan") {
			t.Error("an unrelated failure must not be reported as a plan problem")
		}
	})

	t.Run("a successful removal says so", func(t *testing.T) {
		store := &fakeDemoStore{status: DemoStatus{Subscribers: 5}}
		h := &Handler{demo: store}
		body := serveRemoveDemo(h).Body.String()

		if !store.removed {
			t.Error("Remove was not called")
		}
		if !strings.Contains(body, "Demo data removed") {
			t.Errorf("a successful removal should be confirmed, got:\n%s", body)
		}
	})
}
