// Command api runs the BSS/OSS HTTP API and the subscriber self-service portal.
//
// Wires the persistence layer, the live session store and the AES key store
// into the route handlers, then serves the API over HTTPS on API_ADDR and
// Prometheus metrics on METRICS_ADDR.
//
// IDD §8.1 | API §7
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/envfile"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/winservice"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/cache"
	"github.com/maaransoft/isp-bss-oss/internal/config"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/portalui"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
	"github.com/maaransoft/isp-bss-oss/internal/svclog"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
	"github.com/maaransoft/isp-bss-oss/pkg/chromium"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
	"github.com/maaransoft/isp-bss-oss/pkg/tlscert"
	"github.com/shopspring/decimal"
)

const (
	readTimeout     = 15 * time.Second
	writeTimeout    = 30 * time.Second
	idleTimeout     = 60 * time.Second
	shutdownTimeout = 15 * time.Second
	// tlsValidity is deliberately long. The certificate is self-signed with
	// no ACME renewal path (see pkg/tlscert), so a short validity would only
	// force an operator to re-trust it by hand on a schedule nobody asked
	// for, with no security benefit — nothing is validating this chain.
	tlsValidity = 10 * 365 * 24 * time.Hour

	// serviceName is the Windows service name register_services.ps1
	// registers this binary under. It doubles as the Event Log source
	// winservice.Fatal writes a startup failure to when there is no
	// console to print one to.
	serviceName = "ISPBSSApi"
)

func main() {
	envFile := flag.String("env-file", os.Getenv("ISP_ENV_FILE"),
		"dotenv file to load before reading configuration (for Windows services, which start with no shell to source app.env)")
	flag.Parse()

	if err := envfile.Load(*envFile); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}

	// Under the Service Control Manager there is no SIGTERM and no console
	// — the SCM sends stop requests on its own channel and winservice.Run
	// bridges that to the same ctx cancellation run() already expected.
	// Interactively, ctx is cancelled by Ctrl+C or SIGTERM exactly as
	// before; the two paths converge back into the one run(ctx) below.
	if winservice.IsWindowsService() {
		if err := winservice.Run(serviceName, run); err != nil {
			winservice.Fatal(serviceName, err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		// zerolog is configured inside run(); if it failed before that, stderr
		// is the only channel guaranteed to work.
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load("api")
	if err != nil {
		return err
	}
	svclog.Configure(cfg, "api")

	log.Info().Interface("config", cfg.Redact()).Msg("api: starting")

	// ── Dependencies ────────────────────────────────────────────────────────

	database, err := db.Connect(ctx, dbConfig(cfg))
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer database.Close()
	log.Info().Msg("api: PostgreSQL connected")

	keyStore, err := crypto.LoadKeyStore(cfg.AESKeyStoreURL)
	if err != nil {
		return fmt.Errorf("load AES key store: %w", err)
	}
	log.Info().Str("active_version", keyStore.ActiveVersion()).Msg("api: key store loaded")

	// Encrypts webhook signing secrets at rest (FR-API-002). Built once at
	// startup so a broken key store fails here rather than on the first
	// partner registration.
	encryptor, err := crypto.NewAESEncryptor(keyStore)
	if err != nil {
		return fmt.Errorf("build AES encryptor: %w", err)
	}

	// Live session state, written by radiusd's accounting path and read here
	// for the health endpoint and the portal's live-usage panel. Postgres
	// rather than Redis since the move off Docker — see internal/cache's
	// package doc.
	sessions := cache.NewSessionStore(database.Pool())

	// The API enqueues session-control (PoD/CoA) tasks for the radiusd worker
	// pool to execute; it never talks RADIUS itself. The queue is a table in
	// the same PostgreSQL (internal/jobqueue), so a task and the row whose
	// change scheduled it commit against the same database.
	taskClient := jobqueue.NewClient(database.Pool())
	defer taskClient.Close() //nolint:errcheck

	// PDF generation is optional, the same rule Razorpay below follows:
	// GetInvoicePDF reports 503 rather than the process refusing to start
	// when no browser is found. pkg/chromium.Locate auto-detects Microsoft
	// Edge or Google Chrome when CHROMIUM_PATH is unset — invoice PDFs no
	// longer go to a Gotenberg container, there is nowhere left to run one.
	var pdfGen api.PDFGenerator
	if chromiumPath, err := chromium.Locate(cfg.ChromiumPath); err != nil {
		log.Warn().Err(err).Msg("api: no Chromium-based browser found — invoice PDF downloads will return 503")
	} else {
		pdfGen = billing.NewInvoicePDFClient(chromiumPath)
		log.Info().Str("path", chromiumPath).Msg("api: invoice PDF renderer configured")
	}

	// Razorpay order creation is optional: /portal/renew reports 503 rather
	// than the process refusing to start when credentials are not configured.
	var razorpayClient portal.RazorpayOrderCreator
	if cfg.RazorpayKeyID != "" && cfg.RazorpayKeySecret != "" {
		razorpayClient = billing.NewRazorpayClient(cfg.RazorpayKeyID, cfg.RazorpayKeySecret)
		log.Info().Msg("api: Razorpay payment link client configured")
	} else {
		log.Warn().Msg("api: RAZORPAY_KEY_ID/RAZORPAY_KEY_SECRET unset — /portal/renew will return 503")
	}

	// ── Handlers ────────────────────────────────────────────────────────────

	walletSvc := billing.NewWalletService(database.Billing())

	// Built before the API handler so it can be passed in as a dependency:
	// registering it on the mux afterwards would place it outside the API's
	// authorisation middleware.
	healthHandler := health.NewHandler(database.Health(), sessions)

	apiHandler := api.NewHandler(api.HandlerDeps{
		DB:       database.API(),
		KYC:      database.API(),
		Wallet:   walletSvc,
		KeyStore: keyStore,

		Ledger:     database.Billing(),
		Sessions:   sessions,
		SessionCtl: database.FUP(),
		Tasks:      taskClient,
		Invoices:   database.Billing(),
		PDF:        pdfGen,
		Tickets:    database.Tickets(),
		LEA:        database.FUP(),
		LEAAudit:   database.FUP(),
		// Franchise/LCO (FR-FRN-003..006). RevenueStore satisfies both:
		// the commission engine and the P&L reporting read the same tables.
		Franchises:       database.Revenue(),
		SubscriberLister: database.Revenue(),
		// Subscriber lifecycle (FR-LC-001..003, FR-BIL-010..011). APIStore
		// satisfies LifecycleQuerier; BillingStore satisfies RefundQuerier
		// (it already owns wallet_ledgers, which payment_refunds references).
		Lifecycle: database.API(),
		Refunds:   database.Billing(),
		SubCache:  &subCacheInvalidator{},
		// Task & approval workflows (FR-WFL-001..002). WorkflowStore
		// satisfies both queriers — approvals and field tasks share a
		// migration and a store, but nothing else.
		Approvals:  database.Workflow(),
		FieldTasks: database.Workflow(),
		// CRM lead pipeline and CPE inventory (FR-CRM, FR-INV). Separate
		// stores: they share a migration and the conversion moment, nothing
		// else.
		Leads:     database.CRM(),
		Inventory: database.Inventory(),
		// Announcements and push-token registration (FR-ANN, FR-NOTIF-013).
		Announcements: database.Announcements(),
		PushTokens:    database.Notifications(),
		// EAP-MSCHAPv2 enrolment (FR-AAA-006).
		EAPEnrolment: database.API(),
		Credentials:  database.API(),
		// TR-069 remote control (FR-CPE-003).
		CPEControl: database.TR069(),
		// Partner API and webhooks (FR-API-001..003 | MDS §4.22). Partners and
		// PartnerAuth are the same store behind two interfaces: one for
		// management, one for the authentication middleware, kept separate so a
		// handler cannot reach the authenticator by accident.
		Partners:        database.Partner(),
		PartnerAuth:     database.Partner(),
		SecretEncryptor: encryptor,
		// Fans lifecycle events out to subscribed partner endpoints. Emission
		// never fails the operation that triggered it — a third party's
		// configuration must not be able to break subscriber creation.
		Events: partner.NewEmitter(database.Partner(), taskClient),
		// Hotspot voucher issuance and MAB device registration (FR-HSP-001..002).
		// The same store backs the captive portal below; the split is in the
		// interfaces, not the data — staff can mint what the public portal can
		// only redeem.
		Hotspot: database.Hotspot(),
		// NAS inventory management (FR-NAS-001..004). Removes the direct-SQL
		// prerequisite for registering a NAS and for turning on allow_mab, which
		// a hotspot deployment cannot do without.
		NAS: database.NAS(),
		// Report export and scheduling (FR-RPT-002). The views existed since
		// migration 032 with no HTTP surface serving them; Archives backs the
		// status lookup for a queued export, which is delivered into the same
		// archival storage FR-DOC-001 built.
		Reports:       database.Reporting(),
		Archives:      database.Archive(),
		Procurement:   database.Procurement(),
		GeneralLedger: database.Ledger(),
		Health:        http.HandlerFunc(healthHandler.GetSubscriberHealth),

		RazorpayWebhookSecret: cfg.RazorpayWebhookSecret,
	})

	portalHandler := portal.NewHandler(
		database.Portal(),
		sessions.Portal(),
		database.Portal(),
		database.Portal(),
		razorpayClient,
		cfg.PortalJWTSecret,
	)
	portalHandler.SetRenewalProcessor(&renewalProcessor{
		wallet:     walletSvc,
		planExpiry: database.Portal(),
		invoicing:  database.Billing(),
		franchises: database.Revenue(),
	})

	portalUIHandler := portalui.NewHandler(portalui.Deps{
		Subscribers:    database.Portal(),
		Sessions:       sessions.Portal(),
		SessionHistory: database.Portal(),
		Invoices:       database.Billing(),
		PDF:            pdfGen,
		Razorpay:       razorpayClient,
		Tickets:        database.Portal(),
		Notifications:  database.Portal(),
		JWTSecret:      cfg.PortalJWTSecret,
	})

	// Operations console. It signs sessions with the API JWT secret, not the
	// portal one, so the token it issues is exactly what the JSON API validates
	// — the console cannot grant reach the API would refuse.
	staffUIHandler := staffui.NewHandler(staffui.HandlerDeps{
		Staff:       database.Staff(),
		Subscribers: database.API(),
		Health:      database.Health(),
		Sessions:    sessions.Portal(),
		Billing:     database.Billing(),
		Tickets:     staffTicketStore{portal: database.Portal(), admin: database.Tickets()},
		LEA:         database.FUP(),
		Revenue:     database.Revenue(),
		Catalogue:   database.Catalogue(),
		GSTR1:       database.Billing(),
		GSTSupplier: billing.Supplier{
			GSTIN: cfg.GSTSupplierGSTIN,
			State: cfg.GSTHomeState,
			Name:  cfg.GSTSupplierName,
		},
		// The API handler itself, not a store: creating a subscriber from
		// the console has to hash the password, encrypt KYC and write the
		// audit entry exactly as the API route does, and apiHandler owns
		// that path (api.ProvisionSubscriber).
		SubscriberCreator: apiHandler,
		// Same encryptor instance internal/api's NAS handlers use, so a
		// secret saved from the console decrypts identically to one saved
		// through the JSON API.
		NAS:             database.NAS(),
		SecretEncryptor: encryptor,
		Demo:            database.Demo(),
		TicketCreator:   database.Tickets(),
		InvoiceSeeder:   database.Billing(),
		SpeedOverride:   database.FUP(),
		// Same *api.Handler instance as SubscriberCreator above: the console
		// bulk toolbar calls straight into its exported *ForMany methods,
		// not a second copy of the plan-change/status/credit/notify logic.
		BulkActions: apiHandler,
		Tasks:       taskClient,
		JWTSecret:   cfg.JWTSecret,
		// Same *db.RevenueStore instance as Revenue above — it already
		// implements FranchiseStore, so the Franchises screen needs no
		// store of its own.
		Franchises: database.Revenue(),
		// Same *db.InventoryStore instance already wired for internal/api.
		Inventory: database.Inventory(),
		// Same *db.ReportingStore instance already wired for internal/api.
		Reporting: database.Reporting(),
		// Same *db.WorkflowStore instance already wired for internal/api.
		FieldTasks: database.Workflow(),
		Approvals:  database.Workflow(),
		// The API handler itself, same as SubscriberCreator/BulkActions:
		// approving a request runs real wallet-credit/refund/terminate
		// logic behind stores only internal/api holds.
		ApprovalExecutor: apiHandler,
		// Same *db.ProcurementStore instance already wired for internal/api.
		Procurement: database.Procurement(),
		// Same *db.LedgerStore instance already wired for internal/api.
		GeneralLedger: database.Ledger(),
		// Same *db.HotspotStore instance already wired for internal/api.
		VoucherCommissions: database.Hotspot(),
		// Same *db.NASStore instance already wired as NAS above.
		NetworkHealth: database.NAS(),
	})

	// Captive portal (FR-HSP-001 | MDS §4.23). Unauthenticated by necessity —
	// its visitors have no account and no network yet — so the attempt limiter
	// is not optional: an unconfigured one makes the portal return 503 rather
	// than accept unmetered guesses at the voucher space.
	hotspotHandler := hotspot.NewHandler(hotspot.Deps{
		Grants:      database.Hotspot(),
		Subscribers: database.Portal(),
		Limiter:     hotspot.NewLimiter(),
	})

	notificationWebhook := notifications.NewWebhookHandler(
		database.Notifications(), cfg.WhatsAppAppSecret, cfg.WhatsAppWebhookVerifyToken)

	mux := http.NewServeMux()
	apiHandler.RegisterRoutes(mux, cfg.JWTSecret)
	portalHandler.RegisterRoutes(mux)
	portalUIHandler.RegisterRoutes(mux)
	staffUIHandler.RegisterRoutes(mux)
	hotspotHandler.RegisterRoutes(mux)

	// Meta delivery callbacks: GET is the subscription handshake, POST carries
	// delivery statuses. Both are HMAC- or token-verified, never JWT.
	mux.HandleFunc("GET /webhooks/whatsapp", notificationWebhook.Verify)
	mux.HandleFunc("POST /webhooks/whatsapp/status", notificationWebhook.HandleDeliveryStatus)

	// TR-069 ACS (FR-CPE-001..003 | MDS §4.19). No JWT: CWMP devices
	// authenticate with their own HTTP credentials, not staff tokens, and
	// the endpoint is CPE-initiated by protocol design. It identifies the
	// device from the Inform's serial number and manages only devices it
	// already knows or chooses to record.
	acs := tr069.NewACS(database.TR069())
	mux.Handle("POST /tr069", acs)

	mux.HandleFunc("GET /readyz", readinessHandler(database))

	// ── Servers ─────────────────────────────────────────────────────────────

	// TLS is terminated here rather than by a reverse proxy in front. On
	// Docker Compose that was Caddy's job (config/caddy/Caddyfile, which
	// pinned `protocols tls1.3` for NFR-SEC-001); a single-machine native
	// install has no proxy container to run, and net/http serves TLS
	// directly. MinVersion TLS 1.3 reproduces that pin exactly — Go's
	// crypto/tls offers nothing newer that this could accidentally allow.
	cert, err := tlscert.LoadOrGenerate(cfg.TLSCertDir,
		[]string{cfg.TLSHostname, "localhost", "127.0.0.1"}, tlsValidity)
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	log.Info().Str("dir", cfg.TLSCertDir).Str("hostname", cfg.TLSHostname).
		Msg("api: TLS certificate ready (self-signed)")

	apiServer := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           requestLogger(mux),
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		},
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: readTimeout,
	}

	errCh := make(chan error, 2)

	go func() {
		log.Info().Str("addr", cfg.APIAddr).Msg("api: listening (HTTPS, TLS 1.3)")
		// Empty paths: the certificate and key are already in TLSConfig above.
		if err := apiServer.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("API server: %w", err)
		}
	}()

	go func() {
		log.Info().Str("addr", cfg.MetricsAddr).Msg("api: metrics listening")
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info().Msg("api: shutdown signal received")
	}

	// Drain in-flight requests before closing the pools they may still be using.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("api: graceful shutdown failed")
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("api: metrics shutdown failed")
	}

	log.Info().Msg("api: stopped")
	return nil
}

// renewalInvoicer is the subset of *db.BillingStore ApplyRenewal needs to
// generate the GST invoice for a completed renewal (FR-BIL-008).
type renewalInvoicer interface {
	GetInvoiceInputs(ctx context.Context, subscriberID int) (registeredState string, planVolumeGB int, err error)
	CreateInvoice(ctx context.Context, inv billing.Invoice) (int, error)
	GetActiveGstRate(ctx context.Context) (billing.GstRate, error)
}

// renewalProcessor credits a completed portal renewal through the wallet
// service, which supplies the idempotency the gateway callback needs,
// extends the subscriber's plan_expiry to match, and invoices the cycle.
type renewalProcessor struct {
	wallet     *billing.WalletService
	planExpiry portal.PlanExpiryStore
	invoicing  renewalInvoicer
	franchises revenue.FranchiseQuerier
}

func (p *renewalProcessor) ApplyRenewal(ctx context.Context, subscriberID int, amount decimal.Decimal, paymentID string) (*portal.RenewalPayment, error) {
	tx, err := p.wallet.Recharge(ctx, billing.RechargeRequest{
		SubscriberID:     subscriberID,
		Amount:           amount,
		TransactionToken: paymentID,
		Description:      "portal one-tap renewal",
	})
	if err != nil {
		return nil, fmt.Errorf("apply renewal: %w", err)
	}

	// The wallet credit above already committed and must not be undone just
	// because the expiry extension fails below — log and let ops reconcile,
	// rather than leaving a paid subscriber uncredited on a retry.
	if p.planExpiry != nil {
		if err := extendPlanExpiry(ctx, p.planExpiry, subscriberID, time.Now); err != nil {
			log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("renewal: plan expiry extension failed")
		}
	}

	// Same reasoning: the payment already landed, so a failure here is
	// logged for reconciliation rather than treated as the renewal failing.
	if p.invoicing != nil {
		if err := createRenewalInvoice(ctx, p.invoicing, subscriberID, amount); err != nil {
			log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("renewal: invoice creation failed")
		}
	}

	if p.franchises != nil {
		revenue.SettleCommissionForRecharge(ctx, p.franchises, subscriberID, amount, paymentID)
	}

	return &portal.RenewalPayment{TransactionID: tx.ID, Balance: tx.BalanceAfter}, nil
}

// createRenewalInvoice builds and persists the GST invoice for one renewal
// cycle. base_amount is the amount actually charged (what the subscriber
// paid via the Razorpay payment link), not the plan's list price — those can
// differ, and the invoice must reflect the real transaction.
//
// FR: FR-BIL-008 | MDS §4.14
func createRenewalInvoice(ctx context.Context, inv renewalInvoicer, subscriberID int, amount decimal.Decimal) error {
	registeredState, planVolumeGB, err := inv.GetInvoiceInputs(ctx, subscriberID)
	if err != nil {
		return fmt.Errorf("get invoice inputs: %w", err)
	}
	rate, err := inv.GetActiveGstRate(ctx)
	if err != nil {
		return fmt.Errorf("get active gst rate: %w", err)
	}

	invoice := billing.CalculateGstInvoice(amount, registeredState, rate)
	invoice.SubscriberID = subscriberID
	invoice.GbIncluded = planVolumeGB
	invoice.GbUsed = decimal.Zero

	if _, err := inv.CreateInvoice(ctx, invoice); err != nil {
		return fmt.Errorf("create invoice: %w", err)
	}
	return nil
}

// subCacheInvalidator used to delete a subscriber's RADIUS auth-cache entry
// from Redis after a lifecycle action (plan change, termination), so radiusd
// would reload it on the next Access-Request.
//
// That cache now lives in radiusd's own process memory (see
// internal/localcache), which this process cannot reach into — so this is a
// no-op, and invalidation falls back to what cache.SubscriberCache already
// documents as the backstop: its 60-second TTL. That is a bounded, already
// accounted-for delay rather than a regression in enforcement, because
// suspension does not rely on the cache expiring at all — it issues a
// Disconnect-Request, which takes effect immediately.
//
// Kept as a type rather than deleted so the api.HandlerDeps wiring and its
// SubCache interface stay intact: Phase 2 of the native port stands up
// Postgres LISTEN/NOTIFY for the job queue, and this becomes a NOTIFY the
// daemon subscribes to, closing the window to milliseconds.
type subCacheInvalidator struct{}

func (s *subCacheInvalidator) InvalidateSubscriber(_ context.Context, _ string) error {
	return nil
}

// extendPlanExpiry computes and applies the new plan_expiry for a renewal:
// max(now, currentExpiry) + validityDays. Extending from now unconditionally
// would silently discard remaining days for a subscriber who renews early;
// extending from a stale (already-lapsed) currentExpiry would grant days
// retroactively. max(now, currentExpiry) is the only rule that never loses
// paid-for days and never backdates the extension.
//
// now is injected (rather than calling time.Now directly) so the date math
// is unit-testable without a real clock.
func extendPlanExpiry(ctx context.Context, store portal.PlanExpiryStore, subscriberID int, now func() time.Time) error {
	validityDays, currentExpiry, err := store.GetPlanRenewalInfo(ctx, subscriberID)
	if err != nil {
		return fmt.Errorf("get plan renewal info: %w", err)
	}

	base := now()
	if currentExpiry != nil && currentExpiry.After(base) {
		base = *currentExpiry
	}
	newExpiry := base.AddDate(0, 0, validityDays)

	if err := store.SetPlanExpiry(ctx, subscriberID, newExpiry); err != nil {
		return fmt.Errorf("set plan expiry: %w", err)
	}
	return nil
}

// readinessHandler reports whether the backing store is reachable, so an
// orchestrator can withhold traffic from an instance that cannot serve it.
//
// Postgres only, and that is now the whole story: Redis used to be checked
// here too, and there is no longer a Redis to check — the caches and session
// state that made it load-bearing moved in-process or into Postgres, and the
// task queue followed (internal/jobqueue).
func readinessHandler(database *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := database.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`)) //nolint:errcheck
	}
}

// requestLogger records method, path, status and duration for every request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Only the path is logged, never the query string: LEA lookups and
		// webhook callbacks carry identifiers that must not reach logs.
		log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rec.status).
			Dur("duration", time.Since(start)).
			Msg("http_request")
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func dbConfig(cfg *config.Config) db.Config {
	c := db.DefaultConfig(cfg.DBDSN)
	c.MaxConns = cfg.DBMaxConns
	c.MinConns = cfg.DBMinConns
	c.ConnectTimeout = cfg.DBConnTimeout
	return c
}

// staffTicketStore joins the two halves of ticket access the console needs:
// listing lives on PortalStore (it is the subscriber-scoped read) and the
// admin update lives on TicketStore. Adapting here keeps internal/staffui
// depending on one small interface rather than on two concrete stores.
type staffTicketStore struct {
	portal *db.PortalStore
	admin  *db.TicketStore
}

func (s staffTicketStore) ListTickets(ctx context.Context, subscriberID int) ([]portal.TicketEntry, error) {
	return s.portal.ListTickets(ctx, subscriberID)
}

func (s staffTicketStore) UpdateTicketAdmin(ctx context.Context, ticketID int, status *string, assignedTo *int, priority *string) (*api.TicketRecord, error) {
	return s.admin.UpdateTicketAdmin(ctx, ticketID, status, assignedTo, priority)
}
