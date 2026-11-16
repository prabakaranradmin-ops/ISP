package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/crm"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// LeadManager is the lead pipeline as the API runs it (FR-CRM-001..003).
// Satisfied by *api.Handler: validation, the converted/lost guards, the
// race-safe claim, KYC, CPE issue and the audit line all live there, and the
// console only supplies the operator and a nil (ISP-wide) scope — the Leads
// section is never open to franchise-scoped roles.
type LeadManager interface {
	NewLead(ctx context.Context, l crm.Lead, scope *int) (*crm.Lead, error)
	MoveLead(ctx context.Context, id int, status, assignedTo, notes, lostReason *string, scope *int) (*crm.Lead, error)
	ConvertLeadToSubscriber(ctx context.Context, id int, in api.ConvertLeadInput, scope *int) (*api.ConvertLeadResult, error)
	LeadList(ctx context.Context, status, source *string, scope *int) ([]crm.Lead, error)
	LeadFunnelReport(ctx context.Context, scope *int) (*crm.FunnelReport, error)
	LeadByID(ctx context.Context, id int, scope *int) (*crm.Lead, error)
}

// leadStages are the stages in pipeline order, for chips and the funnel strip.
var leadStages = []string{crm.StatusNew, crm.StatusContacted, crm.StatusQualified, crm.StatusConverted, crm.StatusLost}

// leadMovableStages are the stages a manual update may set (crm.ValidStatus).
var leadMovableStages = []string{crm.StatusNew, crm.StatusContacted, crm.StatusQualified, crm.StatusLost}

var leadSources = []string{"walk_in", "referral", "website", "campaign", "franchise", "other"}

// canConvertLead mirrors the API's admin gate on POST /leads/{id}/convert:
// conversion creates a billable account, which is billing's call.
func canConvertLead(role string) bool { return role == "isp_owner" || role == "billing_admin" }

type leadStageCount struct {
	Stage string
	Count int
}

type leadsData struct {
	Stage      string // "open" (default), one of leadStages, or "all"
	Source     string
	Query      string
	Leads      []crm.Lead
	Funnel     *crm.FunnelReport
	ByStage    []leadStageCount
	Stages     []string
	Movable    []string
	Sources    []string
	CanConvert bool
	Form       crm.Lead // the new-lead form, refilled after a validation error
}

// Leads is the pipeline screen.
func (h *Handler) Leads(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "leads")
	if !ok {
		return
	}
	h.renderLeads(w, r, s, leadsData{}, "", "")
}

func (h *Handler) renderLeads(w http.ResponseWriter, r *http.Request, s Session, ld leadsData, message, errMsg string) {
	d := h.page(s, "Leads", "leads")
	d.Message, d.Error = message, errMsg
	if d.Message == "" && r.URL.Query().Get("updated") != "" {
		d.Message = "Lead updated."
	}
	if h.leads == nil {
		d.Error = "Lead management is not configured on this deployment."
		h.render(w, "leads", d)
		return
	}
	q := r.URL.Query()
	ld.Stage, ld.Source, ld.Query = q.Get("stage"), q.Get("source"), strings.TrimSpace(q.Get("q"))
	if ld.Stage == "" {
		ld.Stage = "open"
	}
	if ld.Stage != "open" && ld.Stage != "all" && !inStrings(leadStages, ld.Stage) {
		d.Error = "Unknown stage " + ld.Stage + "; showing open leads."
		ld.Stage = "open"
	}
	if ld.Source != "" && !inStrings(leadSources, ld.Source) {
		d.Error = "Unknown source " + ld.Source + "; showing every source."
		ld.Source = ""
	}
	ld.Stages, ld.Movable, ld.Sources = leadStages, leadMovableStages, leadSources
	ld.CanConvert = canConvertLead(s.Role)

	var status, source *string
	if inStrings(leadStages, ld.Stage) {
		status = &ld.Stage
	}
	if ld.Source != "" {
		source = &ld.Source
	}
	all, err := h.leads.LeadList(r.Context(), status, source, nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list leads failed")
		d.Error = "Could not load leads."
	}
	lower := strings.ToLower(ld.Query)
	for _, l := range all {
		if ld.Stage == "open" && (l.Status == crm.StatusConverted || l.Status == crm.StatusLost) {
			continue
		}
		if lower != "" && !strings.Contains(strings.ToLower(l.FullName), lower) &&
			!strings.Contains(l.MobileNumber, ld.Query) && !strings.Contains(strings.ToLower(l.Email), lower) {
			continue
		}
		ld.Leads = append(ld.Leads, l)
	}
	if f, err := h.leads.LeadFunnelReport(r.Context(), nil); err == nil {
		ld.Funnel = f
		counts := map[string]int{}
		for _, st := range f.Stages {
			counts[st.Status] += st.LeadCount
		}
		for _, st := range leadStages {
			ld.ByStage = append(ld.ByStage, leadStageCount{Stage: st, Count: counts[st]})
		}
	}
	d.Data = ld
	h.render(w, "leads", d)
}

// leadCtx attributes the operation to the operator in the audit log and in
// migration 031's status-capture trigger.
func leadCtx(r *http.Request, s Session) context.Context {
	return middleware.WithSubject(r.Context(), s.Username)
}

// CreateLeadForm handles POST /staff/leads/new.
func (h *Handler) CreateLeadForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "leads")
	if !ok {
		return
	}
	if h.leads == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Lead management is not configured on this deployment.")
		return
	}
	l := crm.Lead{
		FullName:     strings.TrimSpace(r.PostFormValue("full_name")),
		MobileNumber: strings.TrimSpace(r.PostFormValue("mobile_number")),
		Email:        strings.TrimSpace(r.PostFormValue("email")),
		Source:       r.PostFormValue("source"),
		AssignedTo:   strings.TrimSpace(r.PostFormValue("assigned_to")),
		Notes:        strings.TrimSpace(r.PostFormValue("notes")),
	}
	created, err := h.leads.NewLead(leadCtx(r, s), l, nil)
	if reason := api.LeadInvalidReason(err); reason != "" {
		h.renderLeads(w, r, s, leadsData{Form: l}, "", "Lead not saved: "+reason+".")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("staffui: create lead failed")
		h.renderLeads(w, r, s, leadsData{Form: l}, "", "Could not save the lead.")
		return
	}
	h.renderLeads(w, r, s, leadsData{}, "Lead #"+strconv.Itoa(created.ID)+" added for "+created.FullName+".", "")
}

// UpdateLeadForm handles POST /staff/leads/{id}/update: stage, owner and notes.
func (h *Handler) UpdateLeadForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "leads")
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid lead id.")
		return
	}
	if h.leads == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Lead management is not configured on this deployment.")
		return
	}
	opt := func(name string) *string {
		if _, present := r.PostForm[name]; !present {
			return nil
		}
		v := strings.TrimSpace(r.PostFormValue(name))
		return &v
	}
	_ = r.ParseForm() //nolint:errcheck // PostFormValue below already parsed; this fills PostForm for opt
	status := opt("status")
	if status != nil && *status == "" {
		status = nil
	}
	lostReason := opt("lost_reason")
	if status != nil && *status == crm.StatusLost && (lostReason == nil || *lostReason == "") {
		h.renderLeads(w, r, s, leadsData{}, "", "Say why the lead was lost before closing it, so the funnel report can be trusted.")
		return
	}
	_, err = h.leads.MoveLead(leadCtx(r, s), id, status, opt("assigned_to"), opt("notes"), lostReason, nil)
	if err != nil {
		h.renderLeads(w, r, s, leadsData{}, "", leadErrorMessage(err, "Could not update the lead."))
		return
	}
	http.Redirect(w, r, "/staff/leads?"+leadReturnQuery(r).Encode(), http.StatusSeeOther) //nolint:gosec // fixed path; values encoded
}

func leadReturnQuery(r *http.Request) url.Values {
	q := url.Values{"updated": {"1"}}
	for _, k := range []string{"stage", "source", "q"} {
		if v := strings.TrimSpace(r.PostFormValue("return_" + k)); v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// leadErrorMessage words a lead-operation error for the operator.
func leadErrorMessage(err error, fallback string) string {
	switch {
	case api.LeadInvalidReason(err) != "":
		return api.LeadInvalidReason(err) + "."
	case errors.Is(err, api.ErrLeadNotFound):
		return "That lead no longer exists."
	case errors.Is(err, crm.ErrAlreadyConverted):
		return "This lead has already been converted, so no second subscriber was created."
	case errors.Is(err, crm.ErrLeadLost):
		return "This lead is marked lost. Move it back to an open stage before converting it."
	case errors.Is(err, api.ErrLeadDuplicateLogin):
		return "That CAF number or username is already in use. Choose another."
	case errors.Is(err, api.ErrLeadsUnavailable):
		return "Lead management is not configured on this deployment."
	default:
		log.Error().Err(err).Msg("staffui: lead operation failed")
		return fallback
	}
}

type convertData struct {
	Lead   *crm.Lead
	Plans  []Plan
	Form   api.ConvertLeadInput
	Result *api.ConvertLeadResult
}

// ConvertLeadPage handles GET /staff/leads/{id}/convert.
func (h *Handler) ConvertLeadPage(w http.ResponseWriter, r *http.Request) {
	s, ok := h.convertGate(w, r)
	if !ok {
		return
	}
	h.renderConvert(w, r, s, convertData{}, "")
}

func (h *Handler) convertGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "leads")
	if !ok {
		return Session{}, false
	}
	if !canConvertLead(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can convert a lead into a subscriber.")
		return Session{}, false
	}
	if h.leads == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Lead management is not configured on this deployment.")
		return Session{}, false
	}
	return s, true
}

func (h *Handler) renderConvert(w http.ResponseWriter, r *http.Request, s Session, cd convertData, errMsg string) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid lead id.")
		return
	}
	lead, err := h.leads.LeadByID(r.Context(), id, nil)
	if errors.Is(err, api.ErrLeadNotFound) {
		h.renderError(w, r, s, http.StatusNotFound, "No lead with that id.")
		return
	}
	if err != nil {
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not load that lead.")
		return
	}
	d := h.page(s, "Convert lead", "leads")
	d.Error = errMsg
	cd.Lead = lead
	if h.catalogue != nil {
		if plans, err := h.catalogue.ListPlans(r.Context()); err == nil {
			cd.Plans = plans
		}
	}
	d.Data = cd
	h.render(w, "lead_convert", d)
}

// ConvertLeadForm handles POST /staff/leads/{id}/convert.
func (h *Handler) ConvertLeadForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.convertGate(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid lead id.")
		return
	}
	planID, _ := strconv.Atoi(r.PostFormValue("plan_id")) //nolint:errcheck // 0 is rejected by validation
	in := api.ConvertLeadInput{
		CAFNumber:       strings.TrimSpace(r.PostFormValue("caf_number")),
		Username:        strings.TrimSpace(r.PostFormValue("username")),
		Password:        r.PostFormValue("password"),
		PlanID:          planID,
		RegisteredState: strings.TrimSpace(r.PostFormValue("registered_state")),
		Aadhaar:         strings.TrimSpace(r.PostFormValue("aadhaar")),
		PAN:             strings.TrimSpace(r.PostFormValue("pan")),
		CPESerial:       strings.TrimSpace(r.PostFormValue("cpe_serial")),
	}
	res, err := h.leads.ConvertLeadToSubscriber(leadCtx(r, s), id, in, nil)
	if err != nil {
		// The password is never echoed back into the form.
		in.Password, in.Aadhaar, in.PAN = "", "", ""
		h.renderConvert(w, r, s, convertData{Form: in}, leadErrorMessage(err, "The conversion did not complete. Nothing was created."))
		return
	}
	h.renderConvert(w, r, s, convertData{Result: res}, "")
}
