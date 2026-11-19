package billing

import (
	"strings"
	"testing"
	"time"
)

// The itemised invoice document — FR-BIL-022.

func itemisedInvoiceData(interstate bool) InvoiceData {
	rate := GstRate{ID: 1, CgstRate: d("9"), SgstRate: d("9"), IgstRate: d("18")}
	state := "TN"
	if interstate {
		state = "KA"
	}
	router := 70
	lines, hdr, err := ComposeInvoice([]LineCharge{
		PlanCharge("Fibre 100", d("500.00")),
		{Kind: LineKindProduct, Description: "Router rental", HSNSAC: "8517",
			ProductID: &router, Quantity: 2, UnitPrice: d("150.00")},
	}, state, "TN", rate)
	if err != nil {
		panic(err)
	}

	return InvoiceData{
		InvoiceNumber:   "INV-000042",
		InvoiceDate:     time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		DueDate:         time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
		SubscriberName:  "Test Subscriber",
		MobileNumber:    "+919000000000",
		RegisteredState: state,
		PlanName:        "Fibre 100",
		PlanPeriod:      "January 2026",
		Lines:           lines,
		BaseAmount:      hdr.BaseAmount,
		CGSTRate:        rate.CgstRate,
		CGSTAmount:      hdr.CgstAmount,
		SGSTRate:        rate.SgstRate,
		SGSTAmount:      hdr.SgstAmount,
		IGSTRate:        rate.IgstRate,
		IGSTAmount:      hdr.IgstAmount,
		TotalAmount:     hdr.TotalAmount,
		GBUsed:          d("40"),
		GBIncluded:      d("100"),
		SpeedActive:     "100 Mbps / 100 Mbps",
	}
}

func TestFR_BIL_022_ItemisedInvoicePrintsEveryLine(t *testing.T) {
	data := itemisedInvoiceData(false)
	html, err := renderInvoiceHTML(data)
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}

	for _, want := range []string{
		"Internet Plan — Fibre 100", // the plan line
		"Router rental",             // the catalogue line
		"HSN/SAC",                   // the column GST requires
		"8517",                      // the rental's own code
		PlanHSNSAC,                  // the plan's
		"800.00",                    // 500 + 2 × 150 taxable
		"944.00",                    // plus 9% + 9%
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the invoice does not print %q", want)
		}
	}

	// Intrastate: two tax columns, no IGST column.
	if !strings.Contains(html, "CGST (₹)") || !strings.Contains(html, "SGST (₹)") {
		t.Error("an intrastate invoice must print CGST and SGST columns")
	}
	if strings.Contains(html, "IGST (₹)") {
		t.Error("an intrastate invoice must not print an IGST column")
	}
	// Each line's quantity and unit rate are shown, not just the total:
	// a customer disputing a bill needs to see how it was arrived at.
	if !strings.Contains(html, "150.00") {
		t.Error("the rental's unit rate is missing")
	}
}

func TestFR_BIL_022_InterstateItemisedInvoicePrintsIGSTOnly(t *testing.T) {
	html, err := renderInvoiceHTML(itemisedInvoiceData(true))
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}
	if !strings.Contains(html, "IGST (₹)") {
		t.Error("an interstate invoice must print an IGST column")
	}
	if strings.Contains(html, "CGST (₹)") || strings.Contains(html, "SGST (₹)") {
		t.Error("an interstate invoice must not print CGST or SGST columns")
	}
	// 800 + 18% = 944.00 either way; the heads differ, not the total.
	if !strings.Contains(html, "944.00") {
		t.Error("the interstate total is missing")
	}
}

// An invoice raised before migration 058 has no lines and must still print
// exactly as it always did — from the header, with the plan description.
func TestFR_BIL_022_InvoiceWithoutLinesStillPrintsFromTheHeader(t *testing.T) {
	data := itemisedInvoiceData(false)
	data.Lines = nil

	html, err := renderInvoiceHTML(data)
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}
	if strings.Contains(html, "HSN/SAC</th>") {
		t.Error("an invoice with no lines printed the itemised table")
	}
	if !strings.Contains(html, "Internet Plan — Fibre 100") {
		t.Error("the plan line is missing from a header-only invoice")
	}
	if !strings.Contains(html, "944.00") {
		t.Error("the total is missing from a header-only invoice")
	}
	// The usage summary belongs to a plan renewal and must survive.
	if !strings.Contains(html, "Data Usage Summary") {
		t.Error("the usage summary is missing")
	}
}

// The itemised invoice keeps FR-BIL-007's usage summary: a cycle with a
// router rental on it is still a plan renewal.
func TestFR_BIL_022_ItemisedInvoiceKeepsTheUsageSummary(t *testing.T) {
	html, err := renderInvoiceHTML(itemisedInvoiceData(false))
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}
	if !strings.Contains(html, "Data Usage Summary") {
		t.Error("the usage summary is missing from an itemised invoice")
	}
	if !strings.Contains(html, "40 GB of 100 GB included") {
		t.Error("the usage figures are missing")
	}
}

// A subscriber name carrying markup is escaped, not rendered — the reason
// this template is html/template. Checked again here because the itemised
// branch is new markup that could have been written with text/template
// habits.
func TestFR_BIL_022_ItemisedInvoiceEscapesSuppliedText(t *testing.T) {
	data := itemisedInvoiceData(false)
	data.SubscriberName = `<script>alert(1)</script>`
	data.Lines[1].Description = `Router <b>rental</b>`

	html, err := renderInvoiceHTML(data)
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Error("a subscriber name was rendered as markup")
	}
	if strings.Contains(html, "Router <b>rental</b>") {
		t.Error("a line description was rendered as markup")
	}
	if !strings.Contains(html, "&lt;b&gt;rental&lt;/b&gt;") {
		t.Error("the line description was not escaped into the document at all")
	}
}
