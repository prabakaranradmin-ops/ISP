package billing

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(i int64) decimal.Decimal { return decimal.NewFromInt(i) }

// The bug this file guards against: registered_state was free text and the
// intra/inter-state decision compared it to the literal "TN", so a Tamil
// Nadu subscriber recorded as "Tamil Nadu" was charged IGST. Same total,
// wrong head - IGST accrues wholly to the centre, CGST/SGST splits with
// the state.

func TestNormaliseState_AcceptsEverySpellingOfOneState(t *testing.T) {
	// Every form a CSR, an import file or an API caller might plausibly
	// supply for one state must land on one code.
	for _, in := range []string{"TN", "tn", "  TN  ", "33", "Tamil Nadu", "tamil nadu", "TAMILNADU", "TamilNadu"} {
		got, ok := NormaliseState(in)
		if !ok {
			t.Errorf("NormaliseState(%q): not recognised", in)
			continue
		}
		if got != "TN" {
			t.Errorf("NormaliseState(%q) = %q, want TN", in, got)
		}
	}
}

func TestNormaliseState_RejectsUnknown(t *testing.T) {
	for _, in := range []string{"", "   ", "XX", "99999", "Atlantis", "T"} {
		if got, ok := NormaliseState(in); ok {
			t.Errorf("NormaliseState(%q) = %q, want rejection — an unresolvable state must fail at entry, "+
				"not silently pick a tax head", in, got)
		}
	}
}

// Pre-bifurcation Andhra Pradesh. Historical records carry 28; new
// registrations use 37. It must resolve rather than fail validation on an
// old row, and must resolve to the current code.
func TestNormaliseState_ResolvesLegacyAndhraPradeshCode(t *testing.T) {
	got, ok := NormaliseState("28")
	if !ok {
		t.Fatal("legacy GST code 28 must still resolve for historical records")
	}
	if got != "AP" {
		t.Errorf("NormaliseState(\"28\") = %q, want AP", got)
	}
}

func TestGSTCodeFor_MatchesTheStatutorySchedule(t *testing.T) {
	// Spot-checked against the published GST state code schedule. These
	// are the ones most likely to be wrong from memory: Tamil Nadu vs
	// Telangana, and the two Andhra codes.
	cases := map[string]string{
		"TN": "33", "KA": "29", "MH": "27", "DL": "07",
		"TS": "36", "AP": "37", "KL": "32", "GJ": "24",
	}
	for code, want := range cases {
		got, ok := GSTCodeFor(code)
		if !ok {
			t.Errorf("GSTCodeFor(%q): not found", code)
			continue
		}
		if got != want {
			t.Errorf("GSTCodeFor(%q) = %q, want %q", code, got, want)
		}
	}
}

// Every registry entry must round-trip, or the export would emit a code
// the filing portal rejects.
func TestStateRegistryIsSelfConsistent(t *testing.T) {
	seenCode := map[string]bool{}
	seenGST := map[string]bool{}
	for _, s := range States() {
		if seenCode[s.Code] {
			t.Errorf("duplicate state code %q", s.Code)
		}
		if seenGST[s.GSTCode] {
			t.Errorf("duplicate GST code %q (%s)", s.GSTCode, s.Name)
		}
		seenCode[s.Code], seenGST[s.GSTCode] = true, true

		if got, ok := NormaliseState(s.Code); !ok || got != s.Code {
			t.Errorf("%s: code does not round-trip", s.Code)
		}
		if got, ok := NormaliseState(s.Name); !ok || got != s.Code {
			t.Errorf("%s: name %q does not resolve to its own code", s.Code, s.Name)
		}
		if got, ok := GSTCodeFor(s.Code); !ok || got != s.GSTCode {
			t.Errorf("%s: GST code does not round-trip", s.Code)
		}
		if len(s.GSTCode) != 2 {
			t.Errorf("%s: GST code %q must be two digits", s.Code, s.GSTCode)
		}
	}
}

// TestCalculateGstInvoiceFrom_StateSpellingDoesNotChangeTheTaxHead is the
// regression this work exists for. Before it, only the exact string "TN"
// took the intrastate branch.
func TestCalculateGstInvoiceFrom_StateSpellingDoesNotChangeTheTaxHead(t *testing.T) {
	rate := GstRate{
		CgstRate: dec(9), SgstRate: dec(9), IgstRate: dec(18),
	}
	base := dec(799)

	// Every spelling of the home state must be intrastate.
	for _, spelling := range []string{"TN", "tn", "Tamil Nadu", "TAMILNADU", "33"} {
		inv := CalculateGstInvoiceFrom(base, spelling, "TN", rate)
		if !inv.IgstAmount.IsZero() {
			t.Errorf("subscriber state %q: charged IGST %s — a home-state supply must be CGST+SGST",
				spelling, inv.IgstAmount.StringFixed(2))
		}
		if inv.CgstAmount.StringFixed(2) != "71.91" || inv.SgstAmount.StringFixed(2) != "71.91" {
			t.Errorf("subscriber state %q: cgst=%s sgst=%s, want 71.91 each",
				spelling, inv.CgstAmount.StringFixed(2), inv.SgstAmount.StringFixed(2))
		}
		// Whichever head it lands under, the customer pays the same.
		if inv.TotalAmount.StringFixed(2) != "942.82" {
			t.Errorf("subscriber state %q: total %s, want 942.82", spelling, inv.TotalAmount.StringFixed(2))
		}
	}

	// A genuinely different state must be interstate, however spelled.
	for _, spelling := range []string{"KA", "Karnataka", "29"} {
		inv := CalculateGstInvoiceFrom(base, spelling, "TN", rate)
		if inv.IgstAmount.StringFixed(2) != "143.82" {
			t.Errorf("subscriber state %q: igst=%s, want 143.82", spelling, inv.IgstAmount.StringFixed(2))
		}
		if !inv.CgstAmount.IsZero() || !inv.SgstAmount.IsZero() {
			t.Errorf("subscriber state %q: an out-of-state supply must not attract CGST/SGST", spelling)
		}
	}
}

// The home state is configuration, not a constant: an ISP registered in
// Karnataka bills a Karnataka subscriber intrastate.
func TestCalculateGstInvoiceFrom_HonoursTheConfiguredHomeState(t *testing.T) {
	rate := GstRate{CgstRate: dec(9), SgstRate: dec(9), IgstRate: dec(18)}

	inv := CalculateGstInvoiceFrom(dec(799), "Karnataka", "KA", rate)
	if !inv.IgstAmount.IsZero() {
		t.Errorf("a Karnataka supplier billing a Karnataka subscriber must charge CGST+SGST, got IGST %s",
			inv.IgstAmount.StringFixed(2))
	}

	inv = CalculateGstInvoiceFrom(dec(799), "Tamil Nadu", "KA", rate)
	if inv.IgstAmount.IsZero() {
		t.Error("a Karnataka supplier billing a Tamil Nadu subscriber must charge IGST")
	}
}

// An unresolvable state falls to interstate: IGST is right for a genuine
// out-of-state supply, and claiming a state share for a state we cannot
// identify would be worse.
func TestCalculateGstInvoiceFrom_UnknownStateFallsBackToInterstate(t *testing.T) {
	rate := GstRate{CgstRate: dec(9), SgstRate: dec(9), IgstRate: dec(18)}
	inv := CalculateGstInvoiceFrom(dec(799), "Atlantis", "TN", rate)
	if inv.IgstAmount.IsZero() {
		t.Error("an unresolvable subscriber state must not be treated as a home-state supply")
	}
}
