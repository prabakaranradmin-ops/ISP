package staffui

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// The connection-limit report on the Sessions screen — FR-AAA-002,
// FR-BIL-022.
//
// The limit ships in report mode, so this list is what an operator works
// through before it is enforced: who holds more connections than they pay
// for, which sessions those are, and a way to end the ones that should not
// be there.

type fakeConnLimits struct {
	rows []radius.OverLimitSubscriber
	err  error
}

func (f *fakeConnLimits) ListOverConnectionLimit(context.Context) ([]radius.OverLimitSubscriber, error) {
	return f.rows, f.err
}

func overLimitRows() []radius.OverLimitSubscriber {
	start := time.Date(2026, 11, 28, 9, 0, 0, 0, time.Local)
	return []radius.OverLimitSubscriber{{
		SubscriberID: 12, Username: "sharing-home", Limit: 1,
		Sessions: []radius.OpenConnection{
			{SessionID: "s-a", NasIP: "10.0.0.1", AssignedIP: "100.64.0.9", DeviceMAC: "D4:CA:6D:01:02:03", StartedAt: start},
			{SessionID: "s-b", NasIP: "10.0.0.2", AssignedIP: "100.64.0.10", StartedAt: start.Add(time.Hour)},
		},
	}}
}

func TestFR_AAA_002_TheReportListsWhoIsOverAndInWhatMode(t *testing.T) {
	h := &Handler{connLimits: &fakeConnLimits{rows: overLimitRows()}, connLimitMode: radius.ConnectionLimitReport}
	w := runSessions(h, "noc_engineer", "GET", "/staff/sessions?tab=limits", nil)
	body := visibleText(w.Body.String())
	if w.Code != 200 {
		t.Fatalf("report: %d", w.Code)
	}
	for _, want := range []string{
		"Report only: nobody is refused yet", "CONNECTION_LIMIT_MODE=enforce",
		"sharing-home", "+1", "D4:CA:6D:01:02:03", "not recorded", "100.64.0.10",
		"Adds a connection",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the report does not show %q", want)
		}
	}
	// One Disconnect per session, each addressed to that session.
	raw := w.Body.String()
	for _, id := range []string{"s-a", "s-b"} {
		if !strings.Contains(raw, `action="/staff/sessions/`+id+`/disconnect?tab=limits"`) {
			t.Errorf("no disconnect for session %s that comes back to the report", id)
		}
	}

	// After a disconnect the operator lands back on the report, not on the
	// list of everyone online.
	h.disconnector = &fakeDisconnector{}
	w = runSessions(h, "noc_engineer", "POST", "/staff/sessions/s-b/disconnect?tab=limits", url.Values{"username": {"sharing-home"}})
	if after := visibleText(w.Body.String()); !strings.Contains(after, "Disconnect sent for sharing-home") ||
		!strings.Contains(after, "Report only") {
		t.Errorf("after a disconnect from the report: %s", after)
	}

	// Enforcing says so instead.
	h.connLimitMode = radius.ConnectionLimitEnforce
	body = visibleText(runSessions(h, "noc_engineer", "GET", "/staff/sessions?tab=limits", nil).Body.String())
	if !strings.Contains(body, "The limit is being enforced") || strings.Contains(body, "Report only") {
		t.Error("an enforcing deployment does not say it is enforcing")
	}
}

// Only the roles that may disconnect get the buttons; everyone with the
// section still sees the list.
func TestFR_AAA_002_OnlyNetworkRolesGetDisconnectOnTheReport(t *testing.T) {
	h := &Handler{connLimits: &fakeConnLimits{rows: overLimitRows()}}
	for _, role := range []string{"noc_engineer", "isp_owner"} {
		if !strings.Contains(runSessions(h, role, "GET", "/staff/sessions?tab=limits", nil).Body.String(), "/disconnect") {
			t.Errorf("%s gets no disconnect button", role)
		}
	}
	w := runSessions(h, "billing_admin", "GET", "/staff/sessions?tab=limits", nil)
	if w.Code == 200 && strings.Contains(w.Body.String(), "/disconnect\"") {
		t.Error("billing gets a disconnect button")
	}
}

// Empty, unavailable and unconfigured each say what they are, rather than
// an empty table reading as "nobody is over".
func TestFR_AAA_002_TheReportSaysWhenItCannotAnswer(t *testing.T) {
	empty := visibleText(runSessions(&Handler{connLimits: &fakeConnLimits{}}, "noc_engineer", "GET",
		"/staff/sessions?tab=limits", nil).Body.String())
	if !strings.Contains(empty, "Nobody holds more connections than they pay for") {
		t.Error("an empty report does not say so")
	}

	failed := visibleText(runSessions(&Handler{connLimits: &fakeConnLimits{err: errors.New("db down")}}, "noc_engineer",
		"GET", "/staff/sessions?tab=limits", nil).Body.String())
	if !strings.Contains(failed, "Could not load the connection limit report") {
		t.Error("a failed report does not say so")
	}
	if strings.Contains(failed, "Nobody holds more connections") {
		t.Error("a failed report claims nobody is over")
	}

	off := visibleText(runSessions(&Handler{}, "noc_engineer", "GET", "/staff/sessions?tab=limits", nil).Body.String())
	if !strings.Contains(off, "not configured on this deployment") {
		t.Error("an unwired report does not say so")
	}
}
