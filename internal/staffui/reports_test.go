package staffui

import (
	"context"
	"errors"
	"html"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/shopspring/decimal"
)

// The report catalogue in the console — FR-RPT-004.
//
// A Go template resolves field names at run time, so a wrong name compiles
// cleanly and fails only when the branch actually renders. These tests render
// every report in the catalogue with real rows for exactly that reason: the
// package has been bitten by it before (see render.go's own note), and
// fourteen report views is fourteen chances.

// oneOfEverything gives the fake a row for every report, so each view renders
// with data in it rather than hitting its empty-state branch — the empty
// branch is tested separately, and a view that only ever renders empty proves
// nothing about its columns.
func oneOfEverything() *fakeRosterStore {
	now := time.Now()
	return &fakeRosterStore{
		planMix: []reporting.PlanMixRow{{
			PlanID: 1, PlanName: "Fibre 100", Price: decimal.RequireFromString("799.00"),
			TotalSubscribers: 3, ActiveSubscribers: 3, MRR: decimal.RequireFromString("2397.00"),
		}},
		growth: []reporting.GrowthRow{{
			Month: now, NewConnections: 4, Reactivations: 1, Churned: 1, Suspended: 2, NetGrowth: 4,
		}},
		ticketResolution: []reporting.TicketResolutionRow{{
			Month: now, Category: "billing", Priority: "P3", Raised: 5, Resolved: 4, Reopens: 1,
			MedianResolutionHours: ptrFloat(3.5), ResolvedWithinSLA: 4,
		}},
		collection: []reporting.CollectionRow{{
			FranchiseID: 1, FranchiseName: "North", FranchiseStatus: "active", Month: now,
			Billed: decimal.RequireFromString("1000.00"), InvoicesRaised: 2,
			Collected:         decimal.RequireFromString("900.00"),
			Commission:        decimal.RequireFromString("90.00"),
			PayingSubscribers: 2, CollectionRatePct: ptrFloat(90),
		}},
		rows: []reporting.SubscriberRosterRow{{
			SubscriberID: 1, Username: "a@isp", Status: "active", PlanName: "Fibre 100",
			MobileNumber: "+919876543210", Groups: []string{"Anna Nagar"}, CreatedAt: now,
		}},
		expiring: []reporting.ExpiringRow{{
			SubscriberID: 1, Username: "a@isp", MobileNumber: "+919876543210",
			PlanName: "Fibre 100", Status: "active", PlanExpiry: now.Add(48 * time.Hour),
			DaysLeft: 2, PlanPrice: decimal.RequireFromString("799.00"),
			WalletBalance: decimal.RequireFromString("100.00"),
		}},
		planChanges: []reporting.PlanChangeRow{{
			SubscriberID: 1, Username: "a@isp", FromPlan: "Fibre 100", ToPlan: "Fibre 200",
			EffectiveOn: now, Status: "applied", Reason: "upgrade", ScheduledBy: "csr",
			ScheduledAt: now, AppliedAt: &now,
		}},
		paymentMethods: []reporting.PaymentMethodRow{{
			Month: now, Method: "cash", Payments: 3, Total: decimal.RequireFromString("2397.00"),
		}},
		staffCollection: []reporting.StaffCollectionRow{{
			Month: now, RecordedBy: "tech_user", Payments: 2,
			CashTotal:    decimal.RequireFromString("1000.00"),
			DigitalTotal: decimal.RequireFromString("799.00"),
			Total:        decimal.RequireFromString("1799.00"),
		}},
		invoiceRegister: []reporting.InvoiceRegisterRow{{
			InvoiceID: 7, InvoiceDate: now, SubscriberID: 1, Username: "a@isp",
			PlanName: "Fibre 100", Description: "Monthly rental",
			BaseAmount: decimal.RequireFromString("799.00"),
			CGST:       decimal.RequireFromString("71.91"),
			SGST:       decimal.RequireFromString("71.91"),
			IGST:       decimal.Zero,
			Total:      decimal.RequireFromString("942.82"),
		}},
		tax: []reporting.TaxRow{{
			Month: now, CGSTRate: decimal.RequireFromString("9.00"),
			SGSTRate: decimal.RequireFromString("9.00"), IGSTRate: decimal.Zero,
			Invoices: 1, TaxableValue: decimal.RequireFromString("799.00"),
			CGST: decimal.RequireFromString("71.91"), SGST: decimal.RequireFromString("71.91"),
			IGST: decimal.Zero, TaxTotal: decimal.RequireFromString("143.82"),
		}},
		turnaround: []reporting.TurnaroundRow{{
			Month: now, Category: "billing", Priority: "P3",
			Raised: 5, Resolved: 4, StillOpen: 1,
			MedianHours: ptrFloat(3.5), P90Hours: ptrFloat(19.25), MaxHours: ptrFloat(48),
		}},
		dataUsers: []reporting.DataUserRow{{
			SubscriberID: 1, Username: "a@isp", PlanName: "Fibre 100", Sessions: 12,
			InputOctets: 3 << 30, OutputOctets: 1 << 30, TotalBytes: 4 << 30,
			AllowanceBytes: 1 << 40,
		}},
		fupExceeded: []reporting.FUPExceededRow{{
			SubscriberID: 1, Username: "a@isp", PlanName: "Fibre 100",
			UsedBytes: 3 << 40, AllowanceBytes: 1 << 40, TopupBytes: 1 << 39,
			AppliedRate: "10M/10M", FUPActive: true,
		}},
	}
}

func ptrFloat(v float64) *float64 { return &v }

// serveReportCSV calls the download handler with the {report} path value set.
// servePR fills {id} from the third path segment, which is the shape every
// other console route takes; this route names its wildcard "report".
func serveReportCSV(h *Handler, role, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey,
		Session{Username: role + "_user", Role: role}))
	path := strings.TrimPrefix(r.URL.Path, "/staff/reports/")
	r.SetPathValue("report", path)
	w := httptest.NewRecorder()
	h.ExportReportCSV(w, r)
	return w
}

// ── The index ───────────────────────────────────────────────────────────────

func TestFR_RPT_004_TheIndexGroupsEveryReportByArea(t *testing.T) {
	h := &Handler{reporting: oneOfEverything()}

	w := servePR(h, "isp_owner", "GET", "/staff/reports", nil, h.Reports)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<no value>") {
		t.Error("the index rendered <no value>, so a template field name does not resolve")
	}

	// Every area heading, and every report's title and link.
	for _, area := range reporting.Areas() {
		// <h3>, not just the text: the top bar has a nav link called
		// "Network" too, and matching that would pass whatever the index did.
		if !strings.Contains(body, "<h3>"+area+"</h3>") {
			t.Errorf("area %q has no heading on the index", area)
		}
	}
	for _, d := range reporting.Catalogue() {
		if !strings.Contains(body, "/staff/reports?report="+d.Name) {
			t.Errorf("%q is in the catalogue but not linked from the index", d.Name)
		}
		// html.EscapeString, because html/template escapes & and ' — a title
		// like "Growth & churn" or a summary with an apostrophe never
		// appears literally in the rendered body.
		if !strings.Contains(body, html.EscapeString(d.Title)) {
			t.Errorf("%q's title is missing from the index", d.Name)
		}
		// The summary is what tells an operator which report they want.
		if !strings.Contains(body, html.EscapeString(d.Summary)) {
			t.Errorf("%q's summary is missing from the index", d.Name)
		}
	}
	// And the filter box itself.
	if !strings.Contains(body, `name="q"`) {
		t.Error("the index has no filter box")
	}
}

// The filter is server-side as well as live, so it works with scripting off
// and a filtered catalogue can be linked to.
func TestFR_RPT_004_TheFilterBoxNarrowsTheIndexServerSide(t *testing.T) {
	h := &Handler{reporting: oneOfEverything()}

	w := servePR(h, "isp_owner", "GET", "/staff/reports?q=GST", nil, h.Reports)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	// "GST" appears only in the tax report's summary.
	if !strings.Contains(body, "/staff/reports?report=tax-charged") {
		t.Error("filtering by GST excludes the tax report")
	}
	if strings.Contains(body, "/staff/reports?report=plan-mix") {
		t.Error("filtering by GST still offers plan mix, so the filter is not applied server-side")
	}
	// An area left with nothing must not keep its heading.
	if strings.Contains(body, "<h3>"+reporting.AreaNetwork+"</h3>") {
		t.Error("an area with no matching report kept its heading")
	}
}

func TestFR_RPT_004_AFilterMatchingNothingSaysSo(t *testing.T) {
	h := &Handler{reporting: oneOfEverything()}
	w := servePR(h, "isp_owner", "GET", "/staff/reports?q=zzzznothing", nil, h.Reports)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No report matches") {
		t.Error("a filter matching nothing rendered an empty page with no explanation")
	}
}

// ── Every report's own view ─────────────────────────────────────────────────

// The test named in the handler's default branch. Every catalogue entry must
// render on the console, with data, without a template error.
func TestFR_RPT_004_EveryCatalogueEntryHasAConsoleView(t *testing.T) {
	for _, d := range reporting.Catalogue() {
		t.Run(d.Name, func(t *testing.T) {
			h := &Handler{reporting: oneOfEverything()}
			w := servePR(h, "isp_owner", "GET", "/staff/reports?report="+d.Name, nil, h.Reports)

			if w.Code != 200 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if strings.Contains(body, "<no value>") {
				t.Error("rendered <no value>, so a template field name does not resolve")
			}
			if strings.Contains(body, "could not be rendered") {
				t.Fatalf("template execution failed: %s", body)
			}
			// Its own heading and summary, so an operator can tell which
			// report they are looking at.
			if !strings.Contains(body, html.EscapeString(d.Title)) {
				t.Errorf("the page does not show the report's title %q", d.Title)
			}
			if !strings.Contains(body, html.EscapeString(d.Summary)) {
				t.Errorf("the page does not show the report's summary")
			}
			// A CSV link to the console route, never to /api/v1 — the API
			// wants a Bearer JWT and an operator has a session cookie.
			if !strings.Contains(body, "/staff/reports/"+d.Name+".csv") {
				t.Error("the page offers no CSV download from the console route")
			}
			if strings.Contains(body, "/api/v1/reports/") {
				t.Error("the page links to the API, which would 401 for a console session")
			}
			// Not the empty state: the fake supplied a row for every report,
			// so an empty table here means the view read the wrong field.
			if strings.Contains(body, `class="empty"`) {
				t.Error("the view rendered its empty state despite being given a row, " +
					"so it is reading a field the handler did not fill")
			}
		})
	}
}

// Each report is offered only the window it declares. A month box on a
// point-in-time snapshot implies a history it does not have.
func TestFR_RPT_004_EachReportOffersOnlyTheWindowItDeclares(t *testing.T) {
	for _, d := range reporting.Catalogue() {
		t.Run(d.Name, func(t *testing.T) {
			h := &Handler{reporting: oneOfEverything()}
			w := servePR(h, "isp_owner", "GET", "/staff/reports?report="+d.Name, nil, h.Reports)
			body := w.Body.String()

			hasMonths := strings.Contains(body, `name="months"`)
			hasDays := strings.Contains(body, `name="days"`)

			if hasMonths != d.TakesMonths() {
				t.Errorf("months box = %v but TakesMonths = %v", hasMonths, d.TakesMonths())
			}
			if hasDays != d.TakesDays() {
				t.Errorf("days box = %v but TakesDays = %v", hasDays, d.TakesDays())
			}
		})
	}
}

// A typo or a stale bookmark must be named, not silently served as a
// different report. The screen used to fall back to plan mix, which would
// show one report's figures under another's name.
func TestFR_RPT_004_AnUnknownReportIsNamedNotSubstituted(t *testing.T) {
	h := &Handler{reporting: oneOfEverything()}
	w := servePR(h, "isp_owner", "GET", "/staff/reports?report=no-such-report", nil, h.Reports)

	if w.Code != 404 {
		t.Fatalf("got %d, want 404: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "no-such-report") {
		t.Error("the refusal does not name the report that was asked for")
	}
	// And emphatically not somebody else's figures.
	if strings.Contains(body, "Plan mix &mdash;") || strings.Contains(body, "current snapshot") {
		t.Error("an unknown report fell back to plan mix")
	}
}

func TestFR_RPT_004_AStoreFailureIsReportedNotRenderedEmpty(t *testing.T) {
	for _, name := range []string{
		reporting.ReportExpiring, reporting.ReportTaxCharged, reporting.ReportFUPExceeded,
	} {
		h := &Handler{reporting: &fakeRosterStore{err: errors.New("boom")}}
		w := servePR(h, "isp_owner", "GET", "/staff/reports?report="+name, nil, h.Reports)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Could not load that report") {
			t.Errorf("%s: got %d: %s", name, w.Code, w.Body.String())
		}
	}
}

func TestFR_RPT_004_AnUnwiredDeploymentSaysSo(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/reports", nil, h.Reports)
	if !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}

// ── The console CSV route ───────────────────────────────────────────────────

func TestFR_RPT_004_EveryReportDownloadsFromTheConsoleRoute(t *testing.T) {
	for _, d := range reporting.Catalogue() {
		t.Run(d.Name, func(t *testing.T) {
			h := &Handler{reporting: oneOfEverything()}
			w := serveReportCSV(h, "isp_owner", "/staff/reports/"+d.Name+".csv")

			if w.Code != 200 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
				t.Errorf("Content-Type = %q", ct)
			}
			disp := w.Header().Get("Content-Disposition")
			if !strings.HasPrefix(disp, "attachment;") || !strings.Contains(disp, d.Name) {
				t.Errorf("Content-Disposition = %q", disp)
			}
			// A header row at minimum, plus the one row the fake supplied.
			lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
			if len(lines) < 2 {
				t.Errorf("the CSV has %d line(s); want a header and a row", len(lines))
			}
		})
	}
}

func TestFR_RPT_004_TheCSVRouteRefusesAnUnknownReport(t *testing.T) {
	h := &Handler{reporting: oneOfEverything()}
	w := serveReportCSV(h, "isp_owner", "/staff/reports/no-such-report.csv")
	if w.Code != 404 {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// A group filter on a report that cannot honour it is refused before any
// header is written — once the 200 and the Content-Disposition are out, the
// operator has a file named after a group holding figures for the whole base.
func TestFR_RPT_004_TheCSVRouteRefusesAnUnhonourableGroupFilter(t *testing.T) {
	h := &Handler{reporting: oneOfEverything()}
	w := serveReportCSV(h, "isp_owner", "/staff/reports/plan-mix.csv?group_id=4")

	if w.Code != 422 {
		t.Fatalf("got %d, want 422: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Disposition") != "" {
		t.Error("a refused download still sent an attachment header")
	}
	if !strings.Contains(w.Body.String(), "Subscribers report") {
		t.Error("the refusal does not name the report that does support a group filter")
	}
}

func TestFR_RPT_004_TheRosterCSVHonoursTheGroupFilter(t *testing.T) {
	store := oneOfEverything()
	h := &Handler{reporting: store}
	w := serveReportCSV(h, "isp_owner", "/staff/reports/subscribers.csv?group_id=4")

	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if store.gotGrp == nil || *store.gotGrp != 4 {
		t.Fatalf("the group reached the store as %v, want 4", store.gotGrp)
	}
	if disp := w.Header().Get("Content-Disposition"); !strings.Contains(disp, "group-4") {
		t.Errorf("the download name does not name the group: %q", disp)
	}
}

// The ranking's cut-off has to reach the store, or "top 10" silently returns
// the default fifty.
func TestFR_RPT_004_TheTopUsersLimitReachesTheStore(t *testing.T) {
	store := oneOfEverything()
	h := &Handler{reporting: store}

	servePR(h, "isp_owner", "GET", "/staff/reports?report=top-data-users&limit=10", nil, h.Reports)
	if store.gotLimit != 10 {
		t.Errorf("limit reached the store as %d, want 10", store.gotLimit)
	}

	// And an absurd one is clamped rather than passed through.
	store2 := oneOfEverything()
	h2 := &Handler{reporting: store2}
	servePR(h2, "isp_owner", "GET", "/staff/reports?report=top-data-users&limit=99999", nil, h2.Reports)
	if store2.gotLimit != reporting.MaxLimit {
		t.Errorf("limit 99999 reached the store as %d, want it clamped to %d",
			store2.gotLimit, reporting.MaxLimit)
	}
}
