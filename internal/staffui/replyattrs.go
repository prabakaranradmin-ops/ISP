package staffui

import (
	"context"
	"errors"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004.
//
// ReplyAttrManager is satisfied by *api.Handler, so the console validates
// and audits exactly as the API does.
type ReplyAttrManager interface {
	SetPlanReplyAttribute(ctx context.Context, in nas.ReplyAttribute) (*nas.ReplyAttribute, error)
	DeletePlanReplyAttribute(ctx context.Context, id int) error
	SetPlanReplyAttributeEnabled(ctx context.Context, id int, enabled bool) error
	ListPlanReplyAttributes(ctx context.Context, planID int) ([]nas.ReplyAttribute, error)
}

var replyAttrMessages = map[string]string{
	"saved":    "Attribute saved. Routers pick it up within a minute, and subscribers get it on their next authentication.",
	"deleted":  "Attribute removed.",
	"enabled":  "Attribute switched on.",
	"disabled": "Attribute switched off. It is kept, so switching it back on does not mean retyping it.",

	"missing": "No attribute with that id.",
	"failed":  "That did not go through. Nothing was changed; try again.",
}

var replyAttrSuccess = map[string]bool{
	"saved": true, "deleted": true, "enabled": true, "disabled": true,
}

// attrRow is one stored attribute as the screen shows it.
type attrRow struct {
	nas.ReplyAttribute
	// Help and Unverified come from the dictionary rather than the row, so
	// the explanation stays with the definition.
	Help       string
	Unverified bool
	// Unknown marks a stored attribute the dictionary no longer defines.
	// It is not sent, and the screen has to say so — otherwise the row
	// looks configured and does nothing, which is the failure this whole
	// feature is built to avoid.
	Unknown bool
}

// attrOption is one definable attribute for the add form.
type attrOption struct {
	Name            string
	Kind            string
	Help            string
	Unverified      bool
	Reserved        bool
	ReservedBecause string
}

type replyAttrData struct {
	Plans     []Plan
	PlanID    int
	PlanName  string
	Vendor    string
	Vendors   []string
	Rows      []attrRow
	Options   []attrOption
	CanManage bool
}

// canManageReplyAttributes: the same reach as Routers, not as Catalogue.
//
// These are RADIUS attributes — an address list, a NAS-side pool, a session
// timeout — and they carry no price. That is the distinction from a
// bandwidth profile (FR-CAT-002), which sets the speed a plan delivers and
// is therefore a commercial decision reaching billing staff: getting an
// address list wrong breaks a firewall rule, while getting a speed wrong
// changes what a customer was sold.
func canManageReplyAttributes(role string) bool {
	return role == "isp_owner" || role == "noc_engineer"
}

func (h *Handler) replyAttrGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "plan-attributes")
	if !ok {
		return s, false
	}
	if h.replyAttrs == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Per-plan RADIUS attributes are not configured on this deployment.")
		return s, false
	}
	return s, true
}

// PlanAttributes handles GET /staff/plan-attributes.
func (h *Handler) PlanAttributes(w http.ResponseWriter, r *http.Request) {
	s, ok := h.replyAttrGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "RADIUS attributes", "plan-attributes")
	q := r.URL.Query()

	pd := replyAttrData{
		Vendors:   nas.Vendors(),
		Vendor:    strings.ToLower(strings.TrimSpace(q.Get("vendor"))),
		CanManage: canManageReplyAttributes(s.Role),
	}
	if pd.Vendor != "" && !nas.KnownVendor(nas.Vendor(pd.Vendor)) {
		// An unknown vendor in the URL is ignored rather than passed on, so
		// a stale bookmark shows the screen instead of an error.
		pd.Vendor = ""
	}
	if pd.Vendor == "" {
		pd.Vendor = string(nas.VendorMikrotik)
	}

	if h.catalogue != nil {
		if plans, err := h.catalogue.ListPlans(r.Context()); err == nil {
			pd.Plans = plans
		}
	}

	if raw := strings.TrimSpace(q.Get("plan_id")); raw != "" {
		id, err := strconv.Atoi(raw)
		if err == nil {
			pd.PlanID = id
		}
	}
	// Default to the first plan so the screen is never a bare dropdown with
	// nothing on it.
	if pd.PlanID == 0 && len(pd.Plans) > 0 {
		pd.PlanID = pd.Plans[0].ID
	}
	for _, p := range pd.Plans {
		if p.ID == pd.PlanID {
			pd.PlanName = p.Name
		}
	}

	if pd.PlanID != 0 {
		attrs, err := h.replyAttrs.ListPlanReplyAttributes(r.Context(), pd.PlanID)
		if err != nil {
			h.renderError(w, r, s, http.StatusInternalServerError,
				"Could not read this plan's RADIUS attributes.")
			return
		}
		for _, a := range attrs {
			row := attrRow{ReplyAttribute: a}
			// Looked up for the help text. A reserved attribute returns an
			// error here, but a stored one cannot be reserved — Validate
			// refuses that — so an error means the dictionary no longer
			// defines it.
			if def, err := nas.LookupAttribute(a.Vendor, a.Name); err == nil {
				row.Help, row.Unverified = def.Help, def.Unverified
			} else {
				row.Unknown = true
			}
			pd.Rows = append(pd.Rows, row)
		}
	}

	// The add form's options, reserved ones included so an operator can see
	// why an attribute is not offered rather than assume the dictionary is
	// incomplete and ask for it.
	for _, def := range nas.AttributesFor(nas.Vendor(pd.Vendor)) {
		pd.Options = append(pd.Options, attrOption{
			Name: def.Name, Kind: string(def.Kind), Help: def.Help,
			Unverified: def.Unverified,
			Reserved:   def.Reserved, ReservedBecause: def.ReservedBecause,
		})
	}

	if code := q.Get("m"); code != "" {
		if m, ok := replyAttrMessages[code]; ok {
			if replyAttrSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}

	d.Data = pd
	h.render(w, "plan_attributes", d)
}

func (h *Handler) requireReplyAttrManager(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.replyAttrGate(w, r)
	if !ok {
		return s, false
	}
	if !canManageReplyAttributes(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only network engineers and the owner can change a plan's RADIUS attributes.")
		return s, false
	}
	return s, true
}

// SetPlanAttributeForm handles POST /staff/plan-attributes.
func (h *Handler) SetPlanAttributeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireReplyAttrManager(w, r)
	if !ok {
		return
	}
	planID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("plan_id")))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid plan id.")
		return
	}
	in := nas.ReplyAttribute{
		PlanID:  planID,
		Vendor:  nas.Vendor(r.PostFormValue("vendor")),
		Name:    r.PostFormValue("attribute"),
		Value:   r.PostFormValue("value"),
		Enabled: true,
	}.Normalise()

	// Validated here so the operator gets the dictionary's sentence —
	// which names the attribute and says what is wrong with the value —
	// rather than a generic failure.
	if err := in.Validate(); err != nil {
		h.redirectPlanAttrs(w, r, planID, in.Vendor, "", err.Error())
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.replyAttrs.SetPlanReplyAttribute(ctx, in); err != nil {
		h.replyAttrFailure(w, r, planID, in.Vendor, err)
		return
	}
	h.redirectPlanAttrs(w, r, planID, in.Vendor, "saved", "")
}

// TogglePlanAttributeForm handles POST /staff/plan-attributes/{id}/enabled.
func (h *Handler) TogglePlanAttributeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireReplyAttrManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid attribute id.")
		return
	}
	enabled := r.PostFormValue("enabled") == "true"
	planID, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("plan_id")))
	vendor := nas.Vendor(r.PostFormValue("vendor"))

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.replyAttrs.SetPlanReplyAttributeEnabled(ctx, id, enabled); err != nil {
		h.replyAttrFailure(w, r, planID, vendor, err)
		return
	}
	code := "disabled"
	if enabled {
		code = "enabled"
	}
	h.redirectPlanAttrs(w, r, planID, vendor, code, "")
}

// DeletePlanAttributeForm handles POST /staff/plan-attributes/{id}/delete.
func (h *Handler) DeletePlanAttributeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireReplyAttrManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid attribute id.")
		return
	}
	planID, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("plan_id")))
	vendor := nas.Vendor(r.PostFormValue("vendor"))

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.replyAttrs.DeletePlanReplyAttribute(ctx, id); err != nil {
		h.replyAttrFailure(w, r, planID, vendor, err)
		return
	}
	h.redirectPlanAttrs(w, r, planID, vendor, "deleted", "")
}

func (h *Handler) redirectPlanAttrs(w http.ResponseWriter, r *http.Request, planID int, vendor nas.Vendor, code, problem string) {
	v := neturl.Values{}
	if planID != 0 {
		v.Set("plan_id", strconv.Itoa(planID))
	}
	if vendor != "" {
		v.Set("vendor", string(vendor))
	}
	switch {
	case problem != "":
		v.Set("err", problem)
	case code != "":
		v.Set("m", code)
	}
	url := "/staff/plan-attributes"
	if len(v) > 0 {
		url += "?" + v.Encode()
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (h *Handler) replyAttrFailure(w http.ResponseWriter, r *http.Request, planID int, vendor nas.Vendor, err error) {
	switch {
	case errors.Is(err, nas.ErrReplyAttributeExists):
		// Reachable only in a race: the store upserts, so a duplicate is
		// normally a successful correction rather than a failure.
		h.redirectPlanAttrs(w, r, planID, vendor, "", "That attribute was just added by someone else. Reload to see it.")
	case strings.Contains(err.Error(), "not found"):
		h.redirectPlanAttrs(w, r, planID, vendor, "missing", "")
	default:
		h.redirectPlanAttrs(w, r, planID, vendor, "failed", "")
	}
}
