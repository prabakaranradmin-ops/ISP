// Subscriber portal contract tests — FR-MOB-002 | MDS §4.9.
//
// A mobile client is not a browser. A web page re-renders from whatever the
// server sends today; a shipped app parses a fixed shape and stays on a user's
// phone for months after the server moves on. Renaming a JSON field, changing
// a number to a string, or dropping one from a response is a silent breaking
// change to every installed copy — and none of the existing tests would notice,
// because they assert on values through Go structs that were renamed in the
// same commit.
//
// These tests assert the wire format instead: exact key sets, JSON types, and a
// single error envelope across every route. They deliberately go through the
// real mux and the real middleware, so route paths and status codes are part of
// the contract too.
package portal_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// ── Schema description ──────────────────────────────────────────────────────

// jsonType names the wire types a client's parser distinguishes. Go's encoding
// /json decodes into these; a change between any two of them breaks a typed
// client even when the field name is unchanged.
type jsonType int

const (
	jsonString jsonType = iota
	jsonNumber
	jsonBool
	jsonObject
	jsonArray
)

func (t jsonType) String() string {
	switch t {
	case jsonString:
		return "string"
	case jsonNumber:
		return "number"
	case jsonBool:
		return "bool"
	case jsonObject:
		return "object"
	case jsonArray:
		return "array"
	default:
		return "unknown"
	}
}

// field is one key in a response.
type field struct {
	name string
	typ  jsonType
	// optional fields may be absent or null — `omitempty` or a nil pointer in
	// the Go type. They may not, however, change type when present.
	optional bool
}

// assertObjectSchema checks that body has exactly the described keys, with the
// described types.
//
// Exactly: an unexpected key is reported as loudly as a missing one. A field
// added server-side is not breaking for a client that ignores it, but it is
// how personal data leaks into a response nobody reviewed, and it is worth a
// deliberate decision rather than a silent one.
func assertObjectSchema(t *testing.T, label string, body []byte, fields []field) {
	t.Helper()

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("%s: response is not a JSON object: %v\nbody: %s", label, err, body)
	}

	expected := make(map[string]field, len(fields))
	for _, f := range fields {
		expected[f.name] = f
	}

	for _, f := range fields {
		raw, present := got[f.name]
		if !present {
			if f.optional {
				continue
			}
			t.Errorf("%s: missing field %q — removing it breaks every shipped client that reads it",
				label, f.name)
			continue
		}
		if isJSONNull(raw) {
			if !f.optional {
				t.Errorf("%s: field %q is null but not optional", label, f.name)
			}
			continue
		}
		if actual := typeOf(raw); actual != f.typ {
			t.Errorf("%s: field %q is %s, contract says %s — a type change breaks a typed "+
				"client even though the name is unchanged", label, f.name, actual, f.typ)
		}
	}

	var unexpected []string
	for name := range got {
		if _, ok := expected[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("%s: undeclared field(s) %v — add them to the contract deliberately, so that "+
			"what a client receives is reviewed rather than incidental", label, unexpected)
	}
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func typeOf(raw json.RawMessage) jsonType {
	s := strings.TrimSpace(string(raw))
	switch {
	case strings.HasPrefix(s, `"`):
		return jsonString
	case strings.HasPrefix(s, "{"):
		return jsonObject
	case strings.HasPrefix(s, "["):
		return jsonArray
	case s == "true" || s == "false":
		return jsonBool
	default:
		return jsonNumber
	}
}

// ── The contract ────────────────────────────────────────────────────────────

// Money is a JSON string, not a number, throughout the portal.
//
// decimal.Decimal marshals as a quoted string, and that is the right wire
// format: a client parsing a rupee balance into a float64 reintroduces exactly
// the rounding this codebase uses decimal to avoid. Pinning it here means a
// change to a numeric type fails loudly rather than reaching a phone.
var meSchema = []field{
	{name: "id", typ: jsonNumber},
	{name: "username", typ: jsonString},
	{name: "mobile_number", typ: jsonString},
	{name: "plan_name", typ: jsonString},
	{name: "plan_expiry", typ: jsonString, optional: true},
	{name: "wallet_balance", typ: jsonString},
	{name: "status", typ: jsonString},
	{name: "dunning_state", typ: jsonString},
}

var dashboardSchema = []field{
	{name: "wallet_balance", typ: jsonString},
	{name: "plan_name", typ: jsonString},
	{name: "plan_expiry", typ: jsonString, optional: true},
	{name: "status", typ: jsonString},
	{name: "active_session", typ: jsonObject, optional: true},
}

var activeSessionSchema = []field{
	{name: "session_id", typ: jsonString},
	{name: "nas_ip", typ: jsonString},
	{name: "assigned_ip", typ: jsonString},
	{name: "bytes_in", typ: jsonNumber},
	{name: "bytes_out", typ: jsonNumber},
	{name: "gb_used", typ: jsonString},
	{name: "gb_included", typ: jsonString},
	{name: "pct_used", typ: jsonNumber},
	{name: "speed_profile", typ: jsonString},
	{name: "fup_throttled", typ: jsonBool},
	{name: "started_at", typ: jsonString},
}

var errorSchema = []field{
	{name: "code", typ: jsonString},
	{name: "message", typ: jsonString},
}

// ── Tests ───────────────────────────────────────────────────────────────────

// TestFR_MOB_002_ProfileContract pins GET /portal/me.
func TestFR_MOB_002_ProfileContract(t *testing.T) {
	mux, token := contractHarness(t)

	rec := contractGet(t, mux, "/portal/me", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	assertObjectSchema(t, "GET /portal/me", rec.Body.Bytes(), meSchema)
}

// TestFR_MOB_002_DashboardContract pins GET /portal/dashboard, including the
// nested session object a mobile home screen renders.
func TestFR_MOB_002_DashboardContract(t *testing.T) {
	mux, token := contractHarness(t)

	rec := contractGet(t, mux, "/portal/dashboard", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	assertObjectSchema(t, "GET /portal/dashboard", rec.Body.Bytes(), dashboardSchema)

	var envelope struct {
		ActiveSession json.RawMessage `json:"active_session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if len(envelope.ActiveSession) > 0 && !isJSONNull(envelope.ActiveSession) {
		assertObjectSchema(t, "dashboard.active_session", envelope.ActiveSession, activeSessionSchema)
	}
}

// TestFR_MOB_002_ListEndpointsAreArrays — a client that binds a list view to a
// JSON array breaks if the server starts wrapping it in an object, and the
// reverse. Both list routes are pinned to the shape they ship with.
func TestFR_MOB_002_ListEndpointsAreArrays(t *testing.T) {
	mux, token := contractHarness(t)

	for _, path := range []string{"/portal/notifications", "/portal/tickets"} {
		rec := contractGet(t, mux, path, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d — %s", path, rec.Code, rec.Body.String())
		}
		if got := typeOf(rec.Body.Bytes()); got != jsonArray {
			t.Errorf("%s: response is %s, contract says array — wrapping a list in an object "+
				"breaks every client that iterates it", path, got)
		}

		// An empty result must still be [], never null: `null` makes a
		// for-each crash in most client languages, where [] iterates zero times.
		var rows []json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Errorf("%s: not a JSON array: %v", path, err)
		}
	}
}

// TestFR_MOB_002_ErrorEnvelopeIsUniform — a client writes one error parser. If
// some routes answer {code,message} and others answer {error} or bare text,
// that parser has to special-case each route, and the one it misses surfaces
// to a user as a blank screen.
func TestFR_MOB_002_ErrorEnvelopeIsUniform(t *testing.T) {
	mux, token := contractHarness(t)

	cases := []struct {
		name, method, path, body, token string
		wantStatus                      int
	}{
		{"unauthenticated profile", http.MethodGet, "/portal/me", "", "", http.StatusUnauthorized},
		{"unauthenticated dashboard", http.MethodGet, "/portal/dashboard", "", "", http.StatusUnauthorized},
		{"unauthenticated tickets", http.MethodGet, "/portal/tickets", "", "", http.StatusUnauthorized},
		{"bad login", http.MethodPost, "/portal/login", `{"username":"nobody","password":"wrong"}`, "", http.StatusUnauthorized},
		{"malformed ticket", http.MethodPost, "/portal/tickets", `{`, token, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := contractRequest(t, mux, tc.method, tc.path, tc.body, tc.token)
			if rec.Code != tc.wantStatus {
				t.Fatalf("want %d, got %d — %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("error responses must be JSON, got Content-Type %q with body %q",
					ct, rec.Body.String())
			}
			assertObjectSchema(t, tc.name, rec.Body.Bytes(), errorSchema)

			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			// A machine-readable code, not just prose: a client decides whether
			// to prompt for re-login or show a message based on this.
			if !strings.HasPrefix(body.Code, "ERR_") {
				t.Errorf("error code %q must be a stable ERR_* identifier, not prose", body.Code)
			}
		})
	}
}

// TestFR_MOB_002_LoginContract pins the token response an app stores.
func TestFR_MOB_002_LoginContract(t *testing.T) {
	mux, _ := contractHarness(t)

	rec := contractRequest(t, mux, http.MethodPost, "/portal/login",
		`{"username":"alice@isp","password":"testpass"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	assertObjectSchema(t, "POST /portal/login", rec.Body.Bytes(), []field{
		{name: "token", typ: jsonString},
	})
}

// TestFR_MOB_002_RoutesAndMethodsAreStable — a shipped app has the paths
// compiled in. Moving one, or dropping a method, strands every installed copy,
// so the route table itself is part of the contract.
func TestFR_MOB_002_RoutesAndMethodsAreStable(t *testing.T) {
	mux, token := contractHarness(t)

	routes := []struct{ method, path string }{
		{http.MethodPost, "/portal/login"},
		{http.MethodGet, "/portal/me"},
		{http.MethodGet, "/portal/dashboard"},
		{http.MethodGet, "/portal/notifications"},
		{http.MethodGet, "/portal/tickets"},
		{http.MethodPost, "/portal/tickets"},
		{http.MethodPost, "/portal/renew"},
		{http.MethodPost, "/portal/renew/callback"},
	}

	for _, rt := range routes {
		rec := contractRequest(t, mux, rt.method, rt.path, "{}", token)
		// The route must exist and accept the method. What it does with an
		// empty body is each handler's business; 404 and 405 are the two
		// answers that mean the contract moved.
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d — the route a shipped client calls has moved or lost "+
				"its method", rt.method, rt.path, rec.Code)
		}
	}
}

// TestFR_MOB_002_NoCredentialsLeakIntoResponses — the profile is the response
// most likely to grow a field by accident, since it is built from a struct that
// also carries authentication data elsewhere in the codebase.
func TestFR_MOB_002_NoCredentialsLeakIntoResponses(t *testing.T) {
	mux, token := contractHarness(t)

	for _, path := range []string{"/portal/me", "/portal/dashboard"} {
		body := strings.ToLower(contractGet(t, mux, path, token).Body.String())
		for _, forbidden := range []string{"password", "password_hash", "nt_hash", "aadhaar", "pan_number", "$2a$"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s response mentions %q: %s", path, forbidden, body)
			}
		}
	}
}

// ── Harness ─────────────────────────────────────────────────────────────────

func contractGet(t *testing.T, mux *http.ServeMux, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return contractRequest(t, mux, http.MethodGet, path, "", token)
}

func contractRequest(t *testing.T, mux *http.ServeMux, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)) //nolint:noctx // httptest.NewRequestWithContext needs go1.23; module is go1.22
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// contractHarness builds the real portal on the real mux with the same stubs
// the rest of this package's tests use, and returns a valid subscriber token.
//
// Deliberately the production RegisterRoutes rather than calling handlers
// directly: the middleware chain decides the status code and body of every
// unauthenticated response, which is half of what a client parses.
func contractHarness(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	mux := newTestHandler(t)
	return mux, contractToken(t, mux)
}

// contractToken logs in through the real endpoint, so the token under test is
// one the server actually issues.
func contractToken(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	rec := contractRequest(t, mux, http.MethodPost, "/portal/login",
		`{"username":"alice@isp","password":"testpass"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("contract harness login failed: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if body.Token == "" {
		t.Fatal("login returned an empty token")
	}
	return body.Token
}
