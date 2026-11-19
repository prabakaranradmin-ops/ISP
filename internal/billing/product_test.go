package billing

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

// Product catalogue and per-line GST — FR-BIL-022.

// rate18 is the ordinary 9/9/18 Indian GST rate.
func rate18() GstRate {
	return GstRate{ID: 7, CgstRate: d("9"), SgstRate: d("9"), IgstRate: d("18")}
}

func TestFR_BIL_022_ProductRules(t *testing.T) {
	valid := Product{Code: "install", Name: "  Installation   charge ", Kind: ProductOneTime,
		Price: d("1500"), HSNSAC: "998432"}

	got, err := valid.Normalise()
	if err != nil {
		t.Fatalf("valid product rejected: %v", err)
	}
	if got.Code != "INSTALL" {
		t.Errorf("code not upper-cased: %q", got.Code)
	}
	if got.Name != "Installation charge" {
		t.Errorf("name not collapsed: %q", got.Name)
	}

	cases := []struct {
		name string
		mut  func(*Product)
	}{
		{"no code", func(p *Product) { p.Code = "" }},
		{"one-character code", func(p *Product) { p.Code = "X" }},
		{"code with a space", func(p *Product) { p.Code = "STATIC IP" }},
		{"code starting with a hyphen", func(p *Product) { p.Code = "-IP" }},
		{"no name", func(p *Product) { p.Name = "   " }},
		{"unknown kind", func(p *Product) { p.Kind = "monthly" }},
		{"no kind", func(p *Product) { p.Kind = "" }},
		{"zero price", func(p *Product) { p.Price = decimal.Zero }},
		{"negative price", func(p *Product) { p.Price = d("-1") }},
		{"three decimal places", func(p *Product) { p.Price = d("99.999") }},
		{"price over the cap", func(p *Product) { p.Price = ProductMaxPrice.Add(d("0.01")) }},
		{"no HSN/SAC", func(p *Product) { p.HSNSAC = "" }},
		{"short HSN/SAC", func(p *Product) { p.HSNSAC = "998" }},
		{"non-numeric HSN/SAC", func(p *Product) { p.HSNSAC = "99843A" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mut(&p)
			if _, err := p.Normalise(); !errors.Is(err, ErrProductInvalid) {
				t.Fatalf("want ErrProductInvalid, got %v", err)
			} else if ProductInvalidReason(err) == "" {
				t.Error("no operator-facing reason on the error")
			}
		})
	}
}

func TestFR_BIL_022_AttachRequestRules(t *testing.T) {
	base := AttachRequest{SubscriberID: 5, ProductID: 9}

	got, err := base.Normalise()
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if got.Quantity != 1 {
		t.Errorf("quantity not defaulted to 1: %d", got.Quantity)
	}

	cases := []struct {
		name string
		mut  func(*AttachRequest)
	}{
		{"no subscriber", func(r *AttachRequest) { r.SubscriberID = 0 }},
		{"no product", func(r *AttachRequest) { r.ProductID = 0 }},
		{"negative quantity", func(r *AttachRequest) { r.Quantity = -2 }},
		{"quantity over the cap", func(r *AttachRequest) { r.Quantity = ProductMaxQuantity + 1 }},
		{"negative override", func(r *AttachRequest) { r.UnitPrice = d("-5") }},
		{"override with three decimals", func(r *AttachRequest) { r.UnitPrice = d("5.005") }},
		{"override over the cap", func(r *AttachRequest) { r.UnitPrice = ProductMaxPrice.Add(d("1")) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mut(&r)
			if _, err := r.Normalise(); !errors.Is(err, ErrProductInvalid) {
				t.Fatalf("want ErrProductInvalid, got %v", err)
			}
		})
	}
}

// A plan-only invoice must come out byte-for-byte as the single-amount
// calculation produced before lines existed. This is what keeps invoices
// raised before FR-BIL-022 comparable with the ones after it, and it is
// the regression this whole change could most easily cause.
func TestFR_BIL_022_PlanOnlyInvoiceMatchesSingleAmountCalculation(t *testing.T) {
	rate := rate18()
	price := d("763.57") // deliberately not round: 9% of it is 68.7213

	for _, state := range []string{"TN", "KA"} {
		t.Run(state, func(t *testing.T) {
			want := CalculateGstInvoiceFrom(price, state, "TN", rate)

			lines, hdr, err := ComposeInvoice(
				[]LineCharge{PlanCharge("Fibre 100", price)}, state, "TN", rate)
			if err != nil {
				t.Fatalf("ComposeInvoice: %v", err)
			}
			if len(lines) != 1 {
				t.Fatalf("want 1 line, got %d", len(lines))
			}
			if !hdr.BaseAmount.Equal(want.BaseAmount) ||
				!hdr.CgstAmount.Equal(want.CgstAmount) ||
				!hdr.SgstAmount.Equal(want.SgstAmount) ||
				!hdr.IgstAmount.Equal(want.IgstAmount) ||
				!hdr.TotalAmount.Equal(want.TotalAmount) {
				t.Errorf("header %+v, want %+v", hdr, want)
			}
			if lines[0].Kind != LineKindPlan || lines[0].HSNSAC != PlanHSNSAC {
				t.Errorf("plan line is %s / %s", lines[0].Kind, lines[0].HSNSAC)
			}
			if lines[0].ProductID != nil {
				t.Error("plan line names a product")
			}
		})
	}
}

// The header is the sum of the lines, to the paisa, for both tax regimes.
//
// These three prices are picked so that per-line rounding and
// whole-invoice rounding genuinely disagree under *both* 9% and 18% —
// otherwise the test would pass whichever order the code taxed in, and
// would prove nothing. Bases are 918.04, 272.76 (3 × 90.92) and 100.15,
// summing to 1290.95:
//
//	 9%: per-line 116.18, taxing the total 116.19
//	18%: per-line 232.38, taxing the total 232.37
//
// Note they diverge in opposite directions, so neither regime is merely
// inheriting the other's luck. Confirmed by a negative control: replacing
// the per-line sum with tax on the total fails this test in both regimes.
func TestFR_BIL_022_HeaderEqualsSumOfLines(t *testing.T) {
	rate := rate18()
	router, staticIP := 11, 12
	charges := []LineCharge{
		PlanCharge("Fibre 100", d("918.04")),
		{Kind: LineKindProduct, Description: "Router rental", HSNSAC: "8517",
			ProductID: &router, Quantity: 3, UnitPrice: d("90.92")},
		{Kind: LineKindProduct, Description: "Static IP", HSNSAC: "998432",
			ProductID: &staticIP, Quantity: 1, UnitPrice: d("100.15")},
	}

	for _, state := range []string{"TN", "KA"} {
		t.Run(state, func(t *testing.T) {
			lines, hdr, err := ComposeInvoice(charges, state, "TN", rate)
			if err != nil {
				t.Fatalf("ComposeInvoice: %v", err)
			}
			if len(lines) != 3 {
				t.Fatalf("want 3 lines, got %d", len(lines))
			}

			base, cgst, sgst, igst, total := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
			for i, l := range lines {
				if l.LineNo != i+1 {
					t.Errorf("line %d numbered %d", i, l.LineNo)
				}
				// Each line stands on its own: its total is its base plus
				// its own tax.
				if !l.TotalAmount.Equal(l.BaseAmount.Add(l.Tax())) {
					t.Errorf("line %d does not add up: %+v", l.LineNo, l)
				}
				base = base.Add(l.BaseAmount)
				cgst = cgst.Add(l.CGSTAmount)
				sgst = sgst.Add(l.SGSTAmount)
				igst = igst.Add(l.IGSTAmount)
				total = total.Add(l.TotalAmount)
			}

			if !hdr.BaseAmount.Equal(base) || !hdr.CgstAmount.Equal(cgst) ||
				!hdr.SgstAmount.Equal(sgst) || !hdr.IgstAmount.Equal(igst) ||
				!hdr.TotalAmount.Equal(total) {
				t.Errorf("header does not equal the sum of its lines:\n header %+v\n lines  base=%s cgst=%s sgst=%s igst=%s total=%s",
					hdr, base, cgst, sgst, igst, total)
			}
			if !hdr.TotalAmount.Equal(hdr.BaseAmount.Add(cgst).Add(sgst).Add(igst)) {
				t.Errorf("header total %s is not base plus tax", hdr.TotalAmount)
			}
			// Quantity is multiplied into the base, not left on the side.
			if !lines[1].BaseAmount.Equal(d("272.76")) {
				t.Errorf("3 × 90.92 came to %s, want 272.76", lines[1].BaseAmount)
			}
			if !base.Equal(d("1290.95")) {
				t.Errorf("total base %s, want 1290.95 — the divergence this test relies on is gone", base)
			}
		})
	}
}

// One tax regime or the other, per line — migration 058's
// chk_invoice_line_gst_logic, checked in code so a bad line is caught
// before the database has to reject the whole transaction.
func TestFR_BIL_022_LinesCarryOneTaxRegime(t *testing.T) {
	rate := rate18()
	charges := []LineCharge{PlanCharge("Fibre 100", d("500")),
		{Kind: LineKindOneOff, Description: "Relocation", HSNSAC: "998432", Quantity: 1, UnitPrice: d("250")}}

	t.Run("intrastate is CGST and SGST only", func(t *testing.T) {
		lines, _, err := ComposeInvoice(charges, "TN", "TN", rate)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			if !l.IGSTAmount.IsZero() || l.CGSTAmount.IsZero() || l.SGSTAmount.IsZero() {
				t.Errorf("line %d: cgst=%s sgst=%s igst=%s", l.LineNo, l.CGSTAmount, l.SGSTAmount, l.IGSTAmount)
			}
		}
	})
	t.Run("interstate is IGST only", func(t *testing.T) {
		lines, _, err := ComposeInvoice(charges, "KA", "TN", rate)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			if l.IGSTAmount.IsZero() || !l.CGSTAmount.IsZero() || !l.SGSTAmount.IsZero() {
				t.Errorf("line %d: cgst=%s sgst=%s igst=%s", l.LineNo, l.CGSTAmount, l.SGSTAmount, l.IGSTAmount)
			}
		}
	})
}

func TestFR_BIL_022_ComposeInvoiceRefusesBadCharges(t *testing.T) {
	rate := rate18()
	ok := PlanCharge("Fibre 100", d("500"))

	t.Run("no charges at all", func(t *testing.T) {
		if _, _, err := ComposeInvoice(nil, "TN", "TN", rate); !errors.Is(err, ErrNoInvoiceLines) {
			t.Fatalf("want ErrNoInvoiceLines, got %v", err)
		}
	})

	cases := []struct {
		name string
		bad  LineCharge
	}{
		{"zero price", LineCharge{Kind: LineKindOneOff, Description: "x", HSNSAC: "998432", Quantity: 1, UnitPrice: decimal.Zero}},
		{"negative price", LineCharge{Kind: LineKindOneOff, Description: "x", HSNSAC: "998432", Quantity: 1, UnitPrice: d("-5")}},
		{"zero quantity", LineCharge{Kind: LineKindOneOff, Description: "x", HSNSAC: "998432", Quantity: 0, UnitPrice: d("5")}},
		{"quantity over the cap", LineCharge{Kind: LineKindOneOff, Description: "x", HSNSAC: "998432", Quantity: 1000, UnitPrice: d("5")}},
		{"blank description", LineCharge{Kind: LineKindOneOff, Description: "  ", HSNSAC: "998432", Quantity: 1, UnitPrice: d("5")}},
		{"missing HSN/SAC", LineCharge{Kind: LineKindOneOff, Description: "x", HSNSAC: "", Quantity: 1, UnitPrice: d("5")}},
		{"malformed HSN/SAC", LineCharge{Kind: LineKindOneOff, Description: "x", HSNSAC: "99", Quantity: 1, UnitPrice: d("5")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ComposeInvoice([]LineCharge{ok, tc.bad}, "TN", "TN", rate); err == nil {
				t.Fatal("bad charge accepted")
			}
		})
	}
}

// A product charge carries the attachment it came from, and marks itself
// one-time or not, because that is what lets the store close a one-time
// charge in the same transaction that bills it.
func TestFR_BIL_022_ProductChargeCarriesItsAttachment(t *testing.T) {
	recurring := SubscriberProduct{ID: 42, ProductID: 7, ProductName: "Router rental",
		Kind: ProductRecurring, HSNSAC: "8517", Quantity: 2, UnitPrice: d("100")}
	oneTime := SubscriberProduct{ID: 43, ProductID: 8, ProductName: "Installation",
		Kind: ProductOneTime, HSNSAC: "998432", Quantity: 1, UnitPrice: d("1500")}

	rc := ProductCharge(recurring)
	if rc.AttachmentID == nil || *rc.AttachmentID != 42 {
		t.Errorf("recurring charge lost its attachment: %+v", rc.AttachmentID)
	}
	if rc.ProductID == nil || *rc.ProductID != 7 {
		t.Errorf("recurring charge lost its product: %+v", rc.ProductID)
	}
	if rc.OneTime {
		t.Error("a recurring product was marked one-time")
	}

	oc := ProductCharge(oneTime)
	if !oc.OneTime {
		t.Error("a one-time product was not marked one-time")
	}

	// The attachment pointers must not alias a shared loop variable: two
	// charges built from different attachments have to stay distinct.
	if *rc.AttachmentID == *oc.AttachmentID {
		t.Error("two charges share one attachment id")
	}

	if !recurring.Base().Equal(d("200")) {
		t.Errorf("2 × 100 came to %s", recurring.Base())
	}
}

// A long product name is truncated rather than rejected at the line, since
// invoice_lines.chk_invoice_line_description would otherwise fail the
// whole renewal for a cosmetic reason.
func TestFR_BIL_022_LineDescriptionsFitTheColumn(t *testing.T) {
	long := ""
	for len(long) < LineMaxDescription*2 {
		long += "Ultra-High-Speed Fibre Broadband "
	}

	plan := PlanCharge(long, d("500"))
	if n := len([]rune(plan.Description)); n > LineMaxDescription {
		t.Errorf("plan description is %d runes", n)
	}

	prod := ProductCharge(SubscriberProduct{ID: 1, ProductID: 2, ProductName: long,
		Kind: ProductRecurring, HSNSAC: "8517", Quantity: 1, UnitPrice: d("50")})
	if n := len([]rune(prod.Description)); n > LineMaxDescription {
		t.Errorf("product description is %d runes", n)
	}
}
