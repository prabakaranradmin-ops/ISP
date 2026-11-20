package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Reusable bandwidth profiles — FR-CAT-002.
//
// BandwidthManager is satisfied by *api.Handler, so the console validates and
// audits exactly as the API does rather than reaching the store directly.
type BandwidthManager interface {
	CreateBandwidthProfile(ctx context.Context, in nas.BandwidthProfile) (*nas.BandwidthProfile, error)
	UpdateBandwidthProfile(ctx context.Context, id int, in nas.BandwidthProfile) (*nas.BandwidthProfile, error)
	SetBandwidthProfileActive(ctx context.Context, id int, active bool) error
	DeleteBandwidthProfile(ctx context.Context, id int) error
	ListBandwidthProfiles(ctx context.Context, includeRetired bool) ([]nas.BandwidthProfile, error)
}

// bandwidthMessages words each outcome carried back in the URL.
var bandwidthMessages = map[string]string{
	"created":  "Bandwidth profile added. Attach it to a plan from the catalogue to start shaping with it.",
	"updated":  "Bandwidth profile saved. Every plan on it re-shapes from each subscriber's next authentication, not immediately.",
	"retired":  "Profile retired. It is out of the dropdowns; the plans already on it are unchanged and still shaping from it.",
	"restored": "Profile restored and offered again.",
	"deleted":  "Bandwidth profile deleted.",

	"duplicate": "Not saved: another active profile already has that name.",
	"inuse":     "Not deleted: plans still use this profile. Retire it instead — deleting it would re-shape those subscribers with nothing to explain why.",
	"missing":   "No bandwidth profile with that id.",
	"failed":    "That did not go through. Nothing was changed; try again.",
}

var bandwidthSuccess = map[string]bool{
	"created": true, "updated": true, "retired": true, "restored": true, "deleted": true,
}

// profileRow is one profile as the screen shows it.
type profileRow struct {
	nas.BandwidthProfile

	// RateLimit is the attribute a MikroTik NAS actually receives. Shown
	// because the numbers in the form and the string on the wire are not
	// obviously the same thing, and this is the one place an operator can
	// check what their entries produce before a subscriber does.
	RateLimit string
	// RenderError is set when the stored row cannot be rendered at all,
	// which would mean authentication failing for every plan on it.
	RenderError string
}

// Mbps renders a stored bits-per-second value in the unit the form accepts
// and the tariff sheet uses, so the edit form round-trips: what the screen
// shows is what can be typed straight back in. Whole numbers lose the
// decimal point, because "50" reads as a speed and "50.0" reads as a
// measurement.
func mbpsLabel(bps int64) string {
	if bps == 0 {
		return ""
	}
	if bps%1_000_000 == 0 {
		return strconv.FormatInt(bps/1_000_000, 10)
	}
	return strconv.FormatFloat(float64(bps)/1_000_000, 'f', -1, 64)
}

func (r profileRow) MaxDownMbps() string { return mbpsLabel(r.MaxDownBPS) }
func (r profileRow) MaxUpMbps() string   { return mbpsLabel(r.MaxUpBPS) }
func (r profileRow) MinDownMbps() string { return mbpsLabel(r.MinDownBPS) }
func (r profileRow) MinUpMbps() string   { return mbpsLabel(r.MinUpBPS) }

func (r profileRow) BurstDownMbps() string { return mbpsLabel(r.BurstDownBPS) }
func (r profileRow) BurstUpMbps() string   { return mbpsLabel(r.BurstUpBPS) }
func (r profileRow) BurstThresholdDownMbps() string {
	return mbpsLabel(r.BurstThresholdDown)
}
func (r profileRow) BurstThresholdUpMbps() string { return mbpsLabel(r.BurstThresholdUp) }

type bandwidthData struct {
	Rows           []profileRow
	IncludeRetired bool
	CanManage      bool
}

// canManageBandwidth: the same reach as Catalogue, and for the same reason.
// A profile sets the speed the plans on it deliver, so changing one changes
// what every subscriber on those plans was sold — a tariff decision, not an
// operations task. It is deliberately not widened to noc_engineer: the
// numbers are network engineering, but the decision is commercial, and the
// NOC sees the result on the Routers and Sessions screens either way.
func canManageBandwidth(role string) bool {
	return role == "isp_owner" || role == "billing_admin"
}

func (h *Handler) bandwidthGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "bandwidth")
	if !ok {
		return s, false
	}
	if h.bandwidth == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Bandwidth profiles are not configured on this deployment.")
		return s, false
	}
	return s, true
}

func (h *Handler) requireBandwidthManager(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.bandwidthGate(w, r)
	if !ok {
		return s, false
	}
	if !canManageBandwidth(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only the owner and billing staff can change bandwidth profiles.")
		return s, false
	}
	return s, true
}

// Bandwidth handles GET /staff/bandwidth.
func (h *Handler) Bandwidth(w http.ResponseWriter, r *http.Request) {
	s, ok := h.bandwidthGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Bandwidth profiles", "bandwidth")
	q := r.URL.Query()

	includeRetired := q.Get("retired") == "true"
	profiles, err := h.bandwidth.ListBandwidthProfiles(r.Context(), includeRetired)
	if err != nil {
		h.renderError(w, r, s, http.StatusInternalServerError,
			"Could not read the bandwidth profiles.")
		return
	}

	rows := make([]profileRow, 0, len(profiles))
	for _, p := range profiles {
		row := profileRow{BandwidthProfile: p}
		if rl, err := p.MikrotikRateLimit(); err != nil {
			// A stored row that cannot be rendered means every plan on it
			// fails to authenticate, so it is surfaced on the row rather
			// than left to be discovered from the RADIUS log.
			row.RenderError = err.Error()
		} else {
			row.RateLimit = rl
		}
		rows = append(rows, row)
	}

	if code := q.Get("m"); code != "" {
		if m, ok := bandwidthMessages[code]; ok {
			if bandwidthSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	// A validation sentence is specific to what the operator typed, so it
	// is carried verbatim rather than mapped to a code.
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}

	d.Data = bandwidthData{
		Rows:           rows,
		IncludeRetired: includeRetired,
		CanManage:      canManageBandwidth(s.Role),
	}
	h.render(w, "bandwidth", d)
}

// CreateBandwidthForm handles POST /staff/bandwidth.
func (h *Handler) CreateBandwidthForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireBandwidthManager(w, r)
	if !ok {
		return
	}
	in, err := bandwidthFromForm(r)
	if err != nil {
		h.redirectBandwidth(w, r, "", err.Error())
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.bandwidth.CreateBandwidthProfile(ctx, in); err != nil {
		h.bandwidthFailure(w, r, err)
		return
	}
	h.redirectBandwidth(w, r, "created", "")
}

// UpdateBandwidthForm handles POST /staff/bandwidth/{id}.
func (h *Handler) UpdateBandwidthForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireBandwidthManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid profile id.")
		return
	}
	in, err := bandwidthFromForm(r)
	if err != nil {
		h.redirectBandwidth(w, r, "", err.Error())
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.bandwidth.UpdateBandwidthProfile(ctx, id, in); err != nil {
		h.bandwidthFailure(w, r, err)
		return
	}
	h.redirectBandwidth(w, r, "updated", "")
}

// SetBandwidthActiveForm handles POST /staff/bandwidth/{id}/active.
func (h *Handler) SetBandwidthActiveForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireBandwidthManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid profile id.")
		return
	}
	active := r.PostFormValue("active") == "true"
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.bandwidth.SetBandwidthProfileActive(ctx, id, active); err != nil {
		h.bandwidthFailure(w, r, err)
		return
	}
	if active {
		h.redirectBandwidth(w, r, "restored", "")
		return
	}
	h.redirectBandwidth(w, r, "retired", "")
}

// DeleteBandwidthForm handles POST /staff/bandwidth/{id}/delete.
func (h *Handler) DeleteBandwidthForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireBandwidthManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid profile id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.bandwidth.DeleteBandwidthProfile(ctx, id); err != nil {
		h.bandwidthFailure(w, r, err)
		return
	}
	h.redirectBandwidth(w, r, "deleted", "")
}

// bandwidthFromForm reads a profile out of the posted form.
//
// Rates are entered in Mbps, because that is the unit an operator sells in
// and the unit on the tariff sheet; the database stores bits per second.
// Doing the conversion here rather than asking for bits means a typo is a
// factor of ten, not a factor of a million.
func bandwidthFromForm(r *http.Request) (nas.BandwidthProfile, error) {
	mbps := func(field string) (int64, error) {
		raw := strings.TrimSpace(r.PostFormValue(field))
		if raw == "" {
			return 0, nil
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number of Mbps", raw)
		}
		if v < 0 {
			return 0, fmt.Errorf("a rate cannot be negative")
		}
		// Mbps to bps. Rounded rather than truncated so 1.5 is 1,500,000
		// and not 1,499,999.
		return int64(v*1_000_000 + 0.5), nil
	}

	var in nas.BandwidthProfile
	in.Name = r.PostFormValue("name")

	for _, f := range []struct {
		field string
		dest  *int64
	}{
		{"max_down", &in.MaxDownBPS}, {"max_up", &in.MaxUpBPS},
		{"min_down", &in.MinDownBPS}, {"min_up", &in.MinUpBPS},
		{"burst_down", &in.BurstDownBPS}, {"burst_up", &in.BurstUpBPS},
		{"burst_threshold_down", &in.BurstThresholdDown},
		{"burst_threshold_up", &in.BurstThresholdUp},
	} {
		v, err := mbps(f.field)
		if err != nil {
			return in, err
		}
		*f.dest = v
	}

	if raw := strings.TrimSpace(r.PostFormValue("burst_time_seconds")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return in, fmt.Errorf("the burst duration must be a whole number of seconds")
		}
		in.BurstTimeSeconds = v
	}
	if raw := strings.TrimSpace(r.PostFormValue("priority")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return in, fmt.Errorf("the queue priority must be a number from 1 to 8")
		}
		in.Priority = v
	}

	// Normalise and validate here as well as in the store, so the operator
	// gets the sentence rather than a generic failure.
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return in, err
	}
	return in, nil
}

func (h *Handler) redirectBandwidth(w http.ResponseWriter, r *http.Request, code, problem string) {
	url := "/staff/bandwidth"
	switch {
	case problem != "":
		// A validation sentence is specific to what was typed, so it is
		// carried verbatim rather than mapped to a code.
		url += "?err=" + neturl.QueryEscape(problem)
	case code != "":
		url += "?m=" + code
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// bandwidthFailure maps a store error to one of the worded outcomes.
func (h *Handler) bandwidthFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, nas.ErrProfileNameTaken):
		h.redirectBandwidth(w, r, "duplicate", "")
	case errors.Is(err, nas.ErrProfileInUse):
		h.redirectBandwidth(w, r, "inuse", "")
	case strings.Contains(err.Error(), "not found"):
		h.redirectBandwidth(w, r, "missing", "")
	default:
		// Anything else is unexpected: bandwidthFromForm already ran the
		// same Normalise/Validate the store runs, so a validation message
		// cannot reach here. Showing the raw error would leak storage
		// detail to an operator who can do nothing with it.
		h.redirectBandwidth(w, r, "failed", "")
	}
}
