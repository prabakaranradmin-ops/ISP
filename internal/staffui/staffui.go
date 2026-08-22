// Package staffui serves the operations console: the screens the five staff
// personas in CRD §2 use to do their jobs.
//
// Until this existed, the only web interface was the subscriber portal, and
// every staff task — looking a subscriber up, disconnecting a session,
// crediting a wallet, working a ticket, running a law-enforcement lookup —
// could only be done by calling the JSON API with a hand-minted token. That
// is fine for a load test and unusable as a day job.
//
// The console is a client of the API's authorisation rules, not a second
// implementation of them: it issues the same JWT the API validates, carrying
// the same role and lea_access claims, so a screen can never grant reach the
// API would refuse. Each handler re-checks the caller's role rather than
// trusting that navigation hid the link.
//
// FR: FR-SEC-005 | CRD PER-001..005 | SecD §9.2, §9.3
package staffui

import (
	"context"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// StaffAccount is a console operator.
type StaffAccount struct {
	ID           int
	Username     string
	PasswordHash string
	FullName     string
	Role         string
	LeaAccess    bool
}

// StaffQuerier looks up console operators.
type StaffQuerier interface {
	GetStaffByUsername(ctx context.Context, username string) (*StaffAccount, error)
	TouchStaffLogin(ctx context.Context, staffID int) error
}

// SubscriberQuerier serves the subscriber search and 360 view.
type SubscriberQuerier interface {
	GetSubscriberByID(ctx context.Context, id int) (*api.SubscriberRecord, error)
	GetSubscriberByUsername(ctx context.Context, username string) (*api.SubscriberRecord, error)
}

// SessionReader reports whether a subscriber is online right now. Live session
// state lives in Redis, not the database, so the health record alone cannot
// answer it — and "is this customer online" is the first thing a CSR or NOC
// engineer asks.
type SessionReader interface {
	GetActiveSession(ctx context.Context, subscriberID int) (*portal.ActiveSession, error)
}

// HealthQuerier serves the diagnostic panel (FR-OBS-004).
type HealthQuerier interface {
	GetSubscriberWithMeta(ctx context.Context, subscriberID int) (*health.SubscriberRecord, error)
}

// BillingQuerier serves the billing screens.
type BillingQuerier interface {
	ListLedgerEntries(ctx context.Context, subscriberID int, from, to *time.Time, limit int) ([]api.LedgerEntry, error)
	GetSubscriberBalance(ctx context.Context, subscriberID int) (decimal.Decimal, error)
}

// TicketQuerier serves the support queue.
type TicketQuerier interface {
	ListTickets(ctx context.Context, subscriberID int) ([]portal.TicketEntry, error)
	UpdateTicketAdmin(ctx context.Context, ticketID int, status *string, assignedTo *int, priority *string) (*api.TicketRecord, error)
}

// TaskEnqueuer is the subset of *jobqueue.Client the console needs to trigger
// background work — currently just the ticket status-change notification
// (FR-NOTIF-007). Redefined per package rather than shared with internal/api,
// matching how internal/billing and internal/fup each keep their own
// Notifier-shaped interface.
type TaskEnqueuer interface {
	Enqueue(task *jobqueue.Task, opts ...jobqueue.Option) (*jobqueue.TaskInfo, error)
}

// LEAQuerier serves the law-enforcement lookup.
type LEAQuerier interface {
	LookupByPublicIP(ctx context.Context, publicIP string, port *int, at time.Time) (*api.LEAResult, error)
	RecordLEAAudit(ctx context.Context, entry api.LEAAuditEntry) error
}

// RevenueQuerier serves the owner dashboard.
type RevenueQuerier interface {
	GetUnbilledActiveSubscribers(ctx context.Context) (int, error)
	GetLedgerVariance(ctx context.Context) (decimal.Decimal, error)
	GetTotalWalletBalance(ctx context.Context) (decimal.Decimal, error)
	ListSubscribers(ctx context.Context, franchiseID *int) ([]revenue.SubscriberRow, error)
	// Collections (FR-REV-003): exposure per dunning stage, and what has
	// actually been collected month by month.
	GetCollectionsByDunningStage(ctx context.Context) ([]revenue.CollectionsStageRow, error)
	GetMonthlyRecovery(ctx context.Context, months int) ([]revenue.RecoveryMonth, error)
}

// HandlerDeps bundles the console's dependencies. Every one is optional: a
// screen whose store is absent reports itself unavailable rather than
// panicking, so a partially-configured deployment still serves the rest.
type HandlerDeps struct {
	Staff             StaffQuerier
	Subscribers       SubscriberQuerier
	Health            HealthQuerier
	Sessions          SessionReader
	Billing           BillingQuerier
	Tickets           TicketQuerier
	LEA               LEAQuerier
	Revenue           RevenueQuerier
	Catalogue         CatalogueStore
	SubscriberCreator SubscriberCreator
	Tasks             TaskEnqueuer
	JWTSecret         string
}

// Handler serves the console.
type Handler struct {
	staff             StaffQuerier
	subscribers       SubscriberQuerier
	health            HealthQuerier
	sessions          SessionReader
	billing           BillingQuerier
	tickets           TicketQuerier
	lea               LEAQuerier
	revenue           RevenueQuerier
	catalogue         CatalogueStore
	subscriberCreator SubscriberCreator
	tasks             TaskEnqueuer
	jwtSecret         string
}

// NewHandler constructs the console handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		staff:             deps.Staff,
		subscribers:       deps.Subscribers,
		health:            deps.Health,
		sessions:          deps.Sessions,
		billing:           deps.Billing,
		tickets:           deps.Tickets,
		lea:               deps.LEA,
		revenue:           deps.Revenue,
		catalogue:         deps.Catalogue,
		subscriberCreator: deps.SubscriberCreator,
		tasks:             deps.Tasks,
		jwtSecret:         deps.JWTSecret,
	}
}

// ── Role model ───────────────────────────────────────────────────────────────

// Section is one area of the console.
type Section struct {
	Key   string
	Label string
	Path  string
	Roles []string
	// NeedsLEA marks a section that also requires the lea_access claim, which
	// no role confers on its own.
	NeedsLEA bool
}

// sections is the console's navigation and its authorisation table at once.
//
// The role lists mirror internal/api/routes.go exactly. Any divergence would
// mean a screen offering something the API refuses (a dead end for the
// operator) or, worse, appearing to withhold something the API allows.
var sections = []Section{
	{"subscribers", "Subscribers", "/staff/subscribers",
		[]string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}, false},
	{"billing", "Billing", "/staff/billing",
		[]string{"isp_owner", "billing_admin", "csr"}, false},
	{"tickets", "Support", "/staff/tickets",
		[]string{"isp_owner", "csr", "technician"}, false},
	{"revenue", "Revenue", "/staff/revenue",
		[]string{"isp_owner"}, false},
	// Catalogue is owner/billing only: a tariff change re-prices every
	// subscriber on that plan and a GST change alters every invoice raised
	// after it, which is not reach a CSR or technician needs to do their
	// job.
	{"catalogue", "Catalogue", "/staff/catalogue",
		[]string{"isp_owner", "billing_admin"}, false},
	{"lea", "LEA Lookup", "/staff/lea",
		[]string{"isp_owner", "noc_engineer"}, true},
}

// AllowedSections returns the sections a given operator may use.
func AllowedSections(role string, leaAccess bool) []Section {
	var out []Section
	for _, s := range sections {
		if s.NeedsLEA && !leaAccess {
			continue
		}
		for _, r := range s.Roles {
			if r == role {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// canAccess reports whether a role (plus LEA claim) may use a section.
func canAccess(key, role string, leaAccess bool) bool {
	for _, s := range AllowedSections(role, leaAccess) {
		if s.Key == key {
			return true
		}
	}
	return false
}

// RegisterRoutes wires the console onto mux under /staff.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Login is the only unauthenticated route.
	mux.HandleFunc("GET /staff/login", h.LoginPage)
	mux.HandleFunc("POST /staff/login", h.Login)
	mux.HandleFunc("POST /staff/logout", h.Logout)

	mux.Handle("GET /staff/", h.authed(h.Home))
	mux.Handle("GET /staff/subscribers", h.authed(h.Subscribers))
	// Registered before the {id} pattern's sibling so "new" is never read
	// as a subscriber id. Go 1.22's mux scores a literal segment above a
	// wildcard regardless of registration order, so this is for the reader
	// rather than the router.
	mux.Handle("GET /staff/subscribers/new", h.authed(h.NewSubscriber))
	mux.Handle("POST /staff/subscribers/new", h.authed(h.requireCSRF(h.CreateSubscriber)))
	mux.Handle("GET /staff/subscribers/{id}", h.authed(h.SubscriberDetail))
	mux.Handle("GET /staff/catalogue", h.authed(h.Catalogue))
	mux.Handle("POST /staff/catalogue/plans", h.authed(h.requireCSRF(h.CreatePlan)))
	mux.Handle("POST /staff/catalogue/gst", h.authed(h.requireCSRF(h.CreateGSTRate)))
	mux.Handle("GET /staff/billing", h.authed(h.Billing))
	mux.Handle("GET /staff/tickets", h.authed(h.Tickets))
	mux.Handle("POST /staff/tickets/{id}/status", h.authed(h.requireCSRF(h.UpdateTicketStatus)))
	mux.Handle("GET /staff/revenue", h.authed(h.Revenue))
	mux.Handle("GET /staff/lea", h.authed(h.LEAPage))
	mux.Handle("POST /staff/lea", h.authed(h.requireCSRF(h.LEALookup)))
	mux.Handle("GET /staff/static/", http.StripPrefix("/staff/static/", staticHandler()))
}
