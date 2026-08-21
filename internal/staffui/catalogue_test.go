package staffui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postForm builds a request the form parsers can read, so these exercise the
// same r.PostFormValue path the handlers use rather than a stand-in.
func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/staff/catalogue/plans",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func validPlanForm() url.Values {
	return url.Values{
		"name":                {"Home_100M_Unlimited"},
		"rate_limit_string":   {"100M/100M"},
		"volume_gb":           {"3300"},
		"price":               {"799.00"},
		"validity_days":       {"30"},
		"fup_threshold_gb":    {"3300"},
		"fup_throttle_string": {"10M/10M"},
	}
}

func TestPlanFromForm_ValidPlanParses(t *testing.T) {
	p, err := planFromForm(postForm(validPlanForm()))
	if err != nil {
		t.Fatalf("planFromForm: %v", err)
	}
	if p.Name != "Home_100M_Unlimited" || p.RateLimitString != "100M/100M" {
		t.Errorf("name/rate not carried through: %+v", p)
	}
	if p.Price.StringFixed(2) != "799.00" || p.ValidityDays != 30 || p.VolumeGB != 3300 {
		t.Errorf("numeric fields wrong: %+v", p)
	}
	// 3300 GB expressed in bytes - the column stores bytes, the form asks
	// for GB, and getting that conversion wrong would set a FUP threshold
	// a billion times too low.
	if want := int64(3300) * 1024 * 1024 * 1024; p.FUPThresholdBytes != want {
		t.Errorf("FUP threshold: want %d bytes, got %d", want, p.FUPThresholdBytes)
	}
}

func TestPlanFromForm_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"blank name", func(v url.Values) { v.Set("name", "  ") }},
		// Passed to the NAS verbatim; a value without "/" applies no
		// shaping at all rather than failing loudly.
		{"speed missing the slash", func(v url.Values) { v.Set("rate_limit_string", "100M") }},
		{"non-numeric volume", func(v url.Values) { v.Set("volume_gb", "lots") }},
		{"negative volume", func(v url.Values) { v.Set("volume_gb", "-1") }},
		{"non-numeric price", func(v url.Values) { v.Set("price", "free") }},
		{"negative price", func(v url.Values) { v.Set("price", "-1") }},
		{"zero validity", func(v url.Values) { v.Set("validity_days", "0") }},
		{"non-numeric validity", func(v url.Values) { v.Set("validity_days", "a month") }},
		// A threshold with nothing to throttle down to would silently never
		// apply.
		{"FUP threshold without throttle", func(v url.Values) { v.Set("fup_throttle_string", "") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validPlanForm()
			tc.mutate(v)
			if _, err := planFromForm(postForm(v)); err == nil {
				t.Error("expected a validation error, got nil")
			}
		})
	}
}

// A blank FUP threshold is the "unlimited" case, and must clear any
// throttle left in the form rather than storing one that can never fire.
func TestPlanFromForm_BlankFUPMeansUnlimited(t *testing.T) {
	v := validPlanForm()
	v.Set("fup_threshold_gb", "")
	p, err := planFromForm(postForm(v))
	if err != nil {
		t.Fatalf("planFromForm: %v", err)
	}
	if p.FUPThresholdBytes != 0 {
		t.Errorf("threshold: want 0 for unlimited, got %d", p.FUPThresholdBytes)
	}
	if p.FUPThrottleString != "" {
		t.Errorf("throttle: want cleared when unlimited, got %q", p.FUPThrottleString)
	}
}

func TestGSTRateFromForm_ValidSplitParses(t *testing.T) {
	g, err := gstRateFromForm(postForm(url.Values{
		"cgst_rate": {"9.00"}, "sgst_rate": {"9.00"}, "igst_rate": {"18.00"},
		"effective_from": {"2026-04-01"},
	}))
	if err != nil {
		t.Fatalf("gstRateFromForm: %v", err)
	}
	if g.CGSTRate.StringFixed(2) != "9.00" || g.IGSTRate.StringFixed(2) != "18.00" {
		t.Errorf("rates not carried through: %+v", g)
	}
	if got := g.EffectiveFrom.Format("2006-01-02"); got != "2026-04-01" {
		t.Errorf("effective_from: want 2026-04-01, got %s", got)
	}
}

// The intra-state pair and the inter-state single rate are the same tax
// split two ways. If they disagree, an invoice's total depends on which
// side of a state line the subscriber sits - which is the one thing this
// screen must not allow through.
func TestGSTRateFromForm_RejectsSplitThatDoesNotSumToIGST(t *testing.T) {
	_, err := gstRateFromForm(postForm(url.Values{
		"cgst_rate": {"9"}, "sgst_rate": {"9"}, "igst_rate": {"25"},
	}))
	if err == nil {
		t.Fatal("expected a mismatched CGST+SGST vs IGST to be rejected")
	}
	if !strings.Contains(err.Error(), "IGST") {
		t.Errorf("error should name the rule it broke, got %q", err)
	}
}

func TestGSTRateFromForm_RejectsOutOfRangeAndUnparsableRates(t *testing.T) {
	cases := map[string]url.Values{
		"negative":     {"cgst_rate": {"-1"}, "sgst_rate": {"9"}, "igst_rate": {"8"}},
		"above 100":    {"cgst_rate": {"101"}, "sgst_rate": {"0"}, "igst_rate": {"101"}},
		"not a number": {"cgst_rate": {"nine"}, "sgst_rate": {"9"}, "igst_rate": {"18"}},
		"bad date":     {"cgst_rate": {"9"}, "sgst_rate": {"9"}, "igst_rate": {"18"}, "effective_from": {"01-04-2026"}},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := gstRateFromForm(postForm(v)); err == nil {
				t.Error("expected a validation error, got nil")
			}
		})
	}
}

// An omitted effective date means "in force now", not the zero time - a
// zero-time row would sort as the oldest rate forever and never be picked
// as current.
func TestGSTRateFromForm_BlankEffectiveDateDefaultsToNow(t *testing.T) {
	g, err := gstRateFromForm(postForm(url.Values{
		"cgst_rate": {"9"}, "sgst_rate": {"9"}, "igst_rate": {"18"},
	}))
	if err != nil {
		t.Fatalf("gstRateFromForm: %v", err)
	}
	if g.EffectiveFrom.IsZero() {
		t.Error("effective_from must default to now, not the zero time")
	}
}
