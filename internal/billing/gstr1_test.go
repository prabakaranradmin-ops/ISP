package billing

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d2(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func jan(day int) time.Time { return time.Date(2026, time.January, day, 10, 0, 0, 0, time.UTC) }

// A Tamil Nadu (home state) invoice: CGST+SGST, 799 base at 18%.
func tnInvoice(id int, gstin string) InvoiceRow {
	return InvoiceRow{
		InvoiceID: id, InvoiceDate: jan(id), SubscriberID: id,
		SubscriberName: "Sub", RecipientGSTIN: gstin, RecipientState: "TN",
		TaxableValue: d2("799.00"), CGST: d2("71.91"), SGST: d2("71.91"),
		IGST: decimal.Zero, Total: d2("942.82"),
	}
}

// A Karnataka invoice: IGST.
func kaInvoice(id int, gstin string) InvoiceRow {
	return InvoiceRow{
		InvoiceID: id, InvoiceDate: jan(id), SubscriberID: id,
		SubscriberName: "Sub", RecipientGSTIN: gstin, RecipientState: "KA",
		TaxableValue: d2("799.00"), CGST: decimal.Zero, SGST: decimal.Zero,
		IGST: d2("143.82"), Total: d2("942.82"),
	}
}

var testSupplier = Supplier{GSTIN: "33AABCU9603R1ZM", State: "TN", Name: "Maaran Soft"}

// The split is decided by one thing only: whether the recipient holds a
// GSTIN.
func TestBuildReturn_SplitsB2BFromB2COnGSTINAlone(t *testing.T) {
	rows := []InvoiceRow{
		tnInvoice(1, "33AABCU9603R1ZM"), // registered -> B2B
		tnInvoice(2, ""),                // unregistered -> B2C
		tnInvoice(3, ""),                // unregistered -> B2C, same bucket
	}
	ret := BuildReturn(Period{2026, time.January}, testSupplier, rows, nil)

	if len(ret.B2B) != 1 {
		t.Fatalf("B2B: want 1 line, got %d", len(ret.B2B))
	}
	if ret.B2B[0].InvoiceNumber != "INV-000001" {
		t.Errorf("B2B invoice number = %q, want INV-000001", ret.B2B[0].InvoiceNumber)
	}
	// B2C is aggregated, not listed: two invoices, one line.
	if len(ret.B2C) != 1 {
		t.Fatalf("B2C: want 1 aggregated line, got %d", len(ret.B2C))
	}
	if ret.B2C[0].InvoiceCount != 2 {
		t.Errorf("B2C invoice count = %d, want 2", ret.B2C[0].InvoiceCount)
	}
	if got := ret.B2C[0].TaxableValue.StringFixed(2); got != "1598.00" {
		t.Errorf("B2C taxable = %s, want 1598.00 (two invoices summed)", got)
	}
}

// Place of supply must be the two-digit GST code, not the internal
// two-letter one: the return is filed against the numeric code.
func TestBuildReturn_UsesNumericGSTStateCodeAsPlaceOfSupply(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{
		tnInvoice(1, "33AABCU9603R1ZM"),
		kaInvoice(2, ""),
	}, nil)
	if ret.B2B[0].PlaceOfSupply != "33" {
		t.Errorf("B2B place of supply = %q, want 33 (Tamil Nadu)", ret.B2B[0].PlaceOfSupply)
	}
	if ret.B2C[0].PlaceOfSupply != "29" {
		t.Errorf("B2C place of supply = %q, want 29 (Karnataka)", ret.B2C[0].PlaceOfSupply)
	}
	if ret.B2C[0].PlaceOfSupplyName != "Karnataka" {
		t.Errorf("state name = %q, want Karnataka", ret.B2C[0].PlaceOfSupplyName)
	}
}

// Two states must not collapse into one bucket, and neither must two rates
// within one state - GSTR-1 reports rate by rate.
func TestBuildReturn_B2CBucketsByStateAndRateSeparately(t *testing.T) {
	half := InvoiceRow{
		InvoiceID: 3, InvoiceDate: jan(3), RecipientState: "TN",
		TaxableValue: d2("1000.00"), CGST: d2("25.00"), SGST: d2("25.00"),
		Total: d2("1050.00"), // 5%
	}
	ret := BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{
		tnInvoice(1, ""), // TN @ 18
		kaInvoice(2, ""), // KA @ 18
		half,             // TN @ 5
	}, nil)
	if len(ret.B2C) != 3 {
		t.Fatalf("want 3 buckets (TN@18, TN@5, KA@18), got %d: %+v", len(ret.B2C), ret.B2C)
	}
}

// The rate is derived from the amounts actually charged, not from the
// current gst_rates row, so a superseded rate still reports what was
// billed.
func TestBuildReturn_DerivesRateFromTheAmountsCharged(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{tnInvoice(1, "")}, nil)
	if got := ret.B2C[0].Rate.String(); got != "18" {
		t.Errorf("rate = %s, want 18 (derived from 71.91+71.91 on 799.00)", got)
	}

	ret = BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{kaInvoice(1, "")}, nil)
	if got := ret.B2C[0].Rate.String(); got != "18" {
		t.Errorf("IGST rate = %s, want 18", got)
	}
}

// The HSN summary spans B2B and B2C alike: they are supplies of the same
// service, and a summary counting only one would understate the return.
func TestBuildReturn_HSNSummaryCoversEverySupply(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{
		tnInvoice(1, "33AABCU9603R1ZM"), // B2B
		tnInvoice(2, ""),                // B2C
	}, nil)
	if len(ret.HSN) != 1 {
		t.Fatalf("want 1 HSN line, got %d", len(ret.HSN))
	}
	h := ret.HSN[0]
	if h.HSN != HSNInternetServices {
		t.Errorf("HSN = %q, want %s", h.HSN, HSNInternetServices)
	}
	if got := h.TaxableValue.StringFixed(2); got != "1598.00" {
		t.Errorf("HSN taxable = %s, want 1598.00 (B2B + B2C)", got)
	}
	if got := h.Quantity.StringFixed(2); got != "2.00" {
		t.Errorf("HSN quantity = %s, want 2.00", got)
	}
}

// The three sections must agree. An accountant reconciles B2B+B2C against
// the totals and against HSN; if those disagree the return is unfilable.
func TestBuildReturn_SectionsReconcileToTheSameTotals(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{
		tnInvoice(1, "33AABCU9603R1ZM"),
		tnInvoice(2, ""),
		kaInvoice(3, ""),
		kaInvoice(4, "29AABCU9603R1ZM"),
	}, nil)

	sum := decimal.Zero
	for _, l := range ret.B2B {
		sum = sum.Add(l.TaxableValue)
	}
	for _, l := range ret.B2C {
		sum = sum.Add(l.TaxableValue)
	}
	if !sum.Equal(ret.Totals.TaxableValue) {
		t.Errorf("B2B + B2C taxable = %s, totals say %s", sum, ret.Totals.TaxableValue)
	}
	if !ret.HSN[0].TaxableValue.Equal(ret.Totals.TaxableValue) {
		t.Errorf("HSN taxable = %s, totals say %s", ret.HSN[0].TaxableValue, ret.Totals.TaxableValue)
	}
	if ret.Totals.Invoices != 4 {
		t.Errorf("invoice count = %d, want 4", ret.Totals.Invoices)
	}
}

// A month with no invoices must not render an HSN row of zeroes, which
// would read as a filed nil return rather than an empty one.
func TestBuildReturn_EmptyPeriodHasNoHSNRow(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier, nil, nil)
	if len(ret.HSN) != 0 {
		t.Errorf("want no HSN line for an empty period, got %+v", ret.HSN)
	}
	if ret.Totals.Invoices != 0 {
		t.Errorf("invoice count = %d, want 0", ret.Totals.Invoices)
	}
}

// Two exports of one month must be byte-identical, so a re-export diffs
// clean against the copy that was filed.
func TestWriteAccountantCSV_IsDeterministic(t *testing.T) {
	rows := []InvoiceRow{
		kaInvoice(3, ""), tnInvoice(1, "33AABCU9603R1ZM"),
		tnInvoice(2, ""), kaInvoice(4, "29AABCU9603R1ZM"),
	}
	render := func() string {
		var buf bytes.Buffer
		if err := WriteAccountantCSV(&buf, BuildReturn(Period{2026, time.January}, testSupplier, rows, nil)); err != nil {
			t.Fatalf("WriteAccountantCSV: %v", err)
		}
		return buf.String()
	}
	if a, b := render(), render(); a != b {
		t.Error("two renders of the same period differ; a re-export would not diff clean")
	}
}

func TestWriteAccountantCSV_CarriesEverySection(t *testing.T) {
	var buf bytes.Buffer
	ret := BuildReturn(Period{2026, time.January}, testSupplier, []InvoiceRow{
		tnInvoice(1, "33AABCU9603R1ZM"), tnInvoice(2, ""),
	}, nil)
	if err := WriteAccountantCSV(&buf, ret); err != nil {
		t.Fatalf("WriteAccountantCSV: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"GSTR-1 outward supplies",
		"33AABCU9603R1ZM", // supplier GSTIN in the header
		"012026",          // period as MMYYYY
		"B2B - supplies to registered persons (Table 4)",
		"INV-000001",
		"B2C - supplies to unregistered persons",
		"HSN summary (Table 12)",
		HSNInternetServices,
		"Totals",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CSV missing %q", want)
		}
	}
	// Money must never render as a float.
	if strings.Contains(out, "71.909") || strings.Contains(out, "942.8200000") {
		t.Error("amounts must be fixed 2dp strings, not float text")
	}
}

// ── Credit notes (Table 9B) ─────────────────────────────────────────────
//
// The real correction these were built for: two renewals invoiced at the
// tax-inclusive total but collected at the pre-tax price. The credit note
// brings each invoice down to what was actually collected.

// tnCreditNote credits a TN invoice by 91.38 + 8.22 + 8.22 = 107.82, the
// exact shape migration 049 issues.
func tnCreditNote(id, invoiceID int, gstin string) CreditNoteRow {
	return CreditNoteRow{
		CreditNoteID: id, OriginalInvoiceID: invoiceID,
		OriginalInvoiceDate: jan(invoiceID), NoteDate: jan(20),
		SubscriberID:   invoiceID,
		SubscriberName: "Sub", RecipientGSTIN: gstin, RecipientState: "TN",
		TaxableValue: d2("91.38"), CGST: d2("8.22"), SGST: d2("8.22"),
		IGST: decimal.Zero, Total: d2("107.82"),
	}
}

// A credit note has to reduce the return's liability. If it only appeared in
// its own table, the filer would declare tax on money that was credited back.
func TestBuildReturn_CreditNotesReduceTheTotals(t *testing.T) {
	invoices := []InvoiceRow{tnInvoice(1, ""), tnInvoice(2, "")}
	withNotes := BuildReturn(Period{2026, time.January}, testSupplier, invoices,
		[]CreditNoteRow{tnCreditNote(1, 1, "")})
	without := BuildReturn(Period{2026, time.January}, testSupplier, invoices, nil)

	if withNotes.Totals.CreditNotes != 1 {
		t.Errorf("credit note count: want 1, got %d", withNotes.Totals.CreditNotes)
	}
	// Invoice count is unchanged: a note adjusts a supply, it does not
	// cancel the fact that one was made.
	if withNotes.Totals.Invoices != without.Totals.Invoices {
		t.Errorf("invoice count changed: %d -> %d", without.Totals.Invoices, withNotes.Totals.Invoices)
	}

	wantTaxable := without.Totals.TaxableValue.Sub(d2("91.38"))
	if !withNotes.Totals.TaxableValue.Equal(wantTaxable) {
		t.Errorf("taxable value: want %s, got %s", wantTaxable, withNotes.Totals.TaxableValue)
	}
	wantTotal := without.Totals.Total.Sub(d2("107.82"))
	if !withNotes.Totals.Total.Equal(wantTotal) {
		t.Errorf("total: want %s, got %s — the return would declare tax that was credited back",
			wantTotal, withNotes.Totals.Total)
	}
	if !withNotes.Totals.CGST.Equal(without.Totals.CGST.Sub(d2("8.22"))) {
		t.Errorf("CGST not reduced by the note: got %s", withNotes.Totals.CGST)
	}
}

// The HSN summary must agree with the totals. Two sections of one return
// disagreeing is the failure mode that gets a return rejected at the portal.
func TestBuildReturn_HSNIsNetOfCreditNotes(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier,
		[]InvoiceRow{tnInvoice(1, ""), tnInvoice(2, "")},
		[]CreditNoteRow{tnCreditNote(1, 1, "")})

	if len(ret.HSN) != 1 {
		t.Fatalf("want 1 HSN line, got %d", len(ret.HSN))
	}
	h := ret.HSN[0]
	if !h.TaxableValue.Equal(ret.Totals.TaxableValue) {
		t.Errorf("HSN taxable %s does not equal totals taxable %s", h.TaxableValue, ret.Totals.TaxableValue)
	}
	if !h.TotalValue.Equal(ret.Totals.Total) {
		t.Errorf("HSN total %s does not equal totals total %s", h.TotalValue, ret.Totals.Total)
	}
	// Quantity counts supplies made, which a credit note does not undo.
	if !h.Quantity.Equal(decimal.NewFromInt(2)) {
		t.Errorf("HSN quantity: want 2 supplies, got %s — a note adjusts value, not whether service was delivered", h.Quantity)
	}
}

// CDNR vs CDNUR follows the recipient's registration, exactly as B2B/B2C
// does, and the note must name the invoice it adjusts or it cannot be filed.
func TestBuildReturn_CreditNoteCarriesItsOriginalInvoiceAndKind(t *testing.T) {
	ret := BuildReturn(Period{2026, time.January}, testSupplier,
		[]InvoiceRow{tnInvoice(1, "33AABCU9603R1ZM"), tnInvoice(2, "")},
		[]CreditNoteRow{
			tnCreditNote(1, 1, "33AABCU9603R1ZM"),
			tnCreditNote(2, 2, ""),
		})

	if len(ret.CreditNotes) != 2 {
		t.Fatalf("want 2 credit note lines, got %d", len(ret.CreditNotes))
	}
	byNumber := map[string]CDNLine{}
	for _, l := range ret.CreditNotes {
		byNumber[l.NoteNumber] = l
	}
	if !byNumber["CRN-000001"].Registered {
		t.Error("a note against a GSTIN-holding recipient must be CDNR")
	}
	if byNumber["CRN-000002"].Registered {
		t.Error("a note against an unregistered recipient must be CDNUR")
	}
	if got := byNumber["CRN-000001"].OriginalInvoice; got != "INV-000001" {
		t.Errorf("original invoice reference: want INV-000001, got %q — Table 9B is unfileable without it", got)
	}
	// The rate is derived from the note's own amounts, so a note adjusting an
	// 18% supply reports 18%, not whatever rate is current.
	if got := byNumber["CRN-000001"].Rate.StringFixed(2); got != "18.00" {
		t.Errorf("note rate: want 18.00, got %s", got)
	}
}

// A month whose only activity was crediting an earlier invoice still has a
// net supply to report; emitting no HSN row would read as a nil return.
func TestBuildReturn_CreditNoteOnlyMonthStillReports(t *testing.T) {
	ret := BuildReturn(Period{2026, time.February}, testSupplier, nil,
		[]CreditNoteRow{tnCreditNote(1, 1, "")})

	if len(ret.CreditNotes) != 1 {
		t.Fatalf("want the note listed, got %d", len(ret.CreditNotes))
	}
	if len(ret.HSN) != 1 {
		t.Fatal("a credit-note-only month must still produce an HSN row, or it reads as a nil return")
	}
	if !ret.Totals.Total.Equal(d2("-107.82")) {
		t.Errorf("totals: want -107.82, got %s", ret.Totals.Total)
	}
}

// The export is what an accountant actually files from, so the section has
// to reach the CSV — a note held only in the struct would be silently lost.
func TestWriteAccountantCSV_IncludesCreditNoteSection(t *testing.T) {
	var buf bytes.Buffer
	ret := BuildReturn(Period{2026, time.January}, testSupplier,
		[]InvoiceRow{tnInvoice(1, "")},
		[]CreditNoteRow{tnCreditNote(1, 1, "")})
	if err := WriteAccountantCSV(&buf, ret); err != nil {
		t.Fatalf("WriteAccountantCSV: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Credit notes (Table 9B)",
		"CRN-000001",
		"INV-000001",
		"CDNUR (unregistered)",
		"107.82",
		"Totals (net of credit notes)",
		"net of credit notes", // the HSN header says so too
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CSV is missing %q", want)
		}
	}
}

// derivedGSTRate is what keeps a return fileable, so its edges are pinned
// directly rather than only through the documents that use it.
func TestDerivedGSTRate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		taxable, tax string
		want         string
		why          string
	}{
		{"exact 18 on a full invoice", "799.00", "143.82", "18", "the case that always worked"},
		{"18 on a small credit note", "91.38", "16.44", "18",
			"derives 17.99 from rounded heads; a return reporting 17.99% is rejected"},
		{"5 percent", "1000.00", "50.00", "5", ""},
		{"zero-rated", "1000.00", "0.00", "0", ""},
		{"no taxable value", "0.00", "0.00", "0", "must not divide by zero"},
		{"genuinely off-rate is not forced onto a legal one", "1000.00", "220.00", "22",
			"22% is not a GST rate; relabelling it 18 or 28 would file a figure " +
				"nobody could reconcile back to the document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := derivedGSTRate(d2(tc.taxable), d2(tc.tax))
			if got.String() != tc.want {
				t.Errorf("derivedGSTRate(%s, %s) = %s, want %s — %s",
					tc.taxable, tc.tax, got, tc.want, tc.why)
			}
		})
	}
}
