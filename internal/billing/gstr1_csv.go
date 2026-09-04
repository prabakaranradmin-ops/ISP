package billing

import (
	"encoding/csv"
	"fmt"
	"io"

	"github.com/shopspring/decimal"
)

// Accountant-facing CSV rendering of a GSTR-1 return — FR-BIL-006.
//
// This is the Phase 1 format: one file an accountant opens, reads and
// reconciles, with the three sections laid out under headings rather than
// split across files. It is deliberately not the GST portal's
// offline-utility import format, which is a separate renderer over the
// same Return - see gstr1.go on why the aggregation lives there rather
// than here.
//
// Amounts are written as fixed-2dp strings, never floats: these are the
// figures a return is filed on, and a spreadsheet that reads 71.91 as
// 71.909999 would be reconciling against the wrong number.

// money renders an amount the way every line of this file must.
func money(d decimal.Decimal) string { return d.StringFixed(2) }

// WriteAccountantCSV renders a return as a single reviewable CSV.
func WriteAccountantCSV(w io.Writer, ret Return) error {
	cw := csv.NewWriter(w)

	write := func(rec ...string) error { return cw.Write(rec) }
	blank := func() error { return cw.Write([]string{}) }

	// Header block: who filed, for what month. An export that arrives
	// without this is unusable a month later, when nobody remembers which
	// period it covered.
	for _, rec := range [][]string{
		{"GSTR-1 outward supplies"},
		{"Supplier", ret.Supplier.Name},
		{"GSTIN", ret.Supplier.GSTIN},
		{"Home state", ret.Supplier.State},
		{"Period (MMYYYY)", ret.Period.String()},
	} {
		if err := write(rec...); err != nil {
			return err
		}
	}
	if err := blank(); err != nil {
		return err
	}

	// ── B2B ─────────────────────────────────────────────────────────────
	if err := write("B2B - supplies to registered persons (Table 4)"); err != nil {
		return err
	}
	if err := write("Invoice number", "Invoice date", "Recipient GSTIN", "Recipient name",
		"Place of supply", "State", "Rate %", "Taxable value", "CGST", "SGST", "IGST", "Invoice total"); err != nil {
		return err
	}
	if len(ret.B2B) == 0 {
		if err := write("(none)"); err != nil {
			return err
		}
	}
	for _, l := range ret.B2B {
		if err := write(
			l.InvoiceNumber, l.InvoiceDate.Format("02-01-2006"), l.RecipientGSTIN, l.RecipientName,
			l.PlaceOfSupply, l.PlaceOfSupplyName, money(l.Rate),
			money(l.TaxableValue), money(l.CGST), money(l.SGST), money(l.IGST), money(l.Total),
		); err != nil {
			return err
		}
	}
	if err := blank(); err != nil {
		return err
	}

	// ── B2C ─────────────────────────────────────────────────────────────
	if err := write("B2C - supplies to unregistered persons, aggregated by state and rate"); err != nil {
		return err
	}
	if err := write("Place of supply", "State", "Rate %", "Invoices",
		"Taxable value", "CGST", "SGST", "IGST", "Total"); err != nil {
		return err
	}
	if len(ret.B2C) == 0 {
		if err := write("(none)"); err != nil {
			return err
		}
	}
	for _, l := range ret.B2C {
		if err := write(
			l.PlaceOfSupply, l.PlaceOfSupplyName, money(l.Rate), fmt.Sprintf("%d", l.InvoiceCount),
			money(l.TaxableValue), money(l.CGST), money(l.SGST), money(l.IGST), money(l.Total),
		); err != nil {
			return err
		}
	}
	if err := blank(); err != nil {
		return err
	}

	// ── Credit notes ────────────────────────────────────────────────────
	//
	// One table covering both CDNR and CDNUR, with a column saying which,
	// rather than two nearly identical blocks: the accountant is reading a
	// worksheet, and every note carries the same fields. The portal's own
	// import splits them, which is a concern for that renderer, not this one.
	if err := write("Credit notes (Table 9B) - reduce the supplies above"); err != nil {
		return err
	}
	if err := write("Note number", "Note date", "Original invoice", "Original date",
		"Recipient type", "Recipient GSTIN", "Recipient name", "Place of supply", "State",
		"Rate %", "Taxable value", "CGST", "SGST", "IGST", "Note total"); err != nil {
		return err
	}
	if len(ret.CreditNotes) == 0 {
		if err := write("(none)"); err != nil {
			return err
		}
	}
	for _, l := range ret.CreditNotes {
		kind := "CDNUR (unregistered)"
		if l.Registered {
			kind = "CDNR (registered)"
		}
		if err := write(
			l.NoteNumber, l.NoteDate.Format("02-01-2006"),
			l.OriginalInvoice, l.OriginalDate.Format("02-01-2006"),
			kind, l.RecipientGSTIN, l.RecipientName,
			l.PlaceOfSupply, l.PlaceOfSupplyName, money(l.Rate),
			money(l.TaxableValue), money(l.CGST), money(l.SGST), money(l.IGST), money(l.Total),
		); err != nil {
			return err
		}
	}
	if err := blank(); err != nil {
		return err
	}

	// ── HSN ─────────────────────────────────────────────────────────────
	if err := write("HSN summary (Table 12), net of credit notes"); err != nil {
		return err
	}
	if err := write("HSN/SAC", "Description", "UQC", "Quantity",
		"Total value", "Taxable value", "CGST", "SGST", "IGST"); err != nil {
		return err
	}
	if len(ret.HSN) == 0 {
		if err := write("(none)"); err != nil {
			return err
		}
	}
	for _, l := range ret.HSN {
		if err := write(
			l.HSN, l.Description, l.UQC, money(l.Quantity),
			money(l.TotalValue), money(l.TaxableValue), money(l.CGST), money(l.SGST), money(l.IGST),
		); err != nil {
			return err
		}
	}
	if err := blank(); err != nil {
		return err
	}

	// ── Totals ──────────────────────────────────────────────────────────
	//
	// Present so the accountant can tie the return to the books in one
	// glance. B2B plus B2C less the credit notes must equal these, and the
	// HSN summary must equal them too - three routes to the same figure,
	// which is the point of reproducing it.
	//
	// Net of credit notes, and the header says so: a total that silently
	// included or excluded them would be checked against the ledger, appear
	// to disagree, and send someone hunting for a discrepancy that is only a
	// difference in what the column means.
	if err := write("Totals (net of credit notes)"); err != nil {
		return err
	}
	if err := write("Invoices", "Credit notes", "Taxable value", "CGST", "SGST", "IGST", "Total"); err != nil {
		return err
	}
	if err := write(
		fmt.Sprintf("%d", ret.Totals.Invoices), fmt.Sprintf("%d", ret.Totals.CreditNotes),
		money(ret.Totals.TaxableValue),
		money(ret.Totals.CGST), money(ret.Totals.SGST), money(ret.Totals.IGST), money(ret.Totals.Total),
	); err != nil {
		return err
	}

	cw.Flush()
	return cw.Error()
}
