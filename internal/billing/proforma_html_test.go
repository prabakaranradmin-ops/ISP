package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The rendered document, checked as HTML so it runs without a browser.

func htmlInvoice(mut func(*InvoiceData)) InvoiceData {
	d := InvoiceData{
		InvoiceNumber: "INV-000042", InvoiceDate: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		DueDate: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), SubscriberName: "Test", RegisteredState: "TN",
		PlanName: "TN_Super_100M", PlanPeriod: "January 2026",
		BaseAmount: decimal.RequireFromString("799.00"),
		CGSTRate:   decimal.RequireFromString("9.00"), CGSTAmount: decimal.RequireFromString("71.91"),
		SGSTRate: decimal.RequireFromString("9.00"), SGSTAmount: decimal.RequireFromString("71.91"),
		IGSTRate:    decimal.RequireFromString("18.00"),
		TotalAmount: decimal.RequireFromString("942.82"), GBIncluded: decimal.NewFromInt(3300),
		SpeedActive: "100M/100M",
	}
	if mut != nil {
		mut(&d)
	}
	return d
}

func renderOrFail(t *testing.T, d InvoiceData) string {
	t.Helper()
	html, err := renderInvoiceHTML(d)
	if err != nil {
		t.Fatal(err)
	}
	return html
}

func TestInvoiceHTML_TaxRowsFollowTheInvoicesOwnAmounts(t *testing.T) {
	// Every gst_rates row carries a CGST rate as well as an IGST one, so the
	// rows must be chosen by what this invoice charged, not by the rate table.
	inter := renderOrFail(t, htmlInvoice(func(d *InvoiceData) {
		d.RegisteredState = "KA"
		d.CGSTAmount, d.SGSTAmount = decimal.Zero, decimal.Zero
		d.IGSTAmount = decimal.RequireFromString("143.82")
	}))
	if !strings.Contains(inter, "IGST @ 18") || !strings.Contains(inter, "143.82") {
		t.Error("an interstate invoice must print its IGST line")
	}
	if strings.Contains(inter, "CGST @") {
		t.Error("an interstate invoice must not print CGST/SGST at zero")
	}

	intra := renderOrFail(t, htmlInvoice(nil))
	if !strings.Contains(intra, "CGST @ 9") || strings.Contains(intra, "IGST @") {
		t.Error("an intrastate invoice prints CGST and SGST only")
	}
}

func TestFR_BIL_018_OneOffChargeAndProformaDocuments(t *testing.T) {
	charge := renderOrFail(t, htmlInvoice(func(d *InvoiceData) { d.LineDescription = "Installation & router" }))
	if !strings.Contains(charge, "Installation &amp; router") {
		t.Error("the charge's own description must be the line, escaped")
	}
	if strings.Contains(charge, "Internet Plan —") || strings.Contains(charge, "Data Usage Summary") {
		t.Error("a one-off charge carries no plan line or usage summary")
	}
	if !strings.Contains(charge, "GST Tax Invoice") {
		t.Error("a converted charge is a tax invoice")
	}

	pf := renderOrFail(t, htmlInvoice(func(d *InvoiceData) {
		d.Proforma, d.InvoiceNumber, d.LineDescription = true, "PF-000003", "Static IP for 12 months"
		d.ValidUntil = time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	}))
	for _, want := range []string{"Proforma Invoice", "PF-000003", "Valid until", "20 Jan 2026", "not a tax invoice", "(estimated)"} {
		if !strings.Contains(pf, want) {
			t.Errorf("proforma missing %q", want)
		}
	}
	if strings.Contains(pf, "GST Tax Invoice") || strings.Contains(pf, "Due Date") {
		t.Error("a proforma must never call itself a tax invoice or carry a due date")
	}
}
