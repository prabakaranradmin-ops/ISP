package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/rs/zerolog/log"
)

// FranchiseStore backs the Franchises screen — the console counterpart to
// internal/api/franchises.go, whose own comment says it plainly: "the
// staff-console screen the requirement also asks for is separate work."
// This is that work.
//
// Redefined per package rather than sharing api.FranchiseQuerier, matching
// how every other cross-cutting store in this file (TaskEnqueuer, and NAS's
// use of the nas package's own types) is kept local to the package that
// calls it. Satisfied by *db.RevenueStore, the same instance already passed
// as HandlerDeps.Revenue.
type FranchiseStore interface {
	ListFranchises(ctx context.Context, franchiseID *int) ([]revenue.FranchiseRecord, error)
	CreateFranchise(ctx context.Context, req revenue.CreateFranchiseRequest) (*revenue.FranchiseRecord, error)
	GetFranchisePnL(ctx context.Context, franchiseID int, from, to *time.Time) (*revenue.FranchisePnL, error)
	ListConsolidatedPnL(ctx context.Context, from, to *time.Time) (*revenue.ConsolidatedPnL, error)
}

type franchiseListData struct {
	Franchises   []revenue.FranchiseRecord
	Consolidated *revenue.ConsolidatedPnL
}

type franchiseDetailData struct {
	PnL      *revenue.FranchisePnL
	From, To string
}

// Franchises lists onboarded partners, the consolidated P&L across all of
// them, and hosts the onboarding form. Owner-only: BO-004 ("onboard LCO/
// franchise partners without new systems") is an owner-level business
// outcome, and a consolidated P&L across every partner's commission is
// exactly the cross-partner financial view a single LCO must never see —
// see GetConsolidatedPnL's own reasoning in internal/api/franchises.go.
func (h *Handler) Franchises(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "franchise")
	if !ok {
		return
	}
	h.renderFranchises(w, r, s, "", "")
}

func (h *Handler) renderFranchises(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Franchises", "franchise")
	d.Message, d.Error = message, errMsg

	if h.franchises == nil {
		d.Error = "Franchise management is not configured on this deployment."
		h.render(w, "franchise", d)
		return
	}

	list, err := h.franchises.ListFranchises(r.Context(), nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list franchises failed")
		d.Error = "Could not load franchise partners."
		h.render(w, "franchise", d)
		return
	}
	consolidated, err := h.franchises.ListConsolidatedPnL(r.Context(), nil, nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: consolidated P&L failed")
		d.Error = "Could not load the consolidated P&L."
		h.render(w, "franchise", d)
		return
	}

	d.Data = franchiseListData{Franchises: list, Consolidated: consolidated}
	h.render(w, "franchise", d)
}

// CreateFranchiseForm onboards a partner from the console — the exact gap
// internal/api/franchises.go's CreateFranchise left open.
func (h *Handler) CreateFranchiseForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "franchise")
	if !ok {
		return
	}
	if h.franchises == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Franchise management is not configured.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	ownerName := strings.TrimSpace(r.PostFormValue("owner_name"))
	mobile := strings.TrimSpace(r.PostFormValue("mobile_number"))
	rate := strings.TrimSpace(r.PostFormValue("commission_rate_pct"))

	if name == "" || ownerName == "" {
		h.renderFranchises(w, r, s, "", "Name and owner name are required.")
		return
	}
	if mobile == "" {
		h.renderFranchises(w, r, s, "", "Mobile number is required.")
		return
	}

	created, err := h.franchises.CreateFranchise(r.Context(), revenue.CreateFranchiseRequest{
		Name:              name,
		OwnerName:         ownerName,
		MobileNumber:      mobile,
		CommissionRatePct: rate,
	})
	if err != nil {
		log.Error().Err(err).Str("name", name).Msg("staffui: create franchise failed")
		// Mirrors the API's own validation (commission_rate_pct must parse as
		// 0-100): the console's job is to say so in the same terms a form-filler
		// understands, not to leak "decimal.NewFromString: can't convert".
		h.renderFranchises(w, r, s, "", "Could not onboard that partner — check the commission rate is a number between 0 and 100.")
		return
	}
	h.renderFranchises(w, r, s, fmt.Sprintf("%s onboarded as partner #%d.", created.Name, created.ID), "")
}

// FranchiseDetail shows one partner's P&L, with an optional ?from=&to= date
// window matching internal/api/franchises.go's own YYYY-MM-DD filter.
func (h *Handler) FranchiseDetail(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "franchise")
	if !ok {
		return
	}
	d := h.page(s, "Franchise", "franchise")

	if h.franchises == nil {
		d.Error = "Franchise management is not configured on this deployment."
		h.render(w, "franchise_detail", d)
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusNotFound, "Invalid franchise id.")
		return
	}

	fromStr := strings.TrimSpace(r.URL.Query().Get("from"))
	toStr := strings.TrimSpace(r.URL.Query().Get("to"))
	from, to, err := parseFranchiseDateWindow(fromStr, toStr)
	if err != nil {
		d.Error = err.Error()
		h.render(w, "franchise_detail", d)
		return
	}

	pnl, err := h.franchises.GetFranchisePnL(r.Context(), id, from, to)
	if err != nil {
		log.Error().Err(err).Int("franchise_id", id).Msg("staffui: franchise P&L failed")
		d.Error = "Could not load this partner's P&L."
		h.render(w, "franchise_detail", d)
		return
	}
	if pnl == nil {
		h.renderError(w, r, s, http.StatusNotFound, "No franchise with that id.")
		return
	}

	d.Data = franchiseDetailData{PnL: pnl, From: fromStr, To: toStr}
	h.render(w, "franchise_detail", d)
}

// parseFranchiseDateWindow mirrors internal/api/franchises.go's own
// parseDateWindow (unexported there, so re-implemented rather than shared —
// same reasoning as every other per-package interface in this file).
func parseFranchiseDateWindow(fromStr, toStr string) (from, to *time.Time, err error) {
	const layout = "2006-01-02"
	if fromStr != "" {
		t, parseErr := time.Parse(layout, fromStr)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("From must be a date like 2026-08-01.")
		}
		from = &t
	}
	if toStr != "" {
		t, parseErr := time.Parse(layout, toStr)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("To must be a date like 2026-08-31.")
		}
		end := t.AddDate(0, 0, 1)
		to = &end
	}
	if from != nil && to != nil && to.Before(*from) {
		return nil, nil, fmt.Errorf("To must not be earlier than From.")
	}
	return from, to, nil
}
