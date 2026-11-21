package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Customer Application Form — FR-DOC-002.

func cafFixture() CAFData {
	verified := time.Date(2026, 3, 12, 10, 0, 0, 0, time.UTC)
	return CAFData{
		Supplier:        Supplier{Name: "Maaran Networks Pvt Ltd", GSTIN: "33AAAAA0000A1Z5", State: "TN"},
		CAFNumber:       "CAF-000042",
		FullName:        "Priya Raman",
		Username:        "priya@isp",
		MobileNumber:    "+919876543210",
		Email:           "priya@example.com",
		RegisteredState: "TN",
		PlanName:        "Fibre 100",
		PlanPrice:       decimal.RequireFromString("799.00"),
		RateLimit:       "100M/100M",
		VolumeGB:        3300,
		ValidityDays:    30,
		KYCStatus:       "verified",
		AadhaarOnRecord: true,
		PANOnRecord:     true,
		KYCVerifiedAt:   &verified,
		AppliedOn:       time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC),
		PrintedOn:       time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
		PrintedBy:       "billing",
	}
}

// Rendering the template is the point of this test: every field is reached
// by name at runtime, so a typo or a renamed struct field compiles cleanly
// and fails only when somebody prints a form. (One was caught this way —
// the template asked for Supplier.LegalName, which does not exist.)
func TestFR_DOC_002_TheFormRendersEveryFieldItClaimsTo(t *testing.T) {
	html, err := renderCAFHTML(cafFixture())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		"Customer Application Form",
		"CAF-000042",
		"Priya Raman",
		"priya@isp",
		"919876543210", // html/template escapes the leading +
		"priya@example.com",
		"Maaran Networks Pvt Ltd",
		"33AAAAA0000A1Z5",
		"Fibre 100", "799.00", "100M/100M", "3300 GB", "30 days",
		"Aadhaar and PAN on record",
		"12 March 2026",    // KYC verified
		"10 March 2026",    // applied
		"21 November 2026", // printed
		"by billing",
		// A form that gets signed needs the declaration and the signature
		// blocks, or it is a data sheet rather than an application.
		"Declaration", "Applicant's signature",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the form does not print %q", want)
		}
	}

	// Go templates render a missing field as "<no value>" rather than
	// failing, so that string appearing anywhere means a field was asked
	// for by a name the type does not have.
	if strings.Contains(html, "<no value>") {
		t.Error("the form contains <no value>: a field is being reached by a name the type does not have")
	}
}

// The decision that matters most on this document: identity numbers are
// never printed. They are held AES-GCM encrypted with no decrypt path
// anywhere, and a filed or emailed CAF must not become a second copy of a
// customer's Aadhaar.
func TestFR_DOC_002_IdentityNumbersAreNeverPrinted(t *testing.T) {
	data := cafFixture()
	html, err := renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// The type carries no field that could hold a number, which is the
	// structural guarantee — there is nothing for a future template edit to
	// reach for. Asserted through the rendered form by checking that
	// anything Aadhaar-shaped is absent.
	for _, shape := range []string{
		"1234 5678 9012", "123456789012", // Aadhaar, spaced and bare
		"ABCDE1234F", // PAN
	} {
		if strings.Contains(html, shape) {
			t.Errorf("the form printed something shaped like an identity number: %q", shape)
		}
	}

	// And it says why, so an operator does not go looking for a setting to
	// turn them on. Compared with whitespace normalised, because the
	// template wraps these lines and pinning the exact breaks makes the
	// test fail on reformatting rather than on meaning.
	flat := strings.Join(strings.Fields(html), " ")
	for _, want := range []string{
		"deliberately not printed",
		"held encrypted",
		"cannot become a second copy",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the form does not explain the omission: missing %q", want)
		}
	}
}

// A connection provisioned with no documents at all must carry a visible
// warning, not a quiet blank that reads as "nothing required here".
func TestFR_DOC_002_AFormWithNoDocumentsCarriesAWarning(t *testing.T) {
	data := cafFixture()
	data.AadhaarOnRecord, data.PANOnRecord, data.KYCVerifiedAt = false, false, nil
	data.KYCStatus = "pending"

	html, err := renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "No identity document is on record") {
		t.Error("a form with no documents carries no warning")
	}
	if !strings.Contains(html, "No identity document on record") {
		t.Error("the documents-held row does not say there are none")
	}
	// The verified-on row must read as absent rather than blank.
	if !strings.Contains(html, "—") {
		t.Error("a missing verification date renders as an empty cell rather than a dash")
	}

	// And the warning must be gone once a document is held.
	data.AadhaarOnRecord = true
	html, err = renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(html, "No identity document is on record") {
		t.Error("the warning survives after a document is recorded")
	}
	if !strings.Contains(html, "Aadhaar on record") {
		t.Error("holding only an Aadhaar is not described")
	}
}

func TestFR_DOC_002_KYCSummaryDescribesEachCombination(t *testing.T) {
	for _, tc := range []struct {
		aadhaar, pan bool
		want         string
		complete     bool
	}{
		{true, true, "Aadhaar and PAN on record", true},
		{true, false, "Aadhaar on record", true},
		{false, true, "PAN on record", true},
		{false, false, "No identity document on record", false},
	} {
		d := CAFData{AadhaarOnRecord: tc.aadhaar, PANOnRecord: tc.pan}
		if got := d.KYCSummary(); got != tc.want {
			t.Errorf("aadhaar=%v pan=%v: KYCSummary() = %q, want %q", tc.aadhaar, tc.pan, got, tc.want)
		}
		if got := d.KYCComplete(); got != tc.complete {
			t.Errorf("aadhaar=%v pan=%v: KYCComplete() = %v, want %v", tc.aadhaar, tc.pan, got, tc.complete)
		}
	}
}

// subscribers carries no name, so the name comes from the lead the
// subscriber was converted from. When there was no lead the form falls back
// to the username and says so, rather than presenting an account name as a
// customer's legal name on a document they are about to sign.
func TestFR_DOC_002_AMissingNameFallsBackAndSaysSo(t *testing.T) {
	data := cafFixture()
	data.FullName = ""

	if got := data.CustomerName(); got != "priya@isp" {
		t.Errorf("CustomerName() = %q, want the username as a fallback", got)
	}
	if !data.NameIsAccountName() {
		t.Error("the fallback is not flagged")
	}

	html, err := renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "This is the account name held on the system") {
		t.Error("the form presents the username as the customer's name without qualification")
	}

	// With a name, no qualification.
	data.FullName = "Priya Raman"
	if data.NameIsAccountName() {
		t.Error("a real name is flagged as an account name")
	}
	html, err = renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(html, "This is the account name held on the system") {
		t.Error("the qualification survives when a real name is present")
	}
}

// An unlimited plan must read as unlimited, not as "0 GB".
func TestFR_DOC_002_AnUnlimitedPlanReadsAsUnlimited(t *testing.T) {
	data := cafFixture()
	data.VolumeGB = 0
	html, err := renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "Unlimited") {
		t.Error("a plan with no quota does not read as unlimited")
	}
	if strings.Contains(html, "0 GB") {
		t.Error("a plan with no quota prints as 0 GB")
	}
}

// Optional fields must be absent rather than printed as empty rows, and the
// ones that are always shown must read as a dash.
func TestFR_DOC_002_OptionalFieldsAreOmittedNotBlank(t *testing.T) {
	data := cafFixture()
	data.GSTIN, data.Franchise = "", ""
	data.Email, data.MobileNumber, data.RateLimit = "", "", ""

	html, err := renderCAFHTML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// A customer with no GSTIN gets no GSTIN row at all.
	if strings.Contains(html, "Customer GSTIN") {
		t.Error("an empty customer GSTIN still printed its row")
	}
	if strings.Contains(html, "Partner") {
		t.Error("an empty partner still printed its row")
	}
	// But the always-present rows read as a dash rather than blank, so a
	// signed form cannot have a value added to it afterwards.
	if !strings.Contains(html, "—") {
		t.Error("missing contact details render as empty cells")
	}
	if strings.Contains(html, "<no value>") {
		t.Error("a missing field rendered as <no value>")
	}
}

// A CAF with no application number cannot be filed or matched to an
// account, so printing one is refused rather than producing a sheet of
// paper nobody can place.
func TestFR_DOC_002_AFormWithNoApplicationNumberIsRefused(t *testing.T) {
	c := NewInvoicePDFClient("")
	data := cafFixture()
	data.CAFNumber = ""

	if _, err := c.GenerateCAFPDF(nil, data); err == nil { //nolint:staticcheck // nil ctx never reached
		t.Fatal("a CAF with no application number was printed")
	} else if !strings.Contains(err.Error(), "application number") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// The declaration is the part that makes this a form rather than a report,
// and it has to name what the customer is agreeing to.
func TestFR_DOC_002_TheDeclarationNamesWhatIsBeingAgreed(t *testing.T) {
	html, err := renderCAFHTML(cafFixture())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"identity documents I\n  produced are genuine",
		"terms of service",
		"fair-use",
		"suspended for non-payment",
	} {
		// Normalised: the template wraps lines, so compare without the
		// wrapping rather than pinning the exact line breaks.
		flat := strings.Join(strings.Fields(html), " ")
		if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("the declaration does not mention %q", strings.Join(strings.Fields(want), " "))
		}
	}
}
