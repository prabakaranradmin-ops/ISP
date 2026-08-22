package billing

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// GSTR-1 return construction — FR-BIL-006 | CRD §1.3 | MDS §4.3.
//
// GSTR-1 is the monthly outward-supplies return. This builds the three
// sections an ISP actually needs from its invoices - B2B, B2C and the HSN
// summary - as a structured value, and leaves rendering to a separate
// writer.
//
// The split matters for what comes next. The immediate need is a CSV an
// accountant can work from; the intended next step is the GST portal's
// offline-utility import format. Those differ only in layout, so the
// aggregation lives here once and each format is a renderer over the same
// Return. Putting the arithmetic in a CSV writer would mean doing it twice
// and reconciling two sets of totals.
//
// Aggregation is done in Go rather than SQL for the same reason it is
// testable without a database: every rule below - which supplies are B2B,
// how a rate is derived, what rounds where - can be exercised against a
// literal slice of invoices.

const (
	// HSNInternetServices is the SAC code for internet telecommunication
	// services. Previously written only into the invoice PDF's footer
	// text, which meant the code a customer sees and the code a return
	// reports could drift apart.
	HSNInternetServices = "998432"
	// HSNDescription accompanies the code in the HSN summary.
	HSNDescription = "Internet telecommunication services"
	// UQCOther is the Unit Quantity Code for a service, which has no
	// physical unit. GSTR-1 requires a UQC even where quantity is
	// meaningless; OTH is the designated value.
	UQCOther = "OTH"
)

// Period is one return month.
type Period struct {
	Year  int
	Month time.Month
}

// String renders the period as GSTR-1 writes it: MMYYYY.
func (p Period) String() string { return fmt.Sprintf("%02d%04d", int(p.Month), p.Year) }

// Supplier is the filing entity's own identity.
type Supplier struct {
	GSTIN string
	// State is the canonical two-letter code (see state.go). It decides
	// which supplies are intrastate, and therefore which are CGST+SGST.
	State string
	Name  string
}

// InvoiceRow is one issued invoice as the return builder consumes it.
type InvoiceRow struct {
	InvoiceID    int
	InvoiceDate  time.Time
	SubscriberID int
	// SubscriberName and RecipientGSTIN identify the recipient. An empty
	// GSTIN means an unregistered recipient, which is what makes a supply
	// B2C.
	SubscriberName string
	RecipientGSTIN string
	// RecipientState is the canonical two-letter code.
	RecipientState string
	TaxableValue   decimal.Decimal
	CGST           decimal.Decimal
	SGST           decimal.Decimal
	IGST           decimal.Decimal
	Total          decimal.Decimal
}

// IsB2B reports whether a supply belongs in the B2B section.
//
// The test is solely whether the recipient holds a GSTIN: GSTR-1 defines
// B2B as a supply to a registered person, regardless of value, state or
// what the customer is like. A residential subscriber who happens to run a
// registered business and gave their GSTIN is a B2B supply.
func (r InvoiceRow) IsB2B() bool { return r.RecipientGSTIN != "" }

// Rate returns the combined GST percentage this invoice was charged at,
// derived from the amounts rather than looked up.
//
// Derived deliberately: the rate in force when an invoice was issued is
// what belongs in its return line, and gst_rates rows can be superseded
// afterwards. Reading today's rate would restate an old invoice at a rate
// it was never charged.
func (r InvoiceRow) Rate() decimal.Decimal {
	if r.TaxableValue.IsZero() {
		return decimal.Zero
	}
	tax := r.CGST.Add(r.SGST).Add(r.IGST)
	return tax.Div(r.TaxableValue).Mul(decimal.NewFromInt(100)).Round(2)
}

// InvoiceNumber renders the human-facing number for an invoice id, in the
// same form the PDF and the API use.
func InvoiceNumber(invoiceID int) string { return fmt.Sprintf("INV-%06d", invoiceID) }

// B2BLine is one invoice in the B2B section, reported individually.
type B2BLine struct {
	InvoiceNumber  string
	InvoiceDate    time.Time
	RecipientGSTIN string
	RecipientName  string
	// PlaceOfSupply is the recipient's two-digit GST state code, which is
	// what decides where the tax is due.
	PlaceOfSupply     string
	PlaceOfSupplyName string
	Rate              decimal.Decimal
	TaxableValue      decimal.Decimal
	CGST              decimal.Decimal
	SGST              decimal.Decimal
	IGST              decimal.Decimal
	Total             decimal.Decimal
}

// B2CLine is one state-and-rate bucket in the B2C section.
//
// B2C supplies are reported in aggregate rather than per invoice, which is
// the whole reason the section exists - an ISP issues far too many
// residential invoices to list individually.
type B2CLine struct {
	PlaceOfSupply     string
	PlaceOfSupplyName string
	Rate              decimal.Decimal
	InvoiceCount      int
	TaxableValue      decimal.Decimal
	CGST              decimal.Decimal
	SGST              decimal.Decimal
	IGST              decimal.Decimal
	Total             decimal.Decimal
}

// HSNLine is one row of the HSN summary, which covers every supply in the
// period regardless of whether it was B2B or B2C.
type HSNLine struct {
	HSN          string
	Description  string
	UQC          string
	Quantity     decimal.Decimal
	TotalValue   decimal.Decimal
	TaxableValue decimal.Decimal
	CGST         decimal.Decimal
	SGST         decimal.Decimal
	IGST         decimal.Decimal
}

// Totals are the whole-return figures, for the accountant to tie against
// the books before filing.
type Totals struct {
	Invoices     int
	TaxableValue decimal.Decimal
	CGST         decimal.Decimal
	SGST         decimal.Decimal
	IGST         decimal.Decimal
	Total        decimal.Decimal
}

// Return is one month's outward supplies, ready to render.
type Return struct {
	Period   Period
	Supplier Supplier
	B2B      []B2BLine
	B2C      []B2CLine
	HSN      []HSNLine
	Totals   Totals
}

// BuildReturn assembles a return from the invoices issued in a period.
func BuildReturn(period Period, supplier Supplier, rows []InvoiceRow) Return {
	ret := Return{Period: period, Supplier: supplier}

	// B2C is keyed on place of supply and rate together: the same state at
	// two different rates is two lines, because GSTR-1 reports tax rate by
	// rate.
	type b2cKey struct {
		state string
		rate  string
	}
	b2c := map[b2cKey]*B2CLine{}
	var hsn HSNLine

	for _, row := range rows {
		gstCode, _ := GSTCodeFor(row.RecipientState)
		stateName, _ := StateName(row.RecipientState)
		rate := row.Rate()

		if row.IsB2B() {
			ret.B2B = append(ret.B2B, B2BLine{
				InvoiceNumber:     InvoiceNumber(row.InvoiceID),
				InvoiceDate:       row.InvoiceDate,
				RecipientGSTIN:    row.RecipientGSTIN,
				RecipientName:     row.SubscriberName,
				PlaceOfSupply:     gstCode,
				PlaceOfSupplyName: stateName,
				Rate:              rate,
				TaxableValue:      row.TaxableValue,
				CGST:              row.CGST,
				SGST:              row.SGST,
				IGST:              row.IGST,
				Total:             row.Total,
			})
		} else {
			k := b2cKey{state: gstCode, rate: rate.String()}
			line, ok := b2c[k]
			if !ok {
				line = &B2CLine{
					PlaceOfSupply:     gstCode,
					PlaceOfSupplyName: stateName,
					Rate:              rate,
				}
				b2c[k] = line
			}
			line.InvoiceCount++
			line.TaxableValue = line.TaxableValue.Add(row.TaxableValue)
			line.CGST = line.CGST.Add(row.CGST)
			line.SGST = line.SGST.Add(row.SGST)
			line.IGST = line.IGST.Add(row.IGST)
			line.Total = line.Total.Add(row.Total)
		}

		// The HSN summary spans everything: B2B and B2C alike are supplies
		// of the same service.
		hsn.Quantity = hsn.Quantity.Add(decimal.NewFromInt(1))
		hsn.TaxableValue = hsn.TaxableValue.Add(row.TaxableValue)
		hsn.CGST = hsn.CGST.Add(row.CGST)
		hsn.SGST = hsn.SGST.Add(row.SGST)
		hsn.IGST = hsn.IGST.Add(row.IGST)
		hsn.TotalValue = hsn.TotalValue.Add(row.Total)

		ret.Totals.Invoices++
		ret.Totals.TaxableValue = ret.Totals.TaxableValue.Add(row.TaxableValue)
		ret.Totals.CGST = ret.Totals.CGST.Add(row.CGST)
		ret.Totals.SGST = ret.Totals.SGST.Add(row.SGST)
		ret.Totals.IGST = ret.Totals.IGST.Add(row.IGST)
		ret.Totals.Total = ret.Totals.Total.Add(row.Total)
	}

	// Sorted so two runs over the same month produce byte-identical
	// output. An accountant diffing a re-export against the copy they
	// filed should see nothing, not a reshuffle.
	sort.Slice(ret.B2B, func(i, j int) bool {
		return ret.B2B[i].InvoiceNumber < ret.B2B[j].InvoiceNumber
	})
	for _, line := range b2c {
		ret.B2C = append(ret.B2C, *line)
	}
	sort.Slice(ret.B2C, func(i, j int) bool {
		if ret.B2C[i].PlaceOfSupply != ret.B2C[j].PlaceOfSupply {
			return ret.B2C[i].PlaceOfSupply < ret.B2C[j].PlaceOfSupply
		}
		return ret.B2C[i].Rate.LessThan(ret.B2C[j].Rate)
	})

	// A month with no invoices gets no HSN row rather than a row of
	// zeroes, which would otherwise read as a filed nil return.
	if ret.Totals.Invoices > 0 {
		hsn.HSN = HSNInternetServices
		hsn.Description = HSNDescription
		hsn.UQC = UQCOther
		ret.HSN = []HSNLine{hsn}
	}
	return ret
}
