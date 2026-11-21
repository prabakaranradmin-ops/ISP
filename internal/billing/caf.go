package billing

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"time"

	"github.com/shopspring/decimal"
)

// Customer Application Form — FR-DOC-002 | FR-DOC-001's pipeline.
//
// The CAF is the form a customer signs when they take a connection, and the
// copy an operator files and produces for a licence inspection. It prints
// through the same headless browser as invoices and receipts, so there is
// one PDF path to keep working rather than three.

// CAFData is what the form prints.
//
// Assembled by the caller from the subscriber, their plan and their KYC
// record. Deliberately a flat value with no store access of its own: the
// template cannot then reach for a field nobody decided to put on a form
// that gets printed, signed and filed.
type CAFData struct {
	Supplier Supplier

	// CAFNumber is the subscriber's own caf_number, which is what the form
	// is filed under and what an inspection asks for.
	CAFNumber string
	// FullName comes from the lead the subscriber was converted from
	// (FR-CRM-002), because subscribers carries no name of its own. Empty
	// for a subscriber created directly, and the form says so rather than
	// printing a blank line somebody might fill in later.
	FullName string
	Username string

	MobileNumber string
	Email        string
	// RegisteredState is the GST state of supply, which is also the state
	// the connection is licensed in.
	RegisteredState string
	GSTIN           string
	Franchise       string

	PlanName     string
	PlanPrice    decimal.Decimal
	RateLimit    string
	VolumeGB     int
	ValidityDays int

	// KYCStatus is the subscriber's kyc_status.
	KYCStatus string
	// AadhaarOnRecord and PANOnRecord say whether each document is held.
	//
	// The numbers themselves are deliberately absent. They are stored
	// AES-GCM encrypted (FR-SEC-002) and there is no decrypt path for them
	// anywhere in this system — the only reads that decrypt are subscriber
	// credentials and NAS secrets. Printing even a masked Aadhaar would
	// mean adding one, and a new way to extract identity numbers is a
	// decision to take deliberately under the DPDP Act, not a side effect
	// of making a form printable. What a filed CAF needs to show is that
	// the documents were checked and when; that is what these carry.
	AadhaarOnRecord bool
	PANOnRecord     bool
	KYCVerifiedAt   *time.Time

	AppliedOn time.Time
	PrintedOn time.Time
	PrintedBy string
}

// CustomerName is what the form addresses, falling back to the username.
//
// A CAF with no name at all is not a form anybody can file, so the username
// stands in — it is what every other screen identifies the subscriber by,
// and it is honest about being an account name rather than inventing one.
func (d CAFData) CustomerName() string {
	if d.FullName != "" {
		return d.FullName
	}
	return d.Username
}

// NameIsAccountName reports whether the form is falling back to the
// username, so the template can say so instead of implying the account name
// is the customer's legal name on a document they are about to sign.
func (d CAFData) NameIsAccountName() bool { return d.FullName == "" }

// KYCSummary describes the documents held, for the form's KYC box.
func (d CAFData) KYCSummary() string {
	switch {
	case d.AadhaarOnRecord && d.PANOnRecord:
		return "Aadhaar and PAN on record"
	case d.AadhaarOnRecord:
		return "Aadhaar on record"
	case d.PANOnRecord:
		return "PAN on record"
	default:
		return "No identity document on record"
	}
}

// KYCComplete reports whether there is anything on record at all, so the
// form can carry a visible warning rather than a quiet blank when a
// connection has been provisioned without documents.
func (d CAFData) KYCComplete() bool { return d.AadhaarOnRecord || d.PANOnRecord }

// GenerateCAFPDF prints a Customer Application Form through the same
// headless browser as invoices and receipts — FR-DOC-002.
func (c *InvoicePDFClient) GenerateCAFPDF(ctx context.Context, data CAFData) ([]byte, error) {
	if data.CAFNumber == "" {
		// The number is what the form is filed and retrieved under. A CAF
		// without one is a sheet of paper nobody can match to an account,
		// and printing it anyway is worse than refusing.
		return nil, fmt.Errorf("billing: cannot print a CAF with no application number")
	}
	html, err := renderCAFHTML(data)
	if err != nil {
		return nil, fmt.Errorf("billing: render CAF HTML: %w", err)
	}
	return c.printHTML(ctx, html)
}

func renderCAFHTML(data CAFData) (string, error) {
	var buf bytes.Buffer
	if err := cafHTMLTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// html/template, as for invoices and receipts: every field here is
// customer- or staff-entered text, printed by a real browser.
var cafHTMLTemplate = template.Must(template.New("caf").Funcs(template.FuncMap{
	"money": func(d decimal.Decimal) string { return d.StringFixed(2) },
	"day":   func(t time.Time) string { return t.Format("2 January 2006") },
	"dayPtr": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Format("2 January 2006")
	},
	"orDash": func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	},
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<style>
  body { font-family: Arial, sans-serif; font-size: 12px; margin: 36px; color: #222; }
  h1 { font-size: 19px; margin: 0 0 2px; }
  h2 { font-size: 13px; margin: 18px 0 6px; border-bottom: 1px solid #999; padding-bottom: 3px; }
  .supplier { font-size: 12px; color: #444; margin-bottom: 4px; }
  .cafno { font-size: 13px; font-weight: bold; margin-bottom: 14px; }
  table { border-collapse: collapse; width: 100%; margin-bottom: 4px; }
  th, td { border: 1px solid #ccc; padding: 5px 9px; text-align: left; vertical-align: top; }
  th { background: #f5f5f5; width: 30%; font-weight: normal; color: #444; }
  .warn { border: 2px solid #c62828; color: #c62828; padding: 8px 12px; margin: 10px 0; font-weight: bold; }
  .note { font-size: 10px; color: #666; margin: 4px 0 0; }
  .declaration { margin-top: 16px; font-size: 11px; line-height: 1.5; }
  .signatures { margin-top: 26px; width: 100%; }
  .signatures td { border: none; padding-top: 34px; width: 50%; font-size: 11px; }
  .sigline { border-top: 1px solid #333; padding-top: 3px; width: 80%; }
  .footer { margin-top: 22px; font-size: 10px; color: #666; }
</style>
</head>
<body>

<h1>Customer Application Form</h1>
<div class="supplier">{{.Supplier.Name}}{{with .Supplier.GSTIN}} &middot; GSTIN {{.}}{{end}}</div>
<div class="cafno">Application number: {{.CAFNumber}}</div>

<h2>Applicant</h2>
<table>
  <tr><th>Name</th><td>
    {{.CustomerName}}
    {{if .NameIsAccountName}}<div class="note">This is the account name held on the system; no separate
      customer name was recorded when this connection was created.</div>{{end}}
  </td></tr>
  <tr><th>Account username</th><td>{{.Username}}</td></tr>
  <tr><th>Mobile</th><td>{{orDash .MobileNumber}}</td></tr>
  <tr><th>Email</th><td>{{orDash .Email}}</td></tr>
  <tr><th>State of supply</th><td>{{orDash .RegisteredState}}</td></tr>
  {{with .GSTIN}}<tr><th>Customer GSTIN</th><td>{{.}}</td></tr>{{end}}
  {{with .Franchise}}<tr><th>Partner</th><td>{{.}}</td></tr>{{end}}
  <tr><th>Applied on</th><td>{{day .AppliedOn}}</td></tr>
</table>

<h2>Connection applied for</h2>
<table>
  <tr><th>Tariff</th><td>{{orDash .PlanName}}</td></tr>
  <tr><th>Price per cycle</th><td>&#8377;{{money .PlanPrice}}</td></tr>
  <tr><th>Speed</th><td>{{orDash .RateLimit}}</td></tr>
  <tr><th>Data included</th><td>{{if .VolumeGB}}{{.VolumeGB}} GB{{else}}Unlimited{{end}}</td></tr>
  <tr><th>Validity</th><td>{{if .ValidityDays}}{{.ValidityDays}} days{{else}}&mdash;{{end}}</td></tr>
</table>

<h2>Identity documents</h2>
{{if not .KYCComplete}}
<div class="warn">No identity document is on record for this connection.</div>
{{end}}
<table>
  <tr><th>Documents held</th><td>{{.KYCSummary}}</td></tr>
  <tr><th>Verification status</th><td>{{orDash .KYCStatus}}</td></tr>
  <tr><th>Verified on</th><td>{{dayPtr .KYCVerifiedAt}}</td></tr>
</table>
<p class="note">Document numbers are deliberately not printed on this form. They are held encrypted, and this
  copy records that they were checked rather than reproducing them &mdash; so a filed or emailed CAF cannot
  become a second copy of a customer's identity numbers.</p>

<div class="declaration">
  <strong>Declaration.</strong> I confirm that the details above are correct, that the identity documents I
  produced are genuine and belong to me, and that I have read and accept the terms of service and the
  acceptable-use and fair-use policies applying to the tariff shown. I understand that the connection may be
  suspended for non-payment, and that my usage is subject to the fair-use limits of my tariff.
</div>

<table class="signatures">
  <tr>
    <td><div class="sigline">Applicant's signature &middot; date</div></td>
    <td><div class="sigline">For {{.Supplier.Name}} &middot; date</div></td>
  </tr>
</table>

<p class="footer">
  Printed {{day .PrintedOn}}{{with .PrintedBy}} by {{.}}{{end}}.
  This form reflects the records held at the time of printing.
</p>

</body>
</html>
`))

// CAFDetail is what a store returns for one subscriber's application form.
//
// Separate from CAFData because the store reads facts while the document
// decides how to present them: CAFData has CustomerName and KYCSummary,
// which are presentation, and no field that could hold an identity number.
// Keeping them apart is what stops a store growing an Aadhaar field because
// a template asked for one.
type CAFDetail struct {
	CAFNumber string
	FullName  string
	Username  string

	MobileNumber    string
	Email           string
	RegisteredState string
	GSTIN           string
	Franchise       string

	PlanName     string
	PlanPrice    decimal.Decimal
	RateLimit    string
	VolumeGB     int
	ValidityDays int

	KYCStatus       string
	AadhaarOnRecord bool
	PANOnRecord     bool
	KYCVerifiedAt   *time.Time

	CreatedAt time.Time
}
