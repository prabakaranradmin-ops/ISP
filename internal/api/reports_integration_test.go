//go:build integration

// Report export API — FR-RPT-002 | MDS §4.8.
//
// The security property under test is franchise scoping: these aggregates say
// how many subscribers a competitor has and how well they collect, so a
// franchise-bound caller must never be able to widen their view by editing a
// URL or a request body.
package api_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/archive"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/shopspring/decimal"
)

// ── Stubs ───────────────────────────────────────────────────────────────────

type stubReports struct {
	mu     sync.Mutex
	calls  []*int // franchise scope each query received
	groups []*int // group filter each roster query received
	// The windows only one report each takes — FR-RPT-004.
	gotDays  int
	gotLimit int
}

func (s *stubReports) record(f *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, f)
}

func (s *stubReports) scopes() []*int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*int(nil), s.calls...)
}

func (s *stubReports) PlanMix(_ context.Context, f *int) ([]reporting.PlanMixRow, error) {
	s.record(f)
	return []reporting.PlanMixRow{{
		PlanID: 1, PlanName: "100 Mbps", Price: decimal.NewFromInt(799),
		TotalSubscribers: 3, ActiveSubscribers: 3, MRR: decimal.NewFromInt(2397),
	}}, nil
}

func (s *stubReports) GrowthMonthly(_ context.Context, _ int, f *int) ([]reporting.GrowthRow, error) {
	s.record(f)
	return []reporting.GrowthRow{{Month: time.Now(), NewConnections: 4, NetGrowth: 4}}, nil
}

func (s *stubReports) TicketResolution(_ context.Context, _ int, f *int) ([]reporting.TicketResolutionRow, error) {
	s.record(f)
	return []reporting.TicketResolutionRow{{Month: time.Now(), Category: "billing", Priority: "P3"}}, nil
}

func (s *stubReports) FranchiseCollection(_ context.Context, _ int, f *int) ([]reporting.CollectionRow, error) {
	s.record(f)
	return []reporting.CollectionRow{{
		FranchiseID: 1, FranchiseName: "North", FranchiseStatus: "active", Month: time.Now(),
		Billed: decimal.NewFromInt(100), Collected: decimal.NewFromInt(90), Commission: decimal.Zero,
	}}, nil
}

// SubscriberRoster records the group it was asked for as well as the scope,
// because the group is the whole point of this report — FR-GRP-001.
func (s *stubReports) SubscriberRoster(_ context.Context, group, f *int) ([]reporting.SubscriberRosterRow, error) {
	s.record(f)
	s.mu.Lock()
	s.groups = append(s.groups, group)
	s.mu.Unlock()
	return []reporting.SubscriberRosterRow{{
		SubscriberID: 1, Username: "a@isp", Status: "active", PlanName: "100 Mbps",
		MobileNumber: "+919876543210", Groups: []string{"Anna Nagar"}, CreatedAt: time.Now(),
	}}, nil
}

type stubArchiveLookup struct {
	rec *archive.Record
}

func (s *stubArchiveLookup) GetArchive(context.Context, string, int) (*archive.Record, error) {
	return s.rec, nil
}

// ── Harness ─────────────────────────────────────────────────────────────────

func reportsMux(reports api.ReportQuerier, archives api.ArchiveLookup, tasks api.TaskEnqueuer) *http.ServeMux {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Reports: reports, Archives: archives, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

// scopedToken mints a token for a franchise-bound role.
func scopedToken(t *testing.T, role string, franchiseID int) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:        role,
		FranchiseID: franchiseID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   role + "@partner",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func reportCall(t *testing.T, mux *http.ServeMux, method, path, token, body string) *httptest.ResponseRecorder {
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

// ── CSV download ────────────────────────────────────────────────────────────

func TestFR_RPT_002_CSVDownloadForEveryReport(t *testing.T) {
	for _, report := range reporting.Reports() {
		t.Run(report, func(t *testing.T) {
			mux := reportsMux(&stubReports{}, nil, nil)
			rec := reportCall(t, mux, http.MethodGet,
				"/api/v1/reports/"+report+"?format=csv", hotspotStaffToken(t, "isp_owner"), "")

			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
				t.Errorf("Content-Type: want text/csv, got %q", ct)
			}
			// Without this a browser renders the CSV as a page instead of saving it.
			cd := rec.Header().Get("Content-Disposition")
			if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, report) {
				t.Errorf("Content-Disposition should offer a named download, got %q", cd)
			}

			records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
			if err != nil {
				t.Fatalf("response is not valid CSV: %v", err)
			}
			if len(records) < 2 {
				t.Errorf("want a header and at least one row, got %d records", len(records))
			}
		})
	}
}

func TestFR_RPT_002_JSONIsTheDefaultFormat(t *testing.T) {
	mux := reportsMux(&stubReports{}, nil, nil)
	rec := reportCall(t, mux, http.MethodGet, "/api/v1/reports/plan-mix",
		hotspotStaffToken(t, "isp_owner"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body struct {
		Report string            `json:"report"`
		Rows   []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("default format must be JSON: %v — %s", err, rec.Body.String())
	}
	if body.Report != "plan-mix" || len(body.Rows) != 1 {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestFR_RPT_002_UnknownReportIs404(t *testing.T) {
	mux := reportsMux(&stubReports{}, nil, nil)
	rec := reportCall(t, mux, http.MethodGet, "/api/v1/reports/nonsense",
		hotspotStaffToken(t, "isp_owner"), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
	// The message names what is available, so a caller does not guess twice.
	if !strings.Contains(rec.Body.String(), "plan-mix") {
		t.Errorf("the refusal should list the available reports: %s", rec.Body.String())
	}
}

// ── Franchise scoping ───────────────────────────────────────────────────────

// TestFR_RPT_002_FranchiseCallerCannotWidenTheirScope is the security property.
// These aggregates describe a competitor's book, so the scope must come from
// the token and nothing in the request may override it.
func TestFR_RPT_002_FranchiseCallerCannotWidenTheirScope(t *testing.T) {
	store := &stubReports{}
	mux := reportsMux(store, nil, nil)

	// An LCO bound to franchise 4 asks for franchise 9's numbers.
	rec := reportCall(t, mux, http.MethodGet,
		"/api/v1/reports/collection?franchise_id=9", scopedToken(t, "lco", 4), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	scopes := store.scopes()
	if len(scopes) != 1 {
		t.Fatalf("want 1 query, got %d", len(scopes))
	}
	if scopes[0] == nil || *scopes[0] != 4 {
		t.Fatalf("the query must be scoped to the caller's own franchise 4, got %v — a partner "+
			"must not be able to read another's book by editing a URL", scopes[0])
	}
}

// TestFR_RPT_002_ISPStaffSeeEverythingUnlessTheyNarrow — the counterpart: an
// owner has no franchise binding, so nil scope means all franchises.
func TestFR_RPT_002_ISPStaffSeeEverythingUnlessTheyNarrow(t *testing.T) {
	store := &stubReports{}
	mux := reportsMux(store, nil, nil)

	reportCall(t, mux, http.MethodGet, "/api/v1/reports/collection",
		hotspotStaffToken(t, "isp_owner"), "")
	if scopes := store.scopes(); len(scopes) != 1 || scopes[0] != nil {
		t.Errorf("ISP-wide staff must query unscoped, got %v", scopes)
	}

	narrowed := &stubReports{}
	reportCall(t, reportsMux(narrowed, nil, nil), http.MethodGet,
		"/api/v1/reports/collection?franchise_id=9", hotspotStaffToken(t, "isp_owner"), "")
	if scopes := narrowed.scopes(); len(scopes) != 1 || scopes[0] == nil || *scopes[0] != 9 {
		t.Errorf("ISP-wide staff may narrow explicitly, got %v", scopes)
	}
}

// TestFR_RPT_002_ScopedRoleWithNoClaimIsRefused — a franchise role whose token
// carries no franchise_id is refused rather than defaulted to ISP-wide, which
// would turn a misissued token into a cross-partner leak.
func TestFR_RPT_002_ScopedRoleWithNoClaimIsRefused(t *testing.T) {
	store := &stubReports{}
	mux := reportsMux(store, nil, nil)

	rec := reportCall(t, mux, http.MethodGet, "/api/v1/reports/collection",
		scopedToken(t, "lco", 0), "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(store.scopes()) != 0 {
		t.Error("nothing may be queried for a scoped caller with no franchise claim")
	}
}

func TestFR_RPT_002_ReportsRequireAToken(t *testing.T) {
	mux := reportsMux(&stubReports{}, nil, nil)
	for _, path := range []string{"/api/v1/reports/plan-mix", "/api/v1/reports/exports/1"} {
		if rec := reportCall(t, mux, http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: want 401, got %d", path, rec.Code)
		}
	}
	rec := reportCall(t, mux, http.MethodGet, "/api/v1/reports/plan-mix",
		hotspotStaffToken(t, "subscriber"), "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("a subscriber token must not read operator reports, got %d", rec.Code)
	}
}

// ── Scheduled export ────────────────────────────────────────────────────────

func TestFR_RPT_002_ExportIsQueuedWithTheCallersScope(t *testing.T) {
	tasks := &stubTaskEnqueuer{}
	mux := reportsMux(&stubReports{}, nil, tasks)

	rec := reportCall(t, mux, http.MethodPost, "/api/v1/reports/growth/export",
		scopedToken(t, "franchise_admin", 6), `{"months":36}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ExportID int    `json:"export_id"`
		Status   string `json:"status"`
		Poll     string `json:"poll"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExportID == 0 || resp.Status != "queued" {
		t.Errorf("unexpected response: %+v", resp)
	}
	// The caller is told where to look rather than having to construct it.
	if !strings.Contains(resp.Poll, "/api/v1/reports/exports/") {
		t.Errorf("response should carry a poll URL, got %q", resp.Poll)
	}

	enqueued := tasks.snapshot()
	if len(enqueued) != 1 {
		t.Fatalf("want 1 queued task, got %d", len(enqueued))
	}
	var payload reporting.ExportPayload
	if err := json.Unmarshal(enqueued[0].Payload(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Report != "growth" || payload.Months != 36 {
		t.Errorf("payload: %+v", payload)
	}
	if payload.FranchiseID == nil || *payload.FranchiseID != 6 {
		t.Errorf("the export must be locked to the requester's franchise, got %v", payload.FranchiseID)
	}
	if payload.EntityID != resp.ExportID {
		t.Errorf("the export id must key the archive row: response %d, payload %d",
			resp.ExportID, payload.EntityID)
	}
	if payload.RequestedBy == "" {
		t.Error("the payload should record who asked, for the archive's audit trail")
	}
}

// TestFR_RPT_002_ExportBodyCannotWidenScope — the same guarantee as the query
// string, on the POST path.
func TestFR_RPT_002_ExportBodyCannotWidenScope(t *testing.T) {
	tasks := &stubTaskEnqueuer{}
	mux := reportsMux(&stubReports{}, nil, tasks)

	rec := reportCall(t, mux, http.MethodPost, "/api/v1/reports/collection/export",
		scopedToken(t, "lco", 2), `{"franchise_id":99}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}

	var payload reporting.ExportPayload
	if err := json.Unmarshal(tasks.snapshot()[0].Payload(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.FranchiseID == nil || *payload.FranchiseID != 2 {
		t.Errorf("a scoped caller's own franchise must win over the body, got %v", payload.FranchiseID)
	}
}

func TestFR_RPT_002_ExportStatusReportsDeliveryHonestly(t *testing.T) {
	// Not yet delivered: reported as pending rather than 404, since the caller
	// was just told 202 and a 404 would read as "your request was lost".
	pending := reportCall(t, reportsMux(&stubReports{}, &stubArchiveLookup{}, &stubTaskEnqueuer{}),
		http.MethodGet, "/api/v1/reports/exports/123", hotspotStaffToken(t, "isp_owner"), "")
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), "pending") {
		t.Errorf("undelivered export: want 200/pending, got %d — %s", pending.Code, pending.Body.String())
	}

	delivered := reportCall(t, reportsMux(&stubReports{}, &stubArchiveLookup{rec: &archive.Record{
		ID: 1, DocKind: archive.KindReport, EntityID: 123,
		StorageBackend: archive.BackendLocal, StorageURL: "file:///archive/report/growth.csv",
		ChecksumSHA256: strings.Repeat("a", 64), SizeBytes: 2048,
	}}, &stubTaskEnqueuer{}),
		http.MethodGet, "/api/v1/reports/exports/123", hotspotStaffToken(t, "isp_owner"), "")
	if delivered.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", delivered.Code)
	}
	body := delivered.Body.String()
	if !strings.Contains(body, "delivered") || !strings.Contains(body, "checksum_sha256") {
		t.Errorf("a delivered export should report where it went and its checksum: %s", body)
	}
}

func TestFR_RPT_002_DegradesWhenUnconfigured(t *testing.T) {
	// No reporting store.
	if rec := reportCall(t, reportsMux(nil, nil, nil), http.MethodGet,
		"/api/v1/reports/plan-mix", hotspotStaffToken(t, "isp_owner"), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no reporting store: want 503, got %d", rec.Code)
	}
	// No queue.
	if rec := reportCall(t, reportsMux(&stubReports{}, nil, nil), http.MethodPost,
		"/api/v1/reports/growth/export", hotspotStaffToken(t, "isp_owner"), "{}"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no export queue: want 503, got %d", rec.Code)
	}
	// No archival storage to look an export up in.
	if rec := reportCall(t, reportsMux(&stubReports{}, nil, &stubTaskEnqueuer{}), http.MethodGet,
		"/api/v1/reports/exports/1", hotspotStaffToken(t, "isp_owner"), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no archive store: want 503, got %d", rec.Code)
	}
}

// ── Group filtering — FR-GRP-001 ────────────────────────────────────────────

func TestFR_GRP_001_TheRosterCanBeFilteredToAGroup(t *testing.T) {
	reports := &stubReports{}
	mux := reportsMux(reports, nil, nil)

	rec := reportCall(t, mux, http.MethodGet,
		"/api/v1/reports/"+reporting.ReportSubscribers+"?group_id=3",
		hotspotStaffToken(t, "isp_owner"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	reports.mu.Lock()
	groups := append([]*int(nil), reports.groups...)
	reports.mu.Unlock()
	if len(groups) != 1 {
		t.Fatalf("the roster query ran %d times", len(groups))
	}
	if groups[0] == nil || *groups[0] != 3 {
		t.Fatalf("the group filter reached the store as %v, want 3 — a dropped filter would "+
			"return the whole base under a heading naming one group", groups[0])
	}

	// And the CSV path carries it too.
	rec = reportCall(t, mux, http.MethodGet,
		"/api/v1/reports/"+reporting.ReportSubscribers+"?group_id=3&format=csv",
		hotspotStaffToken(t, "isp_owner"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("CSV: want 200, got %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "group-3") {
		t.Errorf("the download name does not name the group: %q", cd)
	}
}

// 422 with an explanation, not a 500 and not a silently unfiltered report. The
// check has to happen before the CSV headers go out, or the caller gets a 200
// with an empty attachment.
func TestFR_GRP_001_AGroupFilterOnAnAggregateReportIsRefused(t *testing.T) {
	for _, report := range []string{reporting.ReportPlanMix, reporting.ReportGrowth,
		reporting.ReportTicketResolution, reporting.ReportCollection} {
		for _, format := range []string{"", "&format=csv"} {
			t.Run(report+format, func(t *testing.T) {
				reports := &stubReports{}
				mux := reportsMux(reports, nil, nil)
				rec := reportCall(t, mux, http.MethodGet,
					"/api/v1/reports/"+report+"?group_id=3"+format,
					hotspotStaffToken(t, "isp_owner"), "")

				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("want 422, got %d — %s", rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), reporting.ReportSubscribers) {
					t.Errorf("the refusal does not say which report does support it: %s", rec.Body.String())
				}
				if n := len(reports.scopes()); n != 0 {
					t.Errorf("%d quer(ies) ran for a refused request", n)
				}
			})
		}
	}
}

func TestFR_GRP_001_ABadGroupIdIsRejected(t *testing.T) {
	for _, raw := range []string{"notanumber", "0", "-1"} {
		reports := &stubReports{}
		mux := reportsMux(reports, nil, nil)
		rec := reportCall(t, mux, http.MethodGet,
			"/api/v1/reports/"+reporting.ReportSubscribers+"?group_id="+raw,
			hotspotStaffToken(t, "isp_owner"), "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("group_id=%q got %d, want 422", raw, rec.Code)
		}
		if n := len(reports.scopes()); n != 0 {
			t.Errorf("group_id=%q reached the store", raw)
		}
	}
}

// A franchise-scoped caller may use a group, but the franchise predicate still
// applies — a group is a label on your own customers, not a way past the
// partition between partners.
func TestFR_GRP_001_AGroupDoesNotWidenAFranchiseScope(t *testing.T) {
	reports := &stubReports{}
	mux := reportsMux(reports, nil, nil)

	rec := reportCall(t, mux, http.MethodGet,
		"/api/v1/reports/"+reporting.ReportSubscribers+"?group_id=3",
		scopedToken(t, "lco", 7), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	scopes := reports.scopes()
	if len(scopes) != 1 || scopes[0] == nil || *scopes[0] != 7 {
		t.Fatalf("the franchise scope reached the store as %v, want 7", scopes)
	}
}

// The FR-RPT-004 catalogue reports. One row each, so the CSV-for-every-report
// test gets a body rather than only a header, and each records the scope it
// was given.
func (s *stubReports) ExpiringSubscribers(_ context.Context, days int, f *int) ([]reporting.ExpiringRow, error) {
	s.record(f)
	s.mu.Lock()
	s.gotDays = days
	s.mu.Unlock()
	return []reporting.ExpiringRow{{
		SubscriberID: 1, Username: "a@isp", MobileNumber: "+919876543210", PlanName: "100 Mbps",
		Status: "active", PlanExpiry: time.Now().Add(48 * time.Hour), DaysLeft: 2,
		PlanPrice: decimal.NewFromInt(799), WalletBalance: decimal.NewFromInt(100),
	}}, nil
}

func (s *stubReports) PlanChangeHistory(_ context.Context, _ int, f *int) ([]reporting.PlanChangeRow, error) {
	s.record(f)
	return []reporting.PlanChangeRow{{
		SubscriberID: 1, Username: "a@isp", FromPlan: "100 Mbps", ToPlan: "200 Mbps",
		EffectiveOn: time.Now(), Status: "scheduled", ScheduledBy: "csr", ScheduledAt: time.Now(),
	}}, nil
}

func (s *stubReports) PaymentsByMethod(_ context.Context, _ int, f *int) ([]reporting.PaymentMethodRow, error) {
	s.record(f)
	return []reporting.PaymentMethodRow{{
		Month: time.Now(), Method: "cash", Payments: 3, Total: decimal.NewFromInt(2397),
	}}, nil
}

func (s *stubReports) CollectionByStaff(_ context.Context, _ int, f *int) ([]reporting.StaffCollectionRow, error) {
	s.record(f)
	return []reporting.StaffCollectionRow{{
		Month: time.Now(), RecordedBy: "tech", Payments: 2,
		CashTotal: decimal.NewFromInt(1000), DigitalTotal: decimal.NewFromInt(799),
		Total: decimal.NewFromInt(1799),
	}}, nil
}

func (s *stubReports) InvoiceRegister(_ context.Context, _ int, f *int) ([]reporting.InvoiceRegisterRow, error) {
	s.record(f)
	return []reporting.InvoiceRegisterRow{{
		InvoiceID: 1, InvoiceDate: time.Now(), SubscriberID: 1, Username: "a@isp",
		PlanName: "100 Mbps", BaseAmount: decimal.NewFromInt(799),
		CGST: decimal.NewFromInt(72), SGST: decimal.NewFromInt(72), IGST: decimal.Zero,
		Total: decimal.NewFromInt(943),
	}}, nil
}

func (s *stubReports) TaxCharged(_ context.Context, _ int, f *int) ([]reporting.TaxRow, error) {
	s.record(f)
	return []reporting.TaxRow{{
		Month: time.Now(), CGSTRate: decimal.NewFromInt(9), SGSTRate: decimal.NewFromInt(9),
		IGSTRate: decimal.Zero, Invoices: 1, TaxableValue: decimal.NewFromInt(799),
		CGST: decimal.NewFromInt(72), SGST: decimal.NewFromInt(72), IGST: decimal.Zero,
		TaxTotal: decimal.NewFromInt(144),
	}}, nil
}

func (s *stubReports) TicketTurnaround(_ context.Context, _ int, f *int) ([]reporting.TurnaroundRow, error) {
	s.record(f)
	return []reporting.TurnaroundRow{{
		Month: time.Now(), Category: "billing", Priority: "P3", Raised: 4, Resolved: 3, StillOpen: 1,
	}}, nil
}

func (s *stubReports) TopDataUsers(_ context.Context, _, limit int, f *int) ([]reporting.DataUserRow, error) {
	s.record(f)
	s.mu.Lock()
	s.gotLimit = limit
	s.mu.Unlock()
	return []reporting.DataUserRow{{
		SubscriberID: 1, Username: "a@isp", PlanName: "100 Mbps", Sessions: 12,
		InputOctets: 1 << 30, OutputOctets: 1 << 29, TotalBytes: (1 << 30) + (1 << 29),
		AllowanceBytes: 1 << 40,
	}}, nil
}

func (s *stubReports) FUPExceeded(_ context.Context, f *int) ([]reporting.FUPExceededRow, error) {
	s.record(f)
	return []reporting.FUPExceededRow{{
		SubscriberID: 1, Username: "a@isp", PlanName: "100 Mbps",
		UsedBytes: 1 << 41, AllowanceBytes: 1 << 40, AppliedRate: "10M/10M", FUPActive: true,
	}}, nil
}

// A schedule must be able to reproduce what the synchronous download does.
// Without the per-report filters on the payload, "export the next 30 days of
// expiries" quietly delivered the default seven — the schedule and the screen
// would then disagree, which is the drift the shared WriteCSV exists to stop.
func TestFR_RPT_004_AScheduledExportCarriesThePerReportFilters(t *testing.T) {
	tasks := &stubTaskEnqueuer{}
	mux := reportsMux(&stubReports{}, nil, tasks)

	rec := reportCall(t, mux, http.MethodPost,
		"/api/v1/reports/"+reporting.ReportExpiring+"/export",
		hotspotStaffToken(t, "isp_owner"), `{"days":30}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}

	queued := tasks.snapshot()
	if len(queued) != 1 {
		t.Fatalf("%d task(s) queued, want 1", len(queued))
	}
	var payload reporting.ExportPayload
	if err := json.Unmarshal(queued[0].Payload(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Days != 30 {
		t.Errorf("the day window reached the queue as %d, want 30", payload.Days)
	}

	// And the ranking's cut-off.
	tasks2 := &stubTaskEnqueuer{}
	mux2 := reportsMux(&stubReports{}, nil, tasks2)
	rec = reportCall(t, mux2, http.MethodPost,
		"/api/v1/reports/"+reporting.ReportTopDataUsers+"/export",
		hotspotStaffToken(t, "isp_owner"), `{"limit":25}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	queued2 := tasks2.snapshot()
	if len(queued2) != 1 {
		t.Fatalf("%d task(s) queued, want 1", len(queued2))
	}
	if err := json.Unmarshal(queued2[0].Payload(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Limit != 25 {
		t.Errorf("the limit reached the queue as %d, want 25", payload.Limit)
	}
}

// A group filter on a report that cannot honour it is refused at enqueue
// time, so the caller is told now rather than finding an export that never
// arrived.
func TestFR_RPT_004_AnUnhonourableGroupFilterIsRefusedAtEnqueue(t *testing.T) {
	tasks := &stubTaskEnqueuer{}
	mux := reportsMux(&stubReports{}, nil, tasks)

	rec := reportCall(t, mux, http.MethodPost,
		"/api/v1/reports/"+reporting.ReportPlanMix+"/export",
		hotspotStaffToken(t, "isp_owner"), `{"group_id":4}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), reporting.ReportSubscribers) {
		t.Error("the refusal does not name the report that does support a group filter")
	}
	if n := len(tasks.snapshot()); n != 0 {
		t.Errorf("a refused export queued %d task(s)", n)
	}
}
