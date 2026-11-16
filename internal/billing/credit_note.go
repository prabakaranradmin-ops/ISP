package billing

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// Credit-note arithmetic — FR-BIL-012.
//
// A credit note corrects one invoice, so its tax follows that invoice: the
// same heads (CGST+SGST for an intrastate supply, IGST for interstate, none
// for an untaxed one) at the same rates. Recomputing the head from the
// subscriber's current state instead would let a subscriber who moved state
// receive a credit note in a different head from the invoice it reverses,
// which no return reconciles.

// InvoiceAmounts is an invoice's own figures and the rates it was raised at.
type InvoiceAmounts struct {
	Base, CGST, SGST, IGST, Total decimal.Decimal
	CGSTRate, SGSTRate, IGSTRate  decimal.Decimal
}

// CreditNoteAmounts is one credit note's figures.
type CreditNoteAmounts struct {
	Base, CGST, SGST, IGST, Total decimal.Decimal
}

// Tax is the credit note's total GST.
func (c CreditNoteAmounts) Tax() decimal.Decimal { return c.CGST.Add(c.SGST).Add(c.IGST) }

var (
	// ErrCreditNotPositive is returned for a zero or negative credit.
	ErrCreditNotPositive = errors.New("billing: the amount to credit must be more than zero")
	// ErrCreditExceedsInvoice is returned when the credit would take the
	// invoice's taxable value below zero, counting credit notes already
	// issued against it.
	ErrCreditExceedsInvoice = errors.New("billing: the credit exceeds what remains on the invoice")
)

// ComputeCreditNote returns the credit note for base of taxable value
// against inv, given what has already been credited on it.
//
// Crediting exactly what remains takes each remaining head as it stands
// rather than recomputing it, so a series of partial credits that sums to
// the whole invoice brings every head to exactly zero. Recomputing tax on
// the last piece could leave a paisa on the invoice that nothing can ever
// credit, because the trigger in migration 049 would refuse the extra paisa.
func ComputeCreditNote(inv InvoiceAmounts, credited CreditNoteAmounts, base decimal.Decimal) (CreditNoteAmounts, error) {
	base = base.Round(2)
	if !base.IsPositive() {
		return CreditNoteAmounts{}, ErrCreditNotPositive
	}
	remaining := CreditNoteAmounts{
		Base: inv.Base.Sub(credited.Base),
		CGST: inv.CGST.Sub(credited.CGST),
		SGST: inv.SGST.Sub(credited.SGST),
		IGST: inv.IGST.Sub(credited.IGST),
	}
	if base.GreaterThan(remaining.Base) {
		return CreditNoteAmounts{}, fmt.Errorf("%w: %s requested, %s left", ErrCreditExceedsInvoice, base.StringFixed(2), remaining.Base.StringFixed(2))
	}
	if base.Equal(remaining.Base) {
		remaining.Total = remaining.Base.Add(remaining.Tax())
		return remaining, nil
	}

	hundred := decimal.NewFromInt(100)
	out := CreditNoteAmounts{Base: base}
	switch {
	case inv.IGST.IsPositive():
		out.IGST = base.Mul(inv.IGSTRate).Div(hundred).Round(2)
	case inv.CGST.IsPositive() || inv.SGST.IsPositive():
		out.CGST = base.Mul(inv.CGSTRate).Div(hundred).Round(2)
		out.SGST = base.Mul(inv.SGSTRate).Div(hundred).Round(2)
	}
	// A partial credit's rounded tax can never be allowed to exceed the tax
	// left on the invoice, or the final credit would have a negative head.
	out.CGST = decimal.Min(out.CGST, remaining.CGST)
	out.SGST = decimal.Min(out.SGST, remaining.SGST)
	out.IGST = decimal.Min(out.IGST, remaining.IGST)
	out.Total = out.Base.Add(out.Tax())
	return out, nil
}

// IssuedCreditNote is a credit note as written, with the wallet movement
// that returned its value.
type IssuedCreditNote struct {
	ID             int
	InvoiceID      int
	SubscriberID   int
	Amounts        CreditNoteAmounts
	WalletLedgerID int
	NewBalance     decimal.Decimal
}

// Number is the credit note's human-facing number (CRN-000042).
func (c IssuedCreditNote) Number() string { return CreditNoteNumber(c.ID) }
