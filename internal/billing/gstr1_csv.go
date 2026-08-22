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

	// ── HSN ─────────────────────────────────────────────────────────────
	if err := write("HSN summary (Table 12)"); err != nil {
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
	// glance. B2B plus B2C must equal these, and the HSN summary must
	// equal them too - three routes to the same figure, which is the
	// point of reproducing it.
	if err := write("Totals"); err != nil {
		return err
	}
	if err := write("Invoices", "Taxable value", "CGST", "SGST", "IGST", "Total"); err != nil {
		return err
	}
	if err := write(
		fmt.Sprintf("%d", ret.Totals.Invoices), money(ret.Totals.TaxableValue),
		money(ret.Totals.CGST), money(ret.Totals.SGST), money(ret.Totals.IGST), money(ret.Totals.Total),
	); err != nil {
		return err
	}

	cw.Flush()
	return cw.Error()
}
