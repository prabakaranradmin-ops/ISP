//go:build integration

// Integration tests for the subscriber API.
//
// Covers INT-SEC-003 from the Integration Tests tracker sheet: PII submitted to
// POST /api/v1/subscribers must reach storage encrypted and version-prefixed,
// never in plaintext.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/api -Tags integration
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

const itJWTSecret = "integration_jwt_secret_32_chars!!"

// ── Recording stores ────────────────────────────────────────────────────────

// itKYCStore records what would be written to kyc_verifications.
type itKYCStore struct {
	mu   sync.Mutex
	rows []itKYCRow
}

type itKYCRow struct {
	SubscriberID int
	AadhaarEnc   string
	PANEnc       string
	KeyVersion   string
}

func (s *itKYCStore) UpsertKYC(_ context.Context, subscriberID int, aadhaarEnc, panEnc, keyVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, itKYCRow{subscriberID, aadhaarEnc, panEnc, keyVersion})
	return nil
}

func (s *itKYCStore) snapshot() []itKYCRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]itKYCRow(nil), s.rows...)
}

// itSubscriberStore records subscriber inserts, including the password hash.
type itSubscriberStore struct {
	mu     sync.Mutex
	rows   []api.SubscriberRecord
	hashes []string
	nextID int
}

func (s *itSubscriberStore) CreateSubscriber(_ context.Context, sub api.SubscriberRecord, passwordHash string) (*api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	sub.ID = s.nextID
	sub.CreatedAt = time.Now()
	s.rows = append(s.rows, sub)
	s.hashes = append(s.hashes, passwordHash)
	return &sub, nil
}

func (s *itSubscriberStore) GetSubscriberByID(_ context.Context, id int) (*api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ID == id {
			return &s.rows[i], nil
		}
	}
	return nil, nil
}

func (s *itSubscriberStore) UpdateSubscriber(ctx context.Context, id int, _ *int, _ *string, _ *time.Time) (*api.SubscriberRecord, error) {
	return s.GetSubscriberByID(ctx, id)
}

func (s *itSubscriberStore) GetSubscriberByUsername(_ context.Context, username string) (*api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].Username == username {
			return &s.rows[i], nil
		}
	}
	return nil, nil
}

// dump returns everything the store holds, as the raw text a `SELECT *` would
// show — used to assert plaintext PII appears nowhere in persisted state.
func (s *itSubscriberStore) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.rows)
	return string(b) + strings.Join(s.hashes, " ")
}

func itAdminToken(t *testing.T) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "billing_admin",
		RegisteredClaims: jwt.RegisteredClaims{Subject: "admin", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign admin token: %v", err)
	}
	return tok
}

func itKeyStore(t *testing.T, active string, versions ...string) crypto.KeyStore {
	t.Helper()
	keys := map[string][]byte{}
	for i, v := range versions {
		k := make([]byte, 32)
		for j := range k {
			k[j] = byte(i + 1)
		}
		keys[v] = k
	}
	ks, err := crypto.NewInMemoryKeyStore(keys, active)
	if err != nil {
		t.Fatalf("key store: %v", err)
	}
	return ks
}

// ── INT-SEC-003 ─────────────────────────────────────────────────────────────

// TestFR_SEC_002_CreateSubscriber_EncryptsPII verifies Aadhaar and PAN submitted through
// the API are stored as version-prefixed ciphertext, decrypt back to the
// original values, and never appear in plaintext in persisted state.
//
// INT-SEC-003 | FR-SEC-002
func TestFR_SEC_002_CreateSubscriber_EncryptsPII(t *testing.T) {
	const (
		aadhaar = "123456789012"
		pan     = "ABCDE1234F"
	)

	subs := &itSubscriberStore{}
	kyc := &itKYCStore{}
	keyStore := itKeyStore(t, "v1", "v1")

	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: kyc, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: keyStore,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber:       "CAF-2026-0001",
		Username:        "newsub@isp",
		Password:        "initial-password",
		MobileNumber:    "+919876543210",
		Email:           "sub@example.com",
		PlanID:          1,
		RegisteredState: "TN",
		Aadhaar:         aadhaar,
		PAN:             pan,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	rows := kyc.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 kyc_verifications row, got %d", len(rows))
	}
	row := rows[0]

	// Ciphertext must carry the key version prefix so a later rotation can still
	// resolve which key it was sealed under.
	for label, ct := range map[string]string{"aadhaar_encrypted": row.AadhaarEnc, "pan_encrypted": row.PANEnc} {
		if ct == "" {
			t.Errorf("%s is empty", label)
			continue
		}
		if !strings.HasPrefix(ct, "v") || !strings.Contains(ct, ":") {
			t.Errorf("%s must be {version}:{base64}, got %q", label, ct)
		}
	}
	if row.KeyVersion != "v1" {
		t.Errorf("key_version: want v1, got %q", row.KeyVersion)
	}

	// Plaintext must not survive anywhere in what was persisted.
	haystack := row.AadhaarEnc + " " + row.PANEnc + " " + subs.dump() + " " + rec.Body.String()
	for label, secret := range map[string]string{"aadhaar": aadhaar, "PAN": pan} {
		if strings.Contains(haystack, secret) {
			t.Errorf("plaintext %s found in persisted state or API response", label)
		}
	}

	// And the ciphertext must decrypt back to the submitted values.
	gotAadhaar, err := crypto.Decrypt(row.AadhaarEnc, keyStore)
	if err != nil {
		t.Fatalf("decrypt aadhaar: %v", err)
	}
	if gotAadhaar != aadhaar {
		t.Errorf("decrypted aadhaar: want %q, got %q", aadhaar, gotAadhaar)
	}
	gotPAN, err := crypto.Decrypt(row.PANEnc, keyStore)
	if err != nil {
		t.Fatalf("decrypt PAN: %v", err)
	}
	if gotPAN != pan {
		t.Errorf("decrypted PAN: want %q, got %q", pan, gotPAN)
	}
}

// TestFR_SEC_002_CreateSubscriber_PasswordNeverStoredInClear verifies the submitted
// password is bcrypt-hashed before it reaches the store.
//
// INT-SEC-003 (supporting) | FR-SEC-002
func TestFR_SEC_002_CreateSubscriber_PasswordNeverStoredInClear(t *testing.T) {
	const password = "sup3r-s3cret-pw"

	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber:       "CAF-2026-0002",
		Username:        "pwtest@isp",
		Password:        password,
		MobileNumber:    "+919876543211",
		PlanID:          1,
		RegisteredState: "TN",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(subs.dump(), password) {
		t.Error("plaintext password reached the subscriber store")
	}
	if len(subs.hashes) != 1 || !strings.HasPrefix(subs.hashes[0], "$2") {
		t.Errorf("want a bcrypt hash, got %v", subs.hashes)
	}
}

// TestFR_SEC_005_CreateSubscriber_RequiresAdminRole verifies subscriber creation is closed
// to non-admin roles.
//
// INT-SEC-003 (supporting) | FR-SEC-005
func TestFR_SEC_005_CreateSubscriber_RequiresAdminRole(t *testing.T) {
	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	csrToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "csr",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign csr token: %v", err)
	}

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber: "CAF-X", Username: "x@isp", MobileNumber: "+91987", PlanID: 1, RegisteredState: "TN",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+csrToken)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 for csr, got %d", rec.Code)
	}
	if len(subs.rows) != 0 {
		t.Error("no subscriber may be created by a forbidden role")
	}
}

// TestCreateSubscriber_RejectsNonE164Phone verifies the DoD Phase 2 Step 4
// fix: mobile_number must be valid E.164, checked before anything reaches
// the store.
//
// DoD Phase 2 Step 4 | FR-SUB (subscriber onboarding)
func TestCreateSubscriber_RejectsNonE164Phone(t *testing.T) {
	cases := []struct {
		name  string
		phone string
	}{
		{"missing leading +", "919876543210"},
		{"contains a space", "+91 9876543210"},
		{"contains a dash", "+91-9876543210"},
		{"leading zero after +", "+0919876543210"},
		{"not a phone number at all", "not-a-phone"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs := &itSubscriberStore{}
			h := api.NewHandler(api.HandlerDeps{
				DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
			})
			mux := http.NewServeMux()
			h.RegisterRoutes(mux, itJWTSecret)

			body, _ := json.Marshal(api.CreateSubscriberRequest{
				CAFNumber: "CAF-BAD", Username: "bad@isp", Password: "pw",
				MobileNumber: tc.phone, PlanID: 1, RegisteredState: "TN",
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
			if len(subs.rows) != 0 {
				t.Error("no subscriber may be created with an invalid phone number")
			}
		})
	}
}

// TestCreateSubscriber_AcceptsValidE164Phone is the positive counterpart to
// TestCreateSubscriber_RejectsNonE164Phone.
func TestCreateSubscriber_AcceptsValidE164Phone(t *testing.T) {
	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber: "CAF-GOOD", Username: "good@isp", Password: "pw",
		MobileNumber: "+919876543210", PlanID: 1, RegisteredState: "TN",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(subs.rows) != 1 {
		t.Fatalf("want 1 subscriber created, got %d", len(subs.rows))
	}
}

// itStaffToken signs a token for any of the staff-facing roles (as opposed
// to itAdminToken's fixed billing_admin).
func itStaffToken(t *testing.T, role string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             role,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "staff", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign %s token: %v", role, err)
	}
	return tok
}

// ── GetSubscriber / UpdateSubscriber / GetSubscriberHealth ──────────────────
//
// api_test.go's TestGetSubscriber_UnauthenticatedReturns401 only proves the
// route is behind auth; these are the tests that actually reach the handler.

func TestGetSubscriber_Found(t *testing.T) {
	subs := &itSubscriberStore{}
	if _, err := subs.CreateSubscriber(context.Background(), api.SubscriberRecord{
		CAFNumber: "CAF-GET-1", Username: "get-me@isp", Status: "active",
	}, "hash"); err != nil {
		t.Fatalf("seed subscriber: %v", err)
	}

	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/1", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itStaffToken(t, "noc_engineer"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var got api.SubscriberRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Username != "get-me@isp" {
		t.Errorf("username: want get-me@isp, got %q", got.Username)
	}
}

func TestGetSubscriber_NotFound(t *testing.T) {
	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/999", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itStaffToken(t, "csr"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateSubscriber_Success(t *testing.T) {
	subs := &itSubscriberStore{}
	if _, err := subs.CreateSubscriber(context.Background(), api.SubscriberRecord{
		CAFNumber: "CAF-UPD-1", Username: "update-me@isp", Status: "active",
	}, "hash"); err != nil {
		t.Fatalf("seed subscriber: %v", err)
	}

	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(map[string]any{"status": "hard_suspended"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/subscribers/1", bytes.NewReader(body)) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateSubscriber_RequiresAdminRole(t *testing.T) {
	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(map[string]any{"status": "hard_suspended"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/subscribers/1", bytes.NewReader(body)) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itStaffToken(t, "csr"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for csr, got %d", rec.Code)
	}
}

// itHealthProbe is a stand-in for internal/health's handler, which cannot be
// imported here without an import cycle. It records whether it was reached, so
// the tests below can tell "the request got through authorisation" apart from
// "the request was answered by the placeholder".
type itHealthProbe struct{ served bool }

func (p *itHealthProbe) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.served = true
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"subscriber_id":1,"assigned_ip":"100.64.0.7"}`))
}

func itHealthMux(t *testing.T, probe http.Handler) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{
		DB: &itSubscriberStore{}, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}),
		KeyStore: itKeyStore(t, "v1", "v1"), Health: probe,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

// TestFR_OBS_004_SubscriberHealth_ServedToStaff verifies the endpoint actually
// answers rather than reporting itself unimplemented. It returned 501 until
// 2026-08-10 while the working implementation sat on an undocumented route.
func TestFR_OBS_004_SubscriberHealth_ServedToStaff(t *testing.T) {
	probe := &itHealthProbe{}
	mux := itHealthMux(t, probe)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/1/health", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itStaffToken(t, "technician"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if !probe.served {
		t.Error("the request never reached the health implementation")
	}
}

// TestFR_OBS_004_SubscriberHealth_RequiresAuth is the regression test for the
// defect this endpoint actually shipped with: the real implementation was
// registered straight onto the mux in cmd/api/main.go, after RegisterRoutes and
// therefore outside its middleware, on an undocumented path. It answered 200
// with the subscriber's username, wallet balance, session and assigned IP to
// anyone who asked, with no token — the same IP-to-subscriber correlation that
// lea_audit_log exists to keep access-controlled and auditable.
//
// The assertion that matters is not the status code but probe.served: a 401
// that still ran the handler would mean the body had already been disclosed.
func TestFR_OBS_004_SubscriberHealth_RequiresAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no Authorization header", ""},
		{"malformed header", "Bearer not-a-jwt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &itHealthProbe{}
			mux := itHealthMux(t, probe)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/1/health", nil) //nolint:noctx
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("want 401, got %d", rec.Code)
			}
			if probe.served {
				t.Error("the health implementation ran for an unauthenticated request — subscriber data was disclosed")
			}
		})
	}
}

// TestFR_OBS_004_SubscriberHealth_RejectsSubscriberRole — a subscriber token is
// a valid JWT, so authentication alone does not make this endpoint safe. It is
// a staff diagnostic and must refuse the subscriber role outright, otherwise
// any signed-in customer could read every other customer's session and IP.
func TestFR_OBS_004_SubscriberHealth_RejectsSubscriberRole(t *testing.T) {
	probe := &itHealthProbe{}
	mux := itHealthMux(t, probe)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/1/health", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itStaffToken(t, "subscriber"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 for a subscriber token, got %d", rec.Code)
	}
	if probe.served {
		t.Error("the health implementation ran for a subscriber token")
	}
}

// TestFR_OBS_004_SubscriberHealth_UnconfiguredReturns503 covers the deployment
// that has not wired the health package: the route must degrade like every
// other optional dependency here rather than panicking on a nil handler.
func TestFR_OBS_004_SubscriberHealth_UnconfiguredReturns503(t *testing.T) {
	mux := itHealthMux(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers/1/health", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itStaffToken(t, "technician"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when unconfigured, got %d", rec.Code)
	}
}

// ── WalletRecharge ───────────────────────────────────────────────────────────

func TestWalletRecharge_Success(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &itSubscriberStore{}, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(map[string]any{
		"subscriber_id": 1, "amount": "500.00", "payment_method": "razorpay", "transaction_token": "tok_001",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/recharge", bytes.NewReader(body)) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// billing.Transaction has no json tags, so its exported field names are
	// used as-is ("ID", not "id").
	if got["ID"] == nil {
		t.Error("expected a transaction ID in the response")
	}
}

// TestWalletRecharge_InvalidAmount is the real version of the validation
// test its old, misleadingly-named unauthenticated-only counterpart in
// api_test.go never actually was.
func TestWalletRecharge_InvalidAmount(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &itSubscriberStore{}, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(map[string]any{
		"subscriber_id": 1, "amount": "badnum", "payment_method": "razorpay", "transaction_token": "tok_002",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/recharge", bytes.NewReader(body)) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for an unparseable amount, got %d — %s", rec.Code, rec.Body.String())
	}
}

// ── Payment receipt (FR-NOTIF-004 / FR-NOTIF-006) ───────────────────────────

// itTaskRecorder captures enqueued Asynq tasks.
type itTaskRecorder struct {
	mu    sync.Mutex
	tasks []*jobqueue.Task
	err   error
}

func (r *itTaskRecorder) Enqueue(task *jobqueue.Task, _ ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	r.tasks = append(r.tasks, task)
	return &jobqueue.TaskInfo{}, nil
}

func (r *itTaskRecorder) receipts(t *testing.T) []billing.PaymentReceiptPayload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []billing.PaymentReceiptPayload
	for _, task := range r.tasks {
		if task.Type() != billing.TaskTypePaymentReceipt {
			continue
		}
		var p billing.PaymentReceiptPayload
		if err := json.Unmarshal(task.Payload(), &p); err != nil {
			t.Fatalf("decode receipt payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func itRechargeWithStatus(t *testing.T, status string) *itTaskRecorder {
	t.Helper()
	store := &itSubscriberStore{
		rows:   []api.SubscriberRecord{{ID: 1, Username: "sub1", Status: status}},
		nextID: 1,
	}
	tasks := &itTaskRecorder{}
	h := api.NewHandler(api.HandlerDeps{
		DB: store, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}),
		KeyStore: itKeyStore(t, "v1", "v1"), Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(map[string]any{
		"subscriber_id": 1, "amount": "500.00", "payment_method": "razorpay",
		"transaction_token": "tok_receipt_" + status,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/recharge", bytes.NewReader(body)) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("recharge: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	return tasks
}

// TestFR_NOTIF_004_Recharge_EnqueuesPaymentReceipt — a subscriber who pays must
// be told the money arrived. Nothing sent one before this was wired.
func TestFR_NOTIF_004_Recharge_EnqueuesPaymentReceipt(t *testing.T) {
	receipts := itRechargeWithStatus(t, "active").receipts(t)

	if len(receipts) != 1 {
		t.Fatalf("want exactly 1 payment receipt, got %d", len(receipts))
	}
	if receipts[0].SubscriberID != 1 || receipts[0].Amount != "500.00" {
		t.Errorf("receipt does not describe the payment made: %+v", receipts[0])
	}
	// An account that was never cut off must not be told it has been restored.
	if receipts[0].Restored {
		t.Error("an active subscriber's receipt must not claim service was restored")
	}
}

// TestFR_NOTIF_006_Recharge_FlagsRestorationForSuspended covers the other half:
// paying while suspended is the event a subscriber most wants confirmed, and it
// is a different message from an ordinary receipt.
func TestFR_NOTIF_006_Recharge_FlagsRestorationForSuspended(t *testing.T) {
	for _, status := range []string{"grace_period", "soft_suspended", "hard_suspended"} {
		t.Run(status, func(t *testing.T) {
			receipts := itRechargeWithStatus(t, status).receipts(t)
			if len(receipts) != 1 {
				t.Fatalf("want 1 receipt, got %d", len(receipts))
			}
			if !receipts[0].Restored {
				t.Errorf("a payment from %s must be flagged as a restoration", status)
			}
		})
	}
}

// TestFR_NOTIF_004_Recharge_SucceedsWhenReceiptCannotBeQueued — the money is
// banked and the ledger written before the receipt is enqueued, so a queue
// failure must not fail the request. Telling a caller their payment did not go
// through when it did is worse than a missing message.
func TestFR_NOTIF_004_Recharge_SucceedsWhenReceiptCannotBeQueued(t *testing.T) {
	store := &itSubscriberStore{
		rows:   []api.SubscriberRecord{{ID: 1, Username: "sub1", Status: "active"}},
		nextID: 1,
	}
	tasks := &itTaskRecorder{err: errors.New("redis down")}
	h := api.NewHandler(api.HandlerDeps{
		DB: store, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}),
		KeyStore: itKeyStore(t, "v1", "v1"), Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(map[string]any{
		"subscriber_id": 1, "amount": "500.00", "payment_method": "razorpay", "transaction_token": "tok_qfail",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/recharge", bytes.NewReader(body)) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("recharge must still succeed when the receipt cannot be queued, got %d", rec.Code)
	}
}
