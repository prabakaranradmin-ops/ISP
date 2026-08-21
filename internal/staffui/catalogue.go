package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Plan is a tariff as the console shows and edits it.
type Plan struct {
	ID                int
	Name              string
	RateLimitString   string
	VolumeGB          int
	FUPThresholdBytes int64
	FUPThrottleString string
	Price             decimal.Decimal
	ValidityDays      int
	CreatedAt         time.Time
}

// GSTRate is one dated GST slab. Rates are never edited in place: billing
// reads the row in force on an invoice's date, so a change is a new row
// with a later effective_from and the old one stays readable for invoices
// already issued under it.
type GSTRate struct {
	ID            int
	CGSTRate      decimal.Decimal
	SGSTRate      decimal.Decimal
	IGSTRate      decimal.Decimal
	EffectiveFrom time.Time
}

// CatalogueStore reads and writes the tariff catalogue.
//
// Plans and GST rates had no interface anywhere before this: nothing but a
// migration created them, so a fresh install had an empty catalogue and no
// way to fill it that did not involve SQL against production. Subscribers
// cannot be created without a plan_id and invoices cannot be raised without
// a GST rate, which made "run psql by hand" a required step of going live.
type CatalogueStore interface {
	ListPlans(ctx context.Context) ([]Plan, error)
	CreatePlan(ctx context.Context, p Plan) (*Plan, error)
	ListGSTRates(ctx context.Context) ([]GSTRate, error)
	CreateGSTRate(ctx context.Context, r GSTRate) (*GSTRate, error)
}

// SubscriberCreator provisions a subscriber.
//
// Deliberately the API handler's own ProvisionSubscriber rather than a
// direct store call: that method hashes the password, encrypts Aadhaar/PAN
// and writes the audit entry. A console with its own creation path is how
// one of them ends up quietly skipping the encryption step - see the note
// on ProvisionSubscriber itself.
type SubscriberCreator interface {
	ProvisionSubscriber(ctx context.Context, req api.CreateSubscriberRequest) (*api.SubscriberRecord, error)
}

// ── Catalogue screen ─────────────────────────────────────────────────────────

type catalogueData struct {
	Plans    []Plan
	GSTRates []GSTRate
}

// Catalogue lists plans and GST rates, and hosts the forms that add them.
func (h *Handler) Catalogue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	h.renderCatalogue(w, r, s, "", "")
}

// renderCatalogue is shared by the screen itself and by the two create
// handlers, so a validation failure comes back on the same page with the
// existing catalogue still visible rather than on a dead-end error screen.
func (h *Handler) renderCatalogue(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Catalogue", "catalogue")
	d.Message, d.Error = message, errMsg

	if h.catalogue == nil {
		d.Error = "The tariff catalogue is not configured on this deployment."
		h.render(w, "catalogue", d)
		return
	}

	cd := catalogueData{}
	plans, err := h.catalogue.ListPlans(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list plans failed")
		d.Error = "Could not load plans."
		h.render(w, "catalogue", d)
		return
	}
	cd.Plans = plans

	rates, err := h.catalogue.ListGSTRates(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list gst rates failed")
		d.Error = "Could not load GST rates."
		h.render(w, "catalogue", d)
		return
	}
	cd.GSTRates = rates

	d.Data = cd
	h.render(w, "catalogue", d)
}

// CreatePlan adds a tariff.
func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	if h.catalogue == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The tariff catalogue is not configured.")
		return
	}

	p, err := planFromForm(r)
	if err != nil {
		h.renderCatalogue(w, r, s, "", err.Error())
		return
	}

	created, err := h.catalogue.CreatePlan(r.Context(), p)
	if err != nil {
		log.Error().Err(err).Str("plan", p.Name).Msg("staffui: create plan failed")
		h.renderCatalogue(w, r, s, "", "Could not save that plan.")
		return
	}
	h.renderCatalogue(w, r, s,
		fmt.Sprintf("Plan %q created with id %d.", created.Name, created.ID), "")
}

// planFromForm parses and validates the plan form.
//
// Every numeric field is checked rather than silently coerced: a plan whose
// price or validity landed as zero because a field was mistyped would bill
// every subscriber on it incorrectly, and nothing downstream would flag it.
func planFromForm(r *http.Request) (Plan, error) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		return Plan{}, fmt.Errorf("Plan name is required.")
	}

	// MikroTik rate-limit syntax, e.g. "100M/100M" (up/down). Checked for
	// shape here because it is passed to the NAS verbatim: a malformed
	// value is not rejected at RADIUS time, it simply fails to apply and
	// the subscriber silently gets no shaping at all.
	rate := strings.TrimSpace(r.PostFormValue("rate_limit_string"))
	if !strings.Contains(rate, "/") {
		return Plan{}, fmt.Errorf("Speed must be in MikroTik rate-limit form, for example 100M/100M.")
	}

	volumeGB, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("volume_gb")))
	if err != nil || volumeGB < 0 {
		return Plan{}, fmt.Errorf("Included data must be a whole number of GB.")
	}

	price, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("price")))
	if err != nil || price.IsNegative() {
		return Plan{}, fmt.Errorf("Price must be an amount such as 799.00.")
	}

	validityDays, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("validity_days")))
	if err != nil || validityDays <= 0 {
		return Plan{}, fmt.Errorf("Validity must be a whole number of days, at least 1.")
	}

	// FUP is optional: a blank threshold means unlimited, which the schema
	// stores as 0 rather than NULL.
	var fupBytes int64
	throttle := strings.TrimSpace(r.PostFormValue("fup_throttle_string"))
	if raw := strings.TrimSpace(r.PostFormValue("fup_threshold_gb")); raw != "" {
		fupGB, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || fupGB < 0 {
			return Plan{}, fmt.Errorf("FUP threshold must be a whole number of GB, or blank for unlimited.")
		}
		fupBytes = fupGB * 1024 * 1024 * 1024
		if fupBytes > 0 && throttle == "" {
			return Plan{}, fmt.Errorf("A FUP threshold needs a throttle speed, for example 10M/10M.")
		}
	}
	if fupBytes == 0 {
		throttle = ""
	}

	return Plan{
		Name:              name,
		RateLimitString:   rate,
		VolumeGB:          volumeGB,
		FUPThresholdBytes: fupBytes,
		FUPThrottleString: throttle,
		Price:             price,
		ValidityDays:      validityDays,
	}, nil
}

// CreateGSTRate adds a dated GST slab.
func (h *Handler) CreateGSTRate(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "catalogue")
	if !ok {
		return
	}
	if h.catalogue == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The tariff catalogue is not configured.")
		return
	}

	rate, err := gstRateFromForm(r)
	if err != nil {
		h.renderCatalogue(w, r, s, "", err.Error())
		return
	}

	created, err := h.catalogue.CreateGSTRate(r.Context(), rate)
	if err != nil {
		log.Error().Err(err).Msg("staffui: create gst rate failed")
		h.renderCatalogue(w, r, s, "", "Could not save that GST rate.")
		return
	}
	h.renderCatalogue(w, r, s,
		fmt.Sprintf("GST rate effective %s created.", created.EffectiveFrom.Format("02 Jan 2006")), "")
}

// gstRateFromForm parses and validates the GST form.
func gstRateFromForm(r *http.Request) (GSTRate, error) {
	parse := func(field, label string) (decimal.Decimal, error) {
		v, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue(field)))
		if err != nil || v.IsNegative() || v.GreaterThan(decimal.NewFromInt(100)) {
			return decimal.Zero, fmt.Errorf("%s must be a percentage between 0 and 100.", label)
		}
		return v, nil
	}

	cgst, err := parse("cgst_rate", "CGST")
	if err != nil {
		return GSTRate{}, err
	}
	sgst, err := parse("sgst_rate", "SGST")
	if err != nil {
		return GSTRate{}, err
	}
	igst, err := parse("igst_rate", "IGST")
	if err != nil {
		return GSTRate{}, err
	}

	// The intra-state pair must add up to the inter-state single rate:
	// they are the same tax split two ways, and billing picks one or the
	// other by the subscriber's state. A mismatch would make an invoice's
	// total depend on which side of a state line the customer sits.
	if !cgst.Add(sgst).Equal(igst) {
		return GSTRate{}, fmt.Errorf("CGST plus SGST must equal IGST (for example 9 + 9 = 18).")
	}

	effective := time.Now()
	if raw := strings.TrimSpace(r.PostFormValue("effective_from")); raw != "" {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return GSTRate{}, fmt.Errorf("Effective date must be in YYYY-MM-DD form.")
		}
		effective = t
	}

	return GSTRate{CGSTRate: cgst, SGSTRate: sgst, IGSTRate: igst, EffectiveFrom: effective}, nil
}
