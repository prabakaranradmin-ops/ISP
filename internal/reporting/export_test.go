// Report export tests — FR-RPT-002 | MDS §4.8.
//
// The interesting cases here are all about what a spreadsheet does with the
// output. A nullable metric rendered as 0 is a factual claim nobody made; a
// money column rendered as a float is a rounding artefact an owner will find
// when it disagrees with their bank statement.
package reporting

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/shopspring/decimal"
)

// ── Doubles ─────────────────────────────────────────────────────────────────

type stubQuerier struct {
	mu sync.Mutex

	planMix    []PlanMixRow
	growth     []GrowthRow
	resolution []TicketResolutionRow
	collection []CollectionRow
	roster     []SubscriberRosterRow

	// FR-RPT-004
	expiring        []ExpiringRow
	planChanges     []PlanChangeRow
	paymentMethods  []PaymentMethodRow
	staffCollection []StaffCollectionRow
	invoiceRegister []InvoiceRegisterRow
	tax             []TaxRow
	turnaround      []TurnaroundRow
	dataUsers       []DataUserRow
	fupExceeded     []FUPExceededRow

	// calls records the (months, franchiseID) each query was asked for, so a
	// test can prove the filter reached the store rather than being dropped.
	calls []stubCall
	err   error
}

type stubCall struct {
	Method      string
	Months      int
	FranchiseID *int
	// GroupID is recorded only by SubscriberRoster, the one query that takes
	// one — FR-GRP-001.
	GroupID *int
	// Limit is recorded only by TopDataUsers, the one query that takes one.
	Limit int
}

func (s *stubQuerier) record(method string, months int, f *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{Method: method, Months: months, FranchiseID: f})
}

func (s *stubQuerier) snapshot() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

func (s *stubQuerier) PlanMix(_ context.Context, f *int) ([]PlanMixRow, error) {
	s.record("PlanMix", 0, f)
	return s.planMix, s.err
}

func (s *stubQuerier) GrowthMonthly(_ context.Context, months int, f *int) ([]GrowthRow, error) {
	s.record("GrowthMonthly", months, f)
	return s.growth, s.err
}

func (s *stubQuerier) TicketResolution(_ context.Context, months int, f *int) ([]TicketResolutionRow, error) {
	s.record("TicketResolution", months, f)
	return s.resolution, s.err
}

func (s *stubQuerier) FranchiseCollection(_ context.Context, months int, f *int) ([]CollectionRow, error) {
	s.record("FranchiseCollection", months, f)
	return s.collection, s.err
}

// stubArchiver records delivered exports.
type stubArchiver struct {
	mu       sync.Mutex
	filename string
	body     []byte
	entityID int
	err      error
}

func (a *stubArchiver) ArchiveReport(_ context.Context, entityID int, filename string, body []byte) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return "", a.err
	}
	a.entityID, a.filename, a.body = entityID, filename, append([]byte(nil), body...)
	return "file:///archive/report/" + filename, nil
}

func (a *stubArchiver) delivered() (int, string, []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entityID, a.filename, a.body
}

func mustDecimal(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

// parseCSV returns the header and rows of a rendered report.
func parseCSV(t *testing.T, b []byte) (header []string, rows [][]string) {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\n%s", err, b)
	}
	if len(records) == 0 {
		t.Fatal("output has no header row")
	}
	return records[0], records[1:]
}

// ── Encoders ────────────────────────────────────────────────────────────────

// TestFR_RPT_002_MoneyIsRenderedAsFixedDecimals — these columns are read
// against a bank statement, and a float rendering reintroduces exactly the
// rounding decimal.Decimal is used to avoid.
func TestFR_RPT_002_MoneyIsRenderedAsFixedDecimals(t *testing.T) {
	var buf bytes.Buffer
	err := WritePlanMixCSV(&buf, []PlanMixRow{{
		PlanID: 1, PlanName: "100 Mbps", Price: mustDecimal(t, "799"),
		TotalSubscribers: 10, ActiveSubscribers: 8, SuspendedSubscribers: 2,
		MRR: mustDecimal(t, "6392.5"),
	}})
	if err != nil {
		t.Fatalf("WritePlanMixCSV: %v", err)
	}

	header, rows := parseCSV(t, buf.Bytes())
	if header[2] != "price" || header[7] != "mrr" {
		t.Fatalf("unexpected header: %v", header)
	}
	if rows[0][2] != "799.00" {
		t.Errorf("price: want 799.00 (two fixed decimals), got %q", rows[0][2])
	}
	if rows[0][7] != "6392.50" {
		t.Errorf("mrr: want 6392.50, got %q", rows[0][7])
	}
}

// TestFR_RPT_002_NullableMetricsRenderEmptyNotZero is the property the row
// types already document and the CSV has to preserve.
func TestFR_RPT_002_NullableMetricsRenderEmptyNotZero(t *testing.T) {
	month := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	var res bytes.Buffer
	if err := WriteTicketResolutionCSV(&res, []TicketResolutionRow{{
		Month: month, Category: "billing", Priority: "P3",
		Raised: 5, Resolved: 0, MedianResolutionHours: nil,
	}}); err != nil {
		t.Fatalf("WriteTicketResolutionCSV: %v", err)
	}
	_, rows := parseCSV(t, res.Bytes())
	if got := rows[0][7]; got != "" {
		t.Errorf("median_resolution_hours: want empty for a month with nothing resolved, got %q — "+
			"0.0 claims the fastest possible support for a month in which nobody was helped", got)
	}

	var coll bytes.Buffer
	if err := WriteCollectionCSV(&coll, []CollectionRow{{
		FranchiseID: 3, FranchiseName: "New Territory", FranchiseStatus: "active",
		Month: month, Billed: decimal.Zero, Collected: decimal.Zero,
		Commission: decimal.Zero, CollectionRatePct: nil,
	}}); err != nil {
		t.Fatalf("WriteCollectionCSV: %v", err)
	}
	_, collRows := parseCSV(t, coll.Bytes())
	if got := collRows[0][9]; got != "" {
		t.Errorf("collection_rate_pct: want empty when nothing was billed, got %q — 0%% ranks a "+
			"territory bottom of a league table it has not joined", got)
	}
}

// TestFR_RPT_002_NullableIDsRenderEmptyNotZero — a zero franchise id would
// read as a real franchise and sort alongside them.
func TestFR_RPT_002_NullableIDsRenderEmptyNotZero(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteGrowthCSV(&buf, []GrowthRow{{
		Month: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		// ISP-wide row: neither franchise nor plan attributed.
		NewConnections: 12, Churned: 3, NetGrowth: 9,
	}}); err != nil {
		t.Fatalf("WriteGrowthCSV: %v", err)
	}

	header, rows := parseCSV(t, buf.Bytes())
	if header[0] != "month" {
		t.Fatalf("unexpected header: %v", header)
	}
	if rows[0][0] != "2026-07" {
		t.Errorf("month: want 2026-07, got %q", rows[0][0])
	}
	if rows[0][1] != "" || rows[0][2] != "" {
		t.Errorf("unattributed franchise/plan must be empty, got %q and %q", rows[0][1], rows[0][2])
	}
}

// TestFR_RPT_002_FieldsWithCommasAreQuoted — a franchise named "Chennai
// North, Zone 2" would otherwise shift every column after it by one.
func TestFR_RPT_002_FieldsWithCommasAreQuoted(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCollectionCSV(&buf, []CollectionRow{{
		FranchiseID: 1, FranchiseName: `Chennai North, Zone "2"`, FranchiseStatus: "active",
		Month:  time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Billed: mustDecimal(t, "100"), Collected: mustDecimal(t, "50"), Commission: decimal.Zero,
	}}); err != nil {
		t.Fatalf("WriteCollectionCSV: %v", err)
	}

	_, rows := parseCSV(t, buf.Bytes())
	if rows[0][1] != `Chennai North, Zone "2"` {
		t.Errorf("a name with a comma and quotes must round-trip, got %q", rows[0][1])
	}
	if len(rows[0]) != 10 {
		t.Errorf("want 10 columns, got %d — an unquoted comma shifts every later column", len(rows[0]))
	}
}

// ── Dispatch and filtering ──────────────────────────────────────────────────

func TestFR_RPT_002_WriteCSVDispatchesAndPassesFilters(t *testing.T) {
	franchise := 7
	tests := []struct {
		report     string
		wantMethod string
		wantMonths int
	}{
		// Plan mix is a snapshot, so it takes no window.
		{ReportPlanMix, "PlanMix", 0},
		{ReportGrowth, "GrowthMonthly", 24},
		{ReportTicketResolution, "TicketResolution", 24},
		{ReportCollection, "FranchiseCollection", 24},
	}
	for _, tc := range tests {
		t.Run(tc.report, func(t *testing.T) {
			q := &stubQuerier{}
			var buf bytes.Buffer
			if err := WriteCSV(context.Background(), q, tc.report,
				Filter{Months: 24, FranchiseID: &franchise}, &buf); err != nil {
				t.Fatalf("WriteCSV: %v", err)
			}
			calls := q.snapshot()
			if len(calls) != 1 || calls[0].Method != tc.wantMethod {
				t.Fatalf("want one %s call, got %+v", tc.wantMethod, calls)
			}
			if calls[0].Months != tc.wantMonths {
				t.Errorf("months: want %d, got %d", tc.wantMonths, calls[0].Months)
			}
			if calls[0].FranchiseID == nil || *calls[0].FranchiseID != franchise {
				t.Error("the franchise scope must reach the store, or a partner sees everyone's data")
			}
			// A header row is always emitted, even with no data: a CSV with no
			// header is one a spreadsheet imports with the first record as
			// column names.
			if header, _ := parseCSV(t, buf.Bytes()); len(header) == 0 {
				t.Error("want a header row")
			}
		})
	}

	if err := WriteCSV(context.Background(), &stubQuerier{}, "nonsense", Filter{}, &bytes.Buffer{}); err == nil {
		t.Error("an unknown report must be refused")
	}
}

func TestFR_RPT_002_MonthWindowIsClamped(t *testing.T) {
	if got := NormaliseMonths(0); got != DefaultMonths {
		t.Errorf("absent window: want the %d-month default, got %d", DefaultMonths, got)
	}
	if got := NormaliseMonths(-5); got != DefaultMonths {
		t.Errorf("negative window: want the default, got %d", got)
	}
	if got := NormaliseMonths(100000); got != MaxMonths {
		t.Errorf("an unbounded window must be capped at %d, got %d", MaxMonths, got)
	}
	if got := NormaliseMonths(6); got != 6 {
		t.Errorf("a reasonable window must pass through, got %d", got)
	}
}

func TestFR_RPT_002_FilenameIdentifiesScopeAndDate(t *testing.T) {
	at := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	plain := Filename(ReportGrowth, Filter{}, at)
	if plain != "growth-2026-08-15.csv" {
		t.Errorf("want growth-2026-08-15.csv, got %q", plain)
	}

	franchise := 4
	scoped := Filename(ReportGrowth, Filter{FranchiseID: &franchise}, at)
	if !strings.Contains(scoped, "franchise-4") {
		t.Errorf("a scoped export should name its franchise, got %q — three files called "+
			"growth.csv is a filing problem", scoped)
	}
}

// ── Export worker ───────────────────────────────────────────────────────────

func TestFR_RPT_002_ExportWorkerDeliversToArchival(t *testing.T) {
	q := &stubQuerier{planMix: []PlanMixRow{{
		PlanID: 1, PlanName: "100 Mbps", Price: mustDecimal(t, "799"), MRR: mustDecimal(t, "799"),
	}}}
	arch := &stubArchiver{}
	handler := NewExportHandler(q, arch)

	payload, err := json.Marshal(ExportPayload{
		Report: ReportPlanMix, EntityID: 4242, RequestedBy: "owner@isp",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeReportExport, payload)); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	entityID, filename, body := arch.delivered()
	if entityID != 4242 {
		t.Errorf("the export id must key the archive row: want 4242, got %d", entityID)
	}
	if !strings.HasPrefix(filename, "plan-mix-") || !strings.HasSuffix(filename, ".csv") {
		t.Errorf("filename: got %q", filename)
	}
	// Byte-identical to the synchronous path, which is the point of sharing the
	// encoder: a scheduled report that differs from the one on screen is a
	// support ticket.
	var direct bytes.Buffer
	if err := WriteCSV(context.Background(), q, ReportPlanMix, Filter{}, &direct); err != nil {
		t.Fatalf("direct render: %v", err)
	}
	if !bytes.Equal(body, direct.Bytes()) {
		t.Errorf("the delivered export must match the synchronous render:\nqueued: %s\ndirect: %s",
			body, direct.Bytes())
	}
}

// TestFR_RPT_002_UnprocessableExportsSkipRetry — a payload that can never
// succeed should not occupy the retry budget or the dead-letter queue.
func TestFR_RPT_002_UnprocessableExportsSkipRetry(t *testing.T) {
	handler := NewExportHandler(&stubQuerier{}, &stubArchiver{})

	tests := []struct {
		name    string
		payload []byte
	}{
		{"malformed json", []byte("{not json")},
		{"unknown report", mustJSONPayload(t, ExportPayload{Report: "nonsense", EntityID: 1})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeReportExport, tc.payload))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, jobqueue.ErrSkipRetry) {
				t.Errorf("want ErrSkipRetry, got %v", err)
			}
		})
	}
}

// TestFR_RPT_002_QueryFailureIsRetried — the opposite case: a database blip is
// transient, so the task must fail in a way Asynq retries.
func TestFR_RPT_002_QueryFailureIsRetried(t *testing.T) {
	q := &stubQuerier{err: errors.New("connection refused")}
	handler := NewExportHandler(q, &stubArchiver{})

	err := handler.ProcessTask(context.Background(),
		jobqueue.NewTask(TaskTypeReportExport, mustJSONPayload(t, ExportPayload{Report: ReportGrowth, EntityID: 1})))
	if err == nil {
		t.Fatal("a query failure must fail the task")
	}
	if errors.Is(err, jobqueue.ErrSkipRetry) {
		t.Error("a transient database failure must stay retryable")
	}
}

func mustJSONPayload(t *testing.T, p ExportPayload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func (s *stubQuerier) SubscriberRoster(_ context.Context, group, f *int) ([]SubscriberRosterRow, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{Method: "SubscriberRoster", FranchiseID: f, GroupID: group})
	s.mu.Unlock()
	return s.roster, s.err
}

// ── Group filtering — FR-GRP-001 ────────────────────────────────────────────

func TestFR_GRP_001_TheRosterReportCarriesTheGroupToTheStore(t *testing.T) {
	q := &stubQuerier{roster: []SubscriberRosterRow{{
		SubscriberID: 7, Username: "priya@isp", Status: "active", PlanName: "Fibre 100",
		MobileNumber: "+919876543210", Groups: []string{"Anna Nagar", "Corporate"},
		CreatedAt: time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC),
	}}}
	group, franchise := 3, 9

	var buf bytes.Buffer
	if err := WriteCSV(context.Background(), q, ReportSubscribers,
		Filter{GroupID: &group, FranchiseID: &franchise}, &buf); err != nil {
		t.Fatalf("roster: %v", err)
	}

	calls := q.snapshot()
	if len(calls) != 1 || calls[0].Method != "SubscriberRoster" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].GroupID == nil || *calls[0].GroupID != 3 {
		t.Errorf("the group did not reach the store: %v", calls[0].GroupID)
	}
	if calls[0].FranchiseID == nil || *calls[0].FranchiseID != 9 {
		t.Errorf("the franchise scope did not reach the store: %v", calls[0].FranchiseID)
	}

	out := buf.String()
	for _, want := range []string{"subscriber_id,username", "7,priya@isp,active,Fibre 100",
		"+919876543210", "Anna Nagar; Corporate"} {
		if !strings.Contains(out, want) {
			t.Errorf("the roster CSV is missing %q:\n%s", want, out)
		}
	}
	// Every group the subscriber is in, semicolon-joined so a group name
	// cannot split a row into an extra column.
	if strings.Contains(out, "Anna Nagar,Corporate") {
		t.Error("the group list is comma-joined, which shifts every later column on import")
	}
}

// A group filter asked of a report that cannot honour it is refused, not
// dropped. A figure for the whole base under a heading naming one group is the
// kind of number that gets quoted in a meeting.
func TestFR_GRP_001_AnAggregateReportRefusesAGroupFilter(t *testing.T) {
	group := 3
	for _, report := range []string{ReportPlanMix, ReportGrowth, ReportTicketResolution, ReportCollection} {
		t.Run(report, func(t *testing.T) {
			q := &stubQuerier{}
			var buf bytes.Buffer
			err := WriteCSV(context.Background(), q, report, Filter{GroupID: &group}, &buf)
			if !errors.Is(err, ErrGroupFilterUnsupported) {
				t.Fatalf("got %v, want ErrGroupFilterUnsupported", err)
			}
			if !strings.Contains(err.Error(), report) {
				t.Errorf("the error does not name the report: %v", err)
			}
			if n := len(q.snapshot()); n != 0 {
				t.Errorf("%d quer(ies) ran for a filter that cannot be honoured", n)
			}
			if buf.Len() != 0 {
				t.Errorf("a refused report still wrote %d bytes", buf.Len())
			}
		})
	}
}

// Without a group, every report still works — the refusal must not become a
// blanket block on the aggregate reports.
func TestFR_GRP_001_TheAggregateReportsStillWorkWithoutAGroup(t *testing.T) {
	for _, report := range Reports() {
		t.Run(report, func(t *testing.T) {
			q := &stubQuerier{}
			var buf bytes.Buffer
			if err := WriteCSV(context.Background(), q, report, Filter{}, &buf); err != nil {
				t.Fatalf("%s with no group filter: %v", report, err)
			}
			if buf.Len() == 0 {
				t.Errorf("%s wrote nothing at all, not even a header", report)
			}
		})
	}
}

// GroupFilterable and the refusal must agree: one deciding differently from
// the other is how a filter starts being silently dropped again.
func TestFR_GRP_001_GroupFilterableAgreesWithWriteCSV(t *testing.T) {
	group := 7
	for _, report := range Reports() {
		err := WriteCSV(context.Background(), &stubQuerier{}, report, Filter{GroupID: &group}, &bytes.Buffer{})
		refused := errors.Is(err, ErrGroupFilterUnsupported)
		if refused == GroupFilterable(report) {
			t.Errorf("%s: GroupFilterable = %v but WriteCSV refused = %v",
				report, GroupFilterable(report), refused)
		}
	}
}

func TestFR_GRP_001_TheRosterFilenameNamesTheGroup(t *testing.T) {
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	group, franchise := 4, 2

	got := Filename(ReportSubscribers, Filter{GroupID: &group}, at)
	if got != "subscribers-2026-11-21-group-4.csv" {
		t.Errorf("Filename = %q", got)
	}
	// Both scopes, so two franchises' rosters for the same group stay apart.
	got = Filename(ReportSubscribers, Filter{GroupID: &group, FranchiseID: &franchise}, at)
	if got != "subscribers-2026-11-21-franchise-2-group-4.csv" {
		t.Errorf("Filename with both scopes = %q", got)
	}
	if got := Filename(ReportSubscribers, Filter{}, at); got != "subscribers-2026-11-21.csv" {
		t.Errorf("Filename with no scope = %q", got)
	}
}

func TestFR_GRP_001_TheRosterIsInTheCatalogue(t *testing.T) {
	if !ValidReport(ReportSubscribers) {
		t.Error("the roster is not a valid report, so the route would 404")
	}
	var found bool
	for _, r := range Reports() {
		if r == ReportSubscribers {
			found = true
		}
	}
	if !found {
		t.Error("the roster is missing from Reports(), so it would not be offered or validated")
	}
}

// ── FR-RPT-004 stubs ────────────────────────────────────────────────────────
//
// Each records the window it was handed, so a test can prove the filter
// reached the store rather than being dropped — the same reason the original
// four record theirs.

func (s *stubQuerier) ExpiringSubscribers(_ context.Context, days int, f *int) ([]ExpiringRow, error) {
	s.record("ExpiringSubscribers", days, f)
	return s.expiring, s.err
}

func (s *stubQuerier) PlanChangeHistory(_ context.Context, months int, f *int) ([]PlanChangeRow, error) {
	s.record("PlanChangeHistory", months, f)
	return s.planChanges, s.err
}

func (s *stubQuerier) PaymentsByMethod(_ context.Context, months int, f *int) ([]PaymentMethodRow, error) {
	s.record("PaymentsByMethod", months, f)
	return s.paymentMethods, s.err
}

func (s *stubQuerier) CollectionByStaff(_ context.Context, months int, f *int) ([]StaffCollectionRow, error) {
	s.record("CollectionByStaff", months, f)
	return s.staffCollection, s.err
}

func (s *stubQuerier) InvoiceRegister(_ context.Context, months int, f *int) ([]InvoiceRegisterRow, error) {
	s.record("InvoiceRegister", months, f)
	return s.invoiceRegister, s.err
}

func (s *stubQuerier) TaxCharged(_ context.Context, months int, f *int) ([]TaxRow, error) {
	s.record("TaxCharged", months, f)
	return s.tax, s.err
}

func (s *stubQuerier) TicketTurnaround(_ context.Context, months int, f *int) ([]TurnaroundRow, error) {
	s.record("TicketTurnaround", months, f)
	return s.turnaround, s.err
}

func (s *stubQuerier) TopDataUsers(_ context.Context, months, limit int, f *int) ([]DataUserRow, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{Method: "TopDataUsers", Months: months, FranchiseID: f, Limit: limit})
	s.mu.Unlock()
	return s.dataUsers, s.err
}

func (s *stubQuerier) FUPExceeded(_ context.Context, f *int) ([]FUPExceededRow, error) {
	s.record("FUPExceeded", 0, f)
	return s.fupExceeded, s.err
}

// ── The catalogue — FR-RPT-004 ──────────────────────────────────────────────

// Every catalogue entry must be served by WriteCSV and must name a file.
//
// This is the test the catalogue exists for. Adding the roster report for
// FR-GRP-001 meant touching five separate lists, and one of them — a JSON
// switch with no default — would have served {"rows": null} with a 200 to
// anybody who asked for the new report. A registry plus this test turns that
// class of omission into a compile-or-test failure instead of a silently
// empty report.
func TestFR_RPT_004_EveryCatalogueEntryIsServedByEveryPath(t *testing.T) {
	for _, d := range Catalogue() {
		t.Run(d.Name, func(t *testing.T) {
			q := &stubQuerier{}
			var buf bytes.Buffer
			if err := WriteCSV(context.Background(), q, d.Name, Filter{}, &buf); err != nil {
				t.Fatalf("WriteCSV has no case for %q: %v", d.Name, err)
			}
			// A header at minimum. A report that writes nothing at all is
			// indistinguishable from one that is not wired up.
			if buf.Len() == 0 {
				t.Error("wrote no bytes, not even a header row")
			}
			// Exactly one query, so a report cannot quietly read two stores
			// or none.
			if n := len(q.snapshot()); n != 1 {
				t.Errorf("ran %d queries, want 1", n)
			}
			if name := Filename(d.Name, Filter{}, time.Now()); !strings.HasPrefix(name, d.Name) {
				t.Errorf("Filename = %q, which does not identify the report", name)
			}
		})
	}
}

// The catalogue's own shape. A report with no title or no area would render
// as a blank card under a blank heading.
func TestFR_RPT_004_EveryCatalogueEntryIsDescribed(t *testing.T) {
	areas := map[string]bool{
		AreaSubscribers: true, AreaMoney: true, AreaSupport: true, AreaNetwork: true,
	}
	seen := map[string]bool{}
	for _, d := range Catalogue() {
		if d.Name == "" || d.Title == "" || d.Summary == "" {
			t.Errorf("%+v is missing a name, title or summary", d)
		}
		if !areas[d.Area] {
			t.Errorf("%q is in unknown area %q, so it would not appear under any heading",
				d.Name, d.Area)
		}
		// The summary is what the filter box searches, so it has to say
		// something the title does not.
		if strings.EqualFold(d.Summary, d.Title) {
			t.Errorf("%q's summary only restates its title", d.Name)
		}
		if seen[d.Name] {
			t.Errorf("%q appears twice in the catalogue", d.Name)
		}
		seen[d.Name] = true
		if d.Name != strings.ToLower(d.Name) || strings.Contains(d.Name, " ") ||
			strings.Contains(d.Name, "_") {
			t.Errorf("%q is not kebab-case, but it goes in URLs and filenames", d.Name)
		}
	}

	// The nine FR-RPT-004 asked for by name, so the requirement cannot be
	// reported as met with one of them missing.
	for _, required := range []string{
		ReportExpiring, ReportPaymentsByMethod, ReportCollectionByStaff,
		ReportInvoiceRegister, ReportTaxCharged, ReportTopDataUsers,
		ReportFUPExceeded, ReportTicketTurnaround, ReportPlanChanges,
	} {
		if !seen[required] {
			t.Errorf("%q is named in FR-RPT-004 and is not in the catalogue", required)
		}
	}
}

// Areas and InArea must partition the catalogue: a report in no area would
// vanish from the screen even though every other check passed.
func TestFR_RPT_004_TheAreasCoverEveryReportExactlyOnce(t *testing.T) {
	var total int
	for _, area := range Areas() {
		in := InArea(area)
		if len(in) == 0 {
			t.Errorf("area %q is listed with no reports, so it would be an empty heading", area)
		}
		total += len(in)
	}
	if total != len(Catalogue()) {
		t.Errorf("the areas hold %d reports but the catalogue has %d", total, len(Catalogue()))
	}
	if len(Reports()) != len(Catalogue()) {
		t.Errorf("Reports() lists %d but the catalogue has %d", len(Reports()), len(Catalogue()))
	}
}

// The filter box. Substring over title, area, summary and name — an operator
// types "GST" or "renewal", and those words are in the summary.
func TestFR_RPT_004_TheFilterBoxFindsReportsByWhatTheyAnswer(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"", ReportPlanMix},                 // empty matches everything
		{"plan mix", ReportPlanMix},         // by title
		{"PLAN MIX", ReportPlanMix},         // case-insensitively
		{"GST", ReportTaxCharged},           // by a word only the summary has
		{"renewal", ReportExpiring},         // likewise
		{"Money", ReportTaxCharged},         // by area
		{"fup-exceeded", ReportFUPExceeded}, // by name
	}
	for _, tc := range cases {
		d, ok := Lookup(tc.want)
		if !ok {
			t.Fatalf("%q is not in the catalogue", tc.want)
		}
		if !d.Matches(tc.query) {
			t.Errorf("filtering by %q excludes %q, which an operator would expect to find",
				tc.query, tc.want)
		}
	}

	// And it excludes what does not match, or it is not a filter.
	d, _ := Lookup(ReportPlanMix)
	if d.Matches("zzz-no-such-thing") {
		t.Error("the filter box matches a query nothing should match")
	}
}

func TestFR_RPT_004_TheWindowsAreClamped(t *testing.T) {
	if got := NormaliseDays(0); got != DefaultDays {
		t.Errorf("NormaliseDays(0) = %d, want the default %d — an empty expiry list would "+
			"stop a day's renewal calls being made", got, DefaultDays)
	}
	if got := NormaliseDays(-5); got != DefaultDays {
		t.Errorf("NormaliseDays(-5) = %d", got)
	}
	if got := NormaliseDays(10_000); got != MaxDays {
		t.Errorf("NormaliseDays(10000) = %d, want %d", got, MaxDays)
	}
	if got := NormaliseDays(14); got != 14 {
		t.Errorf("NormaliseDays(14) = %d", got)
	}

	if got := NormaliseLimit(0); got != DefaultLimit {
		t.Errorf("NormaliseLimit(0) = %d", got)
	}
	if got := NormaliseLimit(10_000); got != MaxLimit {
		t.Errorf("NormaliseLimit(10000) = %d", got)
	}
}

// The window each report declares must be the one it is actually given.
func TestFR_RPT_004_EachReportIsGivenTheWindowItDeclares(t *testing.T) {
	for _, d := range Catalogue() {
		t.Run(d.Name, func(t *testing.T) {
			q := &stubQuerier{}
			f := Filter{Months: 6, Days: 14, Limit: 25}
			if err := WriteCSV(context.Background(), q, d.Name, f, &bytes.Buffer{}); err != nil {
				t.Fatalf("WriteCSV: %v", err)
			}
			call := q.snapshot()[0]
			switch d.Window {
			case WindowMonths:
				if call.Months != 6 {
					t.Errorf("%s declares a month window but was given Months=%d",
						d.Name, call.Months)
				}
			case WindowDays:
				// Carried in the Months field of the recording, since the
				// stub records one number; what matters is that it is the
				// day window and not the month one.
				if call.Months != 14 {
					t.Errorf("%s declares a day window but was given %d", d.Name, call.Months)
				}
			case WindowNone:
				if call.Months != 0 {
					t.Errorf("%s is a point-in-time snapshot but was given a window of %d, "+
						"which implies a history it does not have", d.Name, call.Months)
				}
			}
		})
	}
}
