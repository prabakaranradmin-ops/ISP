// Package api wires all HTTP routes for the BSS/OSS API service.
//
// FR: FR-AAA-001..004, FR-BIL-001..007, FR-NET-001..003, FR-SUB-001..005,
//
//	FR-OBS-004, FR-SEC-005 | DDS §5.7, §5.9 | API §7
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
	"github.com/maaransoft/isp-bss-oss/pkg/validate"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/bcrypt"
)

// SubscriberRecord is the API representation of a subscriber.
type SubscriberRecord struct {
	ID              int        `json:"id"`
	CAFNumber       string     `json:"caf_number"`
	Username        string     `json:"username"`
	MobileNumber    string     `json:"mobile_number"`
	Email           string     `json:"email,omitempty"`
	PlanID          int        `json:"plan_id"`
	FranchiseID     *int       `json:"franchise_id,omitempty"`
	Status          string     `json:"status"`
	DunningState    string     `json:"dunning_state"`
	WalletBalance   string     `json:"wallet_balance"`
	RegisteredState string     `json:"registered_state"`
	KYCStatus       string     `json:"kyc_status"`
	PlanExpiry      *time.Time `json:"plan_expiry,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	// SpeedOverrideRateLimit and SpeedOverrideExpiresAt reflect an
	// owner-triggered temporary rate (console "Speed override"), set via
	// SessionController.SetSpeedOverride. Empty/nil means none is active.
	SpeedOverrideRateLimit string     `json:"speed_override_rate_limit,omitempty"`
	SpeedOverrideExpiresAt *time.Time `json:"speed_override_expires_at,omitempty"`
}

// CreateSubscriberRequest is the POST /api/v1/subscribers body.
type CreateSubscriberRequest struct {
	CAFNumber       string `json:"caf_number"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	MobileNumber    string `json:"mobile_number"`
	Email           string `json:"email"`
	PlanID          int    `json:"plan_id"`
	RegisteredState string `json:"registered_state"`
	Aadhaar         string `json:"aadhaar,omitempty"`
	PAN             string `json:"pan,omitempty"`
}

// SubscriberQuerier is the DB interface for subscriber operations.
type SubscriberQuerier interface {
	CreateSubscriber(ctx context.Context, sub SubscriberRecord, passwordHash string) (*SubscriberRecord, error)
	GetSubscriberByID(ctx context.Context, id int) (*SubscriberRecord, error)
	UpdateSubscriber(ctx context.Context, id int, planID *int, status *string, planExpiry *time.Time) (*SubscriberRecord, error)
	GetSubscriberByUsername(ctx context.Context, username string) (*SubscriberRecord, error)
}

// KYCQuerier handles KYC persistence.
type KYCQuerier interface {
	UpsertKYC(ctx context.Context, subscriberID int, aadhaarEnc, panEnc, keyVersion string) error
}

// Handler holds all API route dependencies.
type Handler struct {
	db        SubscriberQuerier
	kycDB     KYCQuerier
	walletSvc *billing.WalletService
	keyStore  crypto.KeyStore

	ledger          LedgerQuerier
	sessions        SessionReader
	sessionCtl      SessionController
	tasks           TaskEnqueuer
	invoices        InvoiceQuerier
	pdfGen          PDFGenerator
	tickets         TicketAdminQuerier
	lea             LEAQuerier
	leaAudit        LEAAuditRecorder
	franchises      FranchiseQuerier
	lifecycle       LifecycleQuerier
	refunds         RefundQuerier
	subCache        SubscriberCacheInvalidator
	approvals       ApprovalQuerier
	fieldTasks      FieldTaskQuerier
	leads           LeadQuerier
	inventory       InventoryQuerier
	announcements   AnnouncementQuerier
	pushTokens      PushTokenQuerier
	eapEnrolment    EAPEnrolmentQuerier
	credentials     CredentialQuerier
	cpeControl      CPEControlQuerier
	partners        PartnerQuerier
	secretEncryptor SecretEncryptor
	partnerAuth     middleware.APIKeyAuthenticator
	events          EventEmitter
	hotspot         HotspotQuerier
	nas             NASQuerier
	reports         ReportQuerier
	archives        ArchiveLookup
	procurement     ProcurementQuerier
	generalLedger   GeneralLedgerQuerier
	// subscriberLister backs revenue.ListSubscribersHandler, which is a
	// plain http.HandlerFunc rather than a method on Handler — so the
	// dependency is held here and passed to it at route-registration time.
	subscriberLister revenue.SubscriberLister
	health           http.Handler

	razorpayWebhookSecret string
}

// HandlerDeps bundles every Handler dependency.
//
// A plain multi-argument constructor stopped being readable once the wiring
// grew past the original four collaborators (db, kycDB, walletSvc, keyStore);
// a struct makes each dependency's purpose explicit at the call site and lets
// optional ones (Sessions, Invoices, LEA, ...) be left as their zero value
// without every caller having to pass a run of nils in the right order.
//
// A nil optional dependency is not a startup error: each handler that needs it
// checks and returns 503 rather than panicking, so a deployment that has not
// found (say) a Chromium-based browser still serves every other route.
type HandlerDeps struct {
	DB       SubscriberQuerier
	KYC      KYCQuerier
	Wallet   *billing.WalletService
	KeyStore crypto.KeyStore

	Ledger           LedgerQuerier
	Sessions         SessionReader
	SessionCtl       SessionController
	Tasks            TaskEnqueuer
	Invoices         InvoiceQuerier
	PDF              PDFGenerator
	Tickets          TicketAdminQuerier
	LEA              LEAQuerier
	LEAAudit         LEAAuditRecorder
	Franchises       FranchiseQuerier
	SubscriberLister revenue.SubscriberLister
	Lifecycle        LifecycleQuerier
	Refunds          RefundQuerier
	SubCache         SubscriberCacheInvalidator
	Approvals        ApprovalQuerier
	FieldTasks       FieldTaskQuerier
	Leads            LeadQuerier
	Inventory        InventoryQuerier
	Announcements    AnnouncementQuerier
	PushTokens       PushTokenQuerier
	EAPEnrolment     EAPEnrolmentQuerier
	Credentials      CredentialQuerier
	CPEControl       CPEControlQuerier
	Partners         PartnerQuerier
	SecretEncryptor  SecretEncryptor
	PartnerAuth      middleware.APIKeyAuthenticator
	Events           EventEmitter
	Hotspot          HotspotQuerier
	NAS              NASQuerier
	Reports          ReportQuerier
	Archives         ArchiveLookup
	Procurement      ProcurementQuerier
	GeneralLedger    GeneralLedgerQuerier

	// Health serves GET /api/v1/subscribers/{id}/health (FR-OBS-004). The
	// implementation lives in internal/health, which cannot be imported here
	// without a cycle, so it is injected as a plain http.Handler and served
	// through this package's staff-read authorisation like any other route.
	//
	// It must be passed here rather than registered directly on the mux by the
	// caller: a route added to the mux afterwards carries no middleware, and
	// binding it that way once left full subscriber diagnostics — including the
	// assigned IP address — readable with no token at all.
	Health http.Handler

	RazorpayWebhookSecret string
}

// NewHandler constructs the API Handler.
func NewHandler(deps HandlerDeps) *Handler {
	return &Handler{
		db:        deps.DB,
		kycDB:     deps.KYC,
		walletSvc: deps.Wallet,
		keyStore:  deps.KeyStore,

		ledger:           deps.Ledger,
		sessions:         deps.Sessions,
		sessionCtl:       deps.SessionCtl,
		tasks:            deps.Tasks,
		invoices:         deps.Invoices,
		pdfGen:           deps.PDF,
		tickets:          deps.Tickets,
		lea:              deps.LEA,
		leaAudit:         deps.LEAAudit,
		franchises:       deps.Franchises,
		subscriberLister: deps.SubscriberLister,
		lifecycle:        deps.Lifecycle,
		refunds:          deps.Refunds,
		subCache:         deps.SubCache,
		approvals:        deps.Approvals,
		fieldTasks:       deps.FieldTasks,
		leads:            deps.Leads,
		inventory:        deps.Inventory,
		announcements:    deps.Announcements,
		pushTokens:       deps.PushTokens,
		eapEnrolment:     deps.EAPEnrolment,
		credentials:      deps.Credentials,
		cpeControl:       deps.CPEControl,
		partners:         deps.Partners,
		secretEncryptor:  deps.SecretEncryptor,
		partnerAuth:      deps.PartnerAuth,
		events:           deps.Events,
		hotspot:          deps.Hotspot,
		nas:              deps.NAS,
		reports:          deps.Reports,
		archives:         deps.Archives,
		procurement:      deps.Procurement,
		generalLedger:    deps.GeneralLedger,
		health:           deps.Health,

		razorpayWebhookSecret: deps.RazorpayWebhookSecret,
	}
}

// RegisterRoutes wires all API routes onto the provided mux using Go 1.21 pattern syntax.
//
// API §7 | FR: API-003, API-004
func (h *Handler) RegisterRoutes(mux *http.ServeMux, jwtSecret string) {
	auth := middleware.JWTMiddleware(jwtSecret)
	admin := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("billing_admin", "isp_owner")(next))
	}
	staffRead := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "billing_admin", "csr", "technician", "isp_owner")(next))
	}
	nocOnly := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "isp_owner")(next))
	}
	// LEA export requires the noc_engineer role AND the separate lea_access
	// claim (SecD §9.3 "noc + lea_flag"): the two are independent grants, so a
	// noc_engineer token minted without lea_access must not reach this route.
	nocWithLea := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "isp_owner")(middleware.RequireLeaAccess(next)))
	}
	billingOrCSR := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("billing_admin", "csr", "isp_owner")(next))
	}
	csrOrTech := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("csr", "technician", "isp_owner")(next))
	}

	// Health (no auth)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Subscribers CRUD (API-003)
	mux.Handle("POST /api/v1/subscribers",
		admin(http.HandlerFunc(h.CreateSubscriber)))
	mux.Handle("GET /api/v1/subscribers/{id}",
		staffRead(http.HandlerFunc(h.GetSubscriber)))
	mux.Handle("PATCH /api/v1/subscribers/{id}",
		admin(http.HandlerFunc(h.UpdateSubscriber)))
	mux.Handle("GET /api/v1/subscribers/{id}/health",
		staffRead(http.HandlerFunc(h.GetSubscriberHealth)))

	// EAP-MSCHAPv2 enrolment (FR-AAA-006 | MDS §4.18). Admin-tier: enrolling
	// creates a second credential representation for that subscriber, which
	// is a security-posture change rather than routine support work.
	mux.Handle("GET /api/v1/subscribers/{id}/eap",
		admin(http.HandlerFunc(h.GetEAPStatus)))
	mux.Handle("POST /api/v1/subscribers/{id}/eap",
		admin(http.HandlerFunc(h.EnrolEAP)))
	mux.Handle("DELETE /api/v1/subscribers/{id}/eap",
		admin(http.HandlerFunc(h.UnenrolEAP)))

	// Subscriber lifecycle (FR-LC-001..003, FR-BIL-010..011 | MDS §4.14)
	mux.Handle("POST /api/v1/subscribers/{id}/plan-change",
		admin(http.HandlerFunc(h.ChangeSubscriberPlan)))
	mux.Handle("POST /api/v1/subscribers/{id}/terminate",
		admin(http.HandlerFunc(h.TerminateSubscriber)))
	mux.Handle("POST /api/v1/subscribers/{id}/adjustments",
		admin(http.HandlerFunc(h.CreateAdjustment)))

	// Bulk subscriber operations (console multi-select) — same admin tier as
	// the single-subscriber actions they loop, since a batch of fifty is not
	// reach beyond what an operator can already do fifty times over.
	mux.Handle("POST /api/v1/subscribers/bulk/plan-change",
		admin(http.HandlerFunc(h.BulkChangeSubscriberPlan)))
	mux.Handle("POST /api/v1/subscribers/bulk/status",
		admin(http.HandlerFunc(h.BulkUpdateStatus)))
	mux.Handle("POST /api/v1/subscribers/bulk/credit",
		admin(http.HandlerFunc(h.BulkWalletCredit)))
	mux.Handle("POST /api/v1/subscribers/{id}/refunds",
		admin(http.HandlerFunc(h.CreateRefund)))

	// Approval workflow (FR-WFL-001 | MDS §4.15)
	//
	// Reachable by the same billing_admin/isp_owner tier that files the
	// requests: the guarantee this module provides is that the approver is a
	// *different person*, enforced per-request against the token's subject,
	// not that they hold a higher role. A separate approver role would be a
	// different (and additional) control, and one CRD-EXP-002 does not ask
	// for — "second-approver sign-off" is about two people, not two tiers.
	mux.Handle("GET /api/v1/approvals",
		admin(http.HandlerFunc(h.ListApprovals)))
	mux.Handle("GET /api/v1/approvals/{id}",
		admin(http.HandlerFunc(h.GetApproval)))
	mux.Handle("POST /api/v1/approvals/{id}/approve",
		admin(http.HandlerFunc(h.ApproveRequest)))
	mux.Handle("POST /api/v1/approvals/{id}/reject",
		admin(http.HandlerFunc(h.RejectRequest)))

	// Field tasks (FR-WFL-002 | MDS §4.15). Internal staff coordination, so
	// the wider staff tier can see and update their own queue; creating and
	// assigning work stays with csr/technician/owner, matching how tickets
	// are already gated.
	mux.Handle("POST /api/v1/field-tasks",
		csrOrTech(http.HandlerFunc(h.CreateFieldTask)))
	mux.Handle("GET /api/v1/field-tasks",
		staffRead(http.HandlerFunc(h.ListFieldTasks)))
	mux.Handle("PATCH /api/v1/field-tasks/{id}",
		csrOrTech(http.HandlerFunc(h.UpdateFieldTask)))

	// CRM lead pipeline (FR-CRM-001..003 | MDS §4.16)
	//
	// Sales work, so csr/technician reach it alongside owners — and
	// franchise roles reach their own pipeline, scoped from their token the
	// same way their subscribers and P&L are. Conversion is the exception:
	// it creates a billable subscriber, so it sits on the same
	// billing_admin/isp_owner tier as POST /subscribers.
	leadWrite := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole(
			"csr", "technician", "billing_admin", "isp_owner",
			"lco", "franchise_admin", "franchise_staff",
		)(next))
	}

	// Registered before {id} would matter: Go 1.22's mux prefers the more
	// specific literal, so "funnel" is never parsed as a lead id.
	mux.Handle("GET /api/v1/leads/funnel",
		leadWrite(http.HandlerFunc(h.GetLeadFunnel)))
	mux.Handle("POST /api/v1/leads",
		leadWrite(http.HandlerFunc(h.CreateLead)))
	mux.Handle("GET /api/v1/leads",
		leadWrite(http.HandlerFunc(h.ListLeads)))
	mux.Handle("GET /api/v1/leads/{id}",
		leadWrite(http.HandlerFunc(h.GetLead)))
	mux.Handle("PATCH /api/v1/leads/{id}",
		leadWrite(http.HandlerFunc(h.UpdateLead)))
	mux.Handle("POST /api/v1/leads/{id}/convert",
		admin(http.HandlerFunc(h.ConvertLead)))

	// CPE inventory (FR-INV-001..003 | MDS §4.16). Technicians handle
	// hardware day to day; purchases and device-type catalogue changes are
	// procurement, so they stay with billing_admin/isp_owner.
	mux.Handle("GET /api/v1/cpe/types",
		staffRead(http.HandlerFunc(h.ListDeviceTypes)))
	mux.Handle("POST /api/v1/cpe/types",
		admin(http.HandlerFunc(h.CreateDeviceType)))
	mux.Handle("GET /api/v1/cpe/devices",
		staffRead(http.HandlerFunc(h.ListDevices)))
	mux.Handle("POST /api/v1/cpe/devices",
		csrOrTech(http.HandlerFunc(h.CreateDevice)))
	mux.Handle("POST /api/v1/cpe/devices/{serial}/issue",
		csrOrTech(http.HandlerFunc(h.IssueDevice)))
	mux.Handle("POST /api/v1/cpe/devices/{serial}/return",
		csrOrTech(http.HandlerFunc(h.ReturnDevice)))
	mux.Handle("GET /api/v1/cpe/stock",
		staffRead(http.HandlerFunc(h.GetStockLevels)))
	mux.Handle("GET /api/v1/cpe/purchases",
		admin(http.HandlerFunc(h.ListPurchases)))
	mux.Handle("POST /api/v1/cpe/purchases",
		admin(http.HandlerFunc(h.RecordPurchase)))

	// General procurement (CRD-EXP-007) — purchase orders for anything, not
	// just CPE restocking above. Owner/billing_admin end to end: requesting,
	// deciding and updating fulfilment are all a spend decision, not
	// day-to-day operations.
	mux.Handle("POST /api/v1/procurement/orders",
		admin(http.HandlerFunc(h.CreatePurchaseOrder)))
	mux.Handle("GET /api/v1/procurement/orders",
		admin(http.HandlerFunc(h.ListPurchaseOrders)))
	mux.Handle("POST /api/v1/procurement/orders/{id}/decide",
		admin(http.HandlerFunc(h.DecidePurchaseOrder)))
	mux.Handle("POST /api/v1/procurement/orders/{id}/fulfilment",
		admin(http.HandlerFunc(h.UpdateFulfilmentStatus)))

	// General ledger, Phase 1 (CRD-EXP-006 | DBD §6.2). Owner/billing_admin
	// only, matching every other financial screen. Every entry reachable
	// here is 'manual' — there is no route that auto-posts on behalf of a
	// wallet recharge, franchise commission, or purchase order (Phase 2,
	// not implemented).
	mux.Handle("POST /api/v1/ledger/accounts",
		admin(http.HandlerFunc(h.CreateLedgerAccount)))
	mux.Handle("GET /api/v1/ledger/accounts",
		admin(http.HandlerFunc(h.ListLedgerAccounts)))
	mux.Handle("POST /api/v1/ledger/entries",
		admin(http.HandlerFunc(h.PostJournalEntry)))
	mux.Handle("GET /api/v1/ledger/entries",
		admin(http.HandlerFunc(h.ListJournalEntries)))
	mux.Handle("GET /api/v1/ledger/entries/{id}",
		admin(http.HandlerFunc(h.GetJournalEntry)))
	mux.Handle("GET /api/v1/ledger/trial-balance",
		admin(http.HandlerFunc(h.GetTrialBalance)))
	mux.Handle("GET /api/v1/ledger/income-statement",
		admin(http.HandlerFunc(h.GetIncomeStatement)))
	mux.Handle("GET /api/v1/ledger/balance-sheet",
		admin(http.HandlerFunc(h.GetBalanceSheet)))

	// TR-069 remote control (FR-CPE-003 | MDS §4.19). NOC and technicians:
	// these are field-operations actions, and every one of them queues an
	// RPC for the device's next check-in rather than acting immediately.
	nocOrTech := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("noc_engineer", "technician", "isp_owner")(next))
	}
	mux.Handle("GET /api/v1/cpe/devices/{serial}/tasks",
		staffRead(http.HandlerFunc(h.ListCPETasks)))
	mux.Handle("POST /api/v1/cpe/devices/{serial}/reboot",
		nocOrTech(http.HandlerFunc(h.RebootCPE)))
	mux.Handle("POST /api/v1/cpe/devices/{serial}/firmware",
		nocOrTech(http.HandlerFunc(h.UpgradeCPEFirmware)))
	mux.Handle("POST /api/v1/cpe/devices/{serial}/parameters",
		nocOrTech(http.HandlerFunc(h.GetCPEParameters)))
	mux.Handle("POST /api/v1/cpe/devices/{serial}/reprovision",
		nocOrTech(http.HandlerFunc(h.ReprovisionCPE)))

	// Partner API keys (FR-API-001 | MDS §4.22). Issuing a credential that can
	// read subscriber data is an owner-level act, so key management is
	// isp_owner only — deliberately narrower than `admin`, which billing_admin
	// also reaches.
	ownerOnly := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("isp_owner")(next))
	}
	mux.Handle("POST /api/v1/partner-keys", ownerOnly(http.HandlerFunc(h.CreateAPIKey)))
	mux.Handle("GET /api/v1/partner-keys", ownerOnly(http.HandlerFunc(h.ListAPIKeys)))
	mux.Handle("DELETE /api/v1/partner-keys/{id}", ownerOnly(http.HandlerFunc(h.RevokeAPIKey)))

	// NAS inventory (FR-NAS-001..004, FR-HSP-002 | MDS §4.11).
	//
	// NOC tier: this is network infrastructure, and every field on it —
	// the shared secret, the CoA port, allow_mab — changes how the RADIUS
	// daemons treat a device. Deliberately not `staffRead` even for the
	// listing: which NAS exist, on which addresses, with MAB on or off, is a
	// map of the network's soft spots rather than routine support data.
	//
	// Path is /api/v1/nas rather than an /admin/ prefix, matching every other
	// resource in this file — the tier is expressed by the middleware, not by
	// the URL.
	mux.Handle("GET /api/v1/nas",
		nocOnly(http.HandlerFunc(h.ListNASDevices)))
	mux.Handle("POST /api/v1/nas",
		nocOnly(http.HandlerFunc(h.CreateNASDevice)))
	mux.Handle("PATCH /api/v1/nas/{id}",
		nocOnly(http.HandlerFunc(h.UpdateNASDevice)))

	// Hotspot administration (FR-HSP-001..002 | MDS §4.23).
	//
	// Two tiers, split by what the action actually grants. Issuing and voiding
	// vouchers is billing_admin/isp_owner: a batch of vouchers is prepaid
	// service, and generating one is the same kind of act as crediting a
	// wallet. Registering a subscriber's phone for MAC Auth Bypass is routine
	// support work, so it sits with csr/technician alongside CPE handling.
	//
	// The captive portal that redeems these lives in internal/hotspot and is
	// mounted separately, with no authentication at all — it has to be, since
	// its audience has no account yet. Keeping issuance here rather than there
	// is what stops the public-facing surface from being able to mint what it
	// accepts.
	mux.Handle("POST /api/v1/hotspot/vouchers",
		admin(http.HandlerFunc(h.CreateVoucherBatch)))
	mux.Handle("GET /api/v1/hotspot/vouchers",
		staffRead(http.HandlerFunc(h.ListVouchers)))
	mux.Handle("DELETE /api/v1/hotspot/vouchers/{id}",
		admin(http.HandlerFunc(h.VoidVoucher)))
	mux.Handle("GET /api/v1/hotspot/vouchers/commissions",
		admin(http.HandlerFunc(h.GetVoucherCommissions)))
	mux.Handle("POST /api/v1/hotspot/devices",
		csrOrTech(http.HandlerFunc(h.RegisterHotspotDevice)))
	mux.Handle("DELETE /api/v1/hotspot/devices/{mac}",
		csrOrTech(http.HandlerFunc(h.DeactivateHotspotDevice)))

	// Announcements (FR-ANN-001..002 | MDS §4.17). Composing a broadcast to
	// the whole subscriber base is an owner/billing-tier action; the portal
	// banner feed is subscriber-authenticated and handled separately below.
	mux.Handle("POST /api/v1/announcements",
		admin(http.HandlerFunc(h.CreateAnnouncement)))
	mux.Handle("GET /api/v1/announcements",
		staffRead(http.HandlerFunc(h.ListAnnouncements)))
	mux.Handle("POST /api/v1/announcements/{id}/send",
		admin(http.HandlerFunc(h.SendAnnouncement)))

	// Subscriber-facing: the banner feed and push-token registration both
	// derive their subscriber from the token, never from a path or body.
	subscriberSelf := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("subscriber")(next))
	}
	mux.Handle("GET /api/v1/announcements/portal",
		subscriberSelf(http.HandlerFunc(h.GetPortalAnnouncements)))
	mux.Handle("POST /api/v1/push-tokens",
		subscriberSelf(http.HandlerFunc(h.RegisterPushToken)))

	// Wallets (API-003)
	mux.Handle("POST /api/v1/wallets/recharge",
		admin(http.HandlerFunc(h.WalletRecharge)))
	mux.Handle("GET /api/v1/wallets/{subscriber_id}/ledger",
		billingOrCSR(http.HandlerFunc(h.GetLedger)))

	// Sessions (API-004)
	mux.Handle("GET /api/v1/sessions/{subscriber_id}/active",
		staffRead(http.HandlerFunc(h.GetActiveSession)))
	mux.Handle("POST /api/v1/sessions/{session_id}/disconnect",
		nocOnly(http.HandlerFunc(h.DisconnectSession)))
	mux.Handle("POST /api/v1/sessions/{session_id}/fup-override",
		nocOnly(http.HandlerFunc(h.FUPOverride)))
	// Speed override is owner-only, not nocOnly: it changes what a specific
	// customer is being charged to receive, not a network-health action.
	mux.Handle("POST /api/v1/subscribers/{id}/speed-override",
		ownerOnly(http.HandlerFunc(h.SpeedOverride)))
	mux.Handle("POST /api/v1/subscribers/{id}/speed-override/clear",
		ownerOnly(http.HandlerFunc(h.ClearSpeedOverride)))

	// Invoices (API-004)
	mux.Handle("GET /api/v1/invoices/{subscriber_id}",
		billingOrCSR(http.HandlerFunc(h.ListInvoices)))
	mux.Handle("GET /api/v1/invoices/{invoice_id}/pdf",
		billingOrCSR(http.HandlerFunc(h.GetInvoicePDF)))

	// Tickets (API-004)
	mux.Handle("POST /api/v1/tickets",
		billingOrCSR(http.HandlerFunc(h.CreateTicket)))
	mux.Handle("PATCH /api/v1/tickets/{ticket_id}",
		csrOrTech(http.HandlerFunc(h.UpdateTicket)))

	// LEA (API-004)
	mux.Handle("POST /api/v1/lea/lookup",
		nocWithLea(http.HandlerFunc(h.LEALookup)))

	// Franchise / LCO (FR-FRN-003..006)
	//
	// Two tiers. ISP-wide staff (billing_admin, isp_owner) can onboard
	// partners and read the consolidated view. Franchise-scoped roles reach
	// only the per-partner routes, and only for their own partner — the
	// handlers derive that from the caller's token, never from the path, so
	// the id in the URL cannot widen anyone's visibility.
	franchiseRead := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole(
			"billing_admin", "isp_owner",
			"lco", "franchise_admin", "franchise_staff",
		)(next))
	}

	mux.Handle("GET /api/v1/franchises",
		franchiseRead(http.HandlerFunc(h.ListFranchises)))
	mux.Handle("POST /api/v1/franchises",
		admin(http.HandlerFunc(h.CreateFranchise)))
	// Registered before the {franchise_id} pattern would matter: Go 1.22's
	// mux prefers the more specific literal path, so "consolidated-pnl" is
	// never parsed as a franchise id. Ordering here is documentation, not
	// the mechanism.
	mux.Handle("GET /api/v1/franchises/consolidated-pnl",
		admin(http.HandlerFunc(h.GetConsolidatedPnL)))
	mux.Handle("GET /api/v1/franchises/{franchise_id}/pnl",
		franchiseRead(http.HandlerFunc(h.GetFranchisePnL)))

	// Report export and scheduling (FR-RPT-002 | MDS §4.8).
	//
	// franchiseRead, so an LCO can pull their own numbers. The handlers scope
	// every query from the caller's own token, and a franchise-bound caller
	// cannot name a different franchise in the query string. The same tier
	// already reads the P&L above, which is strictly more sensitive than these
	// aggregates.
	//
	// The literal "exports" path is registered before the {report} pattern for
	// readability; Go 1.22's mux prefers the more specific literal regardless,
	// so "exports" is never parsed as a report name.
	mux.Handle("GET /api/v1/reports/exports/{id}",
		franchiseRead(http.HandlerFunc(h.GetReportExport)))
	mux.Handle("GET /api/v1/reports/{report}",
		franchiseRead(http.HandlerFunc(h.GetReport)))
	mux.Handle("POST /api/v1/reports/{report}/export",
		franchiseRead(http.HandlerFunc(h.RequestReportExport)))

	// Franchise-scoped subscriber listing. revenue.ListSubscribersHandler and
	// revenue.FranchiseMiddleware have existed since v2.0 with no route
	// mounting them; this is that mount. The middleware must wrap the
	// handler, not the other way round — the handler reads the scope the
	// middleware injects.
	if h.subscriberLister != nil {
		mux.Handle("GET /api/v1/franchises/subscribers",
			franchiseRead(revenue.FranchiseMiddleware(
				revenue.ListSubscribersHandler(h.subscriberLister))))
	}

	// Partner-facing surface (FR-API-001..003 | MDS §4.22).
	//
	// Authenticated by API key, never by JWT. The two middlewares are separate
	// on purpose: APIKeyMiddleware sets no role in the context, so a partner
	// key cannot satisfy any RequireRole check even if a route were wired
	// wrongly — the separation FR-API-001 asks for is structural rather than a
	// convention to be remembered.
	if h.partnerAuth != nil {
		apiKey := middleware.APIKeyMiddleware(h.partnerAuth)
		manageWebhooks := func(next http.Handler) http.Handler {
			return apiKey(middleware.RequireScope(partner.ScopeManageWebhooks)(next))
		}
		mux.Handle("POST /api/v1/partner/webhooks",
			manageWebhooks(http.HandlerFunc(h.CreateWebhookEndpoint)))
		mux.Handle("GET /api/v1/partner/webhooks",
			manageWebhooks(http.HandlerFunc(h.ListWebhookEndpoints)))
		mux.Handle("DELETE /api/v1/partner/webhooks/{id}",
			manageWebhooks(http.HandlerFunc(h.DeleteWebhookEndpoint)))
		mux.Handle("GET /api/v1/partner/webhooks/{id}/deliveries",
			manageWebhooks(http.HandlerFunc(h.ListWebhookDeliveries)))

		// Read-only data access for integrations, each behind its own scope.
		mux.Handle("GET /api/v1/partner/subscribers/{id}",
			apiKey(middleware.RequireScope(partner.ScopeReadSubscribers)(
				http.HandlerFunc(h.GetSubscriber))))
	}

	// Webhooks (no JWT — uses HMAC)
	mux.HandleFunc("POST /webhooks/razorpay",
		h.RazorpayWebhook)
}

// ── Subscribers ──────────────────────────────────────────────────────────────

// CreateSubscriber handles POST /api/v1/subscribers.
// Hashes password, encrypts PII, persists subscriber.
func (h *Handler) CreateSubscriber(w http.ResponseWriter, r *http.Request) {
	var req CreateSubscriberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}

	created, err := h.ProvisionSubscriber(r.Context(), req)
	switch {
	case errors.Is(err, ErrSubscriberInvalid):
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	case errors.Is(err, ErrSubscriberExists):
		writeError(w, http.StatusConflict, "ERR_CONFLICT", "CAF number or username already exists")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create subscriber failed")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// Sentinel errors so a caller that is not an HTTP handler - the operations
// console (internal/staffui) - can tell a rejected input from a conflict
// from a genuine failure without parsing message text.
var (
	// ErrSubscriberInvalid wraps a validation failure; its message is safe
	// to show an operator, since validateCreateSubscriber writes it for
	// exactly that purpose.
	ErrSubscriberInvalid = errors.New("subscriber details are not valid")
	// ErrSubscriberExists reports a duplicate CAF number or username.
	ErrSubscriberExists = errors.New("subscriber already exists")
)

// ProvisionSubscriber validates, creates and KYC-encrypts one subscriber,
// returning the stored record.
//
// Exported and shared rather than left inline in the HTTP handler because
// the console needs the identical path: this is where a password is hashed
// and Aadhaar/PAN are encrypted, and the note below on the extracted
// helpers applies with more force to a second *entry point* than to a
// second helper - a console that grew its own creation path is precisely
// how one of them ends up quietly skipping the encryption step
// (FR-SEC-002, MDS §4.16). The audit entry and partner webhook fire here
// too, so an operator-created subscriber is as traceable as an
// API-created one.
func (h *Handler) ProvisionSubscriber(ctx context.Context, req CreateSubscriberRequest) (*SubscriberRecord, error) {
	if err := validateCreateSubscriber(req); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSubscriberInvalid, err.Error())
	}

	hash, err := hashSubscriberPassword(req.Password)
	if err != nil {
		return nil, fmt.Errorf("hash subscriber password: %w", err)
	}

	created, err := h.db.CreateSubscriber(ctx, subscriberRecordFrom(req), hash)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSubscriberExists
		}
		return nil, fmt.Errorf("create subscriber: %w", err)
	}

	h.persistKYC(ctx, created.ID, req.Aadhaar, req.PAN)

	middleware.Audit(ctx, "subscriber.create", strconv.Itoa(created.ID), nil)
	h.emit(ctx, partner.EventSubscriberCreated, created.ID)
	return created, nil
}

// ── Shared subscriber-creation steps ─────────────────────────────────────────
//
// CreateSubscriber and ConvertLead (internal/api/crm.go) both need to hash a
// password, build the same starting SubscriberRecord and store encrypted
// KYC. These are extracted rather than copied: two divergent copies of a
// PII-encryption path is exactly how one of them ends up quietly skipping
// the encryption step (FR-SEC-002, MDS §4.16).

// hashSubscriberPassword applies the bcrypt cost this codebase standardises
// on for subscriber credentials.
func hashSubscriberPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// subscriberRecordFrom builds the starting state every new subscriber shares:
// active, undunned, KYC pending, empty wallet.
func subscriberRecordFrom(req CreateSubscriberRequest) SubscriberRecord {
	// Stored canonical rather than as typed. validateCreateSubscriber has
	// already rejected anything unresolvable, so the fallback here is
	// unreachable in practice; keeping the original value rather than
	// silently emptying the column is still the right behaviour if that
	// ever stops being true.
	state := req.RegisteredState
	if canonical, ok := billing.NormaliseState(state); ok {
		state = canonical
	}
	return SubscriberRecord{
		CAFNumber:       req.CAFNumber,
		Username:        req.Username,
		MobileNumber:    req.MobileNumber,
		Email:           req.Email,
		PlanID:          req.PlanID,
		RegisteredState: state,
		// Payment precedes authorisation. Created 'active', a subscriber was
		// on the network from the moment the form was submitted — and because
		// no creation path set plan_expiry, both billing scanners skipped them
		// forever, so nothing ever came to collect. Free service with no
		// mechanism that could ever end it.
		//
		// pending_payment grants nothing (radius.AuthorisesService), and the
		// auto-renewal scanner activates them the moment their wallet covers
		// the first cycle, stamping plan_expiry as it charges. Dunning takes
		// over from there.
		Status:       "pending_payment",
		DunningState: "active",
		// dunning_state is 'active' rather than tracking the pending status:
		// the dunning ladder describes how far behind a paying subscriber has
		// fallen, and someone who has not started yet is not behind. They are
		// invisible to the dunning scanner regardless, which requires a
		// plan_expiry they do not have until activation.
		KYCStatus:     "pending",
		WalletBalance: "0.00",
	}
}

// persistKYC encrypts and stores Aadhaar/PAN if either was supplied.
//
// Best-effort by design, matching the behaviour this path has always had: a
// subscriber who exists without a KYC record can re-submit it, whereas
// failing the whole creation would leave a paying customer uncreated over an
// optional document.
func (h *Handler) persistKYC(ctx context.Context, subscriberID int, aadhaar, pan string) {
	if (aadhaar == "" && pan == "") || h.keyStore == nil || h.kycDB == nil {
		return
	}
	enc, err := crypto.NewAESEncryptor(h.keyStore)
	if err != nil {
		log.Warn().Err(err).Msg("api: KYC encryptor unavailable; subscriber created without KYC")
		return
	}
	aadhaarEnc, panEnc := "", ""
	if aadhaar != "" {
		aadhaarEnc, _ = enc.Encrypt(aadhaar) //nolint:errcheck // logged via the persist error below
	}
	if pan != "" {
		panEnc, _ = enc.Encrypt(pan) //nolint:errcheck
	}
	if err := h.kycDB.UpsertKYC(ctx, subscriberID, aadhaarEnc, panEnc, h.keyStore.ActiveVersion()); err != nil {
		log.Warn().Err(err).Msg("api: KYC persist failed; subscriber created without KYC")
	}
}

// GetSubscriber handles GET /api/v1/subscribers/{id}.
func (h *Handler) GetSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	sub, err := h.db.GetSubscriberByID(r.Context(), id)
	if err != nil || sub == nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND",
			fmt.Sprintf("Subscriber with ID %d not found.", id))
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// UpdateSubscriber handles PATCH /api/v1/subscribers/{id}.
func (h *Handler) UpdateSubscriber(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var body struct {
		PlanID *int    `json:"plan_id"`
		Status *string `json:"status"`
		// PlanExpiry backs OPS §12.2.4's grace-period extension. It was
		// documented as a supported field long before it was one: the
		// decoder below ignored anything it did not recognise, so that
		// procedure returned 200 with the subscriber unchanged and an
		// operator following the runbook during an incident would believe
		// they had extended a customer who then got suspended anyway.
		PlanExpiry *time.Time `json:"plan_expiry"`
	}
	// DisallowUnknownFields is what stops that recurring. A silently
	// discarded field is worse than a rejected one in both directions — the
	// caller thinks the change landed, and nothing anywhere records that it
	// did not. A 400 naming the field is a bad request; a 200 that did
	// nothing is a lie.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	updated, err := h.db.UpdateSubscriber(r.Context(), id, body.PlanID, body.Status, body.PlanExpiry)
	if err != nil {
		// Migration 048's invariant: an authorising status requires a
		// plan_expiry, or the subscriber is online with nothing that can bill
		// them. Reached by switching someone back to active without also
		// dating their cycle, which is a bad request and not a server fault —
		// and the message has to say what to do instead, because the operator
		// is usually mid-incident and reaching for the obvious lever.
		if isBillabilityViolation(err) {
			writeError(w, http.StatusBadRequest, "ERR_NOT_BILLABLE",
				"cannot set this status without a plan_expiry: the subscriber would be online "+
					"with no billing cycle. Send plan_expiry in the same request, or credit their "+
					"wallet and let auto-renewal activate them.")
			return
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update failed")
		return
	}
	middleware.Audit(r.Context(), "subscriber.update", strconv.Itoa(id), map[string]any{
		"plan_id": body.PlanID, "status": body.Status, "plan_expiry": body.PlanExpiry,
	})
	// Only on an actual status change. A plan-only edit is not a lifecycle
	// event, and emitting one would have partners reacting to a suspension
	// that never happened.
	if body.Status != nil {
		h.emit(r.Context(), partner.EventSubscriberStatusChanged, id)
	}
	writeJSON(w, http.StatusOK, updated)
}

// GetSubscriberHealth handles GET /api/v1/subscribers/{id}/health (FR-OBS-004).
//
// The response body is assembled by internal/health; this method exists so the
// route is registered — and therefore authorised — in one place with the rest
// of the API surface. 503 rather than 501 when unconfigured, matching every
// other optional dependency here: the endpoint exists, its backing collaborator
// is absent.
func (h *Handler) GetSubscriberHealth(w http.ResponseWriter, r *http.Request) {
	if h.health == nil {
		http.Error(w, "subscriber health is not configured", http.StatusServiceUnavailable)
		return
	}
	h.health.ServeHTTP(w, r)
}

// ── Wallets ──────────────────────────────────────────────────────────────────

// WalletRecharge handles POST /api/v1/wallets/recharge.
func (h *Handler) WalletRecharge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SubscriberID     int    `json:"subscriber_id"`
		Amount           string `json:"amount"`
		PaymentMethod    string `json:"payment_method"`
		TransactionToken string `json:"transaction_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "invalid amount")
		return
	}
	tx, err := h.walletSvc.Recharge(r.Context(), billing.RechargeRequest{
		SubscriberID:     req.SubscriberID,
		Amount:           amount,
		TransactionToken: req.TransactionToken,
		Description:      "recharge via " + req.PaymentMethod,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
		return
	}
	middleware.Audit(r.Context(), "wallet.recharge", strconv.Itoa(req.SubscriberID), map[string]any{
		"amount": req.Amount, "method": req.PaymentMethod,
	})
	if h.franchises != nil {
		revenue.SettleCommissionForRecharge(r.Context(), h.franchises, req.SubscriberID, amount, req.TransactionToken)
	}
	h.enqueuePaymentReceipt(r.Context(), req.SubscriberID, req.Amount, tx.BalanceAfter.String())
	writeJSON(w, http.StatusOK, tx)
}

// enqueuePaymentReceipt tells the subscriber their money arrived
// (FR-NOTIF-004), and where the payment lifted a suspension, that they are
// back online (FR-NOTIF-006).
//
// Failures are logged, never returned: the money is already banked and the
// ledger already written by this point, so failing the request over an
// undelivered receipt would tell the caller their payment did not go through
// when it did.
func (h *Handler) enqueuePaymentReceipt(ctx context.Context, subscriberID int, amount, newBalance string) {
	if h.tasks == nil || h.db == nil {
		return
	}

	sub, err := h.db.GetSubscriberByID(ctx, subscriberID)
	if err != nil || sub == nil {
		log.Warn().Err(err).Int("subscriber_id", subscriberID).
			Msg("api: payment receipt skipped — subscriber lookup failed")
		return
	}

	// Records the state at the moment the money arrived, for the log. It no
	// longer changes what the subscriber is told: this handler credits a
	// wallet and says so, and the renewal scanner announces restoration when
	// it actually restores them.
	wasSuspended := sub.Status == "grace_period" || sub.Status == "soft_suspended" || sub.Status == "hard_suspended"

	payload, err := json.Marshal(billing.PaymentReceiptPayload{
		SubscriberID: subscriberID,
		Username:     sub.Username,
		Amount:       amount,
		NewBalance:   newBalance,
		WasSuspended: wasSuspended,
	})
	if err != nil {
		log.Warn().Err(err).Msg("api: payment receipt payload marshal failed")
		return
	}

	task := jobqueue.NewTask(billing.TaskTypePaymentReceipt, payload,
		jobqueue.Queue(billing.QueueNotifications),
		jobqueue.MaxRetry(3),
		jobqueue.Retention(24*time.Hour))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Warn().Err(err).Int("subscriber_id", subscriberID).
			Msg("api: payment receipt enqueue failed")
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"code": errCode, "message": msg})
}

func pathInt(r *http.Request, key string) (int, error) {
	return strconv.Atoi(r.PathValue(key))
}

// isBillabilityViolation reports whether err is migration 048's
// chk_authorised_subscriber_is_billable rejecting an authorising status with
// no plan_expiry.
//
// Matched on the constraint name rather than the SQLSTATE, because 23514 is
// every CHECK on the table and only this one is a bad request; the others
// (a negative wallet balance, say) really are faults worth a 500.
func isBillabilityViolation(err error) bool {
	return err != nil && contains(err.Error(), "chk_authorised_subscriber_is_billable")
}

func isUniqueViolation(err error) bool {
	return err != nil && len(err.Error()) > 0 &&
		(contains(err.Error(), "unique") || contains(err.Error(), "duplicate"))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func validateCreateSubscriber(req CreateSubscriberRequest) error {
	if req.CAFNumber == "" {
		return fmt.Errorf("caf_number is required")
	}
	if req.Username == "" {
		return fmt.Errorf("username is required")
	}
	if req.MobileNumber == "" {
		return fmt.Errorf("mobile_number is required")
	}
	if !validate.E164(req.MobileNumber) {
		return fmt.Errorf("mobile_number must be E.164 format (e.g. +919876543210)")
	}
	if req.PlanID == 0 {
		return fmt.Errorf("plan_id is required")
	}
	if req.RegisteredState == "" {
		return fmt.Errorf("registered_state is required")
	}
	// Resolved against the GST state registry, not merely checked for
	// non-emptiness. registered_state decides whether a subscriber is
	// billed CGST+SGST or IGST, and an unrecognised value silently takes
	// the interstate branch - so "Tamil Nadu" typed instead of "TN" filed
	// a Tamil Nadu customer's tax to the centre. Rejecting at entry is
	// what keeps that out of the ledger; see internal/billing/state.go.
	if _, ok := billing.NormaliseState(req.RegisteredState); !ok {
		return fmt.Errorf("registered_state %q is not a recognised Indian state: "+
			"use the two-letter code (TN), the GST code (33) or the full name (Tamil Nadu)",
			req.RegisteredState)
	}
	return nil
}
