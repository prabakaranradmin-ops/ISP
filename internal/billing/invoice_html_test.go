package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func testInvoiceData() InvoiceData {
	return InvoiceData{
		InvoiceNumber:   "INV-000042",
		InvoiceDate:     time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		DueDate:         time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
		SubscriberName:  "Test Subscriber",
		MobileNumber:    "+919876543210",
		RegisteredState: "TN",
		PlanName:        "TN_Super_100M",
		PlanPeriod:      "January 2026",
		BaseAmount:      decimal.RequireFromString("799.00"),
		CGSTRate:        decimal.RequireFromString("9.00"),
		CGSTAmount:      decimal.RequireFromString("71.91"),
		SGSTRate:        decimal.RequireFromString("9.00"),
		SGSTAmount:      decimal.RequireFromString("71.91"),
		TotalAmount:     decimal.RequireFromString("942.82"),
		GBUsed:          decimal.RequireFromString("120.00"),
		GBIncluded:      decimal.RequireFromString("3300.00"),
		SpeedActive:     "100 Mbps / 100 Mbps",
	}
}

// TestFR_BIL_007_RenderInvoiceHTML_IncludesPlainLanguageUsageSummary verifies
// renderInvoiceHTML (the template GeneratePDF hands to the browser — this
// package's only real business logic, everything past it is chromedp
// plumbing exercised by invoice_pdf_test.go instead) carries the real
// invoice fields, GST split correctly, and the plain-language usage summary
// FR-BIL-007 requires.
func TestFR_BIL_007_RenderInvoiceHTML_IncludesPlainLanguageUsageSummary(t *testing.T) {
	html, err := renderInvoiceHTML(testInvoiceData())
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}

	for _, want := range []string{"INV-000042", "Test Subscriber", "TN_Super_100M", "942.82", "CGST", "120 GB of 3300 GB"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered invoice HTML missing %q", want)
		}
	}
}

// TestRenderInvoiceHTML_Interstate verifies the IGST branch of the invoice
// template renders when CGST/SGST are zero (SecD/DBD's mutually-exclusive
// GST rule — chk_gst_logic at the DB layer, mirrored here at the template
// layer).
func TestRenderInvoiceHTML_Interstate(t *testing.T) {
	data := testInvoiceData()
	data.CGSTRate, data.CGSTAmount = decimal.Zero, decimal.Zero
	data.SGSTRate, data.SGSTAmount = decimal.Zero, decimal.Zero
	data.IGSTRate = decimal.RequireFromString("18.00")
	data.IGSTAmount = decimal.RequireFromString("143.82")

	html, err := renderInvoiceHTML(data)
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}
	if !strings.Contains(html, "IGST") {
		t.Error("expected the IGST line item to render when CGST/SGST are both zero")
	}
	if strings.Contains(html, "CGST @") {
		t.Error("CGST/SGST line items must not render when both are zero")
	}
}

// TestRenderInvoiceHTML_EscapesSubscriberSuppliedFields guards the
// text/template to html/template switch: SubscriberName and MobileNumber
// come from subscriber-supplied KYC data, and this HTML is executed by a
// real browser (chromedp's headless Chrome, via GeneratePDF) rather than
// just handed to a converter — a field containing HTML must render as
// inert text, not be interpreted as markup.
func TestRenderInvoiceHTML_EscapesSubscriberSuppliedFields(t *testing.T) {
	data := testInvoiceData()
	data.SubscriberName = `<script>alert(1)</script>`

	html, err := renderInvoiceHTML(data)
	if err != nil {
		t.Fatalf("renderInvoiceHTML: %v", err)
	}
	if strings.Contains(html, "<script>") {
		t.Error("SubscriberName containing markup was rendered unescaped into the invoice HTML")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("expected the markup to appear HTML-escaped in the rendered invoice")
	}
}
