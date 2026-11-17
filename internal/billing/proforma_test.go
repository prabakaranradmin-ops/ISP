package billing_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func TestFR_BIL_018_ProformaRequestRules(t *testing.T) {
	ok := billing.ProformaRequest{SubscriberID: 1, Description: "  Installation \n and   router ", Base: decimal.RequireFromString("1500.00")}
	got, err := ok.Normalise()
	if err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if got.Description != "Installation and router" {
		t.Errorf("description not tidied: %q", got.Description)
	}
	if got.ValidDays != billing.ProformaDefaultValid {
		t.Errorf("default validity: %d", got.ValidDays)
	}

	for name, r := range map[string]billing.ProformaRequest{
		"no description":   {Description: "  ", Base: decimal.NewFromInt(1)},
		"too long":         {Description: strings.Repeat("x", 201), Base: decimal.NewFromInt(1)},
		"zero":             {Description: "x", Base: decimal.Zero},
		"negative":         {Description: "x", Base: decimal.NewFromInt(-5)},
		"three decimals":   {Description: "x", Base: decimal.RequireFromString("10.005")},
		"slipped zero":     {Description: "x", Base: decimal.NewFromInt(15_000_000)},
		"valid for a year": {Description: "x", Base: decimal.NewFromInt(1), ValidDays: 365},
		"negative days":    {Description: "x", Base: decimal.NewFromInt(1), ValidDays: -1},
	} {
		if _, err := r.Normalise(); !errors.Is(err, billing.ErrProformaInvalid) || billing.ProformaInvalidReason(err) == "" {
			t.Errorf("%s: want a worded ErrProformaInvalid, got %v", name, err)
		}
	}
}

func TestFR_BIL_018_ProformaValidity(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	issued := time.Date(2026, 11, 17, 23, 30, 0, 0, ist)
	until := billing.ProformaValidUntil(issued, 15)
	if until.Format("2006-01-02") != "2026-12-02" {
		t.Fatalf("valid until: %s", until)
	}
	// It stands for the whole of its last day, and not a minute after. The
	// date comes back from Postgres as UTC midnight, so test that shape.
	asStored := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	if billing.ProformaExpired(asStored, time.Date(2026, 12, 2, 23, 59, 0, 0, ist)) {
		t.Error("expired during its last day")
	}
	if !billing.ProformaExpired(asStored, time.Date(2026, 12, 3, 0, 0, 0, 0, ist)) {
		t.Error("still valid the day after")
	}
	if billing.ProformaNumber(7) != "PF-000007" || billing.InvoiceNumber(7) == billing.ProformaNumber(7) {
		t.Error("proforma numbers must be their own series")
	}
}
