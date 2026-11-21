package staffui

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// fakeTickets records what the update handler asked the store to change.
type fakeTickets struct {
	calls     int
	gotStatus *string
	gotAssign *int
	returnRec *api.TicketRecord
}

func (f *fakeTickets) ListTickets(context.Context, int) ([]portal.TicketEntry, error) {
	return nil, nil
}
func (f *fakeTickets) UpdateTicketAdmin(_ context.Context, id int, status *string, assignedTo *int, _ *string) (*api.TicketRecord, error) {
	f.calls++
	f.gotStatus, f.gotAssign = status, assignedTo
	if f.returnRec != nil {
		return f.returnRec, nil
	}
	st := "open"
	if status != nil {
		st = *status
	}
	return &api.TicketRecord{ID: id, SubscriberID: 1, Status: st}, nil
}

// fakeQueue counts enqueued notifications.
type fakeQueue struct{ n int }

func (q *fakeQueue) Enqueue(*jobqueue.Task, ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	q.n++
	return &jobqueue.TaskInfo{}, nil
}

func serveTickets(h *Handler, role, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{StaffID: 7, Username: role + "_user", Role: role}))
	w := httptest.NewRecorder()
	if method == "POST" {
		r.SetPathValue("id", strings.Split(strings.Trim(r.URL.Path, "/"), "/")[2])
		h.UpdateTicketStatus(w, r)
	} else {
		h.Tickets(w, r)
	}
	return w
}

var queueAssignees = []Assignee{
	{ID: 7, Username: "csr", FullName: "Divya Nair", Role: "csr"},
	{ID: 9, Username: "tech", FullName: "Suresh Babu", Role: "technician"},
}

func TestTicketQueueScreen(t *testing.T) {
	due := time.Now().Add(-time.Hour)
	queue := []QueueTicket{{ID: 41, SubscriberID: 1, Username: "test_user", Category: "connectivity",
		Description: "No internet since morning", Priority: "high", Status: "open",
		ResolutionDue: &due, CreatedAt: time.Now().Add(-5 * time.Hour), Overdue: true}}

	t.Run("lists every subscriber's tickets with the summary", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{queue: queue, assignees: queueAssignees,
			summary: &HelpdeskSummary{Open: 3, Overdue: 1, Unassigned: 2}}}
		body := serveTickets(h, "csr", "GET", "/staff/tickets", nil).Body.String()
		for _, want := range []string{"No internet since morning", "test_user", "overdue", "Past resolution target", "Suresh Babu"} {
			if !strings.Contains(body, want) {
				t.Errorf("queue page missing %q", want)
			}
		}
	})

	t.Run("filters reach the store; mine uses the operator's id", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		serveTickets(h, "technician", "GET", "/staff/tickets?view=mine&priority=high&q=+router+&subscriber_id=3", nil)
		f := fc.gotTicket
		if f == nil || f.View != "mine" || f.Priority != "high" || f.Query != "router" || f.SubscriberID != 3 || f.StaffUsername != "technician_user" {
			t.Fatalf("filter not passed through: %+v", f)
		}
	})

	t.Run("unknown view and priority are reported and dropped", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		body := serveTickets(h, "csr", "GET", "/staff/tickets?view=vip&priority=urgent", nil).Body.String()
		if fc.gotTicket.View != "open" || fc.gotTicket.Priority != "" {
			t.Fatalf("unknown values must not reach the store: %+v", fc.gotTicket)
		}
		if !strings.Contains(body, "Unknown") {
			t.Error("the operator should be told the filter was not understood")
		}
	})

	t.Run("billing admin cannot open the queue", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{}}
		if code := serveTickets(h, "billing_admin", "GET", "/staff/tickets", nil).Code; code != 403 {
			t.Errorf("want 403, got %d", code)
		}
	})
}

func TestUpdateTicketFromQueue(t *testing.T) {
	newHandler := func() (*Handler, *fakeTickets, *fakeQueue) {
		ft, fq := &fakeTickets{}, &fakeQueue{}
		return &Handler{console: &fakeConsole{assignees: queueAssignees}, tickets: ft, tasks: fq}, ft, fq
	}

	t.Run("assigning without changing status does not notify the subscriber", func(t *testing.T) {
		h, ft, fq := newHandler()
		w := serveTickets(h, "csr", "POST", "/staff/tickets/41/status", url.Values{
			"status": {"open"}, "current_status": {"open"}, "assigned_to": {"9"},
			"return_view": {"unassigned"},
		})
		if w.Code != 303 {
			t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
		}
		if ft.gotStatus != nil || ft.gotAssign == nil || *ft.gotAssign != 9 {
			t.Errorf("want assignee 9 and no status change, got status=%v assign=%v", ft.gotStatus, ft.gotAssign)
		}
		if fq.n != 0 {
			t.Errorf("an internal reassignment must not notify the subscriber (%d enqueued)", fq.n)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "view=unassigned") || !strings.Contains(loc, "updated=1") {
			t.Errorf("should return to the same view, got %q", loc)
		}
	})

	t.Run("a real status change is applied and notified", func(t *testing.T) {
		h, ft, fq := newHandler()
		serveTickets(h, "csr", "POST", "/staff/tickets/41/status", url.Values{
			"status": {"resolved"}, "current_status": {"in_progress"},
		})
		if ft.gotStatus == nil || *ft.gotStatus != "resolved" || ft.gotAssign != nil {
			t.Errorf("want status resolved only, got status=%v assign=%v", ft.gotStatus, ft.gotAssign)
		}
		if fq.n != 1 {
			t.Errorf("want one subscriber notification, got %d", fq.n)
		}
	})

	t.Run("nothing changed means no write at all", func(t *testing.T) {
		h, ft, fq := newHandler()
		serveTickets(h, "csr", "POST", "/staff/tickets/41/status", url.Values{
			"status": {"open"}, "current_status": {"open"},
		})
		if ft.calls != 0 || fq.n != 0 {
			t.Errorf("want no update and no notification, got %d updates %d notifications", ft.calls, fq.n)
		}
	})

	t.Run("an assignee outside the ticket roles is refused", func(t *testing.T) {
		h, ft, _ := newHandler()
		w := serveTickets(h, "csr", "POST", "/staff/tickets/41/status", url.Values{
			"status": {"open"}, "current_status": {"open"}, "assigned_to": {"3"}, // e.g. a billing admin
		})
		if w.Code != 422 || ft.calls != 0 {
			t.Errorf("want 422 and no write, got %d with %d writes", w.Code, ft.calls)
		}
	})

	t.Run("the legacy form without current_status still applies its status", func(t *testing.T) {
		h, ft, fq := newHandler()
		serveTickets(h, "csr", "POST", "/staff/tickets/41/status", url.Values{
			"status": {"in_progress"}, "subscriber_id": {"1"},
		})
		if ft.gotStatus == nil || *ft.gotStatus != "in_progress" || fq.n != 1 {
			t.Errorf("legacy update should apply and notify, got status=%v notified=%d", ft.gotStatus, fq.n)
		}
	})
}

func TestRenderTicketQueue(t *testing.T) {
	h := &Handler{}
	assigned := 9
	now := time.Now()
	hours := 4.5
	for name, d := range map[string]ticketsData{
		"empty queue, no summary": {QueueMode: true, Statuses: ticketStatuses, Views: TicketViews, Priorities: TicketPriorities, Filter: TicketFilter{View: "open"}},
		"rows and summary": {QueueMode: true, Statuses: ticketStatuses, Views: TicketViews, Priorities: TicketPriorities,
			Filter: TicketFilter{View: "mine", Priority: "high", Query: "a&b", SubscriberID: 2}, Assignees: queueAssignees,
			Summary: &HelpdeskSummary{Open: 1, MedianCloseHours: &hours},
			Queue: []QueueTicket{
				{ID: 1, SubscriberID: 2, Username: "u", Category: "billing", Priority: "low", Status: "in_progress", AssignedTo: &assigned, AssigneeName: "Suresh Babu", CreatedAt: now},
				{ID: 2, SubscriberID: 2, Username: "u", Category: "other", Priority: "critical", Status: "open", ResolutionDue: &now, Overdue: true, CreatedAt: now},
			}},
		"legacy lookup": {Statuses: ticketStatuses, Looked: true, SubscriberID: 1},
	} {
		t.Run(name, func(t *testing.T) {
			assertRendersOK(t, h, "tickets", pageData{Session: testSession(), Data: d})
		})
	}
}

// ── Ratings in the queue and the summary strip — FR-SUP-004 ─────────────────

func ptrInt(v int) *int { return &v }

// Rendered with real data: the rating cell and the satisfaction tiles both
// read fields a wrong name would hide until somebody loaded the page.
func TestFR_SUP_004_TheQueueShowsTheSubscribersRating(t *testing.T) {
	four := 4
	now := time.Now()
	h := &Handler{}

	d := h.page(testSession(), "Support", "tickets")
	d.Data = ticketsData{
		QueueMode:  true,
		Filter:     TicketFilter{View: "all"},
		Views:      DirectoryViews,
		Priorities: []string{"low", "medium", "high", "critical"},
		Summary: &HelpdeskSummary{
			Open: 3, Overdue: 1, Unassigned: 2, Closed30d: 4,
			MedianCloseHours: ptrFloat(3.5),
			Ratings: tickets.RatingSummary{
				Rated: 3, Ratable: 4, Average: ptrFloat(2.67),
				Distribution: [tickets.MaxRating]int{1, 1, 0, 0, 1},
			},
		},
		Queue: []QueueTicket{
			{
				ID: 1, SubscriberID: 10, Username: "rated@isp", Category: "billing",
				Description: "overcharged", Priority: "medium", Status: "resolved",
				CreatedAt: now, UpdatedAt: now,
				Rating: &four, RatedAt: &now, RatingComment: "sorted quickly",
			},
			{
				ID: 2, SubscriberID: 11, Username: "unrated@isp", Category: "billing",
				Description: "still waiting", Priority: "low", Status: "open",
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	assertRendersOK(t, h, "tickets", d)

	w := httptest.NewRecorder()
	h.render(w, "tickets", d)
	body := w.Body.String()

	if strings.Contains(body, "<no value>") {
		t.Error("the queue rendered <no value>, so a template field name does not resolve")
	}
	// Four filled stars and one empty, and the comment beside them.
	if !strings.Contains(body, "★★★★☆") {
		t.Error("the rated ticket does not show its stars")
	}
	if !strings.Contains(body, "sorted quickly") {
		t.Error("the rating comment is not shown")
	}
	// The satisfaction tiles.
	for _, want := range []string{"Average rating (30 days)", "1&ndash;2 star", "worth a call back"} {
		if !strings.Contains(body, want) {
			t.Errorf("the summary strip is missing %q", want)
		}
	}
	// Three of four rated is 75%.
	if !strings.Contains(body, "75.0% replied") {
		t.Error("the response rate is not shown beside the average")
	}
	// Two negative ratings, which is the follow-up list.
	if !strings.Contains(body, ">2<") {
		t.Error("the negative count is not shown")
	}
	// And emphatically no way for staff to enter a rating: feedback staff
	// can write about their own work is not feedback.
	if strings.Contains(body, `name="rating"`) {
		t.Error("the console offers a rating input, which would let staff rate their own work")
	}
}

// An unrated helpdesk must not print zero, which reads as the worst possible
// score rather than as no data.
func TestFR_SUP_004_AnUnratedHelpdeskStripShowsADash(t *testing.T) {
	h := &Handler{}
	d := h.page(testSession(), "Support", "tickets")
	d.Data = ticketsData{
		QueueMode: true,
		Filter:    TicketFilter{View: "all"},
		Summary:   &HelpdeskSummary{Closed30d: 0},
	}
	assertRendersOK(t, h, "tickets", d)

	w := httptest.NewRecorder()
	h.render(w, "tickets", d)
	body := w.Body.String()
	if strings.Contains(body, "0.0 / 5") || strings.Contains(body, "0.0% replied") {
		t.Error("an unrated helpdesk reports 0.0, which reads as the worst possible score " +
			"rather than as nothing to report")
	}
}
