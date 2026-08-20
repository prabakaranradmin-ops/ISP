package billing_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/pkg/chromium"
	"github.com/shopspring/decimal"
)

func TestNewInvoicePDFClient(t *testing.T) {
	c := billing.NewInvoicePDFClient("/usr/bin/does-not-need-to-exist-for-this-test")
	if c == nil {
		t.Fatal("NewInvoicePDFClient returned nil")
	}
}

// chromiumPath returns a real Chromium-based browser on this machine, or
// skips the test. GeneratePDF drives an actual browser (see pkg/chromium
// for how it is found) rather than an HTTP fake the way the old
// Gotenberg-based client's tests did — Gotenberg's own container ran the
// exact same headless Chromium underneath, so mocking its HTTP endpoint
// only ever proved this package could speak multipart/form-data, never that
// the invoice actually rendered. A CI or developer machine without any
// Chromium-based browser installed skips rather than fails: the same
// graceful-degradation rule GetInvoicePDF itself follows at 503 when none
// is configured.
func chromiumPath(t *testing.T) string {
	t.Helper()
	path, err := chromium.Locate("")
	if err != nil {
		if errors.Is(err, chromium.ErrNotFound) {
			t.Skip("no Chromium-based browser found on this machine, skipping")
		}
		t.Fatalf("chromium.Locate: %v", err)
	}
	return path
}

func testInvoiceData() billing.InvoiceData {
	return billing.InvoiceData{
		InvoiceNumber:   "INV-000042",
		InvoiceDate:     time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		DueDate:         time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
		SubscriberName:  "Test Subscriber",
		MobileNumber:    "+919876543210",
		RegisteredState: "TN",
		PlanName:        "TN_Super_100M",
		PlanPeriod:      "January 2026",
		BaseAmount:      decimal.RequireFromString("799.00"),
		CGSTRate:        decimal.RequireFromString("9.00"),
		CGSTAmount:      decimal.RequireFromString("71.91"),
		SGSTRate:        decimal.RequireFromString("9.00"),
		SGSTAmount:      decimal.RequireFromString("71.91"),
		TotalAmount:     decimal.RequireFromString("942.82"),
		GBUsed:          decimal.RequireFromString("120.00"),
		GBIncluded:      decimal.RequireFromString("3300.00"),
		SpeedActive:     "100 Mbps / 100 Mbps",
	}
}

func TestGeneratePDF_ReturnsRealPDFBytes(t *testing.T) {
	c := billing.NewInvoicePDFClient(chromiumPath(t))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pdf, err := c.GeneratePDF(ctx, testInvoiceData())
	if err != nil {
		t.Fatalf("GeneratePDF: %v", err)
	}

	// "%PDF-" is the format's own magic bytes (ISO 32000-1 §7.5.2) — the
	// one thing that proves the browser actually printed a PDF rather than
	// this test accidentally passing on an empty or garbage byte slice.
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("GeneratePDF returned %d bytes not starting with the PDF magic bytes: %q", len(pdf), pdf[:min(32, len(pdf))])
	}
	if len(pdf) < 512 {
		t.Errorf("GeneratePDF returned only %d bytes, too small to be a real rendered invoice", len(pdf))
	}
}

func TestGeneratePDF_NonExistentExecPathErrors(t *testing.T) {
	// Exercises the allocator's own failure path (a bad execPath, as
	// opposed to a rendering failure) without needing a real browser on the
	// test machine — this one is expected to fail regardless of what is
	// installed.
	c := billing.NewInvoicePDFClient(t.TempDir() + "/no-such-browser.exe")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.GeneratePDF(ctx, testInvoiceData()); err == nil {
		t.Fatal("expected an error when execPath does not point at a real browser")
	}
}

func TestGeneratePDF_ContextAlreadyCancelledErrors(t *testing.T) {
	c := billing.NewInvoicePDFClient(chromiumPath(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.GeneratePDF(ctx, testInvoiceData()); err == nil {
		t.Fatal("expected an error when the context is already cancelled")
	}
}
