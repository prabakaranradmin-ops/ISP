package billing

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// intrastate is the 799.00 invoice seed_local.sql raises: 9% + 9%.
var intrastate = InvoiceAmounts{
	Base: d("799.00"), CGST: d("71.91"), SGST: d("71.91"), IGST: d("0"), Total: d("942.82"),
	CGSTRate: d("9"), SGSTRate: d("9"), IGSTRate: d("18"),
}

var interstate = InvoiceAmounts{
	Base: d("500.00"), IGST: d("90.00"), CGST: d("0"), SGST: d("0"), Total: d("590.00"),
	CGSTRate: d("9"), SGSTRate: d("9"), IGSTRate: d("18"),
}

func eqAmounts(t *testing.T, name string, got CreditNoteAmounts, base, cgst, sgst, igst, total string) {
	t.Helper()
	want := CreditNoteAmounts{Base: d(base), CGST: d(cgst), SGST: d(sgst), IGST: d(igst), Total: d(total)}
	if !got.Base.Equal(want.Base) || !got.CGST.Equal(want.CGST) || !got.SGST.Equal(want.SGST) ||
		!got.IGST.Equal(want.IGST) || !got.Total.Equal(want.Total) {
		t.Errorf("%s: want %v/%v/%v/%v=%v, got %v/%v/%v/%v=%v", name,
			want.Base, want.CGST, want.SGST, want.IGST, want.Total,
			got.Base, got.CGST, got.SGST, got.IGST, got.Total)
	}
}

func TestComputeCreditNote(t *testing.T) {
	none := CreditNoteAmounts{}

	t.Run("partial intrastate credit uses the invoice's own split", func(t *testing.T) {
		got, err := ComputeCreditNote(intrastate, none, d("100"))
		if err != nil {
			t.Fatal(err)
		}
		eqAmounts(t, "100 of 799", got, "100.00", "9.00", "9.00", "0", "118.00")
	})

	t.Run("interstate invoice is credited in IGST, whatever the subscriber's state now", func(t *testing.T) {
		got, _ := ComputeCreditNote(interstate, none, d("50"))
		eqAmounts(t, "50 of 500", got, "50.00", "0", "0", "9.00", "59.00")
	})

	t.Run("crediting what remains takes the remaining heads exactly", func(t *testing.T) {
		// 333.33 + 465.67 = 799.00. Tax computed piecewise would be 30.00 +
		// 41.91 = 71.91 here, but the point is the last piece is not
		// recomputed at all: it is whatever is left, to the paisa.
		first, _ := ComputeCreditNote(intrastate, none, d("333.33"))
		eqAmounts(t, "first piece", first, "333.33", "30.00", "30.00", "0", "393.33")
		rest, err := ComputeCreditNote(intrastate, first, d("465.67"))
		if err != nil {
			t.Fatal(err)
		}
		eqAmounts(t, "remainder", rest, "465.67", "41.91", "41.91", "0", "549.49")
		sum := first.Total.Add(rest.Total)
		if !sum.Equal(intrastate.Total) {
			t.Errorf("two credits should sum to the invoice total %v, got %v", intrastate.Total, sum)
		}
	})

	t.Run("a split whose pieces round apart still sums to the invoice", func(t *testing.T) {
		// 0.50 x 9% = 0.045 and 798.50 x 9% = 71.865: each lands exactly on
		// half a paisa, so recomputing both would round them to 0.05 + 71.87
		// = 71.92 — a paisa over the 71.91 on the invoice, which the
		// migration 049 trigger would then refuse. Taking the remainder as
		// it stands is what keeps the pair exact.
		first, _ := ComputeCreditNote(intrastate, none, d("0.50"))
		rest, err := ComputeCreditNote(intrastate, first, d("798.50"))
		if err != nil {
			t.Fatal(err)
		}
		if got := first.CGST.Add(rest.CGST); !got.Equal(intrastate.CGST) {
			t.Errorf("CGST across both credits: want %v, got %v", intrastate.CGST, got)
		}
		if got := first.Total.Add(rest.Total); !got.Equal(intrastate.Total) {
			t.Errorf("total across both credits: want %v, got %v", intrastate.Total, got)
		}
	})

	t.Run("the whole invoice at once equals the invoice", func(t *testing.T) {
		got, _ := ComputeCreditNote(intrastate, none, d("799.00"))
		eqAmounts(t, "full", got, "799.00", "71.91", "71.91", "0", "942.82")
	})

	t.Run("a credit beyond what remains is refused", func(t *testing.T) {
		first, _ := ComputeCreditNote(intrastate, none, d("700"))
		if _, err := ComputeCreditNote(intrastate, first, d("99.01")); !errors.Is(err, ErrCreditExceedsInvoice) {
			t.Errorf("want ErrCreditExceedsInvoice, got %v", err)
		}
	})

	t.Run("zero and negative are refused", func(t *testing.T) {
		for _, b := range []string{"0", "-5", "0.001"} {
			if _, err := ComputeCreditNote(intrastate, none, d(b)); !errors.Is(err, ErrCreditNotPositive) {
				t.Errorf("%s: want ErrCreditNotPositive, got %v", b, err)
			}
		}
	})

	t.Run("an untaxed invoice gets an untaxed credit", func(t *testing.T) {
		untaxed := InvoiceAmounts{Base: d("200"), Total: d("200"), CGSTRate: d("9"), SGSTRate: d("9"), IGSTRate: d("18")}
		got, _ := ComputeCreditNote(untaxed, none, d("50"))
		eqAmounts(t, "untaxed", got, "50.00", "0", "0", "0", "50.00")
	})

	t.Run("rounded tax never exceeds the tax left", func(t *testing.T) {
		// Credited so far: 798.99 base, all but 0.01 of the tax. A 0.01
		// credit that is not the exact remainder must not create tax.
		almost := CreditNoteAmounts{Base: d("798.98"), CGST: d("71.91"), SGST: d("71.91")}
		got, err := ComputeCreditNote(intrastate, almost, d("0.01"))
		if err != nil {
			t.Fatal(err)
		}
		if got.CGST.IsPositive() || got.SGST.IsPositive() {
			t.Errorf("no tax is left to credit, got %v/%v", got.CGST, got.SGST)
		}
	})
}
