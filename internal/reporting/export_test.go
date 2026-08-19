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

	// calls records the (months, franchiseID) each query was asked for, so a
	// test can prove the filter reached the store rather than being dropped.
	calls []stubCall
	err   error
}

type stubCall struct {
	Method      string
	Months      int
	FranchiseID *int
}

func (s *stubQuerier) record(method string, months int, f *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{method, months, f})
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
			if !errors.Is(err, jobqueue.SkipRetry) {
				t.Errorf("want SkipRetry, got %v", err)
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
	if errors.Is(err, jobqueue.SkipRetry) {
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
