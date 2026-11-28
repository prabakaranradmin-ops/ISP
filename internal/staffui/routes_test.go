package staffui

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// The console's route table, checked route by route.
//
// FR-SEC-002 (role-based access) and FR-SEC-003 (CSRF on state-changing
// requests) are enforced by two wrappers, authed and requireCSRF, applied by
// hand at each of RegisterRoutes' ~180 registrations. Nothing checked that
// every registration carries them: a route added without h.authed would serve
// the screen to anyone, and one added without h.requireCSRF would accept a
// forged form. Both are one-word omissions that compile and pass every
// handler test, because handler tests call the handler directly.
//
// The table is read out of staffui.go rather than listed here, so a route
// added later is covered without anybody remembering to add it.

// publicRoutes are the only registrations that may be reached without a
// session: signing in, signing out (which only clears a cookie), and the
// stylesheet and script the sign-in page itself needs.
var publicRoutes = map[string]bool{
	"GET /staff/login":   true,
	"POST /staff/login":  true,
	"POST /staff/logout": true,
	"GET /staff/static/": true,
}

// The static route is public, so what it will hand out matters: the two
// assets, nothing from templates/, and no directory listing. A listing is
// harmless with two public files in it and is still the first line of a
// scanner's report, which a pen test would then have to argue down.
func TestTheStaticRouteServesOnlyItsAssets(t *testing.T) {
	_, mux := routedHandler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	for _, asset := range []string{"/staff/static/staffui.css", "/staff/static/console.js"} {
		if w := get(asset); w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("%s: got %d with %d bytes", asset, w.Code, w.Body.Len())
		}
	}

	if w := get("/staff/static/"); w.Code == http.StatusOK {
		t.Errorf("/staff/static/ lists its directory: %s", firstLines(w.Body.String()))
	}

	// The templates sit beside static/ in the same embed. The mux cleans
	// "..", so these arrive as other paths, but whichever handler they reach
	// must not return a template's source.
	for _, path := range []string{
		"/staff/static/../templates/layout.html",
		"/staff/static/%2e%2e/templates/layout.html",
		"/staff/static/templates/layout.html",
	} {
		w := get(path)
		if strings.Contains(w.Body.String(), "{{") {
			t.Errorf("%s returned template source", path)
		}
	}
}

// uploadRoutes take a multipart body and are guarded by requireCSRFUpload.
var uploadRoutes = map[string]bool{
	"POST /staff/portal-branding/logo": true,
}

// consoleRoutes returns every pattern RegisterRoutes passes to mux.Handle or
// mux.HandleFunc, parsed from the source.
func consoleRoutes(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "staffui.go", nil, 0)
	if err != nil {
		t.Fatalf("parse staffui.go: %v", err)
	}
	var patterns []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RegisterRoutes" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mux" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				t.Errorf("a route is registered with a non-literal pattern at %v; this test cannot check it", call.Pos())
				return true
			}
			p, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}
			patterns = append(patterns, p)
			return true
		})
		return false
	})
	// A parser change that finds nothing would pass every check below
	// vacuously.
	if len(patterns) < 150 {
		t.Fatalf("found only %d routes in RegisterRoutes; the parse has gone wrong", len(patterns))
	}
	return patterns
}

var wildcard = regexp.MustCompile(`\{[^}]+\}`)

// concrete turns a pattern into a request for it.
func concrete(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	return method, wildcard.ReplaceAllString(path, "1")
}

func routedHandler() (*Handler, *http.ServeMux) {
	h := NewHandler(HandlerDeps{JWTSecret: testSecret})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

func signedToken(t *testing.T, secret, role string, lea bool) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   role + "_user",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role:      role,
		LeaAccess: lea,
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// routeRequest builds a request for pattern, with a session cookie if token
// is set and a form carrying csrf if csrf is set.
func routeRequest(pattern, tok, csrf string) *http.Request {
	method, path := concrete(pattern)
	var r *http.Request
	switch {
	case method != http.MethodPost:
		r = httptest.NewRequest(method, path, nil)
	case uploadRoutes[pattern]:
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if csrf != "" {
			_ = mw.WriteField("csrf_token", csrf)
		}
		_ = mw.Close()
		r = httptest.NewRequest(method, path, &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
	default:
		form := url.Values{}
		if csrf != "" {
			form.Set("csrf_token", csrf)
		}
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if tok != "" {
		r.AddCookie(&http.Cookie{Name: staffSessionCookie, Value: tok})
	}
	return r
}

// Each concrete request must land on the pattern it was made from. Otherwise
// every check below would be testing some other route — "/staff/subscribers/1"
// shadowed by a later literal, say — and pass for the wrong reason.
func TestEveryConsoleRouteResolvesToItsOwnPattern(t *testing.T) {
	_, mux := routedHandler()
	seen := map[string]bool{}
	for _, p := range consoleRoutes(t) {
		if seen[p] {
			t.Errorf("%s is registered twice", p)
		}
		seen[p] = true
		if _, got := mux.Handler(routeRequest(p, "", "")); got != p {
			t.Errorf("a request made for %q was routed to %q", p, got)
		}
	}
}

// FR-SEC-002: no screen and no action without a session.
func TestEveryConsoleRouteRefusesAnAnonymousRequest(t *testing.T) {
	_, mux := routedHandler()
	for _, p := range consoleRoutes(t) {
		if publicRoutes[p] {
			continue
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, routeRequest(p, "", ""))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/staff/login" {
			t.Errorf("%s served an anonymous request: %d, Location %q — is it registered without h.authed?",
				p, w.Code, w.Header().Get("Location"))
		}
	}
}

// A token signed with any other key is refused and cleared, on every route.
//
// This, not the anonymous check above, is what catches a route registered
// without h.authed. Most handlers also call requireSection, which sends a
// request with no session to /staff/login on its own, so an anonymous request
// to an unwrapped route is still redirected and that check passes. Only
// authed answers a bad token with ?expired=1 and clears the cookie. Confirmed
// by unwrapping GET /staff/revenue: the anonymous check stayed green and this
// one failed.
func TestEveryConsoleRouteRefusesAForgedSession(t *testing.T) {
	_, mux := routedHandler()
	forged := signedToken(t, "some-other-deployments-secret-also-long-enough", "isp_owner", true)
	for _, p := range consoleRoutes(t) {
		if publicRoutes[p] {
			continue
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, routeRequest(p, forged, ""))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/staff/login?expired=1" {
			t.Errorf("%s accepted a token signed with another key: %d, Location %q",
				p, w.Code, w.Header().Get("Location"))
			continue
		}
		cleared := false
		for _, c := range w.Result().Cookies() {
			if c.Name == staffSessionCookie && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Errorf("%s refused the forged session but left the cookie in place", p)
		}
	}
}

// FR-SEC-003: every state-changing route refuses a form without the session's
// CSRF token, and one carrying another session's token.
func TestEveryConsoleWriteRefusesAMissingOrForeignCSRFToken(t *testing.T) {
	h, mux := routedHandler()
	tok := signedToken(t, testSecret, "isp_owner", true)
	other := h.csrfToken(signedToken(t, testSecret, "csr", false))

	writes := 0
	for _, p := range consoleRoutes(t) {
		if publicRoutes[p] || !strings.HasPrefix(p, "POST ") {
			continue
		}
		writes++
		for name, csrf := range map[string]string{"no token": "", "another session's token": other} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, routeRequest(p, tok, csrf))
			if w.Code != http.StatusForbidden {
				t.Errorf("%s with %s: got %d, want 403 — is it registered without h.requireCSRF?",
					p, name, w.Code)
			}
		}
	}
	if writes < 80 {
		t.Fatalf("only %d POST routes; the route table was not read", writes)
	}
}

// A deployment that leaves an optional dependency unwired must answer every
// screen and action with a page, not a panic. net/http recovers a panicking
// handler by dropping the connection, so the operator sees the browser's own
// "connection reset" with nothing logged that says which store is missing.
//
// Every route is driven as the owner with LEA access (so no role refusal
// short-circuits it) and a valid CSRF token (so every write reaches its
// handler), against a handler with nothing configured at all.
func TestNoConsoleRoutePanicsOnAnUnconfiguredDeployment(t *testing.T) {
	h, mux := routedHandler()
	tok := signedToken(t, testSecret, "isp_owner", true)
	csrf := h.csrfToken(tok)

	for _, p := range consoleRoutes(t) {
		if publicRoutes[p] {
			continue
		}
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Errorf("%s panicked with nothing configured: %v", p, v)
				}
			}()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, routeRequest(p, tok, csrf))
			if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "CSRF") {
				t.Errorf("%s refused a valid CSRF token", p)
			}
			if w.Code == http.StatusSeeOther && strings.HasPrefix(w.Header().Get("Location"), "/staff/login") {
				t.Errorf("%s refused a valid owner session", p)
			}
		}()
	}
}

// The forged-token check above is only meaningful if a correctly signed one
// is accepted; this is its control.
func TestAValidSessionReachesTheDashboard(t *testing.T) {
	_, mux := routedHandler()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, routeRequest("GET /staff/dashboard", signedToken(t, testSecret, "isp_owner", false), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("a valid owner session got %d on the dashboard: %s", w.Code, firstLines(w.Body.String()))
	}
	if !strings.Contains(pageText(w.Body.String()), "Dashboard") {
		t.Errorf("the dashboard did not render: %s", firstLines(w.Body.String()))
	}
}
