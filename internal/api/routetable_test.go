package api_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// The API's route table, checked route by route.
//
// RegisterRoutes wraps each of its ~140 registrations by hand in one of a
// dozen role gates (admin, staffRead, nocOnly, ...). Nothing checked that a
// route is wrapped at all, that each gate admits the roles its name and the
// SecD role matrix say it does, or that a subscriber's portal token cannot
// open a staff route. Handler tests call handlers directly and cannot see any
// of it.
//
// The table is read out of routes.go, so a route added later is covered
// without anybody remembering to list it. The role sets below are the
// specification the gates are checked against; changing a gate means
// changing this table too, on purpose.

const (
	rtStaffSecret  = "route-table-staff-secret-long-enough-for-hs256"
	rtPortalSecret = "route-table-portal-secret-long-enough-for-hs256"
)

// gateRoles is who each staff gate admits.
var gateRoles = map[string][]string{
	"admin":         {"billing_admin", "isp_owner"},
	"staffRead":     {"noc_engineer", "billing_admin", "csr", "technician", "isp_owner"},
	"nocOnly":       {"noc_engineer", "isp_owner"},
	"nocWithLea":    {"noc_engineer", "isp_owner"}, // and the lea_access claim
	"billingOrCSR":  {"billing_admin", "csr", "isp_owner"},
	"csrOrTech":     {"csr", "technician", "isp_owner"},
	"nocOrTech":     {"noc_engineer", "technician", "isp_owner"},
	"nocOrBilling":  {"noc_engineer", "billing_admin", "isp_owner"},
	"ownerOnly":     {"isp_owner"},
	"leadWrite":     {"csr", "technician", "billing_admin", "isp_owner", "lco", "franchise_admin", "franchise_staff"},
	"franchiseRead": {"billing_admin", "isp_owner", "lco", "franchise_admin", "franchise_staff"},
}

// Gates that are not a staff JWT.
const (
	gatePublic     = ""               // health, and webhooks verified by HMAC
	gateSubscriber = "subscriberSelf" // the portal's own secret, role subscriber
)

var partnerGates = map[string]bool{"apiKey": true, "manageWebhooks": true}

var everyRole = []string{
	"isp_owner", "billing_admin", "csr", "technician", "noc_engineer",
	"lco", "franchise_admin", "franchise_staff", "subscriber",
}

// publicAPIRoutes are the only registrations reachable with no credential.
var publicAPIRoutes = map[string]bool{
	"GET /health":             true,
	"POST /webhooks/razorpay": true, // authenticated by the gateway's HMAC, not a token
}

type rtRoute struct{ pattern, gate string }

func apiRoutes(t *testing.T) []rtRoute {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "routes.go", nil, 0)
	if err != nil {
		t.Fatalf("parse routes.go: %v", err)
	}
	var routes []rtRoute
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RegisterRoutes" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
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
				t.Errorf("a route is registered with a non-literal pattern; this test cannot check it")
				return true
			}
			p, _ := strconv.Unquote(lit.Value)
			gate := gatePublic
			if inner, ok := call.Args[1].(*ast.CallExpr); ok {
				if id, ok := inner.Fun.(*ast.Ident); ok {
					gate = id.Name
				}
			}
			routes = append(routes, rtRoute{p, gate})
			return true
		})
	}
	if len(routes) < 120 {
		t.Fatalf("found only %d routes in RegisterRoutes; the parse has gone wrong", len(routes))
	}
	return routes
}

type rtLister struct{}

func (rtLister) ListSubscribers(context.Context, *int) ([]revenue.SubscriberRow, error) {
	return nil, nil
}

// rtKeys knows no keys: the partner routes are checked for refusing, and
// their handlers are covered by partner_test.go. An unknown key is (nil, nil),
// as db.PartnerStore answers it; an error means the lookup itself failed.
type rtKeys struct{}

func (rtKeys) AuthenticateKey(context.Context, string) (*partner.APIKey, error) {
	return nil, nil
}
func (rtKeys) TouchKeyUsage(context.Context, int) {}

// rtMux registers every route, including the three conditional groups, with
// only the dependencies every deployment has (subscribers, KYC, wallet).
func rtMux() *http.ServeMux {
	h := api.NewHandler(api.HandlerDeps{
		DB:               &stubDB{},
		KYC:              &stubKYC{},
		Wallet:           billing.NewWalletService(&stubWallet{}),
		PortalJWTSecret:  rtPortalSecret,
		SubscriberLister: rtLister{},
		PartnerAuth:      rtKeys{},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, rtStaffSecret)
	return mux
}

func rtToken(t *testing.T, secret, role string, lea bool) string {
	t.Helper()
	c := middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   role + "_user",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role:      role,
		LeaAccess: lea,
	}
	switch role {
	case "lco", "franchise_admin", "franchise_staff":
		c.FranchiseID = 2
	case "subscriber":
		c.SubscriberID = 7
		c.Subject = "7"
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

var rtWildcard = regexp.MustCompile(`\{[^}]+\}`)

func rtRequest(pattern, bearer string) *http.Request {
	method, path, _ := strings.Cut(pattern, " ")
	path = rtWildcard.ReplaceAllString(path, "1")
	var r *http.Request
	switch method {
	case http.MethodGet, http.MethodDelete:
		r = httptest.NewRequest(method, path, nil)
	default:
		r = httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func rtServe(mux *http.ServeMux, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// roleRefused reports whether a response is a role gate's refusal, as
// opposed to the handler's own 403 (a franchise scope, say).
func roleRefused(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "ERR_FORBIDDEN")
}

func TestEveryAPIRouteResolvesToItsOwnPattern(t *testing.T) {
	mux := rtMux()
	seen := map[string]bool{}
	for _, rt := range apiRoutes(t) {
		if seen[rt.pattern] {
			t.Errorf("%s is registered twice", rt.pattern)
		}
		seen[rt.pattern] = true
		if _, got := mux.Handler(rtRequest(rt.pattern, "")); got != rt.pattern {
			t.Errorf("a request made for %q was routed to %q", rt.pattern, got)
		}
	}
}

// Every gate in routes.go is one this test knows the roles of. A new gate
// would otherwise be checked for nothing but anonymous refusal.
func TestEveryAPIGateIsKnown(t *testing.T) {
	gates := map[string]bool{}
	for _, rt := range apiRoutes(t) {
		gates[rt.gate] = true
		if rt.gate == gatePublic && !publicAPIRoutes[rt.pattern] {
			t.Errorf("%s is registered without any gate; if it is meant to be public, add it to "+
				"publicAPIRoutes with the reason", rt.pattern)
		}
	}
	for g := range gates {
		if _, staff := gateRoles[g]; !staff && g != gatePublic && g != gateSubscriber && !partnerGates[g] {
			t.Errorf("gate %q is not in gateRoles; say who it admits", g)
		}
	}
}

func TestEveryAPIRouteRefusesAnAnonymousRequest(t *testing.T) {
	mux := rtMux()
	for _, rt := range apiRoutes(t) {
		if publicAPIRoutes[rt.pattern] {
			continue
		}
		if w := rtServe(mux, rtRequest(rt.pattern, "")); w.Code != http.StatusUnauthorized {
			t.Errorf("%s answered an anonymous request with %d", rt.pattern, w.Code)
		}
	}
}

// Each staff gate admits exactly its roles: everyone else is refused by the
// gate, and everyone in it gets past the gate (whatever the handler then
// says).
func TestEveryAPIRouteAdmitsExactlyItsRoles(t *testing.T) {
	mux := rtMux()
	checked := 0
	for _, rt := range apiRoutes(t) {
		allowed, staff := gateRoles[rt.gate]
		if !staff {
			continue
		}
		in := map[string]bool{}
		for _, r := range allowed {
			in[r] = true
		}
		for _, role := range everyRole {
			lea := rt.gate == "nocWithLea"
			w := rtServe(mux, rtRequest(rt.pattern, rtToken(t, rtStaffSecret, role, lea)))
			switch {
			case in[role] && (w.Code == http.StatusUnauthorized || roleRefused(w)):
				t.Errorf("%s (%s) refused %s, which the gate should admit: %d %s",
					rt.pattern, rt.gate, role, w.Code, strings.TrimSpace(w.Body.String()))
			case !in[role] && !roleRefused(w):
				t.Errorf("%s (%s) let %s past the gate: %d", rt.pattern, rt.gate, role, w.Code)
			}
			checked++
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d route×role checks ran", checked)
	}
}

// LEA export needs the claim as well as the role (SecD §9.3): a noc_engineer
// or owner token minted without lea_access is refused.
func TestTheLEARouteNeedsTheClaimNotJustTheRole(t *testing.T) {
	mux := rtMux()
	for _, rt := range apiRoutes(t) {
		if rt.gate != "nocWithLea" {
			continue
		}
		for _, role := range gateRoles["nocWithLea"] {
			if w := rtServe(mux, rtRequest(rt.pattern, rtToken(t, rtStaffSecret, role, false))); !roleRefused(w) {
				t.Errorf("%s admitted %s without lea_access: %d", rt.pattern, role, w.Code)
			}
		}
	}
}

// The two secrets do not open each other's doors. A subscriber's portal token
// signed with the portal secret is refused by every staff route, and a staff
// token — even the owner's — is refused by the subscriber routes.
func TestPortalAndStaffTokensDoNotCross(t *testing.T) {
	mux := rtMux()
	subscriber := rtToken(t, rtPortalSecret, "subscriber", false)
	ownerOnPortalSecret := rtToken(t, rtPortalSecret, "isp_owner", true)
	owner := rtToken(t, rtStaffSecret, "isp_owner", true)

	for _, rt := range apiRoutes(t) {
		switch {
		case gateRoles[rt.gate] != nil:
			for name, tok := range map[string]string{
				"a subscriber's portal token":                  subscriber,
				"an owner claim signed with the portal secret": ownerOnPortalSecret,
			} {
				if w := rtServe(mux, rtRequest(rt.pattern, tok)); w.Code != http.StatusUnauthorized {
					t.Errorf("%s answered %s with %d", rt.pattern, name, w.Code)
				}
			}
		case rt.gate == gateSubscriber:
			if w := rtServe(mux, rtRequest(rt.pattern, owner)); w.Code != http.StatusUnauthorized {
				t.Errorf("%s answered a staff token with %d", rt.pattern, w.Code)
			}
			if w := rtServe(mux, rtRequest(rt.pattern, subscriber)); w.Code == http.StatusUnauthorized || roleRefused(w) {
				t.Errorf("%s refused the subscriber's own token: %d", rt.pattern, w.Code)
			}
		case partnerGates[rt.gate]:
			// A JWT of any kind is not an API key.
			if w := rtServe(mux, rtRequest(rt.pattern, owner)); w.Code != http.StatusUnauthorized {
				t.Errorf("%s answered a staff JWT with %d", rt.pattern, w.Code)
			}
		}
	}
}

// A deployment that leaves an optional store unwired answers with an error,
// not a panic. net/http recovers a panicking handler by dropping the
// connection, so the client gets a reset with nothing that names the store.
func TestNoAPIRoutePanicsOnAnUnconfiguredDeployment(t *testing.T) {
	mux := rtMux()
	owner := rtToken(t, rtStaffSecret, "isp_owner", true)
	subscriber := rtToken(t, rtPortalSecret, "subscriber", false)

	var panicked []string
	for _, rt := range apiRoutes(t) {
		if publicAPIRoutes[rt.pattern] || partnerGates[rt.gate] {
			continue
		}
		tok := owner
		if rt.gate == gateSubscriber {
			tok = subscriber
		}
		func() {
			defer func() {
				if v := recover(); v != nil {
					panicked = append(panicked, fmt.Sprintf("%s: %v", rt.pattern, v))
				}
			}()
			rtServe(mux, rtRequest(rt.pattern, tok))
		}()
	}
	sort.Strings(panicked)
	for _, p := range panicked {
		t.Errorf("panicked with its store unwired: %s", p)
	}
}
