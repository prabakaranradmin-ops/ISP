package staffui

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// The franchise screens — FR-FRN-001..003, FR-FRN-006, CRD-EXP-010.
//
// All five handlers were at 0%: 149 statements, none exercised. What matters
// here is not the CRUD but the scoping. /staff/franchise carries a
// consolidated P&L across every partner and is owner-only; /staff/my-pnl and
// /staff/my-subscribers show one partner their own figures, scoped from the
// session's FranchiseID and never from a path parameter. A partner who could
// read another partner's commission — or an unbound account that fell back to
// "all subscribers" — is a cross-customer leak in a product whose whole
// premise is that LCOs share one deployment.

var errFranchiseStore = errors.New("store: franchise tables unreachable")

type fakeFranchises struct {
	list         []revenue.FranchiseRecord
	consolidated *revenue.ConsolidatedPnL
	pnl          *revenue.FranchisePnL
	created      *revenue.FranchiseRecord
	createReq    revenue.CreateFranchiseRequest
	// askedFor records which franchise's P&L was requested — the whole
	// question on the self-scoped screens.
	askedFor  int
	from, to  *time.Time
	listErr   error
	consolErr error
	pnlErr    error
	createErr error
}

func (f *fakeFranchises) ListFranchises(_ context.Context, _ *int) ([]revenue.FranchiseRecord, error) {
	return f.list, f.listErr
}

func (f *fakeFranchises) CreateFranchise(_ context.Context, req revenue.CreateFranchiseRequest) (*revenue.FranchiseRecord, error) {
	f.createReq = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &revenue.FranchiseRecord{ID: 3, Name: req.Name}, nil
}

func (f *fakeFranchises) GetFranchisePnL(_ context.Context, id int, from, to *time.Time) (*revenue.FranchisePnL, error) {
	f.askedFor, f.from, f.to = id, from, to
	return f.pnl, f.pnlErr
}

func (f *fakeFranchises) ListConsolidatedPnL(_ context.Context, _, _ *time.Time) (*revenue.ConsolidatedPnL, error) {
	return f.consolidated, f.consolErr
}

type fakeVoucherCommissions struct {
	summary  *hotspot.VoucherCommissionSummary
	err      error
	askedFor int
}

func (f *fakeVoucherCommissions) GetVoucherCommissionSummary(_ context.Context, id int) (*hotspot.VoucherCommissionSummary, error) {
	f.askedFor = id
	return f.summary, f.err
}

// fakeRevenue satisfies RevenueQuerier. Only ListSubscribers matters to these
// screens; the rest exist because the interface requires them.
type fakeRevenue struct {
	subs    []revenue.SubscriberRow
	err     error
	scopeTo *int
	scoped  bool
}

func (f *fakeRevenue) GetUnbilledActiveSubscribers(context.Context) (int, error) { return 0, nil }
func (f *fakeRevenue) GetLedgerVariance(context.Context) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (f *fakeRevenue) GetTotalWalletBalance(context.Context) (decimal.Decimal, error) {
	return decimal.Zero, nil
}
func (f *fakeRevenue) ListSubscribers(_ context.Context, franchiseID *int) ([]revenue.SubscriberRow, error) {
	f.scopeTo, f.scoped = franchiseID, true
	return f.subs, f.err
}
func (f *fakeRevenue) GetCollectionsByDunningStage(context.Context) ([]revenue.CollectionsStageRow, error) {
	return nil, nil
}
func (f *fakeRevenue) GetMonthlyRecovery(context.Context, int) ([]revenue.RecoveryMonth, error) {
	return nil, nil
}

// serveFr dispatches to the franchise handler the path implies, with a
// session already on the context as the auth middleware would leave it.
func serveFr(h *Handler, s Session, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 && parts[1] == "franchise" {
		r.SetPathValue("id", parts[2])
	}

	w := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(r.URL.Path, "/staff/my-subscribers"):
		h.MySubscribers(w, r)
	case strings.HasPrefix(r.URL.Path, "/staff/my-pnl"):
		h.MyPnL(w, r)
	case method == http.MethodPost:
		h.CreateFranchiseForm(w, r)
	case len(parts) >= 3:
		h.FranchiseDetail(w, r)
	default:
		h.Franchises(w, r)
	}
	return w
}

func owner() Session { return Session{StaffID: 1, Username: "owner_user", Role: "isp_owner"} }
func partner() Session {
	return Session{StaffID: 2, Username: "lco_user", Role: "lco", FranchiseID: 2}
}

func samplePnL() *revenue.FranchisePnL {
	return &revenue.FranchisePnL{
		FranchiseID: 2, FranchiseName: "Anna Nagar Networks", Status: "active",
		CommissionRatePct: "12.50", SubscriberCount: 180, RechargeCount: 174,
		TotalRecharges: "104400.00", CommissionEarned: "13050.00",
	}
}

func fullFranchiseHandler(f *fakeFranchises, v *fakeVoucherCommissions, rev *fakeRevenue) *Handler {
	h := &Handler{}
	if f != nil {
		h.franchises = f
	}
	if v != nil {
		h.voucherCommissions = v
	}
	if rev != nil {
		h.revenue = rev
	}
	return h
}

// Only the owner sees the cross-partner view. The consolidated P&L on this
// screen is every partner's commission side by side — exactly what one LCO
// must never be shown.
func TestFR_FRN_001_OnlyTheOwnerReachesTheCrossPartnerScreens(t *testing.T) {
	h := fullFranchiseHandler(&fakeFranchises{pnl: samplePnL()}, nil, nil)

	for _, role := range []string{"lco", "franchise_admin", "franchise_staff",
		"billing_admin", "csr", "noc_engineer", "technician"} {
		s := Session{Username: role + "_user", Role: role, FranchiseID: 2}
		for _, target := range []string{"/staff/franchise", "/staff/franchise/2"} {
			if code := serveFr(h, s, http.MethodGet, target, nil).Code; code == http.StatusOK {
				t.Errorf("%s reached %s", role, target)
			}
		}
	}
	if code := serveFr(h, owner(), http.MethodGet, "/staff/franchise", nil).Code; code != http.StatusOK {
		t.Errorf("the owner got %d from /staff/franchise", code)
	}
}

// A partner's own P&L is scoped from the session, not the URL. This is the
// assertion that would catch somebody "improving" the screen by reading an id
// from the path.
func TestFR_FRN_003_APartnersOwnPnLIsScopedFromTheSessionNotTheURL(t *testing.T) {
	f := &fakeFranchises{pnl: samplePnL()}
	h := fullFranchiseHandler(f, nil, nil)

	// A partner bound to franchise 2 asking with another id in the query.
	rec := serveFr(h, partner(), http.MethodGet, "/staff/my-pnl?id=99&franchise_id=99", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if f.askedFor != 2 {
		t.Errorf("the store was asked for franchise %d; the session says 2. Anything "+
			"request-supplied reaching this is a cross-partner leak.", f.askedFor)
	}
}

// Likewise the roster.
func TestFR_FRN_002_APartnersRosterIsScopedToTheirOwnFranchise(t *testing.T) {
	rev := &fakeRevenue{subs: []revenue.SubscriberRow{{ID: 10, Username: "a@isp"}}}
	h := fullFranchiseHandler(nil, nil, rev)

	rec := serveFr(h, partner(), http.MethodGet, "/staff/my-subscribers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rev.scopeTo == nil {
		t.Fatal("ListSubscribers was called with a nil franchise id, which means every " +
			"subscriber on the deployment")
	}
	if *rev.scopeTo != 2 {
		t.Errorf("scoped to franchise %d, want 2", *rev.scopeTo)
	}
}

// An account with a franchise-scoped role but no franchise binding must not
// fall back to showing everything. Unreachable through the login flow, which
// is exactly why it is worth asserting: a misissued token is the case nobody
// tests.
func TestFR_FRN_002_AnUnboundFranchiseAccountSeesNothingNotEverything(t *testing.T) {
	rev := &fakeRevenue{subs: []revenue.SubscriberRow{{ID: 10, Username: "a@isp"}}}
	f := &fakeFranchises{pnl: samplePnL()}
	h := fullFranchiseHandler(f, nil, rev)
	unbound := Session{Username: "lco_user", Role: "lco"} // FranchiseID zero

	roster := serveFr(h, unbound, http.MethodGet, "/staff/my-subscribers", nil)
	if rev.scoped {
		t.Error("an unbound account reached the subscriber store; with a nil scope that " +
			"is every subscriber on the deployment")
	}
	if !strings.Contains(pageText(roster.Body.String()), "no franchise binding") {
		t.Errorf("the page should say why it is empty, got: %s", firstLines(roster.Body.String()))
	}

	pnl := serveFr(h, unbound, http.MethodGet, "/staff/my-pnl", nil)
	if f.askedFor != 0 {
		t.Errorf("an unbound account asked for franchise %d's P&L", f.askedFor)
	}
	if !strings.Contains(pageText(pnl.Body.String()), "no franchise binding") {
		t.Errorf("the P&L page should say why it is empty, got: %s", firstLines(pnl.Body.String()))
	}
}

// Onboarding validates before it writes, and says which field is wrong. The
// E.164 check exists because a mobile number typed the way everyone types it
// surfaced as a nonsensical commission-rate error.
func TestFR_FRN_006_OnboardingRefusesBadInputWithTheRightReason(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no name": {url.Values{"owner_name": {"Ravi"}, "mobile_number": {"+919876543210"}},
			"Name and owner name are required"},
		"no owner": {url.Values{"name": {"Anna Nagar"}, "mobile_number": {"+919876543210"}},
			"Name and owner name are required"},
		"no mobile": {url.Values{"name": {"Anna Nagar"}, "owner_name": {"Ravi"}},
			"Mobile number is required"},
		"local mobile format": {url.Values{
			"name": {"Anna Nagar"}, "owner_name": {"Ravi"}, "mobile_number": {"9800000000"},
		}, "E.164"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeFranchises{}
			h := fullFranchiseHandler(f, nil, nil)
			rec := serveFr(h, owner(), http.MethodPost, "/staff/franchise", tc.form)
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want a message containing %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if f.createReq.Name != "" {
				t.Errorf("invalid input reached the store: %+v", f.createReq)
			}
		})
	}
}

func TestFR_FRN_006_AValidOnboardingReportsThePartnerNumber(t *testing.T) {
	f := &fakeFranchises{created: &revenue.FranchiseRecord{ID: 7, Name: "Anna Nagar Networks"}}
	h := fullFranchiseHandler(f, nil, nil)

	rec := serveFr(h, owner(), http.MethodPost, "/staff/franchise", url.Values{
		"name":                {"  Anna Nagar Networks  "},
		"owner_name":          {"Ravi Kumar"},
		"mobile_number":       {"+919876543210"},
		"commission_rate_pct": {"12.5"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if f.createReq.Name != "Anna Nagar Networks" {
		t.Errorf("the name reached the store as %q; it should be trimmed", f.createReq.Name)
	}
	if f.createReq.CommissionRatePct != "12.5" {
		t.Errorf("commission rate = %q", f.createReq.CommissionRatePct)
	}
	if body := pageText(rec.Body.String()); !strings.Contains(body, "partner #7") {
		t.Errorf("the confirmation should name the new partner number, got: %s", firstLines(body))
	}
}

// A rejected write tells the operator what to check rather than showing a
// bare failure — in practice the commission rate, since everything else is
// validated before this point.
func TestFR_FRN_006_ARejectedOnboardingPointsAtTheCommissionRate(t *testing.T) {
	h := fullFranchiseHandler(&fakeFranchises{createErr: errFranchiseStore}, nil, nil)
	rec := serveFr(h, owner(), http.MethodPost, "/staff/franchise", url.Values{
		"name": {"Anna Nagar"}, "owner_name": {"Ravi"},
		"mobile_number": {"+919876543210"}, "commission_rate_pct": {"250"},
	})
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "commission rate") {
		t.Errorf("the message should point at the commission rate, got: %s", firstLines(body))
	}
	if strings.Contains(body, errFranchiseStore.Error()) {
		t.Errorf("the store's own error reached the page: %s", firstLines(body))
	}
}

// Every store failure is a readable page, not a 500 and not a blank screen.
// The operator is told which part could not be loaded.
func TestFR_FRN_001_StoreFailuresRenderAReadablePage(t *testing.T) {
	for name, tc := range map[string]struct {
		f      *fakeFranchises
		target string
		s      Session
		want   string
	}{
		"partner list": {&fakeFranchises{listErr: errFranchiseStore},
			"/staff/franchise", owner(), "Could not load franchise partners"},
		"consolidated": {&fakeFranchises{consolErr: errFranchiseStore},
			"/staff/franchise", owner(), "Could not load the consolidated P&L"},
		"one partner": {&fakeFranchises{pnlErr: errFranchiseStore},
			"/staff/franchise/2", owner(), "Could not load this partner's P&L"},
		"own pnl": {&fakeFranchises{pnlErr: errFranchiseStore},
			"/staff/my-pnl", partner(), "Could not load your P&L"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveFr(fullFranchiseHandler(tc.f, nil, nil), tc.s, http.MethodGet, tc.target, nil)
			if rec.Code != http.StatusOK {
				t.Errorf("want a rendered page, got %d", rec.Code)
			}
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q on the page, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if strings.Contains(pageText(rec.Body.String()), errFranchiseStore.Error()) {
				t.Error("the store's own error text reached the page")
			}
		})
	}
}

// An unconfigured deployment says so on every one of these screens rather
// than panicking or showing an empty table that reads as "no partners".
func TestFR_FRN_001_AnUnconfiguredDeploymentSaysSo(t *testing.T) {
	bare := &Handler{}
	for name, tc := range map[string]struct {
		s      Session
		target string
	}{
		"list":    {owner(), "/staff/franchise"},
		"detail":  {owner(), "/staff/franchise/2"},
		"own pnl": {partner(), "/staff/my-pnl"},
		"roster":  {partner(), "/staff/my-subscribers"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveFr(bare, tc.s, http.MethodGet, tc.target, nil)
			if !strings.Contains(strings.ToLower(pageText(rec.Body.String())), "not configured") {
				t.Errorf("want a 'not configured' message, got: %s", firstLines(rec.Body.String()))
			}
		})
	}

	// The onboarding form is the exception: a POST to an unconfigured
	// deployment is refused with a status, not a rendered page, because
	// there is nothing to render into.
	rec := serveFr(bare, owner(), http.MethodPost, "/staff/franchise", url.Values{"name": {"x"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 from an unconfigured onboarding POST, got %d", rec.Code)
	}
}

// A bad id or a bad date window is reported, and neither reaches the store.
func TestFR_FRN_003_BadIdsAndDateWindowsAreRefused(t *testing.T) {
	f := &fakeFranchises{pnl: samplePnL()}
	h := fullFranchiseHandler(f, nil, nil)

	if rec := serveFr(h, owner(), http.MethodGet, "/staff/franchise/not-a-number", nil); rec.Code != http.StatusNotFound {
		t.Errorf("want 404 for a non-numeric id, got %d", rec.Code)
	}

	f.askedFor = 0
	rec := serveFr(h, owner(), http.MethodGet, "/staff/franchise/2?from=yesterday", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("a bad date should render the page with a message, got %d", rec.Code)
	}
	if f.askedFor != 0 {
		t.Error("a malformed date window still queried the store")
	}
}

// A partner id that does not exist is a 404, not an empty P&L that reads as
// "this partner earned nothing".
func TestFR_FRN_003_AnUnknownPartnerIsNotFound(t *testing.T) {
	h := fullFranchiseHandler(&fakeFranchises{pnl: nil}, nil, nil)
	if rec := serveFr(h, owner(), http.MethodGet, "/staff/franchise/404", nil); rec.Code != http.StatusNotFound {
		t.Errorf("want 404 for an unknown partner, got %d", rec.Code)
	}
}

// Voucher commission is loaded alongside the recharge P&L, scoped to the same
// partner, and a failure there leaves the rest of the page intact — the
// recharge figures are the primary content.
func TestCRD_EXP_010_VoucherCommissionIsBestEffortAndSameScoped(t *testing.T) {
	v := &fakeVoucherCommissions{summary: &hotspot.VoucherCommissionSummary{
		FranchiseID: 2, VoucherCount: 40, TotalSales: "4000.00", TotalCommission: "400.00",
	}}
	h := fullFranchiseHandler(&fakeFranchises{pnl: samplePnL()}, v, nil)

	rec := serveFr(h, partner(), http.MethodGet, "/staff/my-pnl", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if v.askedFor != 2 {
		t.Errorf("voucher commission was requested for franchise %d, want the session's 2", v.askedFor)
	}

	// A failing voucher store must not take the page with it.
	broken := fullFranchiseHandler(&fakeFranchises{pnl: samplePnL()},
		&fakeVoucherCommissions{err: errFranchiseStore}, nil)
	rec = serveFr(broken, partner(), http.MethodGet, "/staff/my-pnl", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("a voucher-summary failure should leave the P&L page intact, got %d", rec.Code)
	}
	if !strings.Contains(pageText(rec.Body.String()), "13050.00") {
		t.Errorf("the recharge commission is missing from the page: %s", firstLines(rec.Body.String()))
	}
}

// The consolidated view renders the totals an owner reads.
func TestFR_FRN_001_TheConsolidatedViewShowsTheTotals(t *testing.T) {
	h := fullFranchiseHandler(&fakeFranchises{
		list: []revenue.FranchiseRecord{{ID: 2, Name: "Anna Nagar Networks", Status: "active"}},
		consolidated: &revenue.ConsolidatedPnL{
			Partners:         []revenue.FranchisePnL{*samplePnL()},
			TotalRecharges:   "104400.00",
			CommissionEarned: "13050.00",
			NetToISP:         "91350.00",
		},
	}, nil, nil)

	body := pageText(serveFr(h, owner(), http.MethodGet, "/staff/franchise", nil).Body.String())
	for _, want := range []string{"Anna Nagar Networks", "104400.00", "13050.00", "91350.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from the consolidated view", want)
		}
	}
}

// pageText is the rendered page as a reader sees it: entities decoded and
// whitespace collapsed.
//
// Asserting on the raw HTML instead cost three false failures here. The
// template escapes an apostrophe to &#39; and an ampersand to &amp;, so
// "Could not load this partner's P&L" never appears in the source of a page
// that displays exactly that. A test that matches only the messages with no
// punctuation in them is a test that stops noticing the others.
func pageText(body string) string {
	return strings.Join(strings.Fields(html.UnescapeString(body)), " ")
}

// firstLines trims a rendered page down to something a failure message can
// carry without burying the assertion.
func firstLines(body string) string {
	t := pageText(body)
	if len(t) > 300 {
		return t[:300] + "..."
	}
	return t
}
