// Package config loads service configuration from the environment.
//
// Every value is read once at startup and validated before any connection is
// opened, so a missing or too-short secret fails the process immediately with a
// list of what is wrong rather than surfacing as a confusing runtime error on
// the first request that needs it.
//
// Secrets are never logged. Redact() produces a safe view for startup logging.
//
// IDD §8.1, §8.5 | SecD §9.3
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// minSecretLength is the floor for anything used as a signing or shared key.
// A 32-byte HMAC secret is the practical minimum for HS256 (SecD §9.3).
const minSecretLength = 32

// Config is the full service configuration.
type Config struct {
	// Service
	Environment string
	LogFormat   string
	LogLevel    string
	// LogFile redirects log output to a file instead of stdout. Empty
	// keeps the historical behaviour (stdout), which is what every
	// container and interactive run wants. A Windows service has no
	// console for stdout to reach at all, so
	// scripts/windows/register_services.ps1 sets this for both services —
	// without it, a service that failed at startup would have logged its
	// reason nowhere.
	LogFile     string
	APIAddr     string
	MetricsAddr string
	RadiusAddr  string
	// RadiusAcctAddr is the accounting listener. Separate from RadiusAddr
	// because RFC 2866 puts accounting on its own port and every NAS sends it
	// there; sharing one port would drop every accounting record on the wire.
	RadiusAcctAddr string

	// ArchiveDir roots the local document-archival backend (FR-DOC-001).
	// Empty disables archival entirely rather than defaulting to a path: a
	// deployment that has not chosen where its invoices and KYC scans live
	// should not have that decided for it by a constant.
	ArchiveDir string

	// TLSCertDir roots the self-signed certificate pkg/tlscert generates and
	// persists for the API service to terminate TLS itself (replacing the
	// Caddy reverse proxy this stack ran on Docker Compose). Unlike
	// ArchiveDir, this defaults to a real path rather than "": TLS is not an
	// optional feature api_service can simply run without.
	TLSCertDir string
	// TLSHostname is the certificate's primary SAN. "localhost" — matching
	// the Caddyfile's own default before it — since a LAN-facing on-prem
	// install has no public FQDN for a real CA to validate anyway; a
	// deployment with a routable hostname should still trust it explicitly
	// (self-signed, no ACME path exists here).
	TLSHostname string

	// PostgreSQL
	DBDSN         string
	DBMaxConns    int32
	DBMinConns    int32
	DBConnTimeout time.Duration

	// SubscriberCacheTTL bounds how long the RADIUS auth cache serves a record.
	// It is the window in which a suspended subscriber could still re-authenticate,
	// so raising it trades enforcement latency for database load.
	SubscriberCacheTTL time.Duration

	// Secrets
	JWTSecret             string
	PortalJWTSecret       string
	RadiusSecret          string
	RadiusVerifierSecret  string
	RazorpayWebhookSecret string
	RazorpayKeyID         string
	RazorpayKeySecret     string
	AESKeyStoreURL        string

	// WhatsApp (Meta Cloud API)
	WhatsAppPhoneNumberID      string
	WhatsAppAccessToken        string
	WhatsAppWebhookVerifyToken string
	WhatsAppAppSecret          string

	// SMS
	SMSProvider string
	SMSAPIKey   string
	SMSSenderID string

	// Email (FR-NOTIF-012). An unset SMTPHost leaves the channel
	// unconfigured, which Dispatcher reports per-send rather than failing
	// startup — the same rule ChromiumPath and Razorpay follow.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string

	// Push (FR-NOTIF-013), via OneSignal.
	OneSignalAppID  string
	OneSignalAPIKey string

	// Integrations
	//
	// ChromiumPath is the executable pkg/chromium.Locate should treat as
	// the answer rather than auto-detecting one — set when an operator
	// needs a specific browser, e.g. a machine with more than one
	// Chromium-based browser installed. Empty (the default) lets Locate
	// find Microsoft Edge or Google Chrome itself; see that package's docs
	// for why this replaced what was GOTENBERG_URL; invoice PDF generation
	// no longer talks to a Gotenberg container at all.
	ChromiumPath        string
	PagerDutyRoutingKey string

	// GST filing identity (FR-BIL-006).
	//
	// GSTHomeState decides which supplies are intrastate and therefore
	// which are charged CGST+SGST rather than IGST, so it is the one
	// value here with a direct effect on money. It defaults to the state
	// this system assumed before the value was configurable, which keeps
	// an existing deployment billing exactly as it did.
	//
	// GSTSupplierGSTIN and GSTSupplierName appear only in the GSTR-1
	// export header, so an operator who never files can leave both unset.
	GSTHomeState     string
	GSTSupplierGSTIN string
	GSTSupplierName  string
}

// Requirement describes how strictly a field is enforced.
type Requirement int

const (
	// Optional fields may be empty; the feature they enable stays off.
	Optional Requirement = iota
	// Required fields must be present.
	Required
	// RequiredSecret must be present and at least minSecretLength characters.
	RequiredSecret
)

// Load reads configuration from the environment and validates it.
//
// service selects which fields are mandatory: the API and the RADIUS daemon need
// overlapping but different sets, and demanding a WhatsApp token from the RADIUS
// daemon would block startup for no reason.
func Load(service string) (*Config, error) {
	cfg := &Config{
		Environment:    env("ENVIRONMENT", "development"),
		LogFormat:      env("LOG_FORMAT", "console"),
		LogLevel:       env("LOG_LEVEL", "info"),
		LogFile:        env("LOG_FILE", ""),
		APIAddr:        env("API_ADDR", ":8080"),
		MetricsAddr:    env("METRICS_ADDR", ":9101"),
		RadiusAddr:     env("RADIUS_ADDR", ":1812"),
		RadiusAcctAddr: env("RADIUS_ACCT_ADDR", ":1813"),
		ArchiveDir:     env("ARCHIVE_DIR", ""),
		TLSCertDir:     env("TLS_CERT_DIR", "./config/tls"),
		TLSHostname:    env("TLS_HOSTNAME", "localhost"),

		DBDSN:         env("DB_DSN", ""),
		DBMaxConns:    int32(envInt("DB_MAX_CONNS", 25)), //nolint:gosec // bounded by envInt
		DBMinConns:    int32(envInt("DB_MIN_CONNS", 5)),  //nolint:gosec // bounded by envInt
		DBConnTimeout: time.Duration(envInt("DB_CONN_TIMEOUT_SECONDS", 10)) * time.Second,

		SubscriberCacheTTL: time.Duration(envInt("SUBSCRIBER_CACHE_TTL_SECONDS", 60)) * time.Second,

		JWTSecret:             env("JWT_SECRET", ""),
		PortalJWTSecret:       env("PORTAL_JWT_SECRET", ""),
		RadiusSecret:          env("RADIUS_SECRET", ""),
		RadiusVerifierSecret:  env("RADIUS_VERIFIER_SECRET", ""),
		RazorpayWebhookSecret: env("RAZORPAY_WEBHOOK_SECRET", ""),
		RazorpayKeyID:         env("RAZORPAY_KEY_ID", ""),
		RazorpayKeySecret:     env("RAZORPAY_KEY_SECRET", ""),
		AESKeyStoreURL:        env("AES_KEY_STORE_URL", ""),

		WhatsAppPhoneNumberID:      env("WHATSAPP_PHONE_NUMBER_ID", ""),
		WhatsAppAccessToken:        env("WHATSAPP_ACCESS_TOKEN", ""),
		WhatsAppWebhookVerifyToken: env("WHATSAPP_WEBHOOK_VERIFY_TOKEN", ""),
		WhatsAppAppSecret:          env("WHATSAPP_APP_SECRET", ""),

		SMSProvider: env("SMS_GATEWAY_PROVIDER", "msg91"),
		SMSAPIKey:   env("SMS_GATEWAY_API_KEY", ""),
		SMSSenderID: env("SMS_GATEWAY_SENDER_ID", "BSSOSS"),

		SMTPHost:     env("SMTP_HOST", ""),
		SMTPPort:     envInt("SMTP_PORT", 587),
		SMTPUsername: env("SMTP_USERNAME", ""),
		SMTPPassword: env("SMTP_PASSWORD", ""),
		SMTPFrom:     env("SMTP_FROM", "no-reply@isp.local"),

		OneSignalAppID:  env("ONESIGNAL_APP_ID", ""),
		OneSignalAPIKey: env("ONESIGNAL_API_KEY", ""),

		// "TN" literal rather than billing.DefaultHomeState: config is a
		// leaf package every service imports, and having it depend on
		// billing would invert that. internal/billing asserts the two
		// agree (TestDefaultHomeStateMatchesConfigDefault).
		GSTHomeState:        env("GST_HOME_STATE", "TN"),
		GSTSupplierGSTIN:    env("GST_SUPPLIER_GSTIN", ""),
		GSTSupplierName:     env("GST_SUPPLIER_NAME", ""),
		ChromiumPath:        env("CHROMIUM_PATH", ""),
		PagerDutyRoutingKey: env("PAGERDUTY_ROUTING_KEY", ""),
	}

	// The portal signs its own tokens so a leaked staff token cannot be replayed
	// against subscriber endpoints, and vice versa.
	if cfg.PortalJWTSecret == "" && cfg.JWTSecret != "" {
		cfg.PortalJWTSecret = cfg.JWTSecret + "_portal"
	}

	rules := map[string]struct {
		value string
		req   Requirement
	}{
		"DB_DSN":                  {cfg.DBDSN, Required},
		"AES_KEY_STORE_URL":       {cfg.AESKeyStoreURL, Optional},
		"JWT_SECRET":              {cfg.JWTSecret, Optional},
		"RADIUS_SECRET":           {cfg.RadiusSecret, Optional},
		"RAZORPAY_WEBHOOK_SECRET": {cfg.RazorpayWebhookSecret, Optional},
	}

	switch service {
	case "api":
		rules["JWT_SECRET"] = struct {
			value string
			req   Requirement
		}{cfg.JWTSecret, RequiredSecret}
		rules["AES_KEY_STORE_URL"] = struct {
			value string
			req   Requirement
		}{cfg.AESKeyStoreURL, Required}
	case "radiusd":
		rules["RADIUS_SECRET"] = struct {
			value string
			req   Requirement
		}{cfg.RadiusSecret, RequiredSecret}
		// Required, not optional: without it the fast-verifier cache (the fix
		// for NFR-PERF-001's 15ms p99 budget) cannot function, and radiusd
		// would silently pay bcrypt cost=12 on every request instead of
		// failing loudly at startup.
		rules["RADIUS_VERIFIER_SECRET"] = struct {
			value string
			req   Requirement
		}{cfg.RadiusVerifierSecret, RequiredSecret}
	default:
		return nil, fmt.Errorf("config: unknown service %q (want api or radiusd)", service)
	}

	var problems []string
	for name, rule := range rules {
		switch rule.req {
		case Required:
			if rule.value == "" {
				problems = append(problems, name+" is required but not set")
			}
		case RequiredSecret:
			if rule.value == "" {
				problems = append(problems, name+" is required but not set")
			} else if len(rule.value) < minSecretLength {
				problems = append(problems,
					fmt.Sprintf("%s must be at least %d characters (got %d)", name, minSecretLength, len(rule.value)))
			}
		case Optional:
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("config: invalid configuration for %s:\n  - %s",
			service, strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// Redact returns a copy safe to log: every secret is replaced by a set/unset
// marker so a startup log can show what is configured without leaking values.
func (c *Config) Redact() map[string]string {
	return map[string]string{
		"environment":            c.Environment,
		"log_file":               logFileOrStdout(c.LogFile),
		"api_addr":               c.APIAddr,
		"metrics_addr":           c.MetricsAddr,
		"radius_addr":            c.RadiusAddr,
		"radius_acct_addr":       c.RadiusAcctAddr,
		"archive_dir":            c.ArchiveDir,
		"tls_cert_dir":           c.TLSCertDir,
		"tls_hostname":           c.TLSHostname,
		"db_dsn":                 redactDSN(c.DBDSN),
		"db_max_conns":           strconv.Itoa(int(c.DBMaxConns)),
		"jwt_secret":             setOrUnset(c.JWTSecret),
		"radius_secret":          setOrUnset(c.RadiusSecret),
		"radius_verifier_secret": setOrUnset(c.RadiusVerifierSecret),
		"aes_key_store":          setOrUnset(c.AESKeyStoreURL),
		"razorpay_secret":        setOrUnset(c.RazorpayWebhookSecret),
		"razorpay_key_id":        setOrUnset(c.RazorpayKeyID),
		"razorpay_key_secret":    setOrUnset(c.RazorpayKeySecret),
		"whatsapp_token":         setOrUnset(c.WhatsAppAccessToken),
		"sms_api_key":            setOrUnset(c.SMSAPIKey),
		"smtp_password":          setOrUnset(c.SMTPPassword),
		"onesignal_api_key":      setOrUnset(c.OneSignalAPIKey),
		"pagerduty_key":          setOrUnset(c.PagerDutyRoutingKey),
		"chromium_path":          chromiumPathOrAutoDetect(c.ChromiumPath),
	}
}

// logFileOrStdout names what Redact should show for LogFile: the actual
// path when one is set, "stdout" (what an empty LogFile really means) when
// it is not — printing "" there would read as a blank left by a bug rather
// than as the deliberate default.
func logFileOrStdout(v string) string {
	if v == "" {
		return "stdout"
	}
	return v
}

// chromiumPathOrAutoDetect names what Redact should show for ChromiumPath:
// the configured path when one is set, "auto-detect" (what an empty one
// really means — see pkg/chromium.Locate) when it is not.
func chromiumPathOrAutoDetect(v string) string {
	if v == "" {
		return "auto-detect"
	}
	return v
}

func setOrUnset(v string) string {
	if v == "" {
		return "unset"
	}
	return "set"
}

// redactDSN strips the password from a PostgreSQL DSN so it can be logged.
func redactDSN(dsn string) string {
	if dsn == "" {
		return "unset"
	}
	// URL form: postgres://user:password@host/db
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		if scheme := strings.Index(dsn, "://"); scheme >= 0 && scheme+3 < at {
			return dsn[:scheme+3] + "***@" + dsn[at+1:]
		}
	}
	// Keyword form: host=... password=... dbname=...
	fields := strings.Fields(dsn)
	for i, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), "password=") {
			fields[i] = "password=***"
		}
	}
	return strings.Join(fields, " ")
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
