package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Subscriber credential management and per-subscriber plan change — the
// view/reset password card and the change-plan form on a subscriber's page.
//
// The console delegates to the API handler (h.credentials, h.bulkActions), the
// same instances the JSON API uses, rather than reaching into the key store or
// the lifecycle logic itself. Each handler re-checks the caller's role here too:
// hiding a card is presentation, not a control.

// credCtxKey is this package's private context key type, distinct from auth.go's
// sessionKey so the two cannot collide.
type credCtxKey int

const revealedPasswordKey credCtxKey = 0

// revealState carries a just-revealed password from the handler to the shared
// detail renderer through the request context, so it is shown on that one
// render only.
type revealState struct {
	Stored   bool
	Password string
}

// canRevealPassword gates seeing the credentials card and revealing the
// password: owner and billing run accounts, plus CSR, who tells a customer their
// password on a support call. A technician or NOC engineer has no such need.
func canRevealPassword(role string) bool {
	return role == "isp_owner" || role == "billing_admin" || role == "csr"
}

// canManageCredentials gates the state-changing actions — resetting a password
// and changing a plan — to the same owner/billing tier the API's admin
// middleware and the bulk toolbar require.
func canManageCredentials(role string) bool {
	return role == "isp_owner" || role == "billing_admin"
}

// RevealPassword handles POST /staff/subscribers/{id}/password/reveal.
func (h *Handler) RevealPassword(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canRevealPassword(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Your role cannot view subscriber passwords.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	if h.credentials == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Password view is not configured on this deployment.")
		return
	}

	// WithSubject so the reveal is attributed to this operator in the audit log.
	ctx := middleware.WithSubject(r.Context(), s.Username)
	password, stored, err := h.credentials.RevealSubscriberPassword(ctx, id)
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: credential reveal failed")
		h.renderSubscriberDetail(w, r, s, id, "Could not reveal the password.")
		return
	}

	// Pass the result to the shared renderer through the context, so it appears
	// on this render only — never stored in the page for an ordinary load.
	r = r.WithContext(context.WithValue(r.Context(), revealedPasswordKey, revealState{Stored: stored, Password: password}))
	h.renderSubscriberDetail(w, r, s, id, "")
}

// ResetPassword handles POST /staff/subscribers/{id}/password.
func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canManageCredentials(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Your role cannot reset subscriber passwords.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	if h.credentials == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Password reset is not configured on this deployment.")
		return
	}

	newPassword := r.PostFormValue("new_password")
	if strings.TrimSpace(newPassword) == "" {
		h.renderSubscriberDetail(w, r, s, id, "A new password is required.")
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.credentials.ResetSubscriberPassword(ctx, id, newPassword); err != nil {
		// A validation failure (too short) carries a message safe to show; any
		// other error is logged and shown generically.
		if errors.Is(err, api.ErrSubscriberInvalid) {
			h.renderSubscriberDetail(w, r, s, id, "Password is too short — use at least 6 characters.")
			return
		}
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: credential reset failed")
		h.renderSubscriberDetail(w, r, s, id, "Could not reset the password.")
		return
	}
	h.redirectToSubscriber(w, r, id, "Password reset. The new password takes effect on the subscriber's next login.")
}

// ChangeSubscriberPlan handles POST /staff/subscribers/{id}/plan-change.
//
// Delegates to the same ChangePlanForMany the bulk toolbar uses, with a single
// id, so one subscriber's plan change goes through the identical proration and
// billing path as a bulk one.
func (h *Handler) ChangeSubscriberPlan(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canManageCredentials(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Your role cannot change a subscriber's plan.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	if h.bulkActions == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Plan change is not configured on this deployment.")
		return
	}

	planID, err := strconv.Atoi(r.PostFormValue("plan_id"))
	if err != nil || planID <= 0 {
		h.renderSubscriberDetail(w, r, s, id, "Choose a plan to change to.")
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	results := h.bulkActions.ChangePlanForMany(ctx, []int{id}, planID)
	if len(results) == 0 {
		h.renderSubscriberDetail(w, r, s, id, "Plan change did not run.")
		return
	}
	if !results[0].OK {
		h.renderSubscriberDetail(w, r, s, id, "Plan change failed: "+results[0].Error)
		return
	}
	h.redirectToSubscriber(w, r, id, "Plan changed. Billing is prorated from today.")
}

// redirectToSubscriber sends the operator back to the subscriber page with a
// confirmation message, following the POST-redirect-GET the speed-override
// handlers already use so a refresh does not resubmit.
func (h *Handler) redirectToSubscriber(w http.ResponseWriter, r *http.Request, id int, msg string) {
	q := url.Values{"msg": {msg}}
	http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(id)+"?"+q.Encode(), http.StatusSeeOther)
}
