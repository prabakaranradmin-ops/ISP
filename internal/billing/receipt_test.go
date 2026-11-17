package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Payment receipts — FR-BIL-021. Internal so the HTML can be checked
// without a browser.

func TestFR_BIL_021_AmountInWords(t *testing.T) {
	for in, want := range map[string]string{
		"0":           "Rupees Zero Only",
		"1":           "Rupees One Only",
		"19":          "Rupees Nineteen Only",
		"20":          "Rupees Twenty Only",
		"99":          "Rupees Ninety Nine Only",
		"100":         "Rupees One Hundred Only",
		"118":         "Rupees One Hundred Eighteen Only",
		"942.82":      "Rupees Nine Hundred Forty Two and Eighty Two Paise Only",
		"1000":        "Rupees One Thousand Only",
		"1180.50":     "Rupees One Thousand One Hundred Eighty and Fifty Paise Only",
		"99999":       "Rupees Ninety Nine Thousand Nine Hundred Ninety Nine Only",
		"100000":      "Rupees One Lakh Only",
		"250000.05":   "Rupees Two Lakh Fifty Thousand and Five Paise Only",
		"9999999":     "Rupees Ninety Nine Lakh Ninety Nine Thousand Nine Hundred Ninety Nine Only",
		"10000000":    "Rupees One Crore Only",
		"123456789":   "Rupees Twelve Crore Thirty Four Lakh Fifty Six Thousand Seven Hundred Eighty Nine Only",
		"10000000000": "Rupees One Thousand Crore Only",
	} {
		if got := AmountInWords(decimal.RequireFromString(in)); got != want {
			t.Errorf("%s:\n got %q\nwant %q", in, got, want)
		}
	}
}

func receiptFor(mut func(*PaymentReceipt)) ReceiptData {
	p := PaymentReceipt{PaymentID: 81, SubscriberID: 5, Username: "acme_ltd", MobileNumber: "+919800000001",
		CAFNumber: "CAF-77", ReceivedAt: time.Date(2026, 11, 17, 10, 30, 0, 0, time.UTC),
		Amount: decimal.RequireFromString("1180.00"), BalanceAfter: decimal.RequireFromString("1500.00"),
		Method: MethodCash, Status: "cleared", RecordedBy: "meena"}
	if mut != nil {
		mut(&p)
	}
	return NewReceiptData(p, Supplier{Name: "Maaran Net", GSTIN: "33ABCDE1234F1Z5"})
}

func render(t *testing.T, d ReceiptData) string {
	t.Helper()
	html, err := renderReceiptHTML(d)
	if err != nil {
		t.Fatal(err)
	}
	return html
}

func TestFR_BIL_021_ReceiptDocument(t *testing.T) {
	t.Run("a cleared cash payment", func(t *testing.T) {
		h := render(t, receiptFor(nil))
		for _, want := range []string{"Payment Receipt", "RCPT-000081", "17 Nov 2026", "acme_ltd", "CAF-77",
			"₹1180.00", "Rupees One Thousand One Hundred Eighty Only", "Cash", "₹1500.00", "meena",
			"Maaran Net", "33ABCDE1234F1Z5", "not a tax invoice"} {
			if !strings.Contains(h, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(h, "CANCELLED") || strings.Contains(h, "subject to realisation") {
			t.Error("a cleared cash payment carries no cancellation or cheque caveat")
		}
	})

	t.Run("a cheque names itself and is subject to realisation", func(t *testing.T) {
		date := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
		h := render(t, receiptFor(func(p *PaymentReceipt) {
			p.Method, p.ChequeNumber, p.ChequeBank, p.ChequeDate = MethodCheque, "000123", "State Bank of India", &date
		}))
		for _, want := range []string{"Cheque 000123, State Bank of India, dated 15 Nov 2026", "subject to realisation"} {
			if !strings.Contains(h, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("a bounced cheque's receipt says it no longer stands", func(t *testing.T) {
		at := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
		h := render(t, receiptFor(func(p *PaymentReceipt) {
			p.Method, p.ChequeNumber, p.Status, p.BouncedAt, p.BounceReason = MethodCheque, "000123", "bounced", &at, "Insufficient funds"
		}))
		for _, want := range []string{"CANCELLED", "20 Nov 2026", "Insufficient funds", "no longer stands"} {
			if !strings.Contains(h, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("TDS names its certificate", func(t *testing.T) {
		h := render(t, receiptFor(func(p *PaymentReceipt) { p.Method, p.TDSCertificate = MethodTDS, "16A-77" }))
		if !strings.Contains(h, "Tax deducted at source (TDS)") || !strings.Contains(h, "Form 16A) 16A-77") {
			t.Error("TDS receipt")
		}
	})

	t.Run("an old payment with no recorded method keeps its description", func(t *testing.T) {
		if got := MethodLabel("razorpay webhook"); got != "razorpay webhook" {
			t.Errorf("legacy method: %q", got)
		}
	})

	t.Run("names and references are printed as text, not markup", func(t *testing.T) {
		h := render(t, receiptFor(func(p *PaymentReceipt) { p.Username, p.Reference = `<script>x</script>`, `"><img src=x>` }))
		if strings.Contains(h, "<script>x") || strings.Contains(h, "<img src=x>") {
			t.Error("unescaped markup in the receipt")
		}
	})
}
