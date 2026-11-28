package staffui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/rs/zerolog/log"
)

// DunningManager sets grace extensions and the dunning ladder's timing
// (FR-BIL-016/017). Satisfied by *api.Handler, so the console and the API
// apply the same limits and write the same audit entries.
type DunningManager interface {
	ExtendGrace(ctx context.Context, subscriberID int, until time.Time, reason string) error
	EndGrace(ctx context.Context, subscriberID int) error
	CurrentDunningPolicy(ctx context.Context) (billing.DunningPolicy, error)
	SaveDunningPolicy(ctx context.Context, p billing.DunningPolicy) error
}

// graceMessages words each grace outcome carried back to the subscriber
// page. The URL holds only the code, so it cannot make the page say
// anything else.
var graceMessages = map[string]string{
	"set":   "Grace extended. Service is restored or kept on from the next dunning pass, within the hour.",
	"ended": "Grace extension ended. The ordinary reminder and suspension schedule applies again from the next pass.",
}

// ExtendGraceForm handles POST /staff/subscribers/{id}/grace.
func (h *Handler) ExtendGraceForm(w http.ResponseWriter, r *http.Request) {
	s, id, ok := h.graceGate(w, r)
	if !ok {
		return
	}
	until, err := time.ParseInLocation("2006-01-02", r.PostFormValue("until"), time.Local)
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Grace not extended: choose the date it should last until.")
		return
	}
	// The date means the whole of that day.
	until = until.Add(24*time.Hour - time.Second)
	ctx := middleware.WithSubject(r.Context(), s.Username)
	err = h.dunning.ExtendGrace(ctx, id, until, r.PostFormValue("reason"))
	switch {
	case err == nil:
		http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(id)+"?grace=set", http.StatusSeeOther)
	case billing.GraceInvalidReason(err) != "":
		h.renderSubscriberDetail(w, r, s, id, "Grace not extended: "+billing.GraceInvalidReason(err)+".")
	case errors.Is(err, billing.ErrGraceNotApplicable):
		h.renderSubscriberDetail(w, r, s, id, "Grace not extended: this subscriber's plan has not expired, so there is nothing to hold off.")
	default:
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: extend grace failed")
		h.renderSubscriberDetail(w, r, s, id, "Grace not extended. Nothing was changed; try again.")
	}
}

// EndGraceForm handles POST /staff/subscribers/{id}/grace/end.
func (h *Handler) EndGraceForm(w http.ResponseWriter, r *http.Request) {
	s, id, ok := h.graceGate(w, r)
	if !ok {
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.dunning.EndGrace(ctx, id); err != nil {
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: end grace failed")
		h.renderSubscriberDetail(w, r, s, id, "Could not end the grace extension. Nothing was changed.")
		return
	}
	http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(id)+"?grace=ended", http.StatusSeeOther)
}

func (h *Handler) graceGate(w http.ResponseWriter, r *http.Request) (Session, int, bool) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return s, 0, false
	}
	// Holding off a suspension is a billing decision: the same roles as
	// recording a payment, which is the other way to keep someone on.
	if !canRecordPayment(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can extend grace.")
		return s, 0, false
	}
	if h.dunning == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Grace extensions are not configured on this deployment.")
		return s, 0, false
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return s, 0, false
	}
	return s, id, true
}

// graceView is the grace card's state on the subscriber Billing tab.
type graceView struct {
	Active     bool // an extension is in force
	Until      time.Time
	Reason     string
	SetBy      string
	PastExpiry bool // an extension can be set
	Expiry     *time.Time
	MinDate    string // the date input's bounds: tomorrow to 30 days out
	MaxDate    string
}

func newGraceView(until *time.Time, reason, setBy string, expiry *time.Time, status string, now time.Time) graceView {
	g := graceView{
		Expiry:  expiry,
		MinDate: now.AddDate(0, 0, 1).Format("2006-01-02"),
		MaxDate: now.AddDate(0, 0, 29).Format("2006-01-02"),
	}
	if until != nil && until.After(now) {
		g.Active, g.Until, g.Reason, g.SetBy = true, *until, reason, setBy
	}
	g.PastExpiry = expiry != nil && !expiry.After(now) && status != "terminated"
	return g
}

// dunningData backs the Dunning Settings screen.
type dunningData struct {
	Policy  billing.DunningPolicy
	Ladder  []ladderStep
	CanEdit bool
	Garden  WalledGardenInfo
	// GardenList is the address list RADIUS puts soft-suspended sessions
	// on, named so the screen and the router rules say the same thing.
	GardenList string
}

// WalledGardenInfo is the payment-page redirector's configuration
// (WALLED_GARDEN_ADDR, WALLED_GARDEN_REDIRECT_URL). Addr is empty when it is
// off.
type WalledGardenInfo struct {
	Addr        string
	RedirectURL string
}

// ladderStep is one row of the spelled-out schedule. State only colours the
// label: the internal names (remind_7d and so on) date from the fixed
// schedule and would contradict the timing shown next to them once it is
// changed.
type ladderStep struct {
	When  string
	State string
	Label string
	What  string
}

// ladderFor spells a policy out as the sequence a subscriber who does not
// pay will see, which is easier to check than five numbers.
func ladderFor(p billing.DunningPolicy) []ladderStep {
	days := func(n int, rel string) string {
		switch n {
		case 0:
			return "On the expiry date"
		case 1:
			return "1 day " + rel
		}
		return strconv.Itoa(n) + " days " + rel
	}
	return []ladderStep{
		{days(p.RemindFirstDays, "before expiry"), "remind_7d", "First reminder", "Renewal reminder sent"},
		{days(p.RemindSecondDays, "before expiry"), "remind_3d", "Second reminder", "Renewal reminder sent"},
		{days(p.RemindLastDays, "before expiry"), "remind_1d", "Last reminder", "Renewal reminder sent"},
		{"On expiry", "grace_period", "Grace", "Service stays on; final notice sent"},
		{days(p.GraceDays, "after expiry"), "soft_suspended", "Soft suspension", "Browsing redirected to the payment page"},
		{days(p.GraceDays+p.SoftSuspendDays, "after expiry"), "hard_suspended", "Hard suspension", "Disconnected"},
	}
}

func canEditDunning(role string) bool { return role == "isp_owner" || role == "billing_admin" }

// DunningSettings handles GET /staff/dunning.
func (h *Handler) DunningSettings(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "dunning")
	if !ok {
		return
	}
	if h.dunning == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Dunning settings are not configured on this deployment.")
		return
	}
	p, err := h.dunning.CurrentDunningPolicy(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: read dunning policy failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not read the dunning settings.")
		return
	}
	msg := ""
	if r.URL.Query().Get("saved") == "1" {
		msg = "Saved. The new schedule applies from the next dunning pass, within the hour."
	}
	h.renderDunning(w, r, s, p, "", msg)
}

func (h *Handler) renderDunning(w http.ResponseWriter, _ *http.Request, s Session, p billing.DunningPolicy, errMsg, msg string) {
	d := h.page(s, "Dunning Settings", "dunning")
	d.Error, d.Message = errMsg, msg
	d.Data = dunningData{Policy: p, Ladder: ladderFor(p), CanEdit: canEditDunning(s.Role),
		Garden: h.walledGarden, GardenList: nas.WalledGardenAddressList}
	h.render(w, "dunning", d)
}

// SaveDunningForm handles POST /staff/dunning.
func (h *Handler) SaveDunningForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "dunning")
	if !ok {
		return
	}
	if !canEditDunning(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can change the dunning schedule.")
		return
	}
	if h.dunning == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Dunning settings are not configured on this deployment.")
		return
	}
	var p billing.DunningPolicy
	fields := []struct {
		name string
		dst  *int
	}{
		{"remind_first_days", &p.RemindFirstDays}, {"remind_second_days", &p.RemindSecondDays},
		{"remind_last_days", &p.RemindLastDays}, {"grace_days", &p.GraceDays}, {"soft_suspend_days", &p.SoftSuspendDays},
	}
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(f.name)))
		if err != nil {
			h.renderDunning(w, r, s, p, "Not saved: every figure is a whole number of days.", "")
			return
		}
		*f.dst = n
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	err := h.dunning.SaveDunningPolicy(ctx, p)
	switch {
	case err == nil:
		http.Redirect(w, r, "/staff/dunning?saved=1", http.StatusSeeOther)
	case errors.Is(err, billing.ErrPolicyInvalid):
		h.renderDunning(w, r, s, p, "Not saved: "+strings.TrimPrefix(err.Error(), billing.ErrPolicyInvalid.Error()+": ")+".", "")
	default:
		log.Error().Err(err).Msg("staffui: save dunning policy failed")
		h.renderDunning(w, r, s, p, "Not saved. The schedule in force is unchanged; try again.", "")
	}
}
