// Command radiusd runs the RADIUS AAA daemon together with the background
// workers that react to what it observes: the FUP scanner, the task workers
// that send CoA and notifications, and the dead-letter monitor.
//
// These share a process because they all operate on live session state and
// would otherwise need a second copy of the same database wiring.
//
// IDD §8.1 | DDS §5.1, §5.3
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/envfile"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/winservice"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/cache"
	"github.com/maaransoft/isp-bss-oss/internal/config"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/maaransoft/isp-bss-oss/internal/svclog"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	metricsReadTimeout = 15 * time.Second
	shutdownTimeout    = 15 * time.Second
	workerConcurrency  = 20

	// serviceName is the Windows service name register_services.ps1
	// registers this binary under — see cmd/api's identical constant for
	// why it doubles as the Event Log source name.
	serviceName = "ISPBSSAaaCore"
)

func main() {
	envFile := flag.String("env-file", os.Getenv("ISP_ENV_FILE"),
		"dotenv file to load before reading configuration (for Windows services, which start with no shell to source app.env)")
	flag.Parse()

	if err := envfile.Load(*envFile); err != nil {
		fmt.Fprintf(os.Stderr, "radiusd: %v\n", err)
		os.Exit(1)
	}

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
		fmt.Fprintf(os.Stderr, "radiusd: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load("radiusd")
	if err != nil {
		return err
	}
	svclog.Configure(cfg, "radiusd")

	log.Info().Interface("config", cfg.Redact()).Msg("radiusd: starting")

	// A child of the ctx main() (or winservice, under a Windows service)
	// handed in, not that ctx directly: the errCh branch below calls
	// cancel() to stop every background worker as soon as one server fails
	// to start, and that must only ever reach into this process's own
	// workers — never back up into the caller's shutdown signal, which
	// main()/winservice still owns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ── Dependencies ────────────────────────────────────────────────────────

	database, err := db.Connect(ctx, dbConfig(cfg))
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer database.Close()
	log.Info().Msg("radiusd: PostgreSQL connected")

	// The task queue (CoA/PoD, notifications, webhooks, report exports)
	// runs on the same PostgreSQL as everything else — see
	// internal/jobqueue. Nothing in this process talks to Redis any more:
	// the caches that used to are in-process (internal/localcache) and live
	// session state is a table (migration 036).
	taskClient := jobqueue.NewClient(database.Pool())
	defer taskClient.Close() //nolint:errcheck

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// ── NAS multi-vendor attribute engine ────────────────────────────────────
	//
	// Optional, same as the API's PDF renderer/Razorpay: with AES_KEY_STORE_URL
	// unset, no resolver is built and every component that would use it
	// falls back to exactly today's behavior — one global RADIUS secret,
	// every Access-Accept/CoA gets the MikroTik VSA unconditionally. A
	// deployment not ready to register its NAS inventory yet is unaffected.
	// FR-NAS-001..004 | MDS §4.11
	// An unloadable key store is a warning, not a fatal error, for the same
	// reason newDispatcher below tolerates missing WhatsApp credentials:
	// RADIUS authentication is what keeps subscribers online, and refusing
	// to start over an optional enhancement takes authentication down with
	// it. This returned a fatal error when the resolver was first wired,
	// which crash-looped radiusd on the shipped docker-compose.yml —
	// aaa_core_daemon sets AES_KEY_STORE_URL but did not mount the key file
	// (fixed alongside this). Found by restarting the container, not by
	// reading the code: every test passed throughout, because none of them
	// runs main().
	// Shared by the NAS resolver and the webhook sender: both need to decrypt
	// secrets this keystore protects. Left nil when AES_KEY_STORE_URL is unset,
	// which disables both features rather than failing startup.
	var partnerKeyStore crypto.KeyStore
	var nasResolver *nas.Resolver
	switch cfg.AESKeyStoreURL {
	case "":
		log.Warn().Msg("radiusd: AES_KEY_STORE_URL unset — multi-vendor NAS support disabled, every NAS gets the MikroTik VSA")
	default:
		nasKeyStore, err := crypto.LoadKeyStore(cfg.AESKeyStoreURL)
		if err != nil {
			log.Error().Err(err).
				Msg("radiusd: AES key store unreadable — multi-vendor NAS support disabled, every NAS gets the MikroTik VSA and the global RADIUS secret")
			break
		}
		partnerKeyStore = nasKeyStore
		nasResolver = nas.NewResolver(database.NAS(), nasKeyStore, []byte(cfg.RadiusSecret), fup.DefaultCoAPort)
		if err := nasResolver.Refresh(ctx); err != nil {
			log.Warn().Err(err).Msg("radiusd: initial NAS device cache load failed, starting on the fallback default")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info().Msg("radiusd: NAS device resolver started")
			nasResolver.Run(ctx)
			log.Info().Msg("radiusd: NAS device resolver stopped")
		}()
		log.Info().Msg("radiusd: multi-vendor NAS support enabled")
	}

	// ── RADIUS daemon ───────────────────────────────────────────────────────

	// Auth reads go through an in-process cache, not straight to PostgreSQL:
	// SAD requires the hot path to stay off the database, and a direct lookup
	// per Access-Request misses the NFR-PERF-001 15ms p99 budget by roughly 3x.
	subscriberCache := cache.NewSubscriberCache(database.Radius(), cfg.SubscriberCacheTTL)

	daemon := radius.NewRadiusDaemon(cfg.RadiusAddr, []byte(cfg.RadiusSecret), subscriberCache, []byte(cfg.RadiusVerifierSecret))
	if nasResolver != nil {
		daemon.SetNASResolver(nasResolver)
	}
	// EAP-MSCHAPv2 (FR-AAA-006, MDS §4.18). Conversation state is held in
	// process memory: a single-machine install runs exactly one radiusd, so
	// consecutive packets of one authentication always land on it.
	daemon.SetEAPSessionStore(radius.NewEAPSessionStore())
	// MAC Auth Bypass for hotspot NAS devices (FR-HSP-002, MDS §4.23). Wiring
	// the querier does not enable MAB anywhere: nas_devices.allow_mab defaults
	// FALSE, so it stays unreachable until an operator turns it on for a
	// specific NAS.
	daemon.SetMABQuerier(database.Hotspot())
	// Session accounting (FR-AAA-003, DDS §5.2). This is what populates
	// subscriber_session_history, which the FUP scanner, the CoA sender, LEA
	// lookups and the portal's usage history all read — without it they each
	// query an empty table and quietly do nothing.
	daemon.SetAccountingStore(database.FUP())
	// Voucher-backed hotspot sessions have no subscriber row, so they cannot go
	// in session history; they are metered on their grant instead (FR-HSP-001).
	daemon.SetGrantUsageDB(database.Hotspot())
	// Live session state (DDS §5.9, IDD §8.4) — what the health endpoint and
	// the subscriber portal's live-usage panel read. Nothing wrote this before
	// the move off Redis: cmd/api constructed the store for reads and
	// scripts/demo_up.sh seeded it by hand for the demo, so on a real
	// deployment both surfaces reported "offline" permanently.
	liveSessions := cache.NewSessionStore(database.Pool())
	daemon.SetLiveSessionStore(liveSessions)
	// A session whose Accounting-Stop never arrives (crashed NAS, lost packet)
	// leaves a row nothing else deletes. Readers already filter on staleness,
	// so this only bounds table growth.
	sessionSweeper := cache.NewStalenessSweeper(database.Pool(), 0)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: live session staleness sweeper started")
		sessionSweeper.Run(ctx)
		log.Info().Msg("radiusd: live session staleness sweeper stopped")
	}()
	daemon.SetAcctAddr(cfg.RadiusAcctAddr)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Str("addr", cfg.RadiusAddr).Str("acct_addr", cfg.RadiusAcctAddr).
			Msg("radiusd: RADIUS listening")
		// Blocks until the listener fails or ctx is cancelled, at which point it
		// stops accepting and drains queued packets before returning.
		if err := daemon.StartContext(ctx); err != nil {
			errCh <- fmt.Errorf("RADIUS daemon: %w", err)
		}
	}()

	// ── FUP scanner ─────────────────────────────────────────────────────────

	scanner := fup.NewScanner(database.FUP(), taskClient)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: FUP scanner started")
		scanner.Run(ctx)
		log.Info().Msg("radiusd: FUP scanner stopped")
	}()

	// ── Dunning scanner ─────────────────────────────────────────────────────

	// Advances subscribers through remind_7d → … → hard_suspended and sends the
	// notice for each stage. The state machine it drives shipped complete and
	// tested but with no caller, so until this was wired nobody was reminded to
	// pay and nobody was suspended for not paying.
	dunningScanner := billing.NewDunningScanner(database.Billing(), taskClient)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: dunning scanner started")
		dunningScanner.Run(ctx)
		log.Info().Msg("radiusd: dunning scanner stopped")
	}()

	// ── Auto-renewal scanner ─────────────────────────────────────────────────

	// Charges a subscriber for their current plan out of an already-funded
	// wallet balance the moment plan_expiry lapses, rather than leaving them
	// to dunning's purely time-based schedule while their money sits
	// uncharged. Runs every 15 minutes — shorter than dunning's hourly tick
	// — so it reliably gets a chance to renew a funded subscriber first
	// (MDS §4.14, FR-BIL-009).
	autoRenewalScanner := billing.NewRecurringBillingScanner(
		database.Billing(), billing.NewWalletService(database.Billing()))
	// Publishes invoice.generated to subscribed partners (FR-API-002). Renewal
	// itself never depends on this succeeding.
	autoRenewalScanner.SetEventEmitter(partner.NewEmitter(database.Partner(), taskClient))
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: auto-renewal scanner started")
		autoRenewalScanner.Run(ctx)
		log.Info().Msg("radiusd: auto-renewal scanner stopped")
	}()

	// ── Nightly revenue reconciliation ──────────────────────────────────────

	// ReconcileJob shipped complete, tested, and documented as running nightly
	// at 02:00 IST — and was never constructed. No snapshot was ever written,
	// no ledger variance ever compared. logAlerter is shared with the
	// dead-letter monitor so a variance surfaces the same way as any other
	// alert while PagerDuty delivery remains unimplemented.
	reconcileScheduler := revenue.NewReconcileScheduler(
		revenue.NewReconcileJob(database.Revenue(), logAlerter{}))
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: revenue reconciliation scheduler started")
		reconcileScheduler.Run(ctx)
		log.Info().Msg("radiusd: revenue reconciliation scheduler stopped")
	}()

	// ── SLA breach scanner ──────────────────────────────────────────────────

	// Notices tickets crossing their response/resolution deadlines and alerts
	// on breaches (FR-SUP-002). Shares logAlerter with the dead-letter monitor
	// and the revenue reconciliation: staff_users carries no contact details,
	// so there is no per-staff channel to notify and pretending otherwise
	// would be worse than routing through the alert path that already exists.
	slaScanner := tickets.NewSLAScanner(database.SLA(), logAlerter{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: SLA breach scanner started")
		slaScanner.Run(ctx)
		log.Info().Msg("radiusd: SLA breach scanner stopped")
	}()

	// ── Reporting view refresh ──────────────────────────────────────────────

	// mv_ticket_resolution (migration 032) computes a percentile across every
	// ticket ever filed, so it is materialised rather than recomputed per
	// page load. A materialised view with nothing refreshing it reports the
	// numbers that were true the day it was created, forever, with no outward
	// sign — so the refresh runs here rather than being left to a cron entry
	// somebody has to remember to add (FR-RPT-001).
	reportingRefresher := reporting.NewRefreshScanner(database.Reporting(), 0)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: reporting view refresh started")
		reportingRefresher.Run(ctx)
		log.Info().Msg("radiusd: reporting view refresh stopped")
	}()

	// ── Per-NAS auth failure alerting (FR-OBS-005 | SAD §3.2) ───────────────
	//
	// Evaluated in process because this deployment runs no Prometheus or
	// Alertmanager; deploy/prometheus/radius_alerts.yml carries the same rule
	// in PromQL for deployments that do. Shares logAlerter with the
	// dead-letter monitor and the SLA scanner — see the note there about why
	// the alert path is the log rather than a per-operator channel.
	authMonitor := radius.NewAuthFailureMonitor(logAlerter{}, nil)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: per-NAS auth failure monitor started")
		authMonitor.Run(ctx)
		log.Info().Msg("radiusd: per-NAS auth failure monitor stopped")
	}()

	// ── Voucher data caps (FR-HSP-001 | migration 035) ──────────────────────
	//
	// A voucher's data_cap_bytes was recorded and never read. Voucher sessions
	// have no subscriber row, so the FUP scanner cannot see them; they are
	// metered on the grant by RADIUS accounting and enforced here.
	quotaScanner := hotspot.NewQuotaScanner(database.Hotspot(),
		&voucherDisconnector{client: taskClient}, 0)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: voucher data-cap scanner started")
		quotaScanner.Run(ctx)
		log.Info().Msg("radiusd: voucher data-cap scanner stopped")
	}()

	// ── Document retention purge (FR-DOC-001 | MDS §4.24) ───────────────────
	//
	// Only runs when a backend is configured. Archival with no purge would be
	// worse than no archival: retain_until would record the date each document
	// should have been deleted while nothing deleted it, which under the DPDP
	// Act's storage-limitation principle is a violation the system has
	// documented against itself.
	// reportArchiver is nil when archival is off, which is what disables the
	// scheduled-export worker below: an export with nowhere to deliver would
	// run the query, produce the CSV and drop it.
	var reportArchiver *archive.Archiver
	if cfg.ArchiveDir != "" {
		archiveStore, err := archive.NewLocalStore(cfg.ArchiveDir)
		if err != nil {
			return fmt.Errorf("document archive storage: %w", err)
		}
		reportArchiver = archive.NewArchiver(archiveStore, database.Archive())
		purgeScanner := archive.NewPurgeScanner(archiveStore, database.Archive(), 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info().Str("dir", cfg.ArchiveDir).Msg("radiusd: document retention purge started")
			purgeScanner.Run(ctx)
			log.Info().Msg("radiusd: document retention purge stopped")
		}()
	} else {
		log.Warn().Msg("radiusd: ARCHIVE_DIR unset — document archival and retention purge are disabled")
	}

	// ── Task workers ────────────────────────────────────────────────────────

	dispatcher := newDispatcher(cfg, database)
	coaHandler := fup.NewCoAHandler(database.FUP(), []byte(cfg.RadiusSecret))
	podHandler := fup.NewPoDHandler(database.FUP(), []byte(cfg.RadiusSecret))
	if nasResolver != nil {
		coaHandler.SetNASResolver(nasResolver)
		podHandler.SetNASResolver(nasResolver)
	}
	warningHandler := fup.NewWarningHandler(dispatcher)
	throttledHandler := fup.NewThrottledHandler(dispatcher)
	dunningNoticeHandler := billing.NewDunningNoticeHandler(dispatcher)
	paymentReceiptHandler := billing.NewPaymentReceiptHandler(dispatcher)
	ticketUpdateHandler := tickets.NewUpdateHandler(dispatcher)
	announcementHandler := notifications.NewAnnouncementHandler(dispatcher)

	workerServer := jobqueue.NewServer(database.Pool(), jobqueue.Config{
		Concurrency: workerConcurrency,
		// network_commands outranks notifications: a CoA that restores a
		// subscriber's speed matters more than the message telling them about it.
		//
		// announcements sits lowest deliberately (MDS §4.17): a
		// 50,000-recipient marketing blast must never queue in front of a
		// payment receipt or a suspension notice.
		Queues: map[string]int{
			"network_commands": 6,
			"notifications":    3,
			"announcements":    1,
			// Webhooks sit below transactional notifications on purpose: a
			// partner retrying against a dead host must never delay a payment
			// receipt or a suspension notice (FR-API-003).
			"webhooks": 2,
			// Reports sit at the bottom with announcements: a ten-year
			// aggregate query is the slowest thing this pool runs, and a CoA
			// waiting behind one leaves a subscriber unthrottled for its
			// duration (FR-RPT-002).
			"reports": 1,
			"default": 1,
		},
		// Long enough to outlast the slowest handler this pool runs (a
		// ten-year report aggregate), so a task still executing is never
		// reclaimed and run a second time in parallel.
		LeaseDuration: 10 * time.Minute,
		ErrorHandler: func(_ context.Context, task *jobqueue.Task, err error) {
			log.Error().Err(err).Str("task_type", task.Type()).Msg("radiusd: task failed")
		},
	})

	workerMux := jobqueue.NewServeMux()
	workerMux.Handle(fup.TaskTypeCoA, coaHandler)
	workerMux.Handle(fup.TaskTypePoD, podHandler)
	workerMux.Handle(fup.TaskTypeFUPWarning, warningHandler)
	workerMux.Handle(fup.TaskTypeFUPThrottled, throttledHandler)
	workerMux.Handle(billing.TaskTypeDunningNotice, dunningNoticeHandler)
	workerMux.Handle(billing.TaskTypePaymentReceipt, paymentReceiptHandler)
	workerMux.Handle(tickets.TaskTypeTicketUpdate, ticketUpdateHandler)
	workerMux.Handle(notifications.TaskTypeAnnouncement, announcementHandler)
	// Scheduled report exports (FR-RPT-002). Registered only with somewhere to
	// deliver to — a worker that generates a CSV and discards it would report
	// success for an export nobody can collect.
	if reportArchiver != nil {
		workerMux.Handle(reporting.TaskTypeReportExport,
			reporting.NewExportHandler(database.Reporting(), reportArchiver))
	} else {
		log.Warn().Msg("radiusd: ARCHIVE_DIR unset — scheduled report exports will not be processed")
	}

	// Outbound partner webhooks (FR-API-002..003 | MDS §4.22). The sender
	// decrypts each endpoint's signing secret through the same AES keystore
	// that protects NAS secrets, and posts over an SSRF-guarded HTTP client.
	//
	// Registered only when a keystore is available: without one no signing
	// secret can be decrypted, and a worker that dequeued every webhook only to
	// abandon it would silently discard events a partner is waiting for.
	if partnerKeyStore != nil {
		webhookSender := partner.NewSender(database.Partner(), crypto.StoreDecryptor{Store: partnerKeyStore})
		workerMux.Handle(partner.TaskTypeWebhook, webhookSender)
		log.Info().Msg("radiusd: partner webhook sender registered")
	} else {
		log.Warn().Msg("radiusd: AES key store unavailable — partner webhooks will not be delivered")
	}

	if err := workerServer.Start(workerMux); err != nil {
		return fmt.Errorf("start task workers: %w", err)
	}
	log.Info().Int("concurrency", workerConcurrency).Msg("radiusd: task workers started")

	// ── Dead-letter monitor ─────────────────────────────────────────────────

	// Only the log sink exists: the PagerDuty Events v2 client is not
	// implemented, so say so rather than let a configured routing key imply
	// alerts are being delivered.
	log.Warn().
		Bool("pagerduty_key_set", cfg.PagerDutyRoutingKey != "").
		Msg("radiusd: PagerDuty delivery is not implemented — alerts go to logs only")

	monitor := fup.NewDeadLetterMonitor(jobqueue.NewInspector(database.Pool()), logAlerter{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: dead-letter monitor started")
		monitor.Run(ctx)
		log.Info().Msg("radiusd: dead-letter monitor stopped")
	}()

	// ── Metrics ─────────────────────────────────────────────────────────────

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: metricsReadTimeout,
	}
	go func() {
		log.Info().Str("addr", cfg.MetricsAddr).Msg("radiusd: metrics listening")
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	// ── Wait ────────────────────────────────────────────────────────────────

	select {
	case err := <-errCh:
		cancel()
		workerServer.Shutdown()
		return err
	case <-ctx.Done():
		log.Info().Msg("radiusd: shutdown signal received")
	}

	// The queue drains in-flight tasks first: a CoA abandoned mid-flight would leave
	// a subscriber throttled in the database but not on the NAS.
	workerServer.Shutdown()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("radiusd: metrics shutdown failed")
	}

	waitWithTimeout(&wg, shutdownTimeout)
	log.Info().Msg("radiusd: stopped")
	return nil
}

// newDispatcher builds the notification dispatcher the FUP warning worker uses.
//
// Missing WhatsApp credentials are not fatal: the RADIUS and CoA paths are what
// keep subscribers online, and refusing to start over an unset notification
// token would take authentication down with it.
func newDispatcher(cfg *config.Config, database *db.DB) *notifications.Dispatcher {
	store := database.Notifications()

	var whatsapp *notifications.WhatsAppClient
	if cfg.WhatsAppPhoneNumberID != "" && cfg.WhatsAppAccessToken != "" {
		whatsapp = notifications.NewWhatsAppClient(cfg.WhatsAppPhoneNumberID, cfg.WhatsAppAccessToken, store)
	} else {
		log.Warn().Msg("radiusd: WhatsApp credentials unset — WhatsApp notifications will fail")
	}

	var sms notifications.SMSSender
	if cfg.SMSAPIKey != "" {
		sms = notifications.NewMSG91Client(cfg.SMSAPIKey, cfg.SMSSenderID)
	} else {
		log.Warn().Msg("radiusd: SMS credentials unset — SMS notifications will fail")
	}

	dispatcher := notifications.NewDispatcher(store, whatsapp, sms)

	// Email and push follow the same rule as the two above: an unconfigured
	// channel is a warning, never a startup failure. A deployment that has
	// not bought a push provider should not lose dunning SMS (MDS §4.17).
	if cfg.SMTPHost != "" {
		dispatcher.SetEmailSender(notifications.NewSMTPClient(notifications.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
		}))
	} else {
		log.Warn().Msg("radiusd: SMTP_HOST unset — email notifications will fail")
	}

	if cfg.OneSignalAppID != "" && cfg.OneSignalAPIKey != "" {
		dispatcher.SetPushSender(notifications.NewOneSignalClient(cfg.OneSignalAppID, cfg.OneSignalAPIKey))
	} else {
		log.Warn().Msg("radiusd: OneSignal credentials unset — push notifications will fail")
	}

	return dispatcher
}

// alertsEmitted is the metric an external alerting pipeline scrapes.
//
// logAlerter is shared by four monitors — dead-letter, revenue reconciliation,
// SLA breach, and per-NAS auth failure — and a structured log line alone
// requires something to be tailing this process's stdout to ever see it. This
// counter is what lets an existing Prometheus/Alertmanager setup catch these
// the same way it catches everything else, without this codebase committing
// to a specific webhook or paging provider it cannot currently configure or
// test against.
var alertsEmitted = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "alerts_emitted_total",
	Help: "Operational alerts raised, by alert name",
}, []string{"alert_name"})

// logAlerter reports operational conditions to the log and to
// alerts_emitted_total, so the signal is visible both to a human reading this
// process's stdout and to a scraping pipeline that is not.
type logAlerter struct{}

// voucherDisconnector ends an exhausted voucher's session by queueing a
// Disconnect-Request.
//
// An adapter rather than internal/hotspot depending on the task queue
// directly: the captive-portal package has no other reason to know how
// background work is queued, and keeping it that way is what lets its
// quota scanner be tested without one.
type voucherDisconnector struct{ client *jobqueue.Client }

func (d *voucherDisconnector) Disconnect(ctx context.Context, nasIP, sessionID string) error {
	task, err := fup.NewDirectPoDTask(nasIP, sessionID)
	if err != nil {
		return err
	}
	// The grant is already revoked by the time this runs, so a failure here
	// costs a session that survives until the NAS re-authenticates it — not
	// unlimited access.
	_, err = d.client.EnqueueContext(ctx, task)
	return err
}

func (logAlerter) Trigger(event string, detail any) {
	alertsEmitted.WithLabelValues(event).Inc()
	log.Error().Str("event", event).Interface("detail", detail).Msg("radiusd: ALERT")
}

func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Warn().Dur("timeout", timeout).Msg("radiusd: some goroutines did not stop in time")
	}
}

func dbConfig(cfg *config.Config) db.Config {
	c := db.DefaultConfig(cfg.DBDSN)
	c.MaxConns = cfg.DBMaxConns
	c.MinConns = cfg.DBMinConns
	c.ConnectTimeout = cfg.DBConnTimeout
	return c
}
