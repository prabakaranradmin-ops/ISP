package config

import (
	"net/url"
	"strings"
	"testing"
)

// Configuration loading and the redacted view both services log at startup.
//
// SecD §8 ("secrets are never logged") is what Redact is for, and it had no
// test: the package was at 0%. The property checked here is the one that
// matters — set every secret to a distinctive value and look for any of them
// anywhere in Redact's output — rather than field-by-field expectations that
// a new field could slip past.

// allKeys is every variable Load reads, so each test starts from a clean
// environment whatever the machine running it has set.
var allKeys = []string{
	"ENVIRONMENT", "MOCK_GATEWAY_URL", "LOG_FORMAT", "LOG_LEVEL", "LOG_FILE",
	"API_ADDR", "METRICS_ADDR", "RADIUS_ADDR", "RADIUS_ACCT_ADDR", "ARCHIVE_DIR",
	"PARTITION_RETENTION_MONTHS", "ARCHIVE_S3_ENDPOINT", "ARCHIVE_S3_BUCKET",
	"ARCHIVE_S3_PREFIX", "ARCHIVE_S3_ACCESS_KEY", "ARCHIVE_S3_SECRET_KEY",
	"ARCHIVE_S3_REGION", "ARCHIVE_S3_USE_SSL", "TLS_CERT_DIR", "TLS_HOSTNAME",
	"DB_DSN", "DB_MAX_CONNS", "DB_MIN_CONNS", "DB_CONN_TIMEOUT_SECONDS",
	"SUBSCRIBER_CACHE_TTL_SECONDS", "JWT_SECRET", "PORTAL_JWT_SECRET",
	"RADIUS_SECRET", "RADIUS_VERIFIER_SECRET", "RAZORPAY_WEBHOOK_SECRET",
	"RAZORPAY_KEY_ID", "RAZORPAY_KEY_SECRET", "AES_KEY_STORE_URL",
	"WHATSAPP_PHONE_NUMBER_ID", "WHATSAPP_ACCESS_TOKEN",
	"WHATSAPP_WEBHOOK_VERIFY_TOKEN", "WHATSAPP_APP_SECRET",
	"SMS_GATEWAY_PROVIDER", "SMS_GATEWAY_API_KEY", "SMS_GATEWAY_SENDER_ID",
	"SMTP_HOST", "SMTP_PORT", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_FROM",
	"ONESIGNAL_APP_ID", "ONESIGNAL_API_KEY", "GST_HOME_STATE",
	"GST_SUPPLIER_GSTIN", "GST_SUPPLIER_NAME", "CHROMIUM_PATH",
	"PAGERDUTY_ROUTING_KEY", "WALLED_GARDEN_ADDR", "WALLED_GARDEN_REDIRECT_URL",
	"CONNECTION_LIMIT_MODE",
}

// The connection limit starts in report mode, so nobody is cut off by a
// deployment, and a value that is neither mode is refused at startup rather
// than read as report: a typo for "enforce" must not leave an operator
// believing the limit is in force.
func TestTheConnectionLimitStartsInReportModeAndRefusesTypos(t *testing.T) {
	apiEnv(t)
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectionLimitMode != "report" {
		t.Errorf("default mode %q, want report", cfg.ConnectionLimitMode)
	}
	t.Setenv("CONNECTION_LIMIT_MODE", " Enforce ")
	if cfg, err = Load("api"); err != nil || cfg.ConnectionLimitMode != "enforce" {
		t.Errorf("Enforce with spaces: %q %v", cfg.ConnectionLimitMode, err)
	}
	for _, bad := range []string{"enforced", "on", "true", "strict"} {
		t.Setenv("CONNECTION_LIMIT_MODE", bad)
		if _, err := Load("api"); err == nil || !strings.Contains(err.Error(), "CONNECTION_LIMIT_MODE must be report or enforce") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

// The walled-garden redirector is off unless asked for, and asking for it
// without saying where to send people is refused at startup rather than
// discovered by a suspended subscriber looking at an error page.
func TestTheWalledGardenListenerNeedsSomewhereToSendPeople(t *testing.T) {
	apiEnv(t)
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WalledGardenAddr != "" {
		t.Errorf("the redirector is on by default at %q", cfg.WalledGardenAddr)
	}

	t.Setenv("WALLED_GARDEN_ADDR", ":8088")
	if _, err := Load("api"); err == nil || !strings.Contains(err.Error(), "WALLED_GARDEN_REDIRECT_URL is required") {
		t.Errorf("a listener with no target: err = %v", err)
	}

	t.Setenv("WALLED_GARDEN_REDIRECT_URL", "https://portal.example.net/ui/login?suspended=1")
	cfg, err = Load("api")
	if err != nil {
		t.Fatalf("a listener with a target: %v", err)
	}
	if cfg.WalledGardenAddr != ":8088" || cfg.Redact()["walled_garden_redirect"] != "https://portal.example.net/ui/login?suspended=1" {
		t.Errorf("addr %q, redact %q", cfg.WalledGardenAddr, cfg.Redact()["walled_garden_redirect"])
	}

	// radiusd runs no listener, so it does not need the target.
	cleanEnv(t)
	t.Setenv("DB_DSN", "host=db")
	t.Setenv("RADIUS_SECRET", longSecret)
	t.Setenv("RADIUS_VERIFIER_SECRET", longSecret)
	t.Setenv("WALLED_GARDEN_ADDR", ":8088")
	if _, err := Load("radiusd"); err != nil {
		t.Errorf("radiusd refused over the API's redirector: %v", err)
	}
}

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range allKeys {
		t.Setenv(k, "")
	}
}

const longSecret = "0123456789abcdef0123456789abcdef" // exactly minSecretLength

func apiEnv(t *testing.T) {
	t.Helper()
	cleanEnv(t)
	t.Setenv("DB_DSN", "host=db user=bss_app password=pw dbname=isp")
	t.Setenv("JWT_SECRET", longSecret)
	t.Setenv("AES_KEY_STORE_URL", "file:///keys")
}

func TestLoadAppliesDefaults(t *testing.T) {
	apiEnv(t)
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]any{
		"Environment":    {cfg.Environment, "development"},
		"APIAddr":        {cfg.APIAddr, ":8080"},
		"DBMaxConns":     {int(cfg.DBMaxConns), 25},
		"SMTPPort":       {cfg.SMTPPort, 587},
		"GSTHomeState":   {cfg.GSTHomeState, "TN"},
		"SMSSenderID":    {cfg.SMSSenderID, "BSSOSS"},
		"UseSSL default": {cfg.ArchiveS3UseSSL, true},
		"retention":      {cfg.PartitionRetentionMonths, ""},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	if cfg.DBConnTimeout.Seconds() != 10 || cfg.SubscriberCacheTTL.Seconds() != 60 {
		t.Errorf("timeouts = %v, %v", cfg.DBConnTimeout, cfg.SubscriberCacheTTL)
	}
}

// Values are trimmed, numbers that are not positive integers fall back to the
// default, and only the literal "false" turns TLS off for the archive.
func TestEnvParsing(t *testing.T) {
	apiEnv(t)
	t.Setenv("API_ADDR", "  :9090  ")
	t.Setenv("DB_MAX_CONNS", "forty")
	t.Setenv("DB_MIN_CONNS", "-3")
	t.Setenv("SMTP_PORT", " 2525 ")
	t.Setenv("ARCHIVE_S3_USE_SSL", "false")

	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != ":9090" {
		t.Errorf("API_ADDR not trimmed: %q", cfg.APIAddr)
	}
	if cfg.DBMaxConns != 25 || cfg.DBMinConns != 5 {
		t.Errorf("bad numbers should fall back: max=%d min=%d", cfg.DBMaxConns, cfg.DBMinConns)
	}
	if cfg.SMTPPort != 2525 {
		t.Errorf("SMTP_PORT = %d", cfg.SMTPPort)
	}
	if cfg.ArchiveS3UseSSL {
		t.Error("ARCHIVE_S3_USE_SSL=false left TLS on")
	}

	t.Setenv("ARCHIVE_S3_USE_SSL", "no")
	cfg, _ = Load("api")
	if !cfg.ArchiveS3UseSSL {
		t.Error("anything but the literal \"false\" must leave TLS on")
	}
}

// The portal signs with its own secret, derived when not given, so a staff
// token is never valid on the portal (see the API route-table test).
func TestThePortalSecretIsDerivedButNeverTheStaffSecret(t *testing.T) {
	apiEnv(t)
	cfg, _ := Load("api")
	if cfg.PortalJWTSecret == "" || cfg.PortalJWTSecret == cfg.JWTSecret {
		t.Errorf("portal secret = %q; it must differ from the staff secret", cfg.PortalJWTSecret)
	}

	t.Setenv("PORTAL_JWT_SECRET", "an-explicit-portal-secret-of-enough-length")
	cfg, _ = Load("api")
	if cfg.PortalJWTSecret != "an-explicit-portal-secret-of-enough-length" {
		t.Errorf("an explicit portal secret was replaced: %q", cfg.PortalJWTSecret)
	}
}

func TestAMockGatewayIsRefusedInProduction(t *testing.T) {
	for _, env := range []string{"production", "Production", "PRODUCTION"} {
		apiEnv(t)
		t.Setenv("ENVIRONMENT", env)
		t.Setenv("MOCK_GATEWAY_URL", "http://127.0.0.1:9999")
		if _, err := Load("api"); err == nil || !strings.Contains(err.Error(), "MOCK_GATEWAY_URL") {
			t.Errorf("ENVIRONMENT=%s with a mock gateway: err = %v", env, err)
		}
	}
	// And allowed anywhere else, which is what it is for.
	apiEnv(t)
	t.Setenv("ENVIRONMENT", "staging")
	t.Setenv("MOCK_GATEWAY_URL", "http://127.0.0.1:9999")
	if _, err := Load("api"); err != nil {
		t.Errorf("staging with a mock gateway was refused: %v", err)
	}
}

func TestEachServiceRequiresItsOwnSecrets(t *testing.T) {
	cleanEnv(t)
	_, err := Load("api")
	if err == nil {
		t.Fatal("api started with nothing configured")
	}
	// Every problem at once, so an operator fixes them in one pass rather
	// than one restart each.
	for _, want := range []string{"DB_DSN is required", "JWT_SECRET is required", "AES_KEY_STORE_URL is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}

	t.Setenv("DB_DSN", "host=db")
	t.Setenv("JWT_SECRET", "short")
	t.Setenv("AES_KEY_STORE_URL", "file:///k")
	if _, err := Load("api"); err == nil || !strings.Contains(err.Error(), "at least 32 characters (got 5)") {
		t.Errorf("a short JWT secret: err = %v", err)
	}

	// radiusd needs neither the JWT secret nor the key store, and does need
	// both RADIUS secrets.
	cleanEnv(t)
	t.Setenv("DB_DSN", "host=db")
	_, err = Load("radiusd")
	if err == nil {
		t.Fatal("radiusd started without its secrets")
	}
	for _, want := range []string{"RADIUS_SECRET is required", "RADIUS_VERIFIER_SECRET is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "JWT_SECRET") || strings.Contains(err.Error(), "AES_KEY_STORE_URL") {
		t.Errorf("radiusd demanded an API secret: %v", err)
	}
	t.Setenv("RADIUS_SECRET", longSecret)
	t.Setenv("RADIUS_VERIFIER_SECRET", longSecret)
	if _, err := Load("radiusd"); err != nil {
		t.Errorf("radiusd with its secrets: %v", err)
	}

	if _, err := Load("worker"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("an unknown service: err = %v", err)
	}
}

// No secret value appears anywhere in what Redact returns.
func TestRedactShowsNoSecret(t *testing.T) {
	apiEnv(t)
	secrets := map[string]string{
		"JWT_SECRET":                    "SECRET-jwt-000000000000000000000000",
		"PORTAL_JWT_SECRET":             "SECRET-portal-0000000000000000000000",
		"RADIUS_SECRET":                 "SECRET-radius",
		"RADIUS_VERIFIER_SECRET":        "SECRET-verifier",
		"RAZORPAY_WEBHOOK_SECRET":       "SECRET-rzp-hook",
		"RAZORPAY_KEY_ID":               "SECRET-rzp-id",
		"RAZORPAY_KEY_SECRET":           "SECRET-rzp-key",
		"AES_KEY_STORE_URL":             "file:///SECRET-keystore",
		"WHATSAPP_ACCESS_TOKEN":         "SECRET-wa-token",
		"WHATSAPP_WEBHOOK_VERIFY_TOKEN": "SECRET-wa-verify",
		"WHATSAPP_APP_SECRET":           "SECRET-wa-app",
		"SMS_GATEWAY_API_KEY":           "SECRET-sms",
		"SMTP_PASSWORD":                 "SECRET-smtp",
		"ONESIGNAL_API_KEY":             "SECRET-onesignal",
		"PAGERDUTY_ROUTING_KEY":         "SECRET-pagerduty",
		"ARCHIVE_S3_ACCESS_KEY":         "SECRET-s3-access",
		"ARCHIVE_S3_SECRET_KEY":         "SECRET-s3-secret",
		"DB_DSN":                        "host=db user=bss_app password=SECRET-db dbname=isp",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	cfg, err := Load("api")
	if err != nil {
		t.Fatal(err)
	}
	for field, shown := range cfg.Redact() {
		if strings.Contains(shown, "SECRET") {
			t.Errorf("Redact()[%q] = %q shows a secret", field, shown)
		}
	}
	if got := cfg.Redact()["jwt_secret"]; got != "set" {
		t.Errorf("jwt_secret = %q, want \"set\"", got)
	}
}

// Every DSN form pgx accepts keeps its password out of the log, and keeps
// enough of the rest to say which database the service is talking to.
func TestRedactDSNHidesThePasswordInEveryForm(t *testing.T) {
	for name, tc := range map[string]struct {
		dsn  string
		keep string
	}{
		"keyword":                  {"host=db user=bss_app password=SECRET dbname=isp", "dbname=isp"},
		"keyword, spaces around =": {"host=db user=bss_app password = SECRET dbname=isp", "dbname=isp"},
		"keyword, quoted":          {"host=db password='SECRETa SECRETb' dbname=isp", "dbname=isp"},
		"keyword, escaped quote":   {`host=db password='SECRET\'s' dbname=isp`, "dbname=isp"},
		"keyword, multi-host":      {"host=a,b port=5432,5432 user=u password=SECRET dbname=isp target_session_attrs=read-write", "host=a,b"},
		"keyword, sslpassword":     {"host=db sslpassword=SECRET dbname=isp", "dbname=isp"},
		"url":                      {"postgres://bss_app:SECRET@db:5432/isp?sslmode=disable", "db:5432/isp"},
		"url, postgresql scheme":   {"postgresql://bss_app:SECRET@db/isp", "db/isp"},
		"url, password in query":   {"postgres://db/isp?user=bss_app&password=SECRET", "db/isp"},
		"url, multi-host":          {"postgres://u:SECRET@a:5432,b:5432/isp", "/isp"},
		"url, @ in the password":   {"postgres://u:SECRET%40SECRET@db/isp", "db/isp"},
		"url, sslpassword":         {"postgres://u@db/isp?sslpassword=SECRET&sslmode=verify-full", "sslmode=verify-full"},
	} {
		t.Run(name, func(t *testing.T) {
			got := redactDSN(tc.dsn)
			if strings.Contains(got, "SECRET") {
				t.Errorf("redactDSN(%q) = %q shows the password", tc.dsn, got)
			}
			if !strings.Contains(got, tc.keep) {
				t.Errorf("redactDSN(%q) = %q lost %q, which an operator needs to see", tc.dsn, got, tc.keep)
			}
		})
	}
	if redactDSN("") != "unset" {
		t.Error("an empty DSN should read as unset")
	}

	// A DSN that cannot be tokenised is not echoed at all.
	for _, bad := range []string{
		"host=db password='SECRET dbname=isp", // unterminated quote
		`host=db password='SECRET\`,           // escape at the very end
		"SECRET",                              // not a DSN
		"host=db =SECRET",                     // no keyword
		"host=db password SECRET",             // no "="
	} {
		if got := redactDSN(bad); strings.Contains(got, "SECRET") {
			t.Errorf("redactDSN(%q) = %q echoed a malformed DSN", bad, got)
		}
	}
	// A bare value ending in a backslash is well-formed and still masked.
	if got := redactDSN(`host=db password=SECRET\`); strings.Contains(got, "SECRET") {
		t.Errorf("trailing backslash: %q", got)
	}
}

// Whatever the password, written the way libpq or a URL requires, it does
// not come out of redactDSN.
func FuzzRedactDSNNeverShowsThePassword(f *testing.F) {
	for _, seed := range []string{"hunter22", "it's mine", `back\slash`, "a b c", "p@ss/w:rd?&=#", "pässwörd"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, pw string) {
		if len(pw) < 6 {
			return // short strings occur in the rest of the DSN by chance
		}
		quoted := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(pw)
		keyword := "host=db user=u password='" + quoted + "' dbname=isp"
		if got := redactDSN(keyword); strings.Contains(got, pw) || strings.Contains(got, quoted) {
			t.Errorf("keyword form %q came out as %q", keyword, got)
		}

		u := url.URL{Scheme: "postgres", User: url.UserPassword("u", pw), Host: "db", Path: "/isp"}
		if got := redactDSN(u.String()); strings.Contains(got, pw) || strings.Contains(got, u.User.String()[2:]) {
			t.Errorf("URL form %q came out as %q", u.String(), got)
		}

		q := "postgres://db/isp?password=" + url.QueryEscape(pw)
		if got := redactDSN(q); strings.Contains(got, pw) || strings.Contains(got, url.QueryEscape(pw)) {
			t.Errorf("query form %q came out as %q", q, got)
		}
	})
}

func TestRedactNamesWhatEmptyMeans(t *testing.T) {
	cfg := &Config{}
	r := cfg.Redact()
	if r["log_file"] != "stdout" || r["chromium_path"] != "auto-detect" || r["db_dsn"] != "unset" {
		t.Errorf("empty values: log_file=%q chromium_path=%q db_dsn=%q",
			r["log_file"], r["chromium_path"], r["db_dsn"])
	}
	cfg = &Config{LogFile: "C:/isp/logs/api.log", ChromiumPath: "/usr/bin/chromium"}
	r = cfg.Redact()
	if r["log_file"] != "C:/isp/logs/api.log" || r["chromium_path"] != "/usr/bin/chromium" {
		t.Errorf("set values were not shown: %v", r)
	}
}
