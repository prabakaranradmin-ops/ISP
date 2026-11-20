package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Multi-tier fair-use policies — FR-CAT-003.
//
// FUPPolicyManager is satisfied by *api.Handler, so the console validates and
// audits exactly as the API does.
type FUPPolicyManager interface {
	SaveFUPPolicy(ctx context.Context, p fup.Policy) (*fup.Policy, error)
	SetFUPPolicyActive(ctx context.Context, id int, active bool) error
	DeleteFUPPolicy(ctx context.Context, id int) error
	ListFUPPolicies(ctx context.Context, includeRetired bool) ([]fup.Policy, error)
	SetPlanFUPPolicy(ctx context.Context, planID int, policyID *int) error
}

var fupPolicyMessages = map[string]string{
	"saved":    "Fair-use policy saved. Every plan on it is re-evaluated within a minute, and subscribers move tier on the scanner's next pass.",
	"retired":  "Policy retired. It is out of the dropdowns; the plans already on it keep being evaluated against it.",
	"restored": "Policy restored and offered again.",
	"deleted":  "Fair-use policy deleted.",
	"attached": "Plan now uses that fair-use policy instead of its own single threshold.",
	"detached": "Plan is back to its own single threshold and throttle.",

	"duplicate": "Not saved: another active policy already has that name.",
	"inuse":     "Not deleted: plans still use this policy. Retire it instead — deleting it would change what those subscribers experience with nothing to explain why.",
	"missing":   "No fair-use policy with that id.",
	"failed":    "That did not go through. Nothing was changed; try again.",
}

var fupPolicySuccess = map[string]bool{
	"saved": true, "retired": true, "restored": true,
	"deleted": true, "attached": true, "detached": true,
}

// tierRow is one tier as the screen shows it.
type tierRow struct {
	fup.Tier
	// ThresholdGB is the threshold in the unit an operator sells in. The
	// database stores bytes, because that is what the scanner compares
	// against; asking for bytes on a form would make a typo a factor of a
	// billion.
	ThresholdGB string
}

type policyRow struct {
	fup.Policy
	Tiers []tierRow
	// FirstThresholdGB is where throttling starts, which is also the figure
	// the 80% warning is measured against.
	FirstThresholdGB string
}

type fupPolicyData struct {
	Rows           []policyRow
	Plans          []Plan
	IncludeRetired bool
	CanManage      bool
	MaxTiers       int
}

// canManageFUPPolicies: the same reach as Catalogue, and for the same
// reason. A fair-use policy decides the speed a plan delivers once a
// customer is over quota, which is part of what they were sold — the same
// commercial decision as a bandwidth profile (FR-CAT-002), and deliberately
// not the Routers tier that per-plan RADIUS attributes (FR-CAT-004) sit in.
func canManageFUPPolicies(role string) bool {
	return role == "isp_owner" || role == "billing_admin"
}

func (h *Handler) fupPolicyGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "fair-use")
	if !ok {
		return s, false
	}
	if h.fupPolicies == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Fair-use policies are not configured on this deployment.")
		return s, false
	}
	return s, true
}

func (h *Handler) requireFUPPolicyManager(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.fupPolicyGate(w, r)
	if !ok {
		return s, false
	}
	if !canManageFUPPolicies(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only the owner and billing staff can change fair-use policies.")
		return s, false
	}
	return s, true
}

// FairUse handles GET /staff/fair-use.
func (h *Handler) FairUse(w http.ResponseWriter, r *http.Request) {
	s, ok := h.fupPolicyGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Fair-use policies", "fair-use")
	q := r.URL.Query()

	includeRetired := q.Get("retired") == "true"
	policies, err := h.fupPolicies.ListFUPPolicies(r.Context(), includeRetired)
	if err != nil {
		h.renderError(w, r, s, http.StatusInternalServerError,
			"Could not read the fair-use policies.")
		return
	}

	pd := fupPolicyData{
		IncludeRetired: includeRetired,
		CanManage:      canManageFUPPolicies(s.Role),
		MaxTiers:       fup.MaxTiers,
	}
	for _, p := range policies {
		row := policyRow{Policy: p, FirstThresholdGB: gbLabel(p.FirstThreshold())}
		for _, t := range p.Tiers {
			row.Tiers = append(row.Tiers, tierRow{Tier: t, ThresholdGB: gbLabel(t.ThresholdBytes)})
		}
		pd.Rows = append(pd.Rows, row)
	}

	if h.catalogue != nil {
		if plans, err := h.catalogue.ListPlans(r.Context()); err == nil {
			pd.Plans = plans
		}
	}

	if code := q.Get("m"); code != "" {
		if m, ok := fupPolicyMessages[code]; ok {
			if fupPolicySuccess[code] {
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
	h.render(w, "fair_use", d)
}

// gbLabel renders bytes in the unit the form accepts and a tariff sheet
// uses, so the screen round-trips: what is shown can be typed straight back
// in. Whole numbers lose the decimal point, because "500" reads as a quota
// and "500.0" reads as a measurement.
func gbLabel(bytes int64) string {
	if bytes <= 0 {
		return ""
	}
	const gb = int64(1) << 30
	if bytes%gb == 0 {
		return strconv.FormatInt(bytes/gb, 10)
	}
	return strconv.FormatFloat(float64(bytes)/float64(gb), 'f', 2, 64)
}

// SaveFUPPolicyForm handles POST /staff/fair-use.
func (h *Handler) SaveFUPPolicyForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireFUPPolicyManager(w, r)
	if !ok {
		return
	}
	p, err := fupPolicyFromForm(r)
	if err != nil {
		h.redirectFairUse(w, r, "", err.Error())
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.fupPolicies.SaveFUPPolicy(ctx, p); err != nil {
		h.fupPolicyFailure(w, r, err)
		return
	}
	h.redirectFairUse(w, r, "saved", "")
}

// SetFUPPolicyActiveForm handles POST /staff/fair-use/{id}/active.
func (h *Handler) SetFUPPolicyActiveForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireFUPPolicyManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid policy id.")
		return
	}
	active := r.PostFormValue("active") == "true"
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.fupPolicies.SetFUPPolicyActive(ctx, id, active); err != nil {
		h.fupPolicyFailure(w, r, err)
		return
	}
	if active {
		h.redirectFairUse(w, r, "restored", "")
		return
	}
	h.redirectFairUse(w, r, "retired", "")
}

// DeleteFUPPolicyForm handles POST /staff/fair-use/{id}/delete.
func (h *Handler) DeleteFUPPolicyForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireFUPPolicyManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid policy id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.fupPolicies.DeleteFUPPolicy(ctx, id); err != nil {
		h.fupPolicyFailure(w, r, err)
		return
	}
	h.redirectFairUse(w, r, "deleted", "")
}

// AttachFUPPolicyForm handles POST /staff/fair-use/attach, pointing a plan
// at a policy or clearing it.
func (h *Handler) AttachFUPPolicyForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireFUPPolicyManager(w, r)
	if !ok {
		return
	}
	planID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("plan_id")))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid plan id.")
		return
	}

	var policyID *int
	if raw := strings.TrimSpace(r.PostFormValue("policy_id")); raw != "" && raw != "0" {
		id, err := strconv.Atoi(raw)
		if err != nil {
			h.redirectFairUse(w, r, "failed", "")
			return
		}
		policyID = &id
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.fupPolicies.SetPlanFUPPolicy(ctx, planID, policyID); err != nil {
		h.fupPolicyFailure(w, r, err)
		return
	}
	if policyID == nil {
		h.redirectFairUse(w, r, "detached", "")
		return
	}
	h.redirectFairUse(w, r, "attached", "")
}

// fupPolicyFromForm reads a policy and its tiers out of the posted form.
//
// Thresholds are entered in GB, the unit on a tariff sheet; the database
// stores bytes. Converting here rather than asking for bytes means a typo is
// a factor of ten, not a factor of a billion.
func fupPolicyFromForm(r *http.Request) (fup.Policy, error) {
	var p fup.Policy
	p.Name = r.PostFormValue("name")
	if raw := strings.TrimSpace(r.PostFormValue("policy_id")); raw != "" && raw != "0" {
		id, err := strconv.Atoi(raw)
		if err != nil {
			return p, fmt.Errorf("that is not a valid policy id")
		}
		p.ID = id
	}

	// The tiers arrive as parallel arrays: an empty threshold means the row
	// was left blank, which is how an operator removes a tier.
	thresholds := r.PostForm["threshold_gb"]
	rates := r.PostForm["throttle_rate"]
	for i := range thresholds {
		raw := strings.TrimSpace(thresholds[i])
		if raw == "" {
			continue
		}
		gb, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return p, fmt.Errorf("%q is not a number of GB", raw)
		}
		if gb <= 0 {
			return p, fmt.Errorf("a tier threshold must be above zero GB")
		}
		var rate string
		if i < len(rates) {
			rate = strings.TrimSpace(rates[i])
		}
		p.Tiers = append(p.Tiers, fup.Tier{
			// Rounded rather than truncated so 1.5 GB is not 1.49999.
			ThresholdBytes: int64(gb*float64(int64(1)<<30) + 0.5),
			ThrottleRate:   rate,
		})
	}

	// Normalised and validated here as well as in the store, so the
	// operator gets the sentence naming which tier is wrong rather than a
	// generic failure.
	p = p.Normalise()
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

func (h *Handler) redirectFairUse(w http.ResponseWriter, r *http.Request, code, problem string) {
	url := "/staff/fair-use"
	switch {
	case problem != "":
		url += "?err=" + neturl.QueryEscape(problem)
	case code != "":
		url += "?m=" + code
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (h *Handler) fupPolicyFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fup.ErrPolicyNameTaken):
		h.redirectFairUse(w, r, "duplicate", "")
	case errors.Is(err, fup.ErrPolicyInUse):
		h.redirectFairUse(w, r, "inuse", "")
	case strings.Contains(err.Error(), "not found"):
		h.redirectFairUse(w, r, "missing", "")
	default:
		// Anything else is unexpected: fupPolicyFromForm already ran the
		// same Normalise/Validate the store runs, so a validation message
		// cannot reach here, and showing the raw error would leak storage
		// detail an operator can do nothing with.
		h.redirectFairUse(w, r, "failed", "")
	}
}
