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
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// StaffAccount is a console operator.
//
// PasswordHash is left unpopulated ("") by any query that lists accounts
// for display (ListStaff) rather than authenticating one (GetStaffByUsername)
// — not fetched then withheld, simply never selected, so there is no value
// sitting in the struct for a template change to accidentally render.
type StaffAccount struct {
	ID           int
	Username     string
	PasswordHash string
	FullName     string
	Role         string
	LeaAccess    bool
	Active       bool
}

// StaffQuerier looks up console operators and, for isp_owner, manages them.
//
// CreateStaff/UpdateStaff/SetStaffPassword exist because until now the only
// way to create a staff account, change a password, or deactivate someone
// was a direct SQL statement against staff_users — the exact gap this
// interface closes. Deactivation rather than deletion matches the schema's
// own policy: migration 021 grants the app role no DELETE on this table at
// all ("accounts are deactivated, never removed").
type StaffQuerier interface {
	GetStaffByUsername(ctx context.Context, username string) (*StaffAccount, error)
	TouchStaffLogin(ctx context.Context, staffID int) error
	ListStaff(ctx context.Context) ([]StaffAccount, error)
	CreateStaff(ctx context.Context, username, fullName, passwordHash, role string, leaAccess bool) (*StaffAccount, error)
	// UpdateStaff applies a partial update; a nil field is left unchanged.
	UpdateStaff(ctx context.Context, id int, role *string, leaAccess, active *bool) (*StaffAccount, error)
	SetStaffPassword(ctx context.Context, id int, passwordHash string) error
}

// SubscriberQuerier serves the subscriber search and 360 view.
type SubscriberQuerier interface {
	GetSubscriberByID(ctx context.Context, id int) (*api.SubscriberRecord, error)
	GetSubscriberByUsername(ctx context.Context, username string) (*api.SubscriberRecord, error)
	// UpdateSubscriber is used only by the Demo Data seeder today, to put a
	// couple of seeded subscribers into a suspended state so the console has
	// something in every status to show a client. *db.APIStore already
	// implements this exact signature for internal/api, so exposing it here
	// costs no new store code.
	UpdateSubscriber(ctx context.Context, id int, planID *int, status *string) (*api.SubscriberRecord, error)
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
	GSTR1             GSTR1Store
	GSTSupplier       billing.Supplier
	SubscriberCreator SubscriberCreator
	// NAS and SecretEncryptor back the Routers screen (registering MikroTik
	// and other vendor devices). Both optional, same as every other store
	// here: a deployment with no encryption key configured still serves the
	// rest of the console, just not that one screen.
	NAS             NASStore
	SecretEncryptor api.SecretEncryptor
	// Demo, TicketCreator and InvoiceSeeder back the Demo Data panel — a
	// one-click way to populate a fresh install with presentable sample
	// data for a client walkthrough. All optional, same as everything else.
	Demo          DemoStore
	TicketCreator TicketCreator
	InvoiceSeeder InvoiceSeeder
	// SpeedOverride backs the owner's temporary speed-override card on a
	// subscriber's detail page.
	SpeedOverride SpeedOverrideController
	// BulkActions backs the Subscribers screen's multi-select toolbar.
	BulkActions BulkActionExecutor
	Tasks       TaskEnqueuer
	JWTSecret   string
	// Franchises backs the owner-only Franchises screen: onboarding partners
	// and viewing each one's (or all partners' consolidated) P&L. Same
	// *db.RevenueStore instance as Revenue above — FranchiseStore's methods
	// already live on that store, so no separate wiring is needed.
	Franchises FranchiseStore
	// Inventory backs the Inventory screen: CPE stock levels, device
	// issue/return, and (owner/billing only) device-type and purchase
	// management.
	Inventory InventoryStore
	// Reporting backs the Reports screen: plan mix, growth/churn, ticket
	// resolution and franchise collection performance.
	Reporting ReportingStore
	// FieldTasks and Approvals back the two halves of the Tasks screen.
	// ApprovalExecutor is the api.Handler instance itself (same pattern as
	// SubscriberCreator/BulkActions above): approving a request executes
	// real wallet/termination logic that lives behind stores only
	// internal/api holds, so the console delegates rather than reimplements.
	FieldTasks       FieldTaskStore
	Approvals        ApprovalStore
	ApprovalExecutor ApprovalExecutor
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
	gstr1             GSTR1Store
	gstSupplier       billing.Supplier
	subscriberCreator SubscriberCreator
	nas               NASStore
	secretEncryptor   api.SecretEncryptor
	demo              DemoStore
	ticketCreator     TicketCreator
	invoiceSeeder     InvoiceSeeder
	speedOverride     SpeedOverrideController
	bulkActions       BulkActionExecutor
	tasks             TaskEnqueuer
	jwtSecret         string
	franchises        FranchiseStore
	inventory         InventoryStore
	reporting         ReportingStore
	fieldTasks        FieldTaskStore
	approvals         ApprovalStore
	approvalExecutor  ApprovalExecutor
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
		gstr1:             deps.GSTR1,
		gstSupplier:       deps.GSTSupplier,
		subscriberCreator: deps.SubscriberCreator,
		nas:               deps.NAS,
		secretEncryptor:   deps.SecretEncryptor,
		demo:              deps.Demo,
		ticketCreator:     deps.TicketCreator,
		invoiceSeeder:     deps.InvoiceSeeder,
		speedOverride:     deps.SpeedOverride,
		bulkActions:       deps.BulkActions,
		tasks:             deps.Tasks,
		jwtSecret:         deps.JWTSecret,
		franchises:        deps.Franchises,
		inventory:         deps.Inventory,
		reporting:         deps.Reporting,
		fieldTasks:        deps.FieldTasks,
		approvals:         deps.Approvals,
		approvalExecutor:  deps.ApprovalExecutor,
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
	// Router hardware is network-ops territory, same reach as LEA Lookup
	// below: the owner and whoever configures the NAS estate, not billing
	// or the front desk.
	{"nas", "Routers", "/staff/nas",
		[]string{"isp_owner", "noc_engineer"}, false},
	{"lea", "LEA Lookup", "/staff/lea",
		[]string{"isp_owner", "noc_engineer"}, true},
	// Owner-only: seeding or removing demo data changes what every other
	// role sees, so no one else should be able to trigger it.
	{"demo", "Demo Data", "/staff/demo",
		[]string{"isp_owner"}, false},
	// Owner-only: who has console access at all, and at what level, is an
	// owner-level decision by definition.
	{"accounts", "Staff Accounts", "/staff/accounts",
		[]string{"isp_owner"}, false},
	// Owner-only, same reasoning as Revenue: onboarding a partner and seeing
	// the consolidated commission P&L across every partner is a business
	// decision, not an operations task. A franchise-scoped role (lco,
	// franchise_admin, franchise_staff) signing in here would see no
	// sections at all today — a restricted partner-facing view is tracked
	// separately (CRD §1.11 follow-up), not this screen widened.
	{"franchise", "Franchises", "/staff/franchise",
		[]string{"isp_owner"}, false},
	// Technicians handle hardware day to day (issue/return); NOC engineers
	// track it against the network estate; purchases and device-type
	// changes are procurement, gated inside the screen itself to
	// isp_owner/billing_admin rather than by hiding the whole section from
	// billing_admin (who has no other reason to see hardware otherwise, but
	// does need to record what was bought). CSR is excluded: they don't
	// touch physical hardware.
	{"inventory", "Inventory", "/staff/inventory",
		[]string{"isp_owner", "noc_engineer", "technician", "billing_admin"}, false},
	// Same reach as Revenue: growth, plan-mix and collection performance are
	// financial/business analytics, not day-to-day operations.
	{"reports", "Reports", "/staff/reports",
		[]string{"isp_owner", "billing_admin"}, false},
	// Field-task visibility is staff-wide (matches the API's own staffRead
	// gate on GET /field-tasks); dispatching/updating tasks and deciding
	// approvals are each gated further inside the screen itself, not by
	// hiding the whole section — a billing_admin needs to see the field
	// queue even though only csr/technician/owner can dispatch it, and vice
	// versa for approvals.
	{"tasks", "Tasks", "/staff/tasks",
		[]string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}, false},
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
	mux.Handle("POST /staff/subscribers/bulk", h.authed(h.requireCSRF(h.BulkAction)))
	mux.Handle("GET /staff/subscribers/{id}", h.authed(h.SubscriberDetail))
	mux.Handle("POST /staff/subscribers/{id}/speed-override", h.authed(h.requireCSRF(h.ApplySpeedOverride)))
	mux.Handle("POST /staff/subscribers/{id}/speed-override/clear", h.authed(h.requireCSRF(h.ClearSpeedOverride)))
	mux.Handle("GET /staff/catalogue", h.authed(h.Catalogue))
	mux.Handle("POST /staff/catalogue/plans", h.authed(h.requireCSRF(h.CreatePlan)))
	mux.Handle("POST /staff/catalogue/gst", h.authed(h.requireCSRF(h.CreateGSTRate)))
	mux.Handle("GET /staff/nas", h.authed(h.NAS))
	mux.Handle("POST /staff/nas/new", h.authed(h.requireCSRF(h.CreateNASDeviceForm)))
	mux.Handle("POST /staff/nas/{id}/update", h.authed(h.requireCSRF(h.UpdateNASDeviceForm)))
	mux.Handle("GET /staff/billing", h.authed(h.Billing))
	mux.Handle("GET /staff/billing/gstr1", h.authed(h.GSTR1Export))
	mux.Handle("GET /staff/tickets", h.authed(h.Tickets))
	mux.Handle("POST /staff/tickets/{id}/status", h.authed(h.requireCSRF(h.UpdateTicketStatus)))
	mux.Handle("GET /staff/revenue", h.authed(h.Revenue))
	mux.Handle("GET /staff/lea", h.authed(h.LEAPage))
	mux.Handle("POST /staff/lea", h.authed(h.requireCSRF(h.LEALookup)))
	mux.Handle("GET /staff/demo", h.authed(h.Demo))
	mux.Handle("POST /staff/demo/load", h.authed(h.requireCSRF(h.LoadDemoData)))
	mux.Handle("POST /staff/demo/remove", h.authed(h.requireCSRF(h.RemoveDemoData)))
	mux.Handle("GET /staff/franchise", h.authed(h.Franchises))
	mux.Handle("POST /staff/franchise/new", h.authed(h.requireCSRF(h.CreateFranchiseForm)))
	mux.Handle("GET /staff/franchise/{id}", h.authed(h.FranchiseDetail))
	mux.Handle("GET /staff/inventory", h.authed(h.Inventory))
	mux.Handle("POST /staff/inventory/types/new", h.authed(h.requireCSRF(h.CreateDeviceTypeForm)))
	mux.Handle("POST /staff/inventory/devices/new", h.authed(h.requireCSRF(h.CreateDeviceForm)))
	mux.Handle("POST /staff/inventory/devices/{serial}/issue", h.authed(h.requireCSRF(h.IssueDeviceForm)))
	mux.Handle("POST /staff/inventory/devices/{serial}/return", h.authed(h.requireCSRF(h.ReturnDeviceForm)))
	mux.Handle("POST /staff/inventory/purchases/new", h.authed(h.requireCSRF(h.RecordPurchaseForm)))
	mux.Handle("GET /staff/reports", h.authed(h.Reports))
	mux.Handle("GET /staff/tasks", h.authed(h.Tasks))
	mux.Handle("POST /staff/tasks/field/new", h.authed(h.requireCSRF(h.CreateFieldTaskForm)))
	mux.Handle("POST /staff/tasks/field/{id}/update", h.authed(h.requireCSRF(h.UpdateFieldTaskForm)))
	mux.Handle("POST /staff/tasks/approvals/{id}/approve", h.authed(h.requireCSRF(h.ApproveTaskRequest)))
	mux.Handle("POST /staff/tasks/approvals/{id}/reject", h.authed(h.requireCSRF(h.RejectTaskRequest)))
	mux.Handle("GET /staff/accounts", h.authed(h.StaffAccounts))
	mux.Handle("POST /staff/accounts/new", h.authed(h.requireCSRF(h.CreateStaffAccount)))
	mux.Handle("POST /staff/accounts/{id}/update", h.authed(h.requireCSRF(h.UpdateStaffAccount)))
	// Available to every signed-in role, not owner-only: anyone should be
	// able to change their own password.
	mux.Handle("GET /staff/change-password", h.authed(h.ChangePasswordPage))
	mux.Handle("POST /staff/change-password", h.authed(h.requireCSRF(h.ChangePassword)))
	mux.Handle("GET /staff/static/", http.StripPrefix("/staff/static/", staticHandler()))
}
