package staffui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/rs/zerolog/log"
)

// newSubscriberData backs the create form.
//
// Plans travel with it so the form can offer a select of real tariffs
// rather than ask an operator to type a plan_id they would have to look up
// separately - and so a deployment with no plans yet can say so plainly
// instead of rendering a form that cannot succeed.
type newSubscriberData struct {
	Plans []Plan
	// Form carries what the operator typed, so a rejected submission comes
	// back filled in rather than blank. Never includes the password.
	Form newSubscriberForm
}

type newSubscriberForm struct {
	CAFNumber       string
	Username        string
	MobileNumber    string
	Email           string
	PlanID          int
	RegisteredState string
	Aadhaar         string
	PAN             string
}

// NewSubscriber renders the create-subscriber form.
func (h *Handler) NewSubscriber(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	h.renderNewSubscriber(w, r, s, newSubscriberForm{}, "")
}

func (h *Handler) renderNewSubscriber(w http.ResponseWriter, r *http.Request, s Session, form newSubscriberForm, errMsg string) {
	d := h.page(s, "New subscriber", "subscribers")
	d.Error = errMsg

	nd := newSubscriberData{Form: form}
	if h.catalogue != nil {
		plans, err := h.catalogue.ListPlans(r.Context())
		if err != nil {
			log.Error().Err(err).Msg("staffui: list plans for new subscriber failed")
		} else {
			nd.Plans = plans
		}
	}
	d.Data = nd
	h.render(w, "subscriber_new", d)
}

// CreateSubscriber provisions a subscriber from the console form.
//
// Creation was API-only before this: a CSR taking a signup over the phone
// had to hand the details to somebody who could mint a JWT and post JSON.
// The work itself is delegated to the API handler's ProvisionSubscriber so
// the password hashing, KYC encryption and audit entry are the same ones
// the API performs - see SubscriberCreator.
func (h *Handler) CreateSubscriber(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if h.subscriberCreator == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Subscriber creation is not configured on this deployment.")
		return
	}

	planID, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("plan_id"))) //nolint:errcheck // 0 fails validation below
	form := newSubscriberForm{
		CAFNumber:       strings.TrimSpace(r.PostFormValue("caf_number")),
		Username:        strings.TrimSpace(r.PostFormValue("username")),
		MobileNumber:    strings.TrimSpace(r.PostFormValue("mobile_number")),
		Email:           strings.TrimSpace(r.PostFormValue("email")),
		PlanID:          planID,
		RegisteredState: strings.TrimSpace(r.PostFormValue("registered_state")),
		Aadhaar:         strings.TrimSpace(r.PostFormValue("aadhaar")),
		PAN:             strings.TrimSpace(r.PostFormValue("pan")),
	}

	password := r.PostFormValue("password")
	if password == "" {
		h.renderNewSubscriber(w, r, s, form, "A password is required.")
		return
	}

	created, err := h.subscriberCreator.ProvisionSubscriber(r.Context(), api.CreateSubscriberRequest{
		CAFNumber:       form.CAFNumber,
		Username:        form.Username,
		Password:        password,
		MobileNumber:    form.MobileNumber,
		Email:           form.Email,
		PlanID:          form.PlanID,
		RegisteredState: form.RegisteredState,
		Aadhaar:         form.Aadhaar,
		PAN:             form.PAN,
	})
	switch {
	case errors.Is(err, api.ErrSubscriberInvalid):
		// The API's own validation message, which is written to be read by
		// a person, so it is shown rather than replaced with something
		// vaguer.
		h.renderNewSubscriber(w, r, s, form, strings.TrimPrefix(err.Error(),
			api.ErrSubscriberInvalid.Error()+": "))
		return
	case errors.Is(err, api.ErrSubscriberExists):
		h.renderNewSubscriber(w, r, s, form,
			"That CAF number or username is already in use.")
		return
	case err != nil:
		log.Error().Err(err).Str("username", form.Username).Msg("staffui: create subscriber failed")
		h.renderNewSubscriber(w, r, s, form, "Could not create that subscriber.")
		return
	}

	// Straight to the new subscriber's own page: the next thing an
	// operator does after creating one is check it, and this saves them
	// searching for the record they just made.
	http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(created.ID), http.StatusSeeOther)
}
