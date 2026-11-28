package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// The owner's Revenue screen and the Billing lookup: what they show when a
// figure could not be read.
//
// FR-REV-002 (reconciliation variance) and FR-REV-003 (collections) on the
// Revenue screen. The Billing lookup has no requirement of its own; the
// wallet ledger it shows is FR-BIL-003's. Both screens
// read several figures and, when one query failed, rendered its zero value
// as though it had been read: "0 unbilled subscribers", "₹0.00 total wallet
// balance", "Nobody is in arrears", a wallet balance of ₹0.00. The first
// four were not even logged. Each of those is the good-news reading of a
// failure — exactly the figure an owner or a CSR would act on without a
// second look. A figure that could not be read now says so.

var errFigure = errors.New("store: query failed")

var tags = regexp.MustCompile(`<[^>]*>`)

// visibleText is what a reader sees: tags removed, entities decoded,
// whitespace collapsed, so "Service stopped 3" matches across the markup
// between the heading and the number.
func visibleText(body string) string {
	if i := strings.Index(body, `<main`); i >= 0 {
		body = body[i:]
	}
	return pageText(tags.ReplaceAllString(body, " "))
}

type revenueFigures struct {
	unbilled  int
	variance  decimal.Decimal
	balance   decimal.Decimal
	subs      int
	stages    []revenue.CollectionsStageRow
	recovery  []revenue.RecoveryMonth
	failing   map[string]bool
	gotMonths int
}

func (f *revenueFigures) fail(name string) error {
	if f.failing[name] {
		return errFigure
	}
	return nil
}

func (f *revenueFigures) GetUnbilledActiveSubscribers(context.Context) (int, error) {
	return f.unbilled, f.fail("unbilled")
}
func (f *revenueFigures) GetLedgerVariance(context.Context) (decimal.Decimal, error) {
	return f.variance, f.fail("variance")
}
func (f *revenueFigures) GetTotalWalletBalance(context.Context) (decimal.Decimal, error) {
	return f.balance, f.fail("balance")
}
func (f *revenueFigures) ListSubscribers(context.Context, *int) ([]revenue.SubscriberRow, error) {
	return make([]revenue.SubscriberRow, f.subs), f.fail("subscribers")
}
func (f *revenueFigures) GetCollectionsByDunningStage(context.Context) ([]revenue.CollectionsStageRow, error) {
	if f.failing["collections"] {
		return nil, errFigure
	}
	return f.stages, nil
}
func (f *revenueFigures) GetMonthlyRecovery(_ context.Context, months int) ([]revenue.RecoveryMonth, error) {
	f.gotMonths = months
	if f.failing["recovery"] {
		return nil, errFigure
	}
	return f.recovery, nil
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func healthyFigures() *revenueFigures {
	return &revenueFigures{
		unbilled: 4, variance: d("0.00"), balance: d("125000.50"), subs: 180,
		stages: []revenue.CollectionsStageRow{
			{DunningState: "remind_3d", Subscribers: 6, Outstanding: d("3000"), ServiceStopped: false},
			{DunningState: "soft_suspended", Subscribers: 2, Outstanding: d("1000"), ServiceStopped: true},
			{DunningState: "hard_suspended", Subscribers: 1, Outstanding: d("500"), ServiceStopped: true},
		},
		recovery: []revenue.RecoveryMonth{{Collected: d("1200")}, {Collected: d("1000")}},
	}
}

func serveRevenue(h *Handler, s Session) string {
	r := httptest.NewRequest(http.MethodGet, "/staff/revenue", nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	w := httptest.NewRecorder()
	h.Revenue(w, r)
	return visibleText(w.Body.String())
}

func TestFR_REV_003_TheRevenueFigures(t *testing.T) {
	f := healthyFigures()
	body := serveRevenue(&Handler{revenue: f}, owner())

	for _, want := range []string{
		"Unbilled active subscribers 4",
		"₹125000.50",
		"Across 180 subscribers",
		"balanced",
		// Outstanding is every lapsed stage; service stopped counts only the
		// suspended ones.
		"Outstanding ₹4500.00",
		"Service stopped 3",
		"₹1200.00",
		"+20.00%",
		"vs ₹1000.00 last month",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in: %s", want, firstLines(body))
		}
	}
	if f.gotMonths != 2 {
		t.Errorf("asked for %d months of recovery; this month against last needs 2", f.gotMonths)
	}
	if strings.Contains(body, "unavailable") {
		t.Error("a healthy page says something is unavailable")
	}
}

// The variance threshold is the nightly alert's: within a paisa either way is
// balanced.
func TestFR_REV_002_TheVarianceThresholdMatchesTheAlert(t *testing.T) {
	for v, want := range map[string]string{
		"0.01": "balanced", "-0.01": "balanced", "0.02": "investigate", "-5.00": "investigate",
	} {
		f := healthyFigures()
		f.variance = d(v)
		if body := serveRevenue(&Handler{revenue: f}, owner()); !strings.Contains(body, want) {
			t.Errorf("variance %s: want %q", v, want)
		}
	}
}

// A quiet month has no recovery row at all, and the screen must neither
// panic nor invent a comparison.
func TestFR_REV_003_AQuietMonthHasNothingToCompare(t *testing.T) {
	for name, rows := range map[string][]revenue.RecoveryMonth{
		"no rows":    nil,
		"one row":    {{Collected: d("900")}},
		"last was 0": {{Collected: d("900")}, {Collected: d("0")}},
	} {
		t.Run(name, func(t *testing.T) {
			f := healthyFigures()
			f.recovery = rows
			body := serveRevenue(&Handler{revenue: f}, owner())
			if !strings.Contains(body, "no prior month to compare against") {
				t.Errorf("want no comparison: %s", firstLines(body))
			}
		})
	}
	f := healthyFigures()
	f.recovery = []revenue.RecoveryMonth{{Collected: d("800")}, {Collected: d("1000")}}
	if body := serveRevenue(&Handler{revenue: f}, owner()); !strings.Contains(body, "-20.00%") {
		t.Errorf("a fall should be shown as one: %s", firstLines(body))
	}
	f.stages = nil
	if body := serveRevenue(&Handler{revenue: f}, owner()); !strings.Contains(body, "Nobody is in arrears") {
		t.Error("an empty ladder should say nobody is in arrears")
	}
}

// Each figure that could not be read says so, and only that one.
func TestFR_REV_003_AFigureThatFailedIsNotShownAsZero(t *testing.T) {
	for name, tc := range map[string]struct {
		says    string   // what the page must say instead
		notSays []string // the good-news reading it must not give
	}{
		"unbilled":    {"Unbilled active subscribers unavailable", []string{"Unbilled active subscribers 0"}},
		"variance":    {"Ledger variance unavailable", []string{"balanced", "investigate"}},
		"balance":     {"Total wallet balance unavailable", []string{"₹0.00 Across"}},
		"subscribers": {"Subscriber count unavailable", []string{"Across 0 subscribers"}},
		"collections": {"Could not load collections by stage", []string{"Nobody is in arrears", "Outstanding ₹0.00", "Service stopped 0"}},
		"recovery":    {"Collected this month unavailable", []string{"no prior month to compare against", "₹0.00 vs"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := healthyFigures()
			f.failing = map[string]bool{name: true}
			body := serveRevenue(&Handler{revenue: f}, owner())

			if !strings.Contains(body, tc.says) {
				t.Errorf("want %q in: %s", tc.says, body)
			}
			for _, bad := range tc.notSays {
				if strings.Contains(body, bad) {
					t.Errorf("a failed %s query was shown as %q", name, bad)
				}
			}
			if !strings.Contains(body, "could not be loaded") {
				t.Error("no banner says some figures are missing")
			}
			if strings.Contains(body, errFigure.Error()) {
				t.Error("the store's error reached the page")
			}
		})
	}
}

func TestFR_REV_003_RevenueIsOwnerOnlyAndSaysWhenUnconfigured(t *testing.T) {
	for _, role := range []string{"csr", "technician", "noc_engineer", "billing_admin", "lco"} {
		r := httptest.NewRequest(http.MethodGet, "/staff/revenue", nil)
		r = r.WithContext(context.WithValue(r.Context(), sessionKey, sess(role)))
		w := httptest.NewRecorder()
		(&Handler{revenue: healthyFigures()}).Revenue(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s reached the revenue screen: %d", role, w.Code)
		}
	}
	body := serveRevenue(&Handler{}, owner())
	if !strings.Contains(body, "not configured") {
		t.Errorf("unconfigured: %s", firstLines(body))
	}
	// And it shows no figures at all, rather than a page of zeros under the
	// banner.
	for _, bad := range []string{"balanced", "Nobody is in arrears", "Unbilled active subscribers 0"} {
		if strings.Contains(body, bad) {
			t.Errorf("an unconfigured screen shows %q", bad)
		}
	}
}

// ── Billing lookup ──────────────────────────────────────────────────────────

type walletFigures struct {
	balance             decimal.Decimal
	entries             []api.LedgerEntry
	balanceErr, listErr error
	gotID, gotLimit     int
}

func (f *walletFigures) GetSubscriberBalance(_ context.Context, id int) (decimal.Decimal, error) {
	f.gotID = id
	return f.balance, f.balanceErr
}

func (f *walletFigures) ListLedgerEntries(_ context.Context, id int, _, _ *time.Time, limit int) ([]api.LedgerEntry, error) {
	f.gotLimit = limit
	return f.entries, f.listErr
}

func serveBilling(h *Handler, s Session, query string) string {
	r := httptest.NewRequest(http.MethodGet, "/staff/billing"+query, nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	w := httptest.NewRecorder()
	h.Billing(w, r)
	return visibleText(w.Body.String())
}

func TestTheBillingLookup(t *testing.T) {
	f := &walletFigures{balance: d("349.00"), entries: []api.LedgerEntry{
		{EntryType: "recharge", Description: "UPI top-up", Amount: "500.00", BalanceAfter: "849.00", CreatedAt: time.Now()},
	}}
	body := serveBilling(&Handler{billing: f}, sess("billing_admin"), "?subscriber_id=+12+")
	if f.gotID != 12 {
		t.Errorf("looked up subscriber %d, want 12", f.gotID)
	}
	if f.gotLimit <= 0 {
		t.Error("the ledger was listed without a bound")
	}
	for _, want := range []string{"Wallet balance ₹349.00", "UPI top-up", "₹849.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in: %s", want, firstLines(body))
		}
	}

	if body := serveBilling(&Handler{billing: f}, sess("billing_admin"), ""); !strings.Contains(body, "Enter a subscriber id") {
		t.Error("the empty form should invite a lookup")
	}
	if body := serveBilling(&Handler{billing: f}, sess("billing_admin"), "?subscriber_id=ravi"); !strings.Contains(body, "numeric subscriber id") {
		t.Errorf("a non-numeric id: %s", firstLines(body))
	}
	if body := serveBilling(&Handler{}, sess("billing_admin"), "?subscriber_id=12"); !strings.Contains(body, "not configured") {
		t.Errorf("unconfigured: %s", firstLines(body))
	}
}

// A balance that could not be read is not ₹0.00, and a ledger that could not
// be read is not "no entries".
func TestABillingFigureThatFailedIsNotShownAsZero(t *testing.T) {
	f := &walletFigures{balanceErr: errFigure, entries: []api.LedgerEntry{
		{EntryType: "recharge", Description: "UPI top-up", Amount: "500.00", BalanceAfter: "500.00"},
	}}
	body := serveBilling(&Handler{billing: f}, sess("billing_admin"), "?subscriber_id=12")
	if strings.Contains(body, "Wallet balance ₹0.00") {
		t.Error("a balance that could not be read was shown as ₹0.00")
	}
	if !strings.Contains(body, "Wallet balance unavailable") {
		t.Errorf("want the balance marked unavailable: %s", firstLines(body))
	}
	if !strings.Contains(body, "UPI top-up") {
		t.Error("the ledger, which did load, was dropped")
	}

	f = &walletFigures{balance: d("10.00"), listErr: errFigure}
	body = serveBilling(&Handler{billing: f}, sess("billing_admin"), "?subscriber_id=12")
	if strings.Contains(body, "No ledger entries for this subscriber") {
		t.Error("a ledger that could not be read was reported as empty")
	}
	if !strings.Contains(body, "Could not load the ledger") {
		t.Errorf("the failure is not reported: %s", firstLines(body))
	}
}

// ── The subscriber page ─────────────────────────────────────────────────────
//
// The CSR's main screen, and the same defect five times: a subscriber lookup
// that failed answered "No subscriber with that id" (404), a session read
// that failed said "Offline — no active session", and a failed balance,
// ledger or ticket read showed ₹0.00, "No ledger entries", "No tickets".
// The session one is the worst: whether the line is up is the whole question
// when somebody calls to say the internet is down, and a failed Redis read
// sent the CSR troubleshooting a line that may be fine.

type sessionFigure struct {
	sess *portal.ActiveSession
	err  error
}

func (f *sessionFigure) GetActiveSession(context.Context, int) (*portal.ActiveSession, error) {
	return f.sess, f.err
}

type ticketFigures struct {
	list []portal.TicketEntry
	err  error
}

func (f *ticketFigures) ListTickets(context.Context, int) ([]portal.TicketEntry, error) {
	return f.list, f.err
}
func (f *ticketFigures) UpdateTicketAdmin(context.Context, int, *string, *int, *string) (*api.TicketRecord, error) {
	return nil, nil
}

func subscriberPage(subs *fakeSubscribers, sessions *sessionFigure, wallet *walletFigures, tickets *ticketFigures) (int, string) {
	h := &Handler{subscribers: subs}
	if sessions != nil {
		h.sessions = sessions
	}
	if wallet != nil {
		h.billing = wallet
	}
	if tickets != nil {
		h.tickets = tickets
	}
	r := httptest.NewRequest(http.MethodGet, "/staff/subscribers/12", nil)
	r.SetPathValue("id", "12")
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, owner()))
	w := httptest.NewRecorder()
	h.SubscriberDetail(w, r)
	return w.Code, visibleText(w.Body.String())
}

func ravi() *fakeSubscribers {
	return &fakeSubscribers{rec: &api.SubscriberRecord{ID: 12, Username: "ravi", Status: "active", PlanID: 1}}
}

func TestASubscriberLookupThatFailedIsNotNoSuchSubscriber(t *testing.T) {
	code, body := subscriberPage(&fakeSubscribers{err: errFigure}, nil, nil, nil)
	if code == http.StatusNotFound || strings.Contains(body, "No subscriber with that id") {
		t.Errorf("a failed lookup was reported as a missing subscriber: %d %s", code, firstLines(body))
	}
	if code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", code)
	}
	// And a genuinely missing one still is.
	if code, _ := subscriberPage(&fakeSubscribers{}, nil, nil, nil); code != http.StatusNotFound {
		t.Errorf("a missing subscriber: %d, want 404", code)
	}
}

func TestASessionReadThatFailedIsNotOffline(t *testing.T) {
	_, body := subscriberPage(ravi(), &sessionFigure{err: errFigure}, nil, nil)
	if strings.Contains(body, "Offline") {
		t.Error("a session read that failed was shown as offline")
	}
	if !strings.Contains(body, "Connection status unavailable") {
		t.Errorf("want the status marked unavailable: %s", body)
	}

	// Control: no session and no error is offline.
	if _, body := subscriberPage(ravi(), &sessionFigure{}, nil, nil); !strings.Contains(body, "Offline") {
		t.Error("a subscriber with no session should read as offline")
	}
	if _, body := subscriberPage(ravi(), &sessionFigure{sess: &portal.ActiveSession{AssignedIP: "100.64.0.14"}}, nil, nil); !strings.Contains(body, "online") || !strings.Contains(body, "100.64.0.14") {
		t.Error("a live session should read as online with its address")
	}
	// Unconfigured is not offline either.
	if _, body := subscriberPage(ravi(), nil, nil, nil); strings.Contains(body, "Offline") {
		t.Error("with no session store wired, the page claimed the subscriber is offline")
	}
}

func TestSubscriberMoneyAndTicketsThatFailedAreNotEmpty(t *testing.T) {
	wallet := &walletFigures{balanceErr: errFigure, listErr: errFigure}
	tickets := &ticketFigures{err: errFigure}
	_, body := subscriberPage(ravi(), &sessionFigure{}, wallet, tickets)

	for _, bad := range []string{"balance ₹0.00", "No ledger entries", "No tickets"} {
		if strings.Contains(body, bad) {
			t.Errorf("a failed read was shown as %q", bad)
		}
	}
	for _, want := range []string{"balance unavailable", "Could not load the ledger", "Could not load tickets"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in: %s", want, body)
		}
	}

	// Control: empty reads that succeeded still say so.
	_, body = subscriberPage(ravi(), &sessionFigure{}, &walletFigures{balance: d("0")}, &ticketFigures{})
	for _, want := range []string{"balance ₹0.00", "No ledger entries", "No tickets"} {
		if !strings.Contains(body, want) {
			t.Errorf("an empty read that succeeded lost %q", want)
		}
	}
}

// ── Home and the directory pager ────────────────────────────────────────────

func TestHomeSendsEachRoleToItsFirstSection(t *testing.T) {
	serve := func(s *Session) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/staff/", nil)
		if s != nil {
			r = r.WithContext(context.WithValue(r.Context(), sessionKey, *s))
		}
		w := httptest.NewRecorder()
		(&Handler{}).Home(w, r)
		return w
	}
	for _, role := range []string{"isp_owner", "billing_admin", "csr", "technician", "noc_engineer", "lco"} {
		s := sess(role)
		w := serve(&s)
		want := AllowedSections(role, false)[0].Path
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
			t.Errorf("%s: %d → %q, want %q", role, w.Code, w.Header().Get("Location"), want)
		}
	}
	nobody := Session{Username: "x", Role: "subscriber"}
	if w := serve(&nobody); w.Code != http.StatusForbidden {
		t.Errorf("a role with no sections: %d", w.Code)
	}
	if w := serve(nil); w.Header().Get("Location") != "/staff/login" {
		t.Errorf("no session: %q", w.Header().Get("Location"))
	}
}

// The pager carries every active filter, so paging does not drop the search
// the operator is paging through.
func TestDirectoryURLCarriesEveryFilter(t *testing.T) {
	for f, want := range map[DirectoryFilter]string{
		{}:                                   "/staff/subscribers",
		{Offset: 0, GroupID: 0}:              "/staff/subscribers",
		{Query: "ravi kumar & sons"}:         "/staff/subscribers?q=ravi+kumar+%26+sons",
		{Status: "active", View: "expiring"}: "/staff/subscribers?status=active&view=expiring",
		{GroupID: 4, Query: "9840"}:          "/staff/subscribers?group_id=4&q=9840",
		{Query: "x", Status: "suspended", View: "unpaid", GroupID: 2}: "/staff/subscribers?group_id=2&offset=50&q=x&status=suspended&view=unpaid",
	} {
		offset := 0
		if strings.Contains(want, "offset=") {
			offset = 50
		}
		if got := directoryURL(f, offset); got != want {
			t.Errorf("directoryURL(%+v, %d) = %q, want %q", f, offset, got, want)
		}
	}
}
