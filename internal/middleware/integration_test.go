//go:build integration

// Integration tests for JWT authentication and RBAC enforcement.
//
// Covers INT-SEC-001 and INT-SEC-002 from the Integration Tests tracker sheet.
// Each case drives the real middleware chain the API mounts, so a protected
// handler is only reached when both authentication and authorisation pass.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/middleware -Tags integration
package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// itProtectedRoute wires JWT auth in front of a role check, mirroring the
// billing_admin routes in api.RegisterRoutes. reached reports whether the
// protected handler ran.
func itProtectedRoute(roles ...string) (http.Handler, *bool) {
	reached := new(bool)
	handler := middleware.JWTMiddleware(testSecret)(
		middleware.RequireRole(roles...)(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				*reached = true
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"balance":"12345.00"}`))
			}),
		),
	)
	return handler, reached
}

func itClaimsToken(t *testing.T, claims middleware.Claims) string {
	t.Helper()
	if claims.ExpiresAt == nil {
		claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// ── INT-SEC-001 ─────────────────────────────────────────────────────────────

// TestFR_SEC_005_RequireRole_NoToken verifies an unauthenticated request to an admin route
// is rejected with 401 and never reaches the handler.
//
// INT-SEC-001 | FR-SEC-005
func TestFR_SEC_005_RequireRole_NoToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"no Authorization header", ""},
		{"bare token without Bearer", "eyJhbGciOiJIUzI1NiJ9.e30.signature"},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"Bearer with garbage token", "Bearer not-a-jwt"},
		{"Bearer with empty token", "Bearer "},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler, reached := itProtectedRoute("billing_admin", "isp_owner")

			req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/1/ledger", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("want 401, got %d", rec.Code)
			}
			if *reached {
				t.Error("protected handler must not run without a valid token")
			}
			if body := rec.Body.String(); len(body) > 0 && !isOnlyAnErrorEnvelope(body) {
				t.Errorf("401 must carry nothing but the error envelope, got %q", body)
			}
		})
	}
}

// TestFR_SEC_005_RequireRole_TokenSignedWithWrongSecret verifies a forged token is refused.
//
// INT-SEC-001 (supporting) | FR-SEC-005
func TestFR_SEC_005_RequireRole_TokenSignedWithWrongSecret(t *testing.T) {
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "isp_owner",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte("attacker-secret-32-chars-minimum!"))
	if err != nil {
		t.Fatalf("sign forged token: %v", err)
	}

	handler, reached := itProtectedRoute("billing_admin", "isp_owner")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/1/ledger", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for a token signed with the wrong secret, got %d", rec.Code)
	}
	if *reached {
		t.Error("forged token must not reach the protected handler")
	}
}

// TestFR_SEC_005_RequireRole_AlgNoneRejected verifies an unsigned "alg: none" token, the
// classic JWT bypass, is refused.
//
// INT-SEC-001 (supporting) | FR-SEC-005
func TestFR_SEC_005_RequireRole_AlgNoneRejected(t *testing.T) {
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, middleware.Claims{
		Role:             "isp_owner",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign alg=none token: %v", err)
	}

	handler, reached := itProtectedRoute("billing_admin", "isp_owner")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/1/ledger", nil)
	req.Header.Set("Authorization", "Bearer "+unsigned)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for alg=none, got %d", rec.Code)
	}
	if *reached {
		t.Error("alg=none token must not reach the protected handler")
	}
}

// ── INT-SEC-002 ─────────────────────────────────────────────────────────────

// TestFR_SEC_005_RequireRole_WrongRoleReturns403 verifies an authenticated CSR cannot reach
// a billing_admin route.
//
// INT-SEC-002 | FR-SEC-005
func TestFR_SEC_005_RequireRole_WrongRoleReturns403(t *testing.T) {
	handler, reached := itProtectedRoute("billing_admin", "isp_owner")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/wallets/recharge", nil)
	req.Header.Set("Authorization", "Bearer "+itClaimsToken(t, middleware.Claims{Role: "csr"}))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 for csr on a billing_admin route, got %d", rec.Code)
	}
	if *reached {
		t.Error("csr must not reach a billing_admin handler")
	}
}

// TestFR_SEC_005_RequireRole_RoleMatrix walks the roles the API grants and denies, so a
// future change to a route's role list cannot silently widen access.
//
// INT-SEC-002 | FR-SEC-005
func TestFR_SEC_005_RequireRole_RoleMatrix(t *testing.T) {
	const (
		allowed = http.StatusOK
		denied  = http.StatusForbidden
	)
	billingAdminRoute := []string{"billing_admin", "isp_owner"}

	cases := []struct {
		role string
		want int
	}{
		{"billing_admin", allowed},
		{"isp_owner", allowed},
		{"csr", denied},
		{"noc_engineer", denied},
		{"technician", denied},
		{"subscriber", denied},
		{"lco", denied},
		{"", denied},
	}

	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			handler, reached := itProtectedRoute(billingAdminRoute...)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/1/ledger", nil)
			req.Header.Set("Authorization", "Bearer "+itClaimsToken(t, middleware.Claims{Role: c.role}))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Errorf("role %q: want %d, got %d", c.role, c.want, rec.Code)
			}
			if (c.want == allowed) != *reached {
				t.Errorf("role %q: handler reached=%v but expected status %d", c.role, *reached, c.want)
			}
		})
	}
}

// TestFR_FRN_001_Claims_FranchiseIDPropagates verifies the franchise binding survives the
// JWT round-trip so downstream row-level scoping can rely on it.
//
// INT-SEC-002 (supporting) | FR-FRN-001
func TestFR_FRN_001_Claims_FranchiseIDPropagates(t *testing.T) {
	var gotRole string
	var gotFranchise, gotSubscriber int

	handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotRole = middleware.RoleFromContext(r.Context())
		gotFranchise = middleware.FranchiseIDFromContext(r.Context())
		gotSubscriber = middleware.SubscriberIDFromContext(r.Context())
	}))

	token := itClaimsToken(t, middleware.Claims{Role: "lco", FranchiseID: 7, SubscriberID: 42})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotRole != "lco" {
		t.Errorf("role: want lco, got %q", gotRole)
	}
	if gotFranchise != 7 {
		t.Errorf("franchise_id: want 7, got %d", gotFranchise)
	}
	if gotSubscriber != 42 {
		t.Errorf("subscriber_id: want 42, got %d", gotSubscriber)
	}
}

// isOnlyAnErrorEnvelope reports whether a body is the {code, message} error
// envelope and nothing else.
//
// This replaces jsonLooksLikeData, which asked whether the body was a JSON
// object at all. That was a sound proxy for "did we leak the resource" while
// auth errors were text/plain, and stopped being one the moment
// middleware.writeAuthError started returning the same JSON envelope every
// other handler uses (FR-MOB-002: an expired token was the one error a mobile
// client could not parse). From then on it fired on every 401, and nobody saw
// it, because the integration suite it lives in had never been run.
//
// The real requirement is that a rejected request returns no data. So this
// checks the shape: exactly the two envelope keys, nothing more. A response
// carrying so much as an extra field fails, which is the leak worth catching.
func isOnlyAnErrorEnvelope(body string) bool {
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return false
	}
	if len(envelope) != 2 {
		return false
	}
	_, hasCode := envelope["code"]
	_, hasMessage := envelope["message"]
	return hasCode && hasMessage
}
