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
	// FranchiseID is non-nil only for the three franchise-scoped roles (lco,
	// franchise_admin, franchise_staff — migration 024's
	// chk_staff_franchise_binding requires exactly that pairing). nil for
	// every ISP-wide role.
	FranchiseID *int
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
	// franchiseID is required exactly when role is lco/franchise_admin/
	// franchise_staff, matching chk_staff_franchise_binding — enforced in
	// the handler before this is ever called, not left to the constraint to
	// reject as a raw 500.
	CreateStaff(ctx context.Context, username, fullName, passwordHash, role string, leaAccess bool, franchiseID *int) (*StaffAccount, error)
	// UpdateStaff applies a partial update; a nil field is left unchanged.
	// franchiseID rebinds the account to a different franchise when
	// non-nil; there is no way to clear it back to NULL from this
	// interface (an ISP-wide role never had one to clear, and moving a
	// franchise account to a different partner is the only real case,
	// covered by passing a new id).
	UpdateStaff(ctx context.Context, id int, role *string, leaAccess, active *bool, franchiseID *int) (*StaffAccount, error)
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
	UpdateSubscriber(ctx context.Context, id int, planID *int, status *string, planExpiry *time.Time) (*api.SubscriberRecord, error)
}

// CredentialManager backs the subscriber "view / reset password" feature.
// Satisfied by *api.Handler, the same instance as SubscriberCreator/BulkActions
// — the console delegates to the API's credential logic rather than reaching
// into the key store itself. Reveal returns (password, stored, err); stored is
// false when no recoverable copy was captured.
type CredentialManager interface {
	RevealSubscriberPassword(ctx context.Context, id int) (string, bool, error)
	ResetSubscriberPassword(ctx context.Context, id int, newPassword string) error
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
	// Credentials backs the subscriber detail page's view/reset password card.
	Credentials CredentialManager
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
	// Procurement backs the Procurement screen: general purchase orders,
	// distinct from Inventory's CPE-restocking purchases.
	Procurement ProcurementStore
	// Vendors backs the Vendors screen and the supplier dropdowns on
	// Inventory and Procurement — the master both of those write through
	// (FR-INV-004).
	Vendors VendorManager
	// PortalBranding backs the captive-portal branding screen (FR-HSP-004).
	PortalBranding PortalBrandingManager
	// Traffic backs the interface traffic screens under Network (FR-NET-006).
	Traffic TrafficStore
	// GeneralLedger backs the Ledger screen (CRD-EXP-006 Phase 1): chart of
	// accounts, manual journal entries, trial balance, P&L, balance sheet.
	GeneralLedger GeneralLedgerStore
	// VoucherCommissions backs the voucher-settlement half of the
	// Franchise/My P&L screens (CRD-EXP-010).
	VoucherCommissions VoucherCommissionStore
	// NetworkHealth backs the Network Map screen (CRD-EXP-008).
	NetworkHealth NetworkHealthStore
	// Console backs the home dashboard and the subscriber directory's
	// filters and multi-field search. Optional: without it the directory
	// falls back to Revenue's plain username/id list.
	Console ConsoleStore
	// Disconnector backs the Sessions screen's Disconnect button. The API
	// handler itself, same as SubscriberCreator/BulkActions.
	Disconnector SessionDisconnector
	// InvoicePDF backs the Invoices screen's PDF link. The API handler itself.
	InvoicePDF InvoicePDFRenderer
	// Leads backs the Leads screen. The API handler itself.
	Leads LeadManager
	// CPE backs the CPE devices screen's actions. The API handler itself.
	CPE CPEController
	// Announcer and Audience back the Announcements screen: the API handler
	// for compose/send, and the console store for the draft reach estimate.
	Announcer AnnouncementManager
	Audience  AudienceCounter
	// CreditNotes files credit-note approval requests from the Invoices
	// screen. The API handler itself.
	CreditNotes CreditNoteRequester
	// Payments records counter payments and cheque bounces. The API handler.
	Payments PaymentManager
	// Dunning sets grace extensions and the dunning schedule
	// (FR-BIL-016/017). The API handler.
	Dunning DunningManager
	// GatewayLog reads the payment-gateway order and callback log
	// (FR-PAY-001).
	GatewayLog GatewayLogReader
	// AuthLog reads the RADIUS authentication attempt log (FR-NET-004).
	AuthLog AuthLogReader
	// Audit reads the append-only staff audit trail (FR-OBS-006).
	Audit AuditReader
	// IPPools manages IP pools and static addresses (FR-NET-005).
	IPPools IPPoolManager
	// Bandwidth manages the reusable bandwidth profiles (FR-CAT-002).
	Bandwidth BandwidthManager
	// Groups manages named subscriber groups (FR-GRP-001).
	Groups GroupManager
	// ReplyAttrs manages per-plan RADIUS reply attributes (FR-CAT-004).
	ReplyAttrs ReplyAttrManager
	// FUPPolicies manages multi-tier fair-use policies (FR-CAT-003).
	FUPPolicies FUPPolicyManager
	// Collection manages field payment collection rounds (FR-COL-001).
	Collection CollectionManager
	// Documents reads archived documents and verifies them (FR-DOC-003).
	Documents DocumentManager
	// CAF prints a subscriber's application form (FR-DOC-002).
	CAF CAFPrinter
	// PlanChanges schedules plan changes for a future renewal (FR-BIL-023).
	PlanChanges PlanChangeManager
	// Products manages the product catalogue and the charges attached to
	// subscribers (FR-BIL-022).
	Products ProductManager
	// Proformas issues, converts and cancels proforma invoices
	// (FR-BIL-018). The API handler.
	Proformas ProformaManager
	// Diagnostics runs the billing consistency checks (FR-BIL-019). The
	// API handler.
	Diagnostics DiagnosticsRunner
	// BillingRuns reads the recurring-billing run history (FR-BIL-020). The
	// API handler.
	BillingRuns BillingRunReader
	// Receipts prints payment receipts (FR-BIL-021). The API handler.
	Receipts ReceiptRenderer
}

// Handler serves the console.
type Handler struct {
	staff              StaffQuerier
	subscribers        SubscriberQuerier
	health             HealthQuerier
	sessions           SessionReader
	billing            BillingQuerier
	tickets            TicketQuerier
	lea                LEAQuerier
	revenue            RevenueQuerier
	catalogue          CatalogueStore
	gstr1              GSTR1Store
	gstSupplier        billing.Supplier
	subscriberCreator  SubscriberCreator
	nas                NASStore
	secretEncryptor    api.SecretEncryptor
	demo               DemoStore
	ticketCreator      TicketCreator
	invoiceSeeder      InvoiceSeeder
	speedOverride      SpeedOverrideController
	bulkActions        BulkActionExecutor
	credentials        CredentialManager
	tasks              TaskEnqueuer
	jwtSecret          string
	franchises         FranchiseStore
	inventory          InventoryStore
	reporting          ReportingStore
	fieldTasks         FieldTaskStore
	approvals          ApprovalStore
	approvalExecutor   ApprovalExecutor
	procurement        ProcurementStore
	vendors            VendorManager
	portalBranding     PortalBrandingManager
	traffic            TrafficStore
	generalLedger      GeneralLedgerStore
	voucherCommissions VoucherCommissionStore
	networkHealth      NetworkHealthStore
	console            ConsoleStore
	disconnector       SessionDisconnector
	invoicePDF         InvoicePDFRenderer
	leads              LeadManager
	cpe                CPEController
	announcer          AnnouncementManager
	audience           AudienceCounter
	creditNotes        CreditNoteRequester
	payments           PaymentManager
	dunning            DunningManager
	proformas          ProformaManager
	products           ProductManager
	planChanges        PlanChangeManager
	ipPools            IPPoolManager
	bandwidth          BandwidthManager
	groups             GroupManager
	replyAttrs         ReplyAttrManager
	fupPolicies        FUPPolicyManager
	collection         CollectionManager
	documents          DocumentManager
	caf                CAFPrinter
	audit              AuditReader
	authLog            AuthLogReader
	gatewayLog         GatewayLogReader
	diagnostics        DiagnosticsRunner
	billingRuns        BillingRunReader
	receipts           ReceiptRenderer
}

// NewHandler constructs the console handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		staff:              deps.Staff,
		subscribers:        deps.Subscribers,
		health:             deps.Health,
		sessions:           deps.Sessions,
		billing:            deps.Billing,
		tickets:            deps.Tickets,
		lea:                deps.LEA,
		revenue:            deps.Revenue,
		catalogue:          deps.Catalogue,
		gstr1:              deps.GSTR1,
		gstSupplier:        deps.GSTSupplier,
		subscriberCreator:  deps.SubscriberCreator,
		nas:                deps.NAS,
		secretEncryptor:    deps.SecretEncryptor,
		demo:               deps.Demo,
		ticketCreator:      deps.TicketCreator,
		invoiceSeeder:      deps.InvoiceSeeder,
		speedOverride:      deps.SpeedOverride,
		bulkActions:        deps.BulkActions,
		credentials:        deps.Credentials,
		tasks:              deps.Tasks,
		jwtSecret:          deps.JWTSecret,
		franchises:         deps.Franchises,
		inventory:          deps.Inventory,
		reporting:          deps.Reporting,
		fieldTasks:         deps.FieldTasks,
		approvals:          deps.Approvals,
		approvalExecutor:   deps.ApprovalExecutor,
		procurement:        deps.Procurement,
		vendors:            deps.Vendors,
		portalBranding:     deps.PortalBranding,
		traffic:            deps.Traffic,
		generalLedger:      deps.GeneralLedger,
		voucherCommissions: deps.VoucherCommissions,
		networkHealth:      deps.NetworkHealth,
		console:            deps.Console,
		disconnector:       deps.Disconnector,
		invoicePDF:         deps.InvoicePDF,
		leads:              deps.Leads,
		cpe:                deps.CPE,
		announcer:          deps.Announcer,
		audience:           deps.Audience,
		creditNotes:        deps.CreditNotes,
		payments:           deps.Payments,
		dunning:            deps.Dunning,
		proformas:          deps.Proformas,
		products:           deps.Products,
		planChanges:        deps.PlanChanges,
		ipPools:            deps.IPPools,
		bandwidth:          deps.Bandwidth,
		groups:             deps.Groups,
		replyAttrs:         deps.ReplyAttrs,
		fupPolicies:        deps.FUPPolicies,
		collection:         deps.Collection,
		documents:          deps.Documents,
		caf:                deps.CAF,
		audit:              deps.Audit,
		authLog:            deps.AuthLog,
		gatewayLog:         deps.GatewayLog,
		diagnostics:        deps.Diagnostics,
		billingRuns:        deps.BillingRuns,
		receipts:           deps.Receipts,
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
// sections is the console's navigation, grouped into menus with a dashboard
// landing screen — FR-CON-001. The individual screens it lists carry their
// own requirements: FR-CON-002 (dashboard), FR-CON-003 (subscriber
// directory), FR-CON-004 (Sessions) and FR-CON-007 (lead pipeline).
//
// Cited here because until 2026-11-23 none of those five ids appeared
// anywhere in the code, so a traceability sweep counted them unimplemented
// while they were on screen the whole time. They are listed on this
// declaration rather than scattered because this slice is what makes a
// screen reachable at all.
var sections = []Section{
	// First on purpose: Home redirects to the first allowed section, so
	// every ISP-wide role lands here. Same reach as Subscribers, since every
	// figure on it is a count of subscribers; the ticket figures are further
	// gated inside the screen to the roles that can open Support.
	{"dashboard", "Dashboard", "/staff/dashboard",
		[]string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}, false},
	{"subscribers", "Subscribers", "/staff/subscribers",
		[]string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}, false},
	// The API's leadWrite roles, minus the franchise-scoped ones: this
	// screen is ISP-wide (it passes a nil scope), and a partner's view of
	// their own pipeline belongs with My Subscribers, not here. Converting
	// is narrower still and gated inside the screen (canConvertLead).
	{"leads", "Leads", "/staff/leads",
		[]string{"isp_owner", "billing_admin", "csr", "technician"}, false},
	{"billing", "Billing", "/staff/billing",
		[]string{"isp_owner", "billing_admin", "csr"}, false},
	{"tickets", "Support", "/staff/tickets",
		[]string{"isp_owner", "csr", "technician"}, false},
	// Same reach as the API's billingOrCSR gate on GET /invoices/{id} and
	// its PDF: a CSR answers "what did I pay, send me the bill" calls.
	{"invoices", "Invoices & Payments", "/staff/invoices",
		[]string{"isp_owner", "billing_admin", "csr"}, false},
	{"revenue", "Revenue", "/staff/revenue",
		[]string{"isp_owner"}, false},
	// Catalogue is owner/billing only: a tariff change re-prices every
	// subscriber on that plan and a GST change alters every invoice raised
	// after it, which is not reach a CSR or technician needs to do their
	// job.
	{"catalogue", "Catalogue", "/staff/catalogue",
		[]string{"isp_owner", "billing_admin"}, false},
	// Products & Charges (FR-BIL-022) sits beside Catalogue for the same
	// reason and with the same reach: adding a router rental or changing
	// what installation costs re-prices what every subscriber attached to
	// it is billed from their next renewal. Reading is widened to CSR
	// inside the screen — a CSR fielding "why is my bill higher this
	// month" needs to see the charges, not change them — matching the
	// API's billingOrCSR gate on GET /products.
	{"products", "Products & Charges", "/staff/products",
		[]string{"isp_owner", "billing_admin", "csr"}, false},
	// Bandwidth profiles (FR-CAT-002) sit with Catalogue and share its
	// reach exactly. A profile sets the speed the plans on it deliver, so
	// editing one changes what every subscriber on those plans was sold.
	// Not widened to noc_engineer: the fields are network engineering, but
	// the decision is commercial, and the NOC sees the rendered result on
	// the Routers and Sessions screens either way.
	{"bandwidth", "Bandwidth Profiles", "/staff/bandwidth",
		[]string{"isp_owner", "billing_admin"}, false},
	// Subscriber groups (FR-GRP-001) reach wider than the catalogue screens
	// on purpose. A group is a label an operator puts on their own customers
	// — a locality, an apartment block, the twenty on one feeder cable — and
	// the people who know which customers belong together are the front desk
	// and the NOC, not whoever sets tariffs. Nothing here changes a price or
	// a speed; what it changes is who an announcement reaches, so every
	// change is audited. Kept in step with canManageGroups by
	// TestFR_GRP_001_TheSectionAndTheHandlerAgreeOnWhoCanManageGroups.
	{"groups", "Subscriber Groups", "/staff/groups",
		[]string{"isp_owner", "billing_admin", "csr", "noc_engineer"}, false},
	// Fair-use policies (FR-CAT-003) sit with Bandwidth Profiles and share
	// its reach: a policy decides the speed a plan delivers once a customer
	// is over quota, which is part of what they were sold. Deliberately not
	// the Routers tier that RADIUS Attributes (FR-CAT-004) sits in — those
	// carry no price, and this does.
	{"fair-use", "Fair-Use Policies", "/staff/fair-use",
		[]string{"isp_owner", "billing_admin"}, false},
	// Router hardware is network-ops territory, same reach as LEA Lookup
	// below: the owner and whoever configures the NAS estate, not billing
	// or the front desk.
	// Viewing is every ISP-wide role, matching the API's staffRead gate on
	// GET /sessions/{id}/active: "is this customer online, on what IP" is the
	// first question at the front desk as much as in the NOC. Disconnecting
	// is narrower and gated inside the screen (canDisconnect).
	{"sessions", "Sessions", "/staff/sessions",
		[]string{"isp_owner", "noc_engineer", "billing_admin", "csr", "technician"}, false},
	// The roles the API lets queue TR-069 actions (nocOrTech). Viewing is
	// the same set: the screen exists to act on equipment.
	{"cpe", "CPE Devices", "/staff/cpe",
		[]string{"isp_owner", "noc_engineer", "technician"}, false},
	{"nas", "Routers", "/staff/nas",
		[]string{"isp_owner", "noc_engineer"}, false},
	// IP pools (FR-NET-005) sit with Routers and share its reach: a pool
	// describes the network's address space, and getting it wrong hands
	// two customers one address. Assigning a subscriber an address is
	// gated separately inside the screen (canAssignStaticIP), because the
	// charge that follows it is a billing act.
	{"ip-pools", "IP Pools", "/staff/ip-pools",
		[]string{"isp_owner", "noc_engineer"}, false},
	// Authentication log (FR-NET-004). NOC and the front desk both:
	// "why can't this customer get online" is the single most common
	// support call, and the answer is in here. Deliberately wider than
	// IP Pools above — reading why an authentication failed is diagnosis,
	// not reach over the network's address space.
	{"auth-log", "Auth Log", "/staff/auth-log",
		[]string{"isp_owner", "noc_engineer", "csr", "technician"}, false},
	// Per-plan RADIUS reply attributes (FR-CAT-004). Same reach as Routers
	// and IP Pools, not as Catalogue: these are RADIUS attributes — an
	// address list, a NAS-side pool, a session timeout — and they carry no
	// price. That is the distinction from a bandwidth profile (FR-CAT-002),
	// which sets the speed a plan delivers and so reaches billing staff:
	// getting an address list wrong breaks a firewall rule, while getting a
	// speed wrong changes what a customer was sold.
	{"plan-attributes", "RADIUS Attributes", "/staff/plan-attributes",
		[]string{"isp_owner", "noc_engineer"}, false},
	// Same reach as Routers: a cross-device view of the same estate, not a
	// new kind of access.
	{"network", "Network Map", "/staff/network",
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
	// Archived documents (FR-DOC-003). Billing and the owner: the archive
	// holds invoice PDFs and KYC scans, which is the reach those already
	// have. Deliberately not CSR or technician — a KYC scan is a
	// customer's identity documents, and the DPDP Act's purpose-limitation
	// principle is not satisfied by "the front desk might want to look".
	{"documents", "Documents", "/staff/documents",
		[]string{"isp_owner", "billing_admin"}, false},
	// Owner-only, as FR-OBS-006 specifies: the trail names every action
	// every operator has taken, which is a record about the staff as much
	// as about the subscribers. Append-only in the database (migration
	// 062) rather than merely read-only here, so the owner cannot quietly
	// edit it either.
	{"audit", "Audit Trail", "/staff/audit",
		[]string{"isp_owner"}, false},
	// Owner-only, same reasoning as Revenue: onboarding a partner and seeing
	// the consolidated commission P&L across every partner is a business
	// decision, not an operations task. The franchise-scoped roles (lco,
	// franchise_admin, franchise_staff) get their own restricted sections
	// below instead — never this one widened, since it can name any
	// partner's id.
	{"franchise", "Franchises", "/staff/franchise",
		[]string{"isp_owner"}, false},
	// A franchise partner's own restricted view: their own subscriber
	// roster and their own P&L, never another partner's — scoped from the
	// session's FranchiseID (set from the JWT's franchise_id claim), never
	// from a URL parameter, the same rule internal/api/franchises.go's
	// callerFranchiseScope already enforces for the API.
	{"my-subscribers", "My Subscribers", "/staff/my-subscribers",
		[]string{"lco", "franchise_admin", "franchise_staff"}, false},
	{"my-pnl", "My P&L", "/staff/my-pnl",
		[]string{"lco", "franchise_admin", "franchise_staff"}, false},
	// Same reach as Inventory's own purchase/device-type gating: a spend
	// decision is procurement, whatever it is being spent on.
	{"procurement", "Procurement", "/staff/procurement",
		[]string{"isp_owner", "billing_admin"}, false},
	// The shared supplier master (FR-INV-004). Same reach as Procurement,
	// reads included and deliberately not widened to the technicians who can
	// see Inventory: a vendor row carries a contact person, a phone number
	// and a GSTIN, which is commercial-relationship data rather than
	// something needed to issue a router. The supplier dropdown those
	// screens show is fed from here without giving them the screen.
	{"vendors", "Vendors", "/staff/vendors",
		[]string{"isp_owner", "billing_admin"}, false},
	// Captive-portal branding (FR-HSP-004). Owner only: the captive portal is
	// the one page in the system shown to strangers by design, under the
	// venue's name, and what it says is the business speaking in public. NOC
	// staff run the hotspot NAS and have no need to change its wording.
	{"portal-branding", "Captive Portal", "/staff/portal-branding",
		[]string{"isp_owner"}, false},
	// Same reach as every other financial screen. Phase 1 only — see
	// internal/staffui/ledger.go's own doc comment on what that excludes.
	{"ledger", "General Ledger", "/staff/ledger",
		[]string{"isp_owner", "billing_admin"}, false},
	// The dunning schedule changes when every subscriber is reminded and
	// suspended: a billing decision, same reach as the ledger.
	{"dunning", "Dunning Settings", "/staff/dunning",
		[]string{"isp_owner", "billing_admin"}, false},
	// Every row is a subscriber's money and how it was billed: the same
	// reach as the ledger.
	{"diagnostics", "Billing Diagnostics", "/staff/diagnostics",
		[]string{"isp_owner", "billing_admin"}, false},
	// Whether last night's renewals went through, and who was charged
	// without being renewed: billing staff, same reach as the ledger.
	{"billing-runs", "Billing Runs", "/staff/billing-runs",
		[]string{"isp_owner", "billing_admin"}, false},
	// Every row is money arriving from the gateway, and the rows that
	// matter are the ones where it arrived and reached nobody: billing
	// staff, same reach as the ledger (FR-PAY-001).
	{"gateway-log", "Gateway Log", "/staff/gateway-log",
		[]string{"isp_owner", "billing_admin"}, false},
	// Field collection (FR-COL-001). Wider than the other financial
	// screens by one role: technicians are the field staff who knock on
	// doors, and the screen is scoped to their own round — they cannot
	// record a payment for anybody not assigned to them, which is what
	// makes including them safe. CSRs are excluded, being desk-based.
	{"collections", "Collections", "/staff/collections",
		[]string{"isp_owner", "billing_admin", "technician"}, false},
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
	// The roles the API lets compose and send (the admin gate on POST
	// /announcements and /send): a broadcast can reach every customer.
	{"announcements", "Announcements", "/staff/announcements",
		[]string{"isp_owner", "billing_admin"}, false},
	// Owner, NOC, billing and CSR: "did the customer get the reminder" is a
	// front-desk and billing question as much as an operations one. The
	// partner-webhook tab is narrower and gated inside (canSeeWebhookLog).
	{"logs", "Message Logs", "/staff/logs",
		[]string{"isp_owner", "noc_engineer", "billing_admin", "csr"}, false},
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
	mux.Handle("GET /staff/dashboard", h.authed(h.Dashboard))
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
	mux.Handle("POST /staff/subscribers/{id}/password/reveal", h.authed(h.requireCSRF(h.RevealPassword)))
	mux.Handle("POST /staff/subscribers/{id}/password", h.authed(h.requireCSRF(h.ResetPassword)))
	mux.Handle("POST /staff/subscribers/{id}/payment", h.authed(h.requireCSRF(h.RecordPaymentForm)))
	mux.Handle("POST /staff/subscribers/{id}/grace", h.authed(h.requireCSRF(h.ExtendGraceForm)))
	mux.Handle("POST /staff/subscribers/{id}/grace/end", h.authed(h.requireCSRF(h.EndGraceForm)))
	mux.Handle("GET /staff/dunning", h.authed(h.DunningSettings))
	mux.Handle("GET /staff/diagnostics", h.authed(h.BillingDiagnostics))
	mux.Handle("GET /staff/billing-runs", h.authed(h.BillingRuns))
	mux.Handle("GET /staff/gateway-log", h.authed(h.GatewayLog))
	mux.Handle("GET /staff/collections", h.authed(h.Collections))
	mux.Handle("GET /staff/documents", h.authed(h.Documents))
	mux.Handle("GET /staff/documents/{id}/download", h.authed(h.DownloadDocument))
	mux.Handle("POST /staff/documents/{id}/verify", h.authed(h.requireCSRF(h.VerifyDocument)))
	mux.Handle("POST /staff/collections/assign", h.authed(h.requireCSRF(h.AssignCollectorForm)))
	mux.Handle("POST /staff/collections/{id}/payment", h.authed(h.requireCSRF(h.RecordCollectionForm)))
	mux.Handle("GET /staff/billing-runs/{id}", h.authed(h.BillingRun))
	mux.Handle("POST /staff/dunning", h.authed(h.requireCSRF(h.SaveDunningForm)))
	mux.Handle("POST /staff/subscribers/{id}/proforma", h.authed(h.requireCSRF(h.IssueProformaForm)))
	mux.Handle("POST /staff/subscribers/{id}/plan-change", h.authed(h.requireCSRF(h.ChangeSubscriberPlan)))
	mux.Handle("GET /staff/leads", h.authed(h.Leads))
	mux.Handle("POST /staff/leads/new", h.authed(h.requireCSRF(h.CreateLeadForm)))
	mux.Handle("POST /staff/leads/{id}/update", h.authed(h.requireCSRF(h.UpdateLeadForm)))
	mux.Handle("GET /staff/leads/{id}/convert", h.authed(h.ConvertLeadPage))
	mux.Handle("POST /staff/leads/{id}/convert", h.authed(h.requireCSRF(h.ConvertLeadForm)))
	mux.Handle("GET /staff/catalogue", h.authed(h.Catalogue))
	mux.Handle("GET /staff/reports/{report}", h.authed(h.ExportReportCSV))

	mux.Handle("GET /staff/groups", h.authed(h.Groups))
	mux.Handle("POST /staff/groups", h.authed(h.requireCSRF(h.CreateGroupForm)))
	mux.Handle("POST /staff/groups/{id}", h.authed(h.requireCSRF(h.RenameGroupForm)))
	mux.Handle("POST /staff/groups/{id}/active", h.authed(h.requireCSRF(h.SetGroupActiveForm)))
	mux.Handle("POST /staff/groups/{id}/delete", h.authed(h.requireCSRF(h.DeleteGroupForm)))
	mux.Handle("POST /staff/groups/{id}/members", h.authed(h.requireCSRF(h.GroupMembersForm)))

	mux.Handle("GET /staff/bandwidth", h.authed(h.Bandwidth))
	mux.Handle("GET /staff/fair-use", h.authed(h.FairUse))
	mux.Handle("POST /staff/fair-use", h.authed(h.requireCSRF(h.SaveFUPPolicyForm)))
	mux.Handle("POST /staff/fair-use/attach", h.authed(h.requireCSRF(h.AttachFUPPolicyForm)))
	mux.Handle("POST /staff/fair-use/{id}/active", h.authed(h.requireCSRF(h.SetFUPPolicyActiveForm)))
	mux.Handle("POST /staff/fair-use/{id}/delete", h.authed(h.requireCSRF(h.DeleteFUPPolicyForm)))
	mux.Handle("POST /staff/bandwidth", h.authed(h.requireCSRF(h.CreateBandwidthForm)))
	mux.Handle("POST /staff/bandwidth/{id}", h.authed(h.requireCSRF(h.UpdateBandwidthForm)))
	mux.Handle("POST /staff/bandwidth/{id}/active", h.authed(h.requireCSRF(h.SetBandwidthActiveForm)))
	mux.Handle("POST /staff/bandwidth/{id}/delete", h.authed(h.requireCSRF(h.DeleteBandwidthForm)))
	mux.Handle("POST /staff/catalogue/plans", h.authed(h.requireCSRF(h.CreatePlan)))
	mux.Handle("POST /staff/catalogue/gst", h.authed(h.requireCSRF(h.CreateGSTRate)))

	// Plan editing, duplication, retirement and CSV export (FR-CAT-001).
	mux.Handle("GET /staff/catalogue/plans.csv", h.authed(h.ExportPlansCSV))
	mux.Handle("POST /staff/catalogue/plans/{id}", h.authed(h.requireCSRF(h.UpdatePlanForm)))
	mux.Handle("POST /staff/catalogue/plans/{id}/duplicate", h.authed(h.requireCSRF(h.DuplicatePlanForm)))
	mux.Handle("POST /staff/catalogue/plans/{id}/active", h.authed(h.requireCSRF(h.SetPlanActiveForm)))
	mux.Handle("GET /staff/sessions", h.authed(h.Sessions))
	mux.Handle("POST /staff/sessions/{session_id}/disconnect", h.authed(h.requireCSRF(h.DisconnectSessionForm)))
	mux.Handle("GET /staff/cpe", h.authed(h.CPEList))
	mux.Handle("GET /staff/cpe/{serial}", h.authed(h.CPEDetail))
	mux.Handle("POST /staff/cpe/{serial}/action", h.authed(h.requireCSRF(h.CPEActionForm)))
	mux.Handle("GET /staff/nas", h.authed(h.NAS))
	mux.Handle("GET /staff/network", h.authed(h.NetworkMap))
	mux.Handle("POST /staff/nas/new", h.authed(h.requireCSRF(h.CreateNASDeviceForm)))
	mux.Handle("POST /staff/nas/{id}/update", h.authed(h.requireCSRF(h.UpdateNASDeviceForm)))
	mux.Handle("GET /staff/billing", h.authed(h.Billing))
	mux.Handle("GET /staff/billing/gstr1", h.authed(h.GSTR1Export))
	mux.Handle("GET /staff/tickets", h.authed(h.Tickets))
	mux.Handle("POST /staff/tickets/{id}/status", h.authed(h.requireCSRF(h.UpdateTicketStatus)))
	mux.Handle("GET /staff/invoices", h.authed(h.Money))
	mux.Handle("GET /staff/invoices/proformas", h.authed(h.Proformas))
	mux.Handle("GET /staff/invoices/proformas/{id}/pdf", h.authed(h.ProformaPDF))
	mux.Handle("POST /staff/invoices/proformas/{id}/convert", h.authed(h.requireCSRF(h.ConvertProformaForm)))
	mux.Handle("POST /staff/invoices/proformas/{id}/cancel", h.authed(h.requireCSRF(h.CancelProformaForm)))

	// Product catalogue and subscriber charges (FR-BIL-022). Reading is
	// gated by the "products" section; the write actions gate again inside
	// themselves, so a CSR can see the catalogue without changing it.
	mux.Handle("GET /staff/products", h.authed(h.Products))
	mux.Handle("POST /staff/products", h.authed(h.requireCSRF(h.CreateProductForm)))
	mux.Handle("POST /staff/products/{id}", h.authed(h.requireCSRF(h.UpdateProductForm)))
	mux.Handle("POST /staff/products/{id}/active", h.authed(h.requireCSRF(h.SetProductActiveForm)))
	mux.Handle("POST /staff/products/{id}/delete", h.authed(h.requireCSRF(h.DeleteProductForm)))
	mux.Handle("POST /staff/subscribers/{id}/products", h.authed(h.requireCSRF(h.AttachProductForm)))
	mux.Handle("POST /staff/subscriber-products/{id}/end", h.authed(h.requireCSRF(h.EndAttachmentForm)))

	// Scheduled plan changes (FR-BIL-023), scheduled and cancelled from
	// the subscriber's own page where the operator can see their plan.
	mux.Handle("POST /staff/subscribers/{id}/scheduled-plan-change", h.authed(h.requireCSRF(h.SchedulePlanChangeForm)))
	mux.Handle("POST /staff/scheduled-plan-changes/{id}/cancel", h.authed(h.requireCSRF(h.CancelPlanChangeForm)))

	// IP pools and static addresses (FR-NET-005). Pool management is on
	// its own screen; assigning an address happens on the subscriber's
	// page, where the operator can see who they are giving it to.
	mux.Handle("GET /staff/ip-pools", h.authed(h.IPPools))
	mux.Handle("GET /staff/auth-log", h.authed(h.AuthLog))
	mux.Handle("GET /staff/plan-attributes", h.authed(h.PlanAttributes))
	mux.Handle("POST /staff/plan-attributes", h.authed(h.requireCSRF(h.SetPlanAttributeForm)))
	mux.Handle("POST /staff/plan-attributes/{id}/enabled", h.authed(h.requireCSRF(h.TogglePlanAttributeForm)))
	mux.Handle("POST /staff/plan-attributes/{id}/delete", h.authed(h.requireCSRF(h.DeletePlanAttributeForm)))
	mux.Handle("POST /staff/ip-pools", h.authed(h.requireCSRF(h.CreatePoolForm)))
	mux.Handle("POST /staff/ip-pools/{id}/delete", h.authed(h.requireCSRF(h.DeletePoolForm)))
	mux.Handle("POST /staff/ip-pool-exclusions", h.authed(h.requireCSRF(h.AddExclusionForm)))
	mux.Handle("POST /staff/ip-pool-exclusions/{id}/delete", h.authed(h.requireCSRF(h.RemoveExclusionForm)))
	mux.Handle("POST /staff/subscribers/{id}/static-ip", h.authed(h.requireCSRF(h.AllocateStaticIPForm)))
	mux.Handle("POST /staff/static-ips/{id}/release", h.authed(h.requireCSRF(h.ReleaseStaticIPForm)))
	mux.Handle("GET /staff/invoices/{id}/pdf", h.authed(h.InvoicePDF))
	mux.Handle("GET /staff/subscribers/{id}/caf", h.authed(h.SubscriberCAF))
	mux.Handle("GET /staff/invoices/{id}/credit-note", h.authed(h.CreditNotePage))
	mux.Handle("GET /staff/invoices/payments/{id}/receipt", h.authed(h.ReceiptPDF))
	mux.Handle("POST /staff/invoices/payments/{id}/bounce", h.authed(h.requireCSRF(h.BounceChequeForm)))
	mux.Handle("POST /staff/invoices/{id}/credit-note", h.authed(h.requireCSRF(h.CreditNoteForm)))
	mux.Handle("GET /staff/revenue", h.authed(h.Revenue))
	mux.Handle("GET /staff/lea", h.authed(h.LEAPage))
	mux.Handle("POST /staff/lea", h.authed(h.requireCSRF(h.LEALookup)))
	mux.Handle("GET /staff/demo", h.authed(h.Demo))
	mux.Handle("POST /staff/demo/load", h.authed(h.requireCSRF(h.LoadDemoData)))
	mux.Handle("POST /staff/demo/remove", h.authed(h.requireCSRF(h.RemoveDemoData)))
	mux.Handle("GET /staff/franchise", h.authed(h.Franchises))
	mux.Handle("POST /staff/franchise/new", h.authed(h.requireCSRF(h.CreateFranchiseForm)))
	mux.Handle("GET /staff/franchise/{id}", h.authed(h.FranchiseDetail))
	mux.Handle("GET /staff/my-subscribers", h.authed(h.MySubscribers))
	mux.Handle("GET /staff/my-pnl", h.authed(h.MyPnL))
	mux.Handle("GET /staff/portal-branding", h.authed(h.PortalBranding))
	mux.Handle("GET /staff/portal-branding/preview", h.authed(h.PortalBrandingPreview))
	mux.Handle("POST /staff/portal-branding", h.authed(h.requireCSRF(h.SavePortalBrandingForm)))
	// The upload has its own wrapper: requireCSRF would parse an uncapped
	// multipart body before checking the token. See requireCSRFUpload.
	mux.Handle("POST /staff/portal-branding/logo", h.authed(
		h.requireCSRFUpload(brandingUploadLimit, "/staff/portal-branding", h.UploadPortalLogoForm)))
	mux.Handle("POST /staff/portal-branding/logo/clear", h.authed(h.requireCSRF(h.ClearPortalLogoForm)))

	// Interface traffic (FR-NET-006), under the Network section's own gate.
	mux.Handle("GET /staff/network/traffic", h.authed(h.Traffic))
	mux.Handle("GET /staff/network/traffic/{id}", h.authed(h.TrafficRouter))
	mux.Handle("POST /staff/network/traffic/{id}/snmp", h.authed(h.requireCSRF(h.SaveTrafficSettingsForm)))

	mux.Handle("GET /staff/vendors", h.authed(h.Vendors))
	mux.Handle("POST /staff/vendors", h.authed(h.requireCSRF(h.CreateVendorForm)))
	mux.Handle("POST /staff/vendors/{id}", h.authed(h.requireCSRF(h.UpdateVendorForm)))
	// Retire or restore. There is no delete route, by design — see
	// SetVendorRetiredForm.
	mux.Handle("POST /staff/vendors/{id}/retired", h.authed(h.requireCSRF(h.SetVendorRetiredForm)))

	mux.Handle("GET /staff/procurement", h.authed(h.Procurement))
	mux.Handle("POST /staff/procurement/new", h.authed(h.requireCSRF(h.CreatePurchaseOrderForm)))
	mux.Handle("POST /staff/procurement/{id}/decide", h.authed(h.requireCSRF(h.DecidePurchaseOrderForm)))
	mux.Handle("POST /staff/procurement/{id}/fulfilment", h.authed(h.requireCSRF(h.UpdateFulfilmentForm)))
	mux.Handle("GET /staff/ledger", h.authed(h.Ledger))
	mux.Handle("POST /staff/ledger/accounts/new", h.authed(h.requireCSRF(h.CreateLedgerAccountForm)))
	mux.Handle("POST /staff/ledger/entries/new", h.authed(h.requireCSRF(h.PostJournalEntryForm)))
	mux.Handle("GET /staff/inventory", h.authed(h.Inventory))
	mux.Handle("POST /staff/inventory/types/new", h.authed(h.requireCSRF(h.CreateDeviceTypeForm)))
	mux.Handle("POST /staff/inventory/devices/new", h.authed(h.requireCSRF(h.CreateDeviceForm)))
	mux.Handle("POST /staff/inventory/devices/{serial}/issue", h.authed(h.requireCSRF(h.IssueDeviceForm)))
	mux.Handle("POST /staff/inventory/devices/{serial}/return", h.authed(h.requireCSRF(h.ReturnDeviceForm)))
	mux.Handle("POST /staff/inventory/purchases/new", h.authed(h.requireCSRF(h.RecordPurchaseForm)))
	mux.Handle("GET /staff/reports", h.authed(h.Reports))
	mux.Handle("GET /staff/announcements", h.authed(h.Announcements))
	mux.Handle("POST /staff/announcements/new", h.authed(h.requireCSRF(h.ComposeAnnouncementForm)))
	mux.Handle("POST /staff/announcements/{id}/send", h.authed(h.requireCSRF(h.SendAnnouncementForm)))
	mux.Handle("GET /staff/logs", h.authed(h.Logs))
	mux.Handle("GET /staff/tasks", h.authed(h.Tasks))
	mux.Handle("POST /staff/tasks/field/new", h.authed(h.requireCSRF(h.CreateFieldTaskForm)))
	mux.Handle("POST /staff/tasks/field/{id}/update", h.authed(h.requireCSRF(h.UpdateFieldTaskForm)))
	mux.Handle("POST /staff/tasks/approvals/{id}/approve", h.authed(h.requireCSRF(h.ApproveTaskRequest)))
	mux.Handle("POST /staff/tasks/approvals/{id}/reject", h.authed(h.requireCSRF(h.RejectTaskRequest)))
	mux.Handle("GET /staff/accounts", h.authed(h.StaffAccounts))
	mux.Handle("GET /staff/audit", h.authed(h.Audit))
	mux.Handle("POST /staff/accounts/new", h.authed(h.requireCSRF(h.CreateStaffAccount)))
	mux.Handle("POST /staff/accounts/{id}/update", h.authed(h.requireCSRF(h.UpdateStaffAccount)))
	// Available to every signed-in role, not owner-only: anyone should be
	// able to change their own password.
	mux.Handle("GET /staff/change-password", h.authed(h.ChangePasswordPage))
	mux.Handle("POST /staff/change-password", h.authed(h.requireCSRF(h.ChangePassword)))
	mux.Handle("GET /staff/static/", http.StripPrefix("/staff/static/", staticHandler()))
}
