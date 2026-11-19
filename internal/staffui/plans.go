package staffui

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// Plan editing, duplication, retirement and CSV export — FR-CAT-001 |
// migration 061. The catalogue screen itself is in catalogue.go.

// UpdatePlanForm handles POST /staff/catalogue/plans/{id}.
//
// Editing a price re-prices every subscriber on the plan from their next
// renewal: the renewal path reads plans.price when it bills, unlike a
// product attachment, which snapshots it. That is what a tariff change
// means, so the screen says how many subscribers an edit affects and the
// change is attributed and audited — but it is not refused, because
// correcting a mistyped tariff is exactly what this exists for.
func (h *Handler) UpdatePlanForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	if h.catalogue == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The tariff catalogue is not configured.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid plan id.")
		return
	}

	p, err := planFromForm(r)
	if err != nil {
		h.renderCatalogue(w, r, s, "", err.Error())
		return
	}
	p.ID = id

	ctx := middleware.WithSubject(r.Context(), s.Username)
	updated, err := h.catalogue.UpdatePlan(ctx, p, s.Username)
	switch {
	case errors.Is(err, ErrPlanNameTaken):
		h.renderCatalogue(w, r, s, "", "Another offered plan already uses that name.")
		return
	case err != nil:
		log.Error().Err(err).Int("plan_id", id).Msg("staffui: update plan failed")
		h.renderCatalogue(w, r, s, "", "Could not save that plan.")
		return
	}
	middleware.Audit(ctx, "plan.updated", updated.Name, map[string]any{
		"plan_id": id, "price": updated.Price.StringFixed(2),
		"rate_limit": updated.RateLimitString, "validity_days": updated.ValidityDays,
		"subscribers_affected": updated.Subscribers,
	})
	msg := fmt.Sprintf("Plan %q saved.", updated.Name)
	if updated.Subscribers > 0 {
		msg += fmt.Sprintf(" %d subscriber(s) are on it and will be billed the new price from their next renewal;"+
			" a speed change reaches the routers within a minute.", updated.Subscribers)
	}
	h.renderCatalogue(w, r, s, msg, "")
}

// DuplicatePlanForm handles POST /staff/catalogue/plans/{id}/duplicate.
//
// Creating a near-copy is how a tariff is varied — same speed, different
// validity, or the same plan at next year's price — and retyping six
// fields to do it is how one of them comes out wrong.
//
// The copy is created offered, with " (copy)" appended to the name, since
// uq_plan_name_active forbids two offered plans sharing one. The operator
// renames it; that is one edit instead of six.
func (h *Handler) DuplicatePlanForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	if h.catalogue == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The tariff catalogue is not configured.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid plan id.")
		return
	}

	plans, err := h.catalogue.ListPlans(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list plans for duplicate failed")
		h.renderCatalogue(w, r, s, "", "Could not read the plan to copy.")
		return
	}
	var source *Plan
	for i := range plans {
		if plans[i].ID == id {
			source = &plans[i]
			break
		}
	}
	if source == nil {
		h.renderCatalogue(w, r, s, "", "No plan with that id.")
		return
	}

	copyOf := *source
	copyOf.ID = 0
	copyOf.Subscribers = 0
	copyOf.Name = duplicateName(source.Name, plans)

	ctx := middleware.WithSubject(r.Context(), s.Username)
	created, err := h.catalogue.CreatePlan(ctx, copyOf)
	switch {
	case errors.Is(err, ErrPlanNameTaken):
		h.renderCatalogue(w, r, s, "",
			"Could not copy that plan: too many copies of it already exist. Rename one first.")
		return
	case err != nil:
		log.Error().Err(err).Int("plan_id", id).Msg("staffui: duplicate plan failed")
		h.renderCatalogue(w, r, s, "", "Could not copy that plan.")
		return
	}
	middleware.Audit(ctx, "plan.duplicated", created.Name, map[string]any{
		"plan_id": created.ID, "copied_from": id,
	})
	h.renderCatalogue(w, r, s,
		fmt.Sprintf("Plan %q created as a copy of %q. Rename it and set its price before selling it.",
			created.Name, source.Name), "")
}

// duplicateName is the source name with " (copy)" appended, or " (copy 2)"
// and upward when earlier copies already hold the name.
//
// Only offered plans are considered, because only they can collide:
// uq_plan_name_active is a partial index over active rows.
func duplicateName(base string, existing []Plan) string {
	taken := map[string]bool{}
	for _, p := range existing {
		if p.Active {
			taken[strings.ToLower(strings.TrimSpace(p.Name))] = true
		}
	}
	candidate := base + " (copy)"
	for n := 2; taken[strings.ToLower(candidate)]; n++ {
		candidate = fmt.Sprintf("%s (copy %d)", base, n)
		if n > 50 {
			// Give up rather than loop: fifty copies of one plan means
			// something else is wrong, and CreatePlan will refuse the
			// duplicate name with a message the operator can act on.
			return candidate
		}
	}
	return candidate
}

// SetPlanActiveForm handles POST /staff/catalogue/plans/{id}/active.
func (h *Handler) SetPlanActiveForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	if h.catalogue == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The tariff catalogue is not configured.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid plan id.")
		return
	}
	active := r.PostFormValue("active") == "true"

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.catalogue.SetPlanActive(ctx, id, active, s.Username); err != nil {
		if errors.Is(err, ErrPlanNameTaken) {
			h.renderCatalogue(w, r, s, "",
				"Could not offer that plan again: another offered plan already uses its name. Rename one of them first.")
			return
		}
		log.Error().Err(err).Int("plan_id", id).Bool("active", active).Msg("staffui: set plan active failed")
		h.renderCatalogue(w, r, s, "", "Could not change that plan.")
		return
	}
	action, msg := "plan.retired", "Plan retired. It is no longer offered for new assignment; the subscribers already on it keep it, and keep renewing at its price."
	if active {
		action, msg = "plan.reinstated", "Plan offered again."
	}
	middleware.Audit(ctx, action, strconv.Itoa(id), nil)
	h.renderCatalogue(w, r, s, msg, "")
}

// ExportPlansCSV handles GET /staff/catalogue/plans.csv.
//
// The whole catalogue, retired plans included, because the reason to
// export it is usually to reconcile what subscribers are being billed —
// and some of them are on plans no longer sold.
func (h *Handler) ExportPlansCSV(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	if h.catalogue == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The tariff catalogue is not configured.")
		return
	}
	plans, err := h.catalogue.ListPlans(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list plans for CSV failed")
		h.renderError(w, r, s, http.StatusBadGateway, "Could not read the catalogue.")
		return
	}

	// Headers before the body: once anything is written the status is
	// fixed, so a failure after this point cannot become an error page.
	filename := fmt.Sprintf("plans-%s.csv", time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")

	cw := csv.NewWriter(w)
	rows := [][]string{{
		"id", "name", "status", "speed", "included_gb", "fup_threshold_gb",
		"fup_throttle", "price_excl_gst", "validity_days", "subscribers",
		"created_at", "updated_by", "updated_at",
	}}
	for _, p := range plans {
		updatedAt := ""
		if p.UpdatedAt != nil {
			updatedAt = p.UpdatedAt.Format("2006-01-02 15:04")
		}
		rows = append(rows, []string{
			strconv.Itoa(p.ID), p.Name, p.StatusLabel(), p.RateLimitString,
			strconv.Itoa(p.VolumeGB), strconv.FormatInt(p.FUPThresholdGB(), 10),
			p.FUPThrottleString, p.Price.StringFixed(2), strconv.Itoa(p.ValidityDays),
			strconv.Itoa(p.Subscribers), p.CreatedAt.Format("2006-01-02 15:04"),
			p.UpdatedBy, updatedAt,
		})
	}
	if err := cw.WriteAll(rows); err != nil {
		// The response is already committed, so this can only be logged.
		log.Error().Err(err).Msg("staffui: writing the plan CSV failed partway")
		return
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Error().Err(err).Msg("staffui: flushing the plan CSV failed")
	}
}
