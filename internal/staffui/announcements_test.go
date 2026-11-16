package staffui

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

type fakeAnnouncer struct {
	list     []notifications.Announcement
	err      error
	composed *api.AnnouncementInput
	sent     []int
	gotActor string
}

func (f *fakeAnnouncer) ComposeAnnouncement(ctx context.Context, in api.AnnouncementInput) (*notifications.Announcement, error) {
	f.gotActor = middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	f.composed = &in
	return &notifications.Announcement{ID: 5}, nil
}

func (f *fakeAnnouncer) BroadcastAnnouncement(ctx context.Context, id int) (*api.SendResult, error) {
	f.gotActor = middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	f.sent = append(f.sent, id)
	return &api.SendResult{Recipients: 3, TasksEnqueued: 6}, nil
}

func (f *fakeAnnouncer) AnnouncementList(context.Context, *string) ([]notifications.Announcement, error) {
	return f.list, nil
}

func serveAnn(h *Handler, role, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 {
		r.SetPathValue("id", parts[2])
	}
	w := httptest.NewRecorder()
	switch {
	case method == "POST" && strings.HasSuffix(r.URL.Path, "/new"):
		h.ComposeAnnouncementForm(w, r)
	case method == "POST":
		h.SendAnnouncementForm(w, r)
	default:
		h.Announcements(w, r)
	}
	return w
}

func TestAnnouncementsScreen(t *testing.T) {
	plan := 2
	status := "active"
	list := []notifications.Announcement{
		{ID: 1, Title: "Outage tonight", Body: "1-3am", Channels: []string{"sms", "whatsapp"}, Class: "transactional",
			SegmentPlanID: &plan, SegmentStatus: &status, Status: "draft", CreatedBy: "owner", CreatedAt: time.Now()},
		{ID: 2, Title: "Old promo", Body: "x", Channels: []string{"email"}, Class: "marketing", Status: "sent", RecipientCount: 9, CreatedAt: time.Now()},
	}

	t.Run("owner and billing reach it; CSR, NOC and technician do not", func(t *testing.T) {
		for role, want := range map[string]int{"isp_owner": 200, "billing_admin": 200, "csr": 403, "noc_engineer": 403, "technician": 403} {
			h := &Handler{announcer: &fakeAnnouncer{}}
			if code := serveAnn(h, role, "GET", "/staff/announcements", nil).Code; code != want {
				t.Errorf("%s: want %d, got %d", role, want, code)
			}
		}
	})

	t.Run("a draft shows its reach and a Send button; a sent one does not", func(t *testing.T) {
		h := &Handler{announcer: &fakeAnnouncer{list: list}, audience: &fakeConsole{}}
		body := serveAnn(h, "billing_admin", "GET", "/staff/announcements", nil).Body.String()
		rows := tableBody(body)
		if !strings.Contains(rows, "Reaches 42 now") {
			t.Error("a draft should show its current reach")
		}
		if strings.Count(rows, "/send\"") != 1 {
			t.Error("only the draft may offer Send")
		}
		if !strings.Contains(rows, "9 messages queued") {
			t.Error("a sent announcement should show what was queued")
		}
	})

	t.Run("compose passes the form through and is attributed", func(t *testing.T) {
		fa := &fakeAnnouncer{}
		h := &Handler{announcer: fa}
		w := serveAnn(h, "isp_owner", "POST", "/staff/announcements/new", url.Values{
			"title": {"Maintenance"}, "body": {"Tonight"}, "class": {"transactional"},
			"channels": {"sms", "whatsapp"}, "segment_plan_id": {"2"}, "segment_status": {"active"},
		})
		if w.Code != 303 || fa.composed == nil {
			t.Fatalf("want redirect after compose, got %d", w.Code)
		}
		in := fa.composed
		if in.Class != "transactional" || len(in.Channels) != 2 || in.SegmentPlanID == nil || *in.SegmentPlanID != 2 ||
			in.SegmentStatus == nil || *in.SegmentStatus != "active" || fa.gotActor != "isp_owner_user" {
			t.Errorf("composed: %+v actor %q", in, fa.gotActor)
		}
	})

	t.Run("a status outside the list is refused before composing", func(t *testing.T) {
		fa := &fakeAnnouncer{}
		h := &Handler{announcer: fa}
		body := serveAnn(h, "isp_owner", "POST", "/staff/announcements/new", url.Values{
			"title": {"x"}, "body": {"y"}, "channels": {"sms"}, "segment_status": {"everyone"},
		}).Body.String()
		if fa.composed != nil || !strings.Contains(body, "Choose a status") {
			t.Error("an unknown status must not reach compose")
		}
	})

	t.Run("a validation failure keeps the draft text", func(t *testing.T) {
		fa := &fakeAnnouncer{err: fmt.Errorf("%w: an announcement needs at least one channel or show_in_portal", api.ErrAnnouncementInvalid)}
		h := &Handler{announcer: fa}
		body := serveAnn(h, "isp_owner", "POST", "/staff/announcements/new", url.Values{"title": {"Keep me"}, "body": {"y"}}).Body.String()
		if !strings.Contains(body, "Draft not saved: an announcement needs at least one channel") || !strings.Contains(body, `value="Keep me"`) {
			t.Error("reason and form text should be shown")
		}
	})

	t.Run("send reports reach and queued messages", func(t *testing.T) {
		fa := &fakeAnnouncer{}
		h := &Handler{announcer: fa}
		body := serveAnn(h, "billing_admin", "POST", "/staff/announcements/1/send", url.Values{}).Body.String()
		if len(fa.sent) != 1 || fa.sent[0] != 1 || !strings.Contains(body, "Sent to 3 subscribers: 6 messages queued") {
			t.Errorf("sent=%v", fa.sent)
		}
	})

	t.Run("a second send is refused, not repeated", func(t *testing.T) {
		h := &Handler{announcer: &fakeAnnouncer{err: notifications.ErrAnnouncementNotDraft}}
		body := serveAnn(h, "billing_admin", "POST", "/staff/announcements/1/send", url.Values{}).Body.String()
		if !strings.Contains(body, "already been sent") || strings.Contains(body, "Sent to") {
			t.Error("a not-draft refusal must be reported as such")
		}
	})
}

func TestRenderAnnouncements(t *testing.T) {
	h := &Handler{}
	plan := 1
	assertRendersOK(t, h, "announcements", pageData{Session: testSession(), Data: announcementsData{
		Statuses: SubscriberStatuses, Channels: announcementChannels,
		Plans: []Plan{{ID: 1, Name: "Home"}}, FormPlan: 1,
		Form: api.AnnouncementInput{Title: "T", Channels: []string{"sms"}, ShowInPortal: true},
		Rows: []announcementRow{
			{Announcement: notifications.Announcement{ID: 1, Title: "a", Status: "draft", SegmentPlanID: &plan, CreatedAt: time.Now()}, Audience: 3, PlanName: "Home"},
			{Announcement: notifications.Announcement{ID: 2, Title: "b", Status: "failed", CreatedAt: time.Now()}, Audience: -1},
		},
	}})
	assertRendersOK(t, h, "announcements", pageData{Session: testSession(), Data: announcementsData{}})
}
