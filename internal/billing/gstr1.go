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
	return derivedGSTRate(r.TaxableValue, r.CGST.Add(r.SGST).Add(r.IGST))
}

// standardGSTRates are the combined rates a supply can legally be taxed at.
// GSTR-1 reports rate by rate and the portal validates against this set, so
// a line carrying anything else is rejected at filing.
var standardGSTRates = []string{"0", "0.25", "3", "5", "12", "18", "28"}

// rateSnapTolerance is how far a derived rate may sit from a standard one
// and still be treated as that rate. Half a percentage point is far wider
// than any rounding error (the worst case here is ~0.01 on a ~100 rupee
// document) and far narrower than the gap between adjacent standard rates,
// the closest of which are 0 and 0.25.
var rateSnapTolerance = decimal.RequireFromString("0.5")

// derivedGSTRate computes the rate a document was taxed at from its own
// amounts, then snaps it to the standard rate it is plainly meant to be.
//
// The snap is not cosmetic. Tax heads are each rounded to the paisa, so the
// ratio of tax to taxable value is only approximately the rate — and the
// smaller the document, the further off it lands. A credit note of 91.38
// carrying 8.22 CGST and 8.22 SGST derives 17.99%, and a GSTR-1 line
// reporting 17.99% is not a rounding nit, it is a rejected return. Larger
// invoices hide this: 799.00 at 71.91 twice comes to exactly 18.
//
// Anything further out than the tolerance is returned as derived rather than
// forced onto the nearest legal value. A document genuinely taxed at some
// other rate is a real problem, and quietly relabelling it 18% would file a
// figure nobody could reconcile back to the invoice.
func derivedGSTRate(taxableValue, tax decimal.Decimal) decimal.Decimal {
	if taxableValue.IsZero() {
		return decimal.Zero
	}
	derived := tax.Div(taxableValue).Mul(decimal.NewFromInt(100)).Round(2)
	for _, s := range standardGSTRates {
		std := decimal.RequireFromString(s)
		if derived.Sub(std).Abs().LessThanOrEqual(rateSnapTolerance) {
			return std
		}
	}
	return derived
}

// InvoiceNumber renders the human-facing number for an invoice id, in the
// same form the PDF and the API use.
func InvoiceNumber(invoiceID int) string { return fmt.Sprintf("INV-%06d", invoiceID) }

// CreditNoteNumber renders the human-facing number for a credit note id,
// in the same shape InvoiceNumber uses. A distinct prefix because GSTR-1
// reports notes and invoices in separate tables and a filer must never have
// to guess which document a number refers to.
func CreditNoteNumber(creditNoteID int) string { return fmt.Sprintf("CRN-%06d", creditNoteID) }

// CreditNoteRow is one issued credit note as the return builder consumes it.
//
// Amounts are held positive, as they are stored, and negated only where the
// return needs them to reduce a total. GSTR-1's own tables report note values
// positively and the portal applies the sign, so carrying them positive here
// keeps the rendered document matching what gets filed.
type CreditNoteRow struct {
	CreditNoteID int
	// OriginalInvoiceID and OriginalInvoiceDate are mandatory on a note:
	// Table 9B identifies which document is being adjusted, and a note
	// without that reference cannot be filed.
	OriginalInvoiceID   int
	OriginalInvoiceDate time.Time
	NoteDate            time.Time
	SubscriberID        int
	SubscriberName      string
	RecipientGSTIN      string
	RecipientState      string
	TaxableValue        decimal.Decimal
	CGST                decimal.Decimal
	SGST                decimal.Decimal
	IGST                decimal.Decimal
	Total               decimal.Decimal
}

// IsB2B mirrors InvoiceRow.IsB2B: a note follows the registration status of
// the supply it adjusts, which is what separates CDNR from CDNUR.
func (c CreditNoteRow) IsB2B() bool { return c.RecipientGSTIN != "" }

// Rate derives the note's GST percentage from its own amounts, for the same
// reason InvoiceRow.Rate does: a note adjusts a supply taxed at the rate in
// force then, not the rate in force now.
func (c CreditNoteRow) Rate() decimal.Decimal {
	return derivedGSTRate(c.TaxableValue, c.CGST.Add(c.SGST).Add(c.IGST))
}

// CDNLine is one credit note in GSTR-1's Table 9B (CDNR where the recipient
// is registered, CDNUR where they are not).
//
// Reported per note rather than aggregated, both kinds alike: a note has to
// name the invoice it adjusts, which aggregation would destroy.
type CDNLine struct {
	NoteNumber        string
	NoteDate          time.Time
	OriginalInvoice   string
	OriginalDate      time.Time
	RecipientGSTIN    string
	RecipientName     string
	PlaceOfSupply     string
	PlaceOfSupplyName string
	Rate              decimal.Decimal
	TaxableValue      decimal.Decimal
	CGST              decimal.Decimal
	SGST              decimal.Decimal
	IGST              decimal.Decimal
	Total             decimal.Decimal
	// Registered separates CDNR from CDNUR at render time without the
	// renderer having to re-derive it from the GSTIN.
	Registered bool
}

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
//
// Net of credit notes, deliberately. These are the numbers someone reconciles
// against the ledger and the bank, and a total that ignored notes would
// overstate the month's liability by exactly the amount credited — which is
// the discrepancy the totals exist to rule out.
type Totals struct {
	Invoices     int
	CreditNotes  int
	TaxableValue decimal.Decimal
	CGST         decimal.Decimal
	SGST         decimal.Decimal
	IGST         decimal.Decimal
	Total        decimal.Decimal
}

// Return is one month's outward supplies, ready to render.
type Return struct {
	Period      Period
	Supplier    Supplier
	B2B         []B2BLine
	B2C         []B2CLine
	CreditNotes []CDNLine
	HSN         []HSNLine
	Totals      Totals
}

// BuildReturn assembles a return from the invoices and credit notes issued in
// a period.
//
// Notes are a separate argument rather than negative invoices because GSTR-1
// reports them in their own table, keyed to the invoice they adjust. Folding
// them into rows would file them as supplies of negative value, which the
// portal rejects, and would lose the original-invoice reference entirely.
func BuildReturn(period Period, supplier Supplier, rows []InvoiceRow, notes []CreditNoteRow) Return {
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

	// Credit notes: listed in their own table, and subtracted from the HSN
	// summary and the totals.
	//
	// The subtraction is the part that matters. HSN reports the period's net
	// outward supply of the service, so a note that appeared only in Table 9B
	// would leave HSN claiming supply that has been credited back, and the
	// return would disagree with itself between two of its own sections.
	for _, note := range notes {
		gstCode, _ := GSTCodeFor(note.RecipientState)
		stateName, _ := StateName(note.RecipientState)

		ret.CreditNotes = append(ret.CreditNotes, CDNLine{
			NoteNumber:        CreditNoteNumber(note.CreditNoteID),
			NoteDate:          note.NoteDate,
			OriginalInvoice:   InvoiceNumber(note.OriginalInvoiceID),
			OriginalDate:      note.OriginalInvoiceDate,
			RecipientGSTIN:    note.RecipientGSTIN,
			RecipientName:     note.SubscriberName,
			PlaceOfSupply:     gstCode,
			PlaceOfSupplyName: stateName,
			Rate:              note.Rate(),
			TaxableValue:      note.TaxableValue,
			CGST:              note.CGST,
			SGST:              note.SGST,
			IGST:              note.IGST,
			Total:             note.Total,
			Registered:        note.IsB2B(),
		})

		// Quantity is not decremented: a credit note adjusts the value of a
		// supply that was made, it does not un-make it. Counting it as
		// negative quantity would report fewer services delivered than were.
		hsn.TaxableValue = hsn.TaxableValue.Sub(note.TaxableValue)
		hsn.CGST = hsn.CGST.Sub(note.CGST)
		hsn.SGST = hsn.SGST.Sub(note.SGST)
		hsn.IGST = hsn.IGST.Sub(note.IGST)
		hsn.TotalValue = hsn.TotalValue.Sub(note.Total)

		ret.Totals.CreditNotes++
		ret.Totals.TaxableValue = ret.Totals.TaxableValue.Sub(note.TaxableValue)
		ret.Totals.CGST = ret.Totals.CGST.Sub(note.CGST)
		ret.Totals.SGST = ret.Totals.SGST.Sub(note.SGST)
		ret.Totals.IGST = ret.Totals.IGST.Sub(note.IGST)
		ret.Totals.Total = ret.Totals.Total.Sub(note.Total)
	}

	// Sorted so two runs over the same month produce byte-identical
	// output. An accountant diffing a re-export against the copy they
	// filed should see nothing, not a reshuffle.
	sort.Slice(ret.CreditNotes, func(i, j int) bool {
		return ret.CreditNotes[i].NoteNumber < ret.CreditNotes[j].NoteNumber
	})
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

	// A month with nothing in it gets no HSN row rather than a row of
	// zeroes, which would otherwise read as a filed nil return. Credit notes
	// count as something: a month whose only activity was crediting an
	// earlier invoice still has a net supply to report, negative though it is.
	if ret.Totals.Invoices > 0 || ret.Totals.CreditNotes > 0 {
		hsn.HSN = HSNInternetServices
		hsn.Description = HSNDescription
		hsn.UQC = UQCOther
		ret.HSN = []HSNLine{hsn}
	}
	return ret
}
