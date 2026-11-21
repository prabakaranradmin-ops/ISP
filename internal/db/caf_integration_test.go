//go:build integration

// Customer Application Form data assembly — FR-DOC-002.
//
// The form itself is tested in internal/billing by rendering its HTML. What
// only a real database can show is the assembly: that the customer's name is
// recovered from the lead they were converted from, that KYC is reported as
// two booleans without the encrypted columns ever being selected, and that
// one query describes one consistent moment.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestFR_DOC_002_TheFormsFactsAreAssembledInOneRead(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.API()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{
		Username: "priya@isp", MobileNumber: "+919876543210", RegisteredSt: "TN",
	})
	if _, err := pool.Exec(ctx, `
		UPDATE subscribers SET email = 'priya@example.com', gstin = '33BBBBB1111B1Z5',
		       kyc_status = 'verified' WHERE id = 1`); err != nil {
		t.Fatalf("set contact details: %v", err)
	}

	// The name lives on the lead this subscriber was converted from
	// (FR-CRM-002), because subscribers carries none.
	if _, err := pool.Exec(ctx, `
		INSERT INTO leads (full_name, mobile_number, source, status, converted_subscriber_id, converted_at)
		VALUES ('Priya Raman', '+919876543210', 'walk_in', 'converted', 1, NOW())`); err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	verified := time.Now().Add(-48 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO kyc_verifications (subscriber_id, aadhaar_encrypted, pan_encrypted,
		                               key_version_id, verified_at)
		VALUES (1, 'ciphertext-aadhaar', 'ciphertext-pan', 'v1', $1)`, verified); err != nil {
		t.Fatalf("seed kyc: %v", err)
	}

	d, err := store.GetCAFDetail(ctx, 1)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d == nil {
		t.Fatal("no detail for a subscriber that exists")
	}

	if d.FullName != "Priya Raman" {
		t.Errorf("FullName = %q; the name should come from the converted lead", d.FullName)
	}
	if d.Username != "priya@isp" {
		t.Errorf("Username = %q", d.Username)
	}
	if d.CAFNumber == "" {
		t.Error("the form has no application number, so it could not be filed")
	}
	if d.MobileNumber != "+919876543210" || d.Email != "priya@example.com" {
		t.Errorf("contact details came back as %q / %q", d.MobileNumber, d.Email)
	}
	if d.RegisteredState != "TN" || d.GSTIN != "33BBBBB1111B1Z5" {
		t.Errorf("state/GSTIN came back as %q / %q", d.RegisteredState, d.GSTIN)
	}
	if d.PlanName != "Fibre 100" || !d.PlanPrice.Equal(decimal.RequireFromString("799.00")) {
		t.Errorf("plan came back as %q at %s", d.PlanName, d.PlanPrice)
	}
	if d.RateLimit != "100M/100M" {
		t.Errorf("speed came back as %q", d.RateLimit)
	}
	if d.CreatedAt.IsZero() {
		t.Error("the applied-on date is missing")
	}

	// KYC as facts about what is held, never the values.
	if !d.AadhaarOnRecord || !d.PANOnRecord {
		t.Errorf("documents on record reported as aadhaar=%v pan=%v", d.AadhaarOnRecord, d.PANOnRecord)
	}
	if d.KYCVerifiedAt == nil {
		t.Error("the verification date is missing")
	}
	if d.KYCStatus != "verified" {
		t.Errorf("KYC status came back as %q", d.KYCStatus)
	}
}

// A subscriber created directly has no lead, so there is no name. The form
// falls back to the username and says so — but the store must report the
// absence rather than inventing something.
func TestFR_DOC_002_ASubscriberWithNoLeadHasNoName(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.API()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "direct@isp"})

	d, err := store.GetCAFDetail(ctx, 1)
	if err != nil || d == nil {
		t.Fatalf("detail: %v, %v", d, err)
	}
	if d.FullName != "" {
		t.Errorf("FullName = %q for a subscriber with no lead; it should be empty so the form "+
			"can say the account name is standing in", d.FullName)
	}
	// And with no KYC row at all, nothing is on record and the date is nil
	// rather than a zero time that would print as year 1.
	if d.AadhaarOnRecord || d.PANOnRecord {
		t.Error("documents are reported on record for a subscriber with no KYC row")
	}
	if d.KYCVerifiedAt != nil {
		t.Errorf("a subscriber with no KYC row has a verification date: %v", d.KYCVerifiedAt)
	}
}

// An empty encrypted column is not a document. A row created with blanks
// must not make the form claim an Aadhaar was checked.
func TestFR_DOC_002_AnEmptyKYCColumnIsNotADocument(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.API()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "blank@isp"})

	// A KYC row where only the PAN was supplied. persistKYC writes a plain
	// empty string for the one that was not given, and these columns are
	// TEXT holding ciphertext — so the fixture is an empty string. An
	// earlier version used ''::bytea, which casts to the two-character
	// text "backslash-x" and made this case look like a document on
	// record: a fixture production never produces.
	if _, err := pool.Exec(ctx, `
		INSERT INTO kyc_verifications (subscriber_id, aadhaar_encrypted, pan_encrypted, key_version_id)
		VALUES (1, '', 'ciphertext-pan', 'v1')`); err != nil {
		t.Fatalf("seed kyc: %v", err)
	}

	d, err := store.GetCAFDetail(ctx, 1)
	if err != nil || d == nil {
		t.Fatalf("detail: %v, %v", d, err)
	}
	if d.AadhaarOnRecord {
		t.Error("an empty aadhaar column is reported as a document on record, so the form would " +
			"claim an identity document was checked when none was supplied")
	}
	if !d.PANOnRecord {
		t.Error("the PAN that was supplied is not reported")
	}
}

func TestFR_DOC_002_AnUnknownSubscriberHasNoForm(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)

	d, err := database.API().GetCAFDetail(ctx, 999999)
	if err != nil {
		t.Errorf("an unknown subscriber errored rather than reporting absence: %v", err)
	}
	if d != nil {
		t.Errorf("an unknown subscriber produced a form: %+v", d)
	}
}
