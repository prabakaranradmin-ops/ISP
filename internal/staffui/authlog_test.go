package staffui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// The authentication log screen — FR-NET-004.

type fakeAuthLog struct {
	entries   []radius.AuthLogEntry
	summary   []radius.AuthRejectSummary
	gotFilter radius.AuthLogFilter
	gotSince  time.Time
	err       error
}

func (f *fakeAuthLog) ListAuthAttempts(_ context.Context, flt radius.AuthLogFilter) ([]radius.AuthLogEntry, error) {
	f.gotFilter = flt
	return f.entries, f.err
}

func (f *fakeAuthLog) SummariseAuthRejects(_ context.Context, since time.Time) ([]radius.AuthRejectSummary, error) {
	f.gotSince = since
	return f.summary, nil
}

func authEntry(username, result, reason string) radius.AuthLogEntry {
	return radius.AuthLogEntry{
		ID: 1, Username: username, NASIP: "198.51.100.1", MAC: "AA:BB:CC:DD:EE:FF",
		Result: result, RejectReason: reason, AuthMethod: radius.MethodPAP,
		CreatedAt: time.Now(),
	}
}

func TestFR_NET_004_AuthLogScreenRendersAttempts(t *testing.T) {
	f := &fakeAuthLog{
		entries: []radius.AuthLogEntry{
			authEntry("acme", radius.ResultReject, radius.RejectBadPassword),
			authEntry("acme", radius.ResultAccept, ""),
		},
		summary: []radius.AuthRejectSummary{
			{Reason: radius.RejectBadPassword, Count: 12},
			{Reason: radius.RejectNotAuthorised, Count: 3},
		},
	}
	h := &Handler{authLog: f}

	w := servePR(h, "csr", "GET", "/staff/auth-log", nil, h.AuthLog)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"acme", "198.51.100.1", "AA:BB:CC:DD:EE:FF",
		"accepted", "rejected",
		// The reason is shown as something an operator can act on, not as
		// the stored token.
		"Wrong password",
		// The summary leads, because "which failure is happening" is the
		// question the log exists to answer.
		"12", "Rejections in the last 24 hours",
		// And it says what the failure usually means.
		"old password after a reset",
		// The security claim is stated on the page.
		"No password is recorded",
		// And the retention, so nobody expects it to be an archive.
		"30 days",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// The stored vocabulary is short and groupable; the screen must not show
// it raw.
func TestFR_NET_004_EveryRejectReasonHasALabelAndAdvice(t *testing.T) {
	for _, reason := range radius.RejectReasons() {
		if rejectReasonLabels[reason] == "" {
			t.Errorf("reject reason %q has no operator-facing label", reason)
		}
		if rejectReasonAdvice[reason] == "" {
			t.Errorf("reject reason %q has no advice; the reason alone says what happened, not what to do", reason)
		}
	}
	// And the filter dropdown offers all of them, so a reason that can be
	// produced can also be searched for.
	offered := map[string]bool{}
	for _, o := range authReasonOptions {
		offered[o.Key] = true
	}
	for _, reason := range radius.RejectReasons() {
		if !offered[reason] {
			t.Errorf("reject reason %q cannot be filtered for", reason)
		}
	}
}

func TestFR_NET_004_AuthLogFiltersReachTheStore(t *testing.T) {
	t.Run("username, result, reason and dates pass through", func(t *testing.T) {
		f := &fakeAuthLog{}
		h := &Handler{authLog: f}
		servePR(h, "noc_engineer", "GET",
			"/staff/auth-log?username=acme&result=reject&reason=bad_password&from=2026-04-01&to=2026-04-30",
			nil, h.AuthLog)

		got := f.gotFilter
		if got.Username != "acme" || got.Result != "reject" || got.Reason != "bad_password" {
			t.Errorf("store got %+v", got)
		}
		if got.From == nil || got.From.Format("2006-01-02") != "2026-04-01" {
			t.Errorf("from reached the store as %v", got.From)
		}
		// Inclusive of the chosen day.
		if got.To == nil || got.To.Format("2006-01-02") != "2026-05-01" {
			t.Errorf("to reached the store as %v, want the start of 1 May", got.To)
		}
	})

	t.Run("an unknown result is ignored rather than passed on", func(t *testing.T) {
		f := &fakeAuthLog{}
		h := &Handler{authLog: f}
		servePR(h, "noc_engineer", "GET", "/staff/auth-log?result=sideways", nil, h.AuthLog)
		if f.gotFilter.Result != "" {
			t.Errorf("an unknown result reached the store as %q", f.gotFilter.Result)
		}
	})

	t.Run("a malformed date is explained", func(t *testing.T) {
		f := &fakeAuthLog{}
		h := &Handler{authLog: f}
		w := servePR(h, "noc_engineer", "GET", "/staff/auth-log?from=yesterday", nil, h.AuthLog)
		if !strings.Contains(w.Body.String(), "must be a date") {
			t.Error("the page does not say the date was unreadable")
		}
		if f.gotFilter.From != nil {
			t.Error("an unreadable date reached the store")
		}
	})

	t.Run("the summary looks back a day", func(t *testing.T) {
		f := &fakeAuthLog{}
		h := &Handler{authLog: f}
		servePR(h, "noc_engineer", "GET", "/staff/auth-log", nil, h.AuthLog)
		if f.gotSince.IsZero() {
			t.Fatal("the summary was not asked for")
		}
		age := time.Since(f.gotSince)
		if age < 23*time.Hour || age > 25*time.Hour {
			t.Errorf("the summary window is %v, want about 24 hours", age)
		}
	})

	t.Run("an unconfigured log says so rather than failing", func(t *testing.T) {
		h := &Handler{}
		w := servePR(h, "noc_engineer", "GET", "/staff/auth-log", nil, h.AuthLog)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "not configured") {
			t.Errorf("got %d", w.Code)
		}
	})

	t.Run("an empty log is explained rather than looking broken", func(t *testing.T) {
		h := &Handler{authLog: &fakeAuthLog{}}
		body := servePR(h, "noc_engineer", "GET", "/staff/auth-log", nil, h.AuthLog).Body.String()
		if !strings.Contains(body, "No authentication attempts recorded yet") {
			t.Error("an empty log renders no explanation")
		}
	})
}

// The front desk can read it: "why can't this customer get online" is the
// commonest support call and the answer is in here. Billing staff cannot,
// because it is network diagnosis rather than account data.
func TestFR_NET_004_AuthLogReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, false) {
			if sec.Key == "auth-log" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "noc_engineer", "csr", "technician"} {
		if !has(role) {
			t.Errorf("%s cannot reach the authentication log", role)
		}
	}
	if has("billing_admin") {
		t.Error("billing_admin can reach the authentication log")
	}
}
