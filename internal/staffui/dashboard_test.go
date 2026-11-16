package staffui

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeConsole records the filter it was asked for, so a test can assert what
// a URL turned into, and returns canned figures.
type fakeConsole struct {
	counts    *DashboardCounts
	page      *DirectoryPage
	live      []LiveSession
	completed []CompletedSession
	queue     []QueueTicket
	summary   *HelpdeskSummary
	assignees []Assignee
	err       error
	gotFilt   *DirectoryFilter
	gotSess   *SessionFilter
	gotTicket *TicketFilter
}

func (f *fakeConsole) ListTicketQueue(_ context.Context, filt TicketFilter) ([]QueueTicket, error) {
	f.gotTicket = &filt
	return f.queue, f.err
}

func (f *fakeConsole) HelpdeskSummary(context.Context) (*HelpdeskSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.summary == nil {
		return &HelpdeskSummary{}, nil
	}
	return f.summary, nil
}

func (f *fakeConsole) ListTicketAssignees(context.Context) ([]Assignee, error) {
	return f.assignees, nil
}

func (f *fakeConsole) ListLiveSessions(_ context.Context, filt SessionFilter) ([]LiveSession, error) {
	f.gotSess = &filt
	return f.live, f.err
}

func (f *fakeConsole) ListCompletedSessions(_ context.Context, filt SessionFilter) ([]CompletedSession, error) {
	f.gotSess = &filt
	return f.completed, f.err
}

func (f *fakeConsole) DashboardCounts(context.Context) (*DashboardCounts, error) {
	return f.counts, f.err
}

func (f *fakeConsole) ListDirectory(_ context.Context, filt DirectoryFilter) (*DirectoryPage, error) {
	f.gotFilt = &filt
	if f.err != nil {
		return nil, f.err
	}
	if f.page == nil {
		return &DirectoryPage{}, nil
	}
	return f.page, nil
}

// TestNavGroupsCoverEverySection fails when a section is added to the role
// table without a place in the top bar. buildNav would still show it (as a
// trailing link), but it would sit outside every menu, which is the kind of
// drift that is only ever noticed by an operator.
func TestNavGroupsCoverEverySection(t *testing.T) {
	seen := map[string]int{}
	for _, g := range navGroups {
		for _, k := range g.Keys {
			seen[k]++
		}
	}
	known := map[string]bool{}
	for _, s := range sections {
		known[s.Key] = true
		if seen[s.Key] != 1 {
			t.Errorf("section %q appears %d times in navGroups, want exactly 1", s.Key, seen[s.Key])
		}
	}
	for k := range seen {
		if !known[k] {
			t.Errorf("navGroups names %q, which is not a section", k)
		}
	}
}

func TestBuildNav(t *testing.T) {
	labels := func(es []NavEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Label)
		}
		return out
	}

	t.Run("owner gets menus, dashboard first as a plain link", func(t *testing.T) {
		nav := buildNav(AllowedSections("isp_owner", true), "ledger")
		want := []string{"Dashboard", "Subscribers", "Finance", "Service", "Network", "Admin"}
		if got := labels(nav); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("labels: want %v, got %v", want, got)
		}
		if len(nav[0].Items) != 1 || nav[0].Items[0].Key != "dashboard" {
			t.Errorf("first entry should be the dashboard link, got %+v", nav[0])
		}
		for _, e := range nav {
			if e.Active != (e.Label == "Finance") {
				t.Errorf("%s: Active=%v, only Finance (holding ledger) should be active", e.Label, e.Active)
			}
		}
	})

	t.Run("a one-item group collapses into that item's own link", func(t *testing.T) {
		// A CSR's Subscribers group holds only "subscribers" (no catalogue),
		// Finance only "billing", and Network only "sessions": each should
		// read as a plain link named after the section, not a one-item menu.
		nav := buildNav(AllowedSections("csr", false), "")
		want := []string{"Dashboard", "Subscribers", "Billing", "Service", "Sessions"}
		if got := labels(nav); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("labels: want %v, got %v", want, got)
		}
		if n := len(nav[2].Items); n != 1 || nav[2].Items[0].Key != "billing" {
			t.Errorf("Finance should collapse to the billing link, got %+v", nav[2])
		}
	})

	t.Run("grouping never adds a section the role lacks", func(t *testing.T) {
		for _, role := range []string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician", "lco", "intern"} {
			allowed := AllowedSections(role, true)
			var flat []string
			for _, e := range buildNav(allowed, "") {
				for _, it := range e.Items {
					flat = append(flat, it.Key)
				}
			}
			if len(flat) != len(allowed) {
				t.Errorf("%s: nav holds %d sections, role allows %d", role, len(flat), len(allowed))
			}
			for _, k := range flat {
				if !canAccess(k, role, true) {
					t.Errorf("%s: nav offers %q, which the role cannot open", role, k)
				}
			}
		}
	})

	t.Run("an unmapped section is kept as a trailing link", func(t *testing.T) {
		stray := Section{Key: "stray", Label: "Stray", Path: "/staff/stray"}
		nav := buildNav([]Section{stray}, "")
		if len(nav) != 1 || nav[0].Label != "Stray" {
			t.Fatalf("want one Stray link, got %+v", nav)
		}
	})
}

func TestStatusSegments(t *testing.T) {
	if got := statusSegments(&DashboardCounts{}); got != nil {
		t.Errorf("no subscribers should give no segments, got %v", got)
	}
	c := &DashboardCounts{Total: 4, ByStatus: map[string]int{"active": 3, "terminated": 1, "grace_period": 0}}
	got := statusSegments(c)
	if len(got) != 2 {
		t.Fatalf("zero-count statuses should be dropped, got %v", got)
	}
	if got[0].Status != "active" || got[0].Pct != 75 || got[1].Status != "terminated" || got[1].Pct != 25 {
		t.Errorf("want active 75%% then terminated 25%%, got %+v", got)
	}
}

func TestRenderDashboard(t *testing.T) {
	h := &Handler{}
	s := testSession()
	full := &DashboardCounts{
		Online: 2, Total: 5,
		ByStatus:     map[string]int{"pending_payment": 1, "active": 3, "grace_period": 0, "soft_suspended": 1, "hard_suspended": 0, "terminated": 0},
		ExpiringWeek: 1, Expired: 1, NewWeek: 2, Churned30d: 0,
		OpenTickets: 3, OverdueTickets: 1, UnassignedTickets: 2,
	}
	empty := &DashboardCounts{ByStatus: map[string]int{}}
	for name, d := range map[string]any{
		"no data (store missing)": nil,
		"empty install":           dashboardData{Counts: empty, UpdatedAt: time.Now()},
		"full, with tickets":      dashboardData{Counts: full, Segments: statusSegments(full), ShowTickets: true, UpdatedAt: time.Now()},
	} {
		t.Run(name, func(t *testing.T) {
			assertRendersOK(t, h, "dashboard", pageData{Session: s, Nav: buildNav(AllowedSections("isp_owner", true), "dashboard"), Data: d})
		})
	}
}

func TestRenderSubscriberDirectory(t *testing.T) {
	h := &Handler{}
	exp := time.Now().Add(72 * time.Hour)
	page := &DirectoryPage{
		Rows: []DirectoryRow{
			{ID: 1, Username: "demo_priya", Status: "active", MobileNumber: "+919800000001", PlanName: "Home 50M", PlanExpiry: &exp, Online: true},
			{ID: 2, Username: "demo_arjun", Status: "pending_payment", MobileNumber: "+919800000002"},
		},
		Matched: 2, Total: 9,
	}
	for _, bulk := range []bool{false, true} {
		assertRendersOK(t, h, "subscribers", pageData{Session: testSession(), Data: subscribersData{
			Directory: page, Statuses: SubscriberStatuses, Views: DirectoryViews,
			Filter: DirectoryFilter{View: "expiring", Query: "a&b"}, ViewLabel: directoryViewLabels["expiring"],
			ShowBulkActions: bulk, Plans: []Plan{{ID: 1, Name: "Home 50M"}},
		}})
		assertRendersOK(t, h, "subscribers", pageData{Session: testSession(), Data: subscribersData{
			Directory: &DirectoryPage{Total: 9}, Statuses: SubscriberStatuses, ShowBulkActions: bulk,
		}})
	}
}

func TestDashboardAndDirectoryHandlers(t *testing.T) {
	// run serves one console handler as a signed-in operator of the given role.
	run := func(h *Handler, role, target string, handler string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{StaffID: 1, Username: role, Role: role}))
		w := httptest.NewRecorder()
		switch handler {
		case "dashboard":
			h.Dashboard(w, r)
		case "subscribers":
			h.Subscribers(w, r)
		}
		return w
	}

	t.Run("dashboard hides ticket figures from roles without Support", func(t *testing.T) {
		fc := &fakeConsole{counts: &DashboardCounts{Total: 1, ByStatus: map[string]int{"active": 1}, OpenTickets: 7}}
		h := &Handler{console: fc}
		owner := run(h, "isp_owner", "/staff/dashboard", "dashboard").Body.String()
		noc := run(h, "noc_engineer", "/staff/dashboard", "dashboard").Body.String()
		if !strings.Contains(owner, "Open tickets") {
			t.Error("owner should see the Support tiles")
		}
		if strings.Contains(noc, "Open tickets") {
			t.Error("a NOC engineer cannot open Support and must not see its figures")
		}
	})

	t.Run("dashboard is refused to franchise roles", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{counts: &DashboardCounts{ByStatus: map[string]int{}}}}
		if code := run(h, "lco", "/staff/dashboard", "dashboard").Code; code != 403 {
			t.Errorf("lco: want 403, got %d", code)
		}
	})

	t.Run("dashboard store failure is reported, not rendered as zeros", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{err: errors.New("boom")}}
		body := run(h, "isp_owner", "/staff/dashboard", "dashboard").Body.String()
		if !strings.Contains(body, "Could not load the dashboard figures.") {
			t.Error("a failed load must say so")
		}
		if strings.Contains(body, "tile-num") {
			t.Error("a failed load must not render tiles that read as real zeros")
		}
	})

	t.Run("a tile's URL becomes the directory filter", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		run(h, "csr", "/staff/subscribers?view=expiring&q=%2B9198", "subscribers")
		if fc.gotFilt == nil || fc.gotFilt.View != "expiring" || fc.gotFilt.Query != "+9198" {
			t.Fatalf("want view=expiring q=+9198, got %+v", fc.gotFilt)
		}
	})

	t.Run("a failed directory load shows the error, not an empty result", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{err: errors.New("boom")}}
		body := run(h, "csr", "/staff/subscribers?q=9876543210", "subscribers").Body.String()
		if !strings.Contains(body, "Could not load subscribers.") {
			t.Error("the failure must be reported")
		}
		if strings.Contains(body, "No subscribers match.") || strings.Contains(body, "shown of") {
			t.Error("a failed load must not also claim that nobody matched")
		}
	})

	t.Run("an unknown filter is reported and dropped, never passed to the store", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		body := run(h, "csr", "/staff/subscribers?status=bogus&view=nope", "subscribers").Body.String()
		if fc.gotFilt == nil || fc.gotFilt.Status != "" || fc.gotFilt.View != "" {
			t.Fatalf("unknown values must be cleared before the query, got %+v", fc.gotFilt)
		}
		if !strings.Contains(body, "showing everyone") {
			t.Error("the operator should be told the filter was not understood")
		}
	})
}
