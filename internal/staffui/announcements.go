package staffui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
	"github.com/rs/zerolog/log"
)

// AnnouncementManager is the broadcast flow as the API runs it
// (FR-ANN-001..002). Satisfied by *api.Handler: compose validates and stores
// a draft without sending; broadcast claims the draft atomically, so a
// double-click cannot send twice, and fans out one queued task per
// recipient per channel.
type AnnouncementManager interface {
	ComposeAnnouncement(ctx context.Context, in api.AnnouncementInput) (*notifications.Announcement, error)
	BroadcastAnnouncement(ctx context.Context, id int) (*api.SendResult, error)
	AnnouncementList(ctx context.Context, status *string) ([]notifications.Announcement, error)
}

// AudienceCounter estimates a draft's reach. Satisfied by *db.ConsoleStore.
type AudienceCounter interface {
	CountAnnouncementAudience(ctx context.Context, franchiseID, planID *int, status *string) (int, error)
}

var announcementChannels = []string{"whatsapp", "sms", "email", "push"}

type announcementRow struct {
	notifications.Announcement
	// Audience is the current reach of a draft's segment, or -1 when it was
	// addressed to a hand-picked list (whose size is not on the row) or the
	// estimate could not be made.
	Audience int
	PlanName string
}

type announcementsData struct {
	Rows     []announcementRow
	Plans    []Plan
	Statuses []string
	Channels []string
	Form     api.AnnouncementInput
	FormPlan int
}

// Announcements lists broadcasts and composes new drafts.
func (h *Handler) Announcements(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "announcements")
	if !ok {
		return
	}
	msg := ""
	if r.URL.Query().Get("created") != "" {
		msg = "Draft saved. Review the audience below, then send it."
	}
	h.renderAnnouncements(w, r, s, announcementsData{}, msg, "")
}

func (h *Handler) renderAnnouncements(w http.ResponseWriter, r *http.Request, s Session, ad announcementsData, message, errMsg string) {
	d := h.page(s, "Announcements", "announcements")
	d.Message, d.Error = message, errMsg
	if h.announcer == nil {
		d.Error = "Announcements are not configured on this deployment."
		h.render(w, "announcements", d)
		return
	}
	ad.Statuses, ad.Channels = SubscriberStatuses, announcementChannels
	planNames := map[int]string{}
	if h.catalogue != nil {
		// Every plan, retired ones included, and deliberately not
		// ListActivePlans: this is a filter over the subscribers who exist,
		// and the ones still on a retired tariff are exactly the audience
		// an operator announcing its withdrawal needs to reach.
		if plans, err := h.catalogue.ListPlans(r.Context()); err == nil {
			ad.Plans = plans
			for _, p := range plans {
				planNames[p.ID] = p.Name
			}
		}
	}
	list, err := h.announcer.AnnouncementList(r.Context(), nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list announcements failed")
		if d.Error == "" {
			d.Error = "Could not load announcements."
		}
	}
	for _, a := range list {
		row := announcementRow{Announcement: a, Audience: -1}
		if a.SegmentPlanID != nil {
			row.PlanName = planNames[*a.SegmentPlanID]
		}
		if a.Status == notifications.AnnouncementDraft && h.audience != nil {
			if n, err := h.audience.CountAnnouncementAudience(r.Context(), a.SegmentFranchiseID, a.SegmentPlanID, a.SegmentStatus); err == nil {
				row.Audience = n
			}
		}
		ad.Rows = append(ad.Rows, row)
	}
	d.Data = ad
	h.render(w, "announcements", d)
}

// ComposeAnnouncementForm handles POST /staff/announcements/new.
func (h *Handler) ComposeAnnouncementForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "announcements")
	if !ok {
		return
	}
	if h.announcer == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Announcements are not configured on this deployment.")
		return
	}
	_ = r.ParseForm() //nolint:errcheck // requireCSRF already parsed it
	in := api.AnnouncementInput{
		Title:        strings.TrimSpace(r.PostFormValue("title")),
		Body:         strings.TrimSpace(r.PostFormValue("body")),
		Class:        r.PostFormValue("class"),
		ShowInPortal: r.PostFormValue("show_in_portal") != "",
	}
	in.Channels = r.PostForm["channels"]
	formPlan := 0
	if v := r.PostFormValue("segment_plan_id"); v != "" {
		if id, err := strconv.Atoi(v); err == nil && id > 0 {
			in.SegmentPlanID, formPlan = &id, id
		}
	}
	if v := r.PostFormValue("segment_status"); v != "" {
		if !inStrings(SubscriberStatuses, v) {
			h.renderAnnouncements(w, r, s, announcementsData{Form: in, FormPlan: formPlan}, "", "Choose a status from the list.")
			return
		}
		in.SegmentStatus = &v
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.announcer.ComposeAnnouncement(ctx, in); err != nil {
		msg := "Could not save the draft."
		if reason := api.AnnouncementInvalidReason(err); reason != "" {
			msg = "Draft not saved: " + reason + "."
		} else {
			log.Error().Err(err).Msg("staffui: compose announcement failed")
		}
		h.renderAnnouncements(w, r, s, announcementsData{Form: in, FormPlan: formPlan}, "", msg)
		return
	}
	http.Redirect(w, r, "/staff/announcements?created=1", http.StatusSeeOther)
}

// SendAnnouncementForm handles POST /staff/announcements/{id}/send.
func (h *Handler) SendAnnouncementForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "announcements")
	if !ok {
		return
	}
	if h.announcer == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Announcements are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid announcement id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	res, err := h.announcer.BroadcastAnnouncement(ctx, id)
	switch {
	case errors.Is(err, notifications.ErrAnnouncementNotDraft):
		h.renderAnnouncements(w, r, s, announcementsData{}, "", "That announcement has already been sent, so it was not sent again.")
	case errors.Is(err, api.ErrAnnouncementNotFound):
		h.renderAnnouncements(w, r, s, announcementsData{}, "", "That announcement no longer exists.")
	case errors.Is(err, api.ErrAnnouncementDispatchDown):
		h.renderAnnouncements(w, r, s, announcementsData{}, "", "Sending is not configured on this deployment.")
	case err != nil:
		log.Error().Err(err).Int("announcement_id", id).Msg("staffui: send announcement failed")
		h.renderAnnouncements(w, r, s, announcementsData{}, "", "The announcement could not be sent. It is marked failed; nothing was retried.")
	default:
		h.renderAnnouncements(w, r, s, announcementsData{},
			"Sent to "+strconv.Itoa(res.Recipients)+" subscriber"+plural(res.Recipients)+": "+
				strconv.Itoa(res.TasksEnqueued)+" message"+plural(res.TasksEnqueued)+" queued for delivery.", "")
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
