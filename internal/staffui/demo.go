package staffui

import (
	"context"
	"fmt"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Demo data — lets the owner show a client a populated console (subscribers
// in every status, a couple of plans, a router, a ticket, an invoice)
// without typing a command. Before this, a fresh install had one owner
// account and nothing else to show.
//
// Every row this creates goes through the same paths a real operator would
// use (h.catalogue.CreatePlan, h.subscriberCreator.ProvisionSubscriber,
// h.nas.CreateNASDevice) and is only *afterwards* flagged is_demo via
// DemoStore, so seeded data is indistinguishable from real data except for
// that one flag — and removable by it.

// DemoStatus reports how much demo data currently exists.
type DemoStatus struct {
	Subscribers int
	Plans       int
	NASDevices  int
}

// Loaded reports whether any demo data currently exists.
func (s DemoStatus) Loaded() bool {
	return s.Subscribers > 0 || s.Plans > 0 || s.NASDevices > 0
}

// DemoStore tags, counts and removes demo data. Satisfied by *db.DemoStore.
type DemoStore interface {
	MarkSubscriberDemo(ctx context.Context, id int) error
	MarkPlanDemo(ctx context.Context, id int) error
	MarkNASDemo(ctx context.Context, id int) error
	Status(ctx context.Context) (DemoStatus, error)
	Remove(ctx context.Context) error
}

// TicketCreator raises a support ticket outside the subscriber portal flow —
// used only to seed one example ticket. Satisfied by *db.TicketStore.
type TicketCreator interface {
	CreateTicketAdmin(ctx context.Context, subscriberID int, category, description string, priority *string) (*api.TicketRecord, error)
}

// InvoiceSeeder raises one demo invoice through the real GST calculation
// path (billing.CalculateGstInvoiceFrom), so a seeded invoice is taxed
// exactly like a real one and the GSTR-1 export has something to show.
// Satisfied by *db.BillingStore.
type InvoiceSeeder interface {
	GetActiveGstRate(ctx context.Context) (billing.GstRate, error)
	CreateInvoice(ctx context.Context, inv billing.Invoice) (int, error)
}

type demoData struct {
	Status DemoStatus
}

// Demo shows current demo-data status with Load/Remove actions.
func (h *Handler) Demo(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "demo")
	if !ok {
		return
	}
	h.renderDemo(w, r, s, "", "")
}

func (h *Handler) renderDemo(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Demo Data", "demo")
	d.Message, d.Error = message, errMsg

	if h.demo == nil {
		d.Error = "Demo data is not configured on this deployment."
		h.render(w, "demo", d)
		return
	}
	status, err := h.demo.Status(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: demo status failed")
		d.Error = "Could not read demo data status."
		h.render(w, "demo", d)
		return
	}
	d.Data = demoData{Status: status}
	h.render(w, "demo", d)
}

// demoPlan is one tariff LoadDemoData creates.
type demoPlan struct {
	name, rateLimit string
	volumeGB        int
	price           int64
}

// demoSubscriber is one account LoadDemoData creates, and what should happen
// to it afterward (nothing, a status change, a ticket, an invoice) so the
// console has something to show in every state a client would ask about.
type demoSubscriber struct {
	username, caf, mobile string
	planName              string
	setStatus             string // "" = leave active
	withTicket            bool
	withInvoice           bool
}

var demoPlans = []demoPlan{
	{name: "Demo_Home_50M", rateLimit: "50M/50M", volumeGB: 1000, price: 599},
	{name: "Demo_Home_100M", rateLimit: "100M/100M", volumeGB: 2000, price: 999},
}

var demoSubscribers = []demoSubscriber{
	{username: "demo_priya", caf: "DEMO-CAF-0001", mobile: "+919800000001", planName: "Demo_Home_50M"},
	{username: "demo_arjun", caf: "DEMO-CAF-0002", mobile: "+919800000002", planName: "Demo_Home_100M", withInvoice: true},
	{username: "demo_lakshmi", caf: "DEMO-CAF-0003", mobile: "+919800000003", planName: "Demo_Home_50M", setStatus: "soft_suspended"},
	{username: "demo_ravi", caf: "DEMO-CAF-0004", mobile: "+919800000004", planName: "Demo_Home_100M", setStatus: "hard_suspended"},
	{username: "demo_meena", caf: "DEMO-CAF-0005", mobile: "+919800000005", planName: "Demo_Home_50M", withTicket: true},
}

// demoNASIP is in the RFC 5737 documentation range (TEST-NET-3) so it is
// obviously not a real router even before the row's is_demo flag is checked.
const demoNASIP = "203.0.113.10"

// demoState is the registered_state every seeded subscriber uses. Fixed
// intrastate against the configured GST home state so the one seeded
// invoice exercises the CGST+SGST branch, the common case, rather than the
// interstate one.
const demoState = "TN"

// demoSecret is 24 characters — comfortably over the 16-character RADIUS
// minimum — and, like the NAS IP, obviously a placeholder rather than
// anything meant to authenticate a real device.
const demoSecret = "demo-router-secret-do-not-use"

// LoadDemoData seeds a presentable console: two plans, five subscribers
// covering active/suspended states, one ticket, one invoice and one NAS
// device — all created through the real console/API paths and flagged
// is_demo afterward.
func (h *Handler) LoadDemoData(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "demo")
	if !ok {
		return
	}
	if h.demo == nil || h.catalogue == nil || h.subscriberCreator == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Demo data is not configured.")
		return
	}
	ctx := r.Context()

	status, err := h.demo.Status(ctx)
	if err != nil {
		log.Error().Err(err).Msg("staffui: demo status failed")
		h.renderDemo(w, r, s, "", "Could not check whether demo data is already loaded.")
		return
	}
	if status.Loaded() {
		h.renderDemo(w, r, s, "", "Demo data is already loaded. Remove it first if you want to reload it.")
		return
	}

	planIDs := make(map[string]int, len(demoPlans))
	for _, dp := range demoPlans {
		created, err := h.catalogue.CreatePlan(ctx, Plan{
			Name:            dp.name,
			RateLimitString: dp.rateLimit,
			VolumeGB:        dp.volumeGB,
			Price:           decimal.NewFromInt(dp.price),
			ValidityDays:    30,
		})
		if err != nil {
			log.Error().Err(err).Str("plan", dp.name).Msg("staffui: demo plan creation failed")
			h.renderDemo(w, r, s, "", "Could not create demo plans — check the server log.")
			return
		}
		if err := h.demo.MarkPlanDemo(ctx, created.ID); err != nil {
			log.Error().Err(err).Int("plan_id", created.ID).Msg("staffui: mark demo plan failed")
		}
		planIDs[dp.name] = created.ID
	}

	invoiceSkipped := false
	for _, ds := range demoSubscribers {
		created, err := h.subscriberCreator.ProvisionSubscriber(ctx, api.CreateSubscriberRequest{
			CAFNumber:       ds.caf,
			Username:        ds.username,
			Password:        "DemoPassword#1",
			MobileNumber:    ds.mobile,
			Email:           ds.username + "@example.invalid",
			PlanID:          planIDs[ds.planName],
			RegisteredState: demoState,
		})
		if err != nil {
			log.Error().Err(err).Str("username", ds.username).Msg("staffui: demo subscriber creation failed")
			h.renderDemo(w, r, s, "", "Could not create demo subscribers — check the server log.")
			return
		}
		if err := h.demo.MarkSubscriberDemo(ctx, created.ID); err != nil {
			log.Error().Err(err).Int("subscriber_id", created.ID).Msg("staffui: mark demo subscriber failed")
		}

		if ds.setStatus != "" {
			status := ds.setStatus
			if _, err := h.subscribers.UpdateSubscriber(ctx, created.ID, nil, &status, nil); err != nil {
				log.Error().Err(err).Str("username", ds.username).Msg("staffui: demo subscriber status change failed")
			}
		}

		if ds.withTicket && h.ticketCreator != nil {
			if _, err := h.ticketCreator.CreateTicketAdmin(ctx, created.ID,
				"connectivity", "Speed drops every evening around 8pm.", nil); err != nil {
				log.Error().Err(err).Str("username", ds.username).Msg("staffui: demo ticket creation failed")
			}
		}

		if ds.withInvoice && h.invoiceSeeder != nil {
			if err := h.seedDemoInvoice(ctx, created.ID, ds.planName, planIDs); err != nil {
				log.Warn().Err(err).Str("username", ds.username).Msg("staffui: demo invoice skipped")
				invoiceSkipped = true
			}
		}
	}

	if h.nas != nil && h.secretEncryptor != nil {
		encrypted, err := h.secretEncryptor.Encrypt(demoSecret)
		if err != nil {
			log.Error().Err(err).Msg("staffui: encrypt demo nas secret failed")
		} else {
			created, err := h.nas.CreateNASDevice(ctx, nas.NewNASDevice{
				IP: demoNASIP, Vendor: "mikrotik", Description: "Demo router (sample data)",
				SecretEncrypted: encrypted, KeyVersion: h.secretEncryptor.ActiveVersion(),
				CoAPort: defaultControlPort, PoDPort: defaultControlPort,
			})
			if err != nil {
				log.Error().Err(err).Msg("staffui: demo nas creation failed")
			} else if err := h.demo.MarkNASDemo(ctx, created.ID); err != nil {
				log.Error().Err(err).Int("nas_id", created.ID).Msg("staffui: mark demo nas failed")
			}
		}
	}

	msg := "Demo data loaded: 2 plans, 5 subscribers (active, soft-suspended, hard-suspended), " +
		"1 support ticket, 1 invoice and 1 sample router."
	if invoiceSkipped {
		msg = "Demo data loaded, except the sample invoice — configure a GST rate under Catalogue first, then remove and reload."
	}
	h.renderDemo(w, r, s, msg, "")
}

// seedDemoInvoice raises one real GST-calculated invoice for a demo
// subscriber. Best-effort: a deployment with no GST rate configured yet
// cannot raise any invoice, real or demo, so this is skipped rather than
// failing the whole load.
func (h *Handler) seedDemoInvoice(ctx context.Context, subscriberID int, planName string, planIDs map[string]int) error {
	rate, err := h.invoiceSeeder.GetActiveGstRate(ctx)
	if err != nil {
		return fmt.Errorf("no active GST rate: %w", err)
	}

	var volumeGB int
	var price int64
	for _, dp := range demoPlans {
		if dp.name == planName {
			volumeGB, price = dp.volumeGB, dp.price
			break
		}
	}

	inv := billing.CalculateGstInvoiceFrom(decimal.NewFromInt(price), demoState, h.gstSupplier.State, rate)
	inv.SubscriberID = subscriberID
	inv.GstRateID = rate.ID
	inv.GbIncluded = volumeGB
	// A believable partial-usage figure, not the full quota — an invoice
	// showing 100% usage every month reads as fabricated in a way a client
	// demo should avoid.
	inv.GbUsed = decimal.NewFromFloat(float64(volumeGB) * 0.62)

	_, err = h.invoiceSeeder.CreateInvoice(ctx, inv)
	return err
}

// RemoveDemoData deletes everything LoadDemoData created.
func (h *Handler) RemoveDemoData(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "demo")
	if !ok {
		return
	}
	if h.demo == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Demo data is not configured.")
		return
	}
	if err := h.demo.Remove(r.Context()); err != nil {
		log.Error().Err(err).Msg("staffui: remove demo data failed")
		h.renderDemo(w, r, s, "", "Could not remove demo data — check the server log.")
		return
	}
	h.renderDemo(w, r, s, "Demo data removed.", "")
}
