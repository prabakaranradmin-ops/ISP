package billing

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Payment receipts — FR-BIL-021.
//
// A receipt acknowledges money credited to a subscriber's prepaid wallet:
// a gateway payment, cash or a cheque at the counter, a transfer, or TDS a
// business customer deducted. It is not a tax invoice — GST is invoiced
// when the balance is spent on a renewal — and says so.
//
// Rendered on demand from the payment's own records, so every recorded
// payment has one and a bounced cheque's receipt shows that it no longer
// stands, from the moment the bounce is recorded.

// ReceiptNumber is the number printed on a receipt: the payment's wallet
// ledger id, in its own series.
func ReceiptNumber(walletLedgerID int) string { return fmt.Sprintf("RCPT-%06d", walletLedgerID) }

// PaymentReceipt is one recorded payment, as the store reads it.
type PaymentReceipt struct {
	PaymentID      int // the wallet credit leg's id
	SubscriberID   int
	Username       string
	MobileNumber   string
	CAFNumber      string
	ReceivedAt     time.Time
	Amount         decimal.Decimal
	BalanceAfter   decimal.Decimal
	Method         string // a billing.Method* value, or a legacy description
	Reference      string
	ChequeNumber   string
	ChequeBank     string
	ChequeDate     *time.Time
	TDSCertificate string
	Status         string // "cleared" or "bounced"
	BouncedAt      *time.Time
	BounceReason   string
	RecordedBy     string
}

// Bounced reports whether the payment was reversed.
func (p PaymentReceipt) Bounced() bool { return p.Status == "bounced" }

// ReceiptData is what the receipt document prints.
type ReceiptData struct {
	PaymentReceipt
	Number        string
	AmountInWords string
	MethodLabel   string
	SupplierName  string
	SupplierGSTIN string
}

// NewReceiptData prepares a payment for printing.
func NewReceiptData(p PaymentReceipt, supplier Supplier) ReceiptData {
	return ReceiptData{
		PaymentReceipt: p,
		Number:         ReceiptNumber(p.PaymentID),
		AmountInWords:  AmountInWords(p.Amount),
		MethodLabel:    MethodLabel(p.Method),
		SupplierName:   supplier.Name,
		SupplierGSTIN:  supplier.GSTIN,
	}
}

// MethodLabel words a payment method for a customer.
func MethodLabel(method string) string {
	switch method {
	case MethodCash:
		return "Cash"
	case MethodUPI:
		return "UPI"
	case MethodBankTransfer:
		return "Bank transfer"
	case MethodCheque:
		return "Cheque"
	case MethodOnline:
		return "Online payment"
	case MethodTDS:
		return "Tax deducted at source (TDS)"
	case "":
		return "Payment"
	}
	// A payment from before methods were recorded carries its description.
	return method
}

var (
	ones = []string{"", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten",
		"Eleven", "Twelve", "Thirteen", "Fourteen", "Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen"}
	tens = []string{"", "", "Twenty", "Thirty", "Forty", "Fifty", "Sixty", "Seventy", "Eighty", "Ninety"}
)

// belowHundred and belowThousand word the small groups.
func belowHundred(n int64) string {
	if n < 20 {
		return ones[n]
	}
	if n%10 == 0 {
		return tens[n/10]
	}
	return tens[n/10] + " " + ones[n%10]
}

func belowThousand(n int64) string {
	switch {
	case n == 0:
		return ""
	case n < 100:
		return belowHundred(n)
	case n%100 == 0:
		return ones[n/100] + " Hundred"
	}
	return ones[n/100] + " Hundred " + belowHundred(n%100)
}

// rupeesInWords words a whole number of rupees in the Indian system:
// crore, lakh, thousand, hundred.
func rupeesInWords(n int64) string {
	if n == 0 {
		return "Zero"
	}
	var parts []string
	if c := n / 10_000_000; c > 0 {
		// Above 99 crore the crore count is itself worded the same way.
		parts = append(parts, rupeesInWords(c)+" Crore")
		n %= 10_000_000
	}
	if l := n / 100_000; l > 0 {
		parts = append(parts, belowHundred(l)+" Lakh")
		n %= 100_000
	}
	if t := n / 1000; t > 0 {
		parts = append(parts, belowHundred(t)+" Thousand")
		n %= 1000
	}
	if n > 0 {
		parts = append(parts, belowThousand(n))
	}
	return strings.Join(parts, " ")
}

// AmountInWords words an amount as an Indian receipt states it:
// "Rupees One Thousand One Hundred Eighty and Fifty Paise Only".
func AmountInWords(amount decimal.Decimal) string {
	amount = amount.Abs().Round(2)
	rupees := amount.IntPart()
	paise := amount.Sub(decimal.NewFromInt(rupees)).Mul(decimal.NewFromInt(100)).IntPart()
	s := "Rupees " + rupeesInWords(rupees)
	if paise > 0 {
		s += " and " + belowHundred(paise) + " Paise"
	}
	return s + " Only"
}

// GenerateReceiptPDF prints a receipt through the same headless browser as
// invoices.
func (c *InvoicePDFClient) GenerateReceiptPDF(ctx context.Context, data ReceiptData) ([]byte, error) {
	html, err := renderReceiptHTML(data)
	if err != nil {
		return nil, fmt.Errorf("billing: render receipt HTML: %w", err)
	}
	return c.printHTML(ctx, html)
}

func renderReceiptHTML(data ReceiptData) (string, error) {
	var buf bytes.Buffer
	if err := receiptHTMLTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// html/template, as for invoices: the name and references are customer- or
// staff-entered text, printed by a real browser.
var receiptHTMLTemplate = template.Must(template.New("receipt").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
  body { font-family: Arial, sans-serif; font-size: 12px; margin: 40px; color: #222; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  .supplier { font-size: 13px; margin-bottom: 18px; color: #444; }
  table { border-collapse: collapse; width: 100%; margin-bottom: 16px; }
  th, td { border: 1px solid #ccc; padding: 6px 10px; text-align: left; vertical-align: top; }
  th { background: #f5f5f5; width: 30%; }
  .amount { font-size: 18px; font-weight: bold; }
  .void { border: 2px solid #c62828; color: #c62828; padding: 10px 14px; margin-bottom: 16px; font-weight: bold; }
  .footer { margin-top: 24px; font-size: 10px; color: #666; }
</style>
</head>
<body>
  {{if .SupplierName}}<div class="supplier"><strong>{{.SupplierName}}</strong>{{if .SupplierGSTIN}} &middot; GSTIN {{.SupplierGSTIN}}{{end}}</div>{{end}}
  <h1>Payment Receipt</h1>
  <p><strong>Receipt Number:</strong> {{.Number}} &nbsp;&nbsp; <strong>Date:</strong> {{.ReceivedAt.Format "02 Jan 2006 15:04"}}</p>

  {{if .Bounced}}
  <div class="void">CANCELLED — this cheque was returned unpaid{{if .BouncedAt}} on {{.BouncedAt.Format "02 Jan 2006"}}{{end}}{{if .BounceReason}} ({{.BounceReason}}){{end}}.
    Its value was taken back out of the account and this receipt no longer stands.</div>
  {{end}}

  <table>
    <tr><th>Received from</th><td>{{.Username}}{{if .CAFNumber}} &middot; CAF {{.CAFNumber}}{{end}}{{if .MobileNumber}} &middot; {{.MobileNumber}}{{end}}</td></tr>
    <tr><th>Amount</th><td><span class="amount">₹{{.Amount.StringFixed 2}}</span><br>{{.AmountInWords}}</td></tr>
    <tr><th>Paid by</th><td>{{.MethodLabel}}
      {{if .ChequeNumber}}<br>Cheque {{.ChequeNumber}}{{if .ChequeBank}}, {{.ChequeBank}}{{end}}{{if .ChequeDate}}, dated {{.ChequeDate.Format "02 Jan 2006"}}{{end}}{{end}}
      {{if .TDSCertificate}}<br>TDS certificate (Form 16A) {{.TDSCertificate}}{{end}}
      {{if .Reference}}<br>Reference {{.Reference}}{{end}}</td></tr>
    <tr><th>Credited to</th><td>Prepaid account of subscriber {{.SubscriberID}} &middot; balance after this payment ₹{{.BalanceAfter.StringFixed 2}}</td></tr>
    {{if .RecordedBy}}<tr><th>Recorded by</th><td>{{.RecordedBy}}</td></tr>{{end}}
  </table>

  <p class="footer">
    This receipt acknowledges money credited to a prepaid account. It is not a tax invoice: a GST invoice is issued
    when the balance is used for a plan renewal or a charge.{{if eq .Method "cheque"}} Payment by cheque is subject to realisation.{{end}}<br>
    This is a computer-generated receipt. For queries, contact support.
  </p>
</body>
</html>
`))
