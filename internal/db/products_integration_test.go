//go:build integration

// Product catalogue, subscriber charges and invoice lines — FR-BIL-022 |
// migration 058.
//
// The properties worth exercising here are the ones only a real database
// shows: that an attachment snapshots its price rather than following the
// catalogue, that a one-time charge is closed in the same transaction as
// the invoice that billed it and so can never be billed twice, and that
// migration 058's deferred trigger actually refuses an invoice whose
// header disagrees with its own lines.
package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/shopspring/decimal"
)

// product is a catalogue entry fixture.
func product(code, name, kind, hsn, price string) billing.Product {
	return billing.Product{Code: code, Name: name, Kind: kind, HSNSAC: hsn,
		Price: decimal.RequireFromString(price)}
}

func TestFR_BIL_022_CatalogueLifecycle(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "100.00", MobileNumber: "+919800000001"})

	install, err := store.CreateProduct(ctx, product("install", "Installation", billing.ProductOneTime, "998432", "1500.00"), "meena")
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	// The code is upper-cased on the way in, as migration 058's CHECK
	// requires and Normalise promises.
	if install.Code != "INSTALL" {
		t.Errorf("code stored as %q, want INSTALL", install.Code)
	}
	if !install.Active {
		t.Error("a new product should be offered")
	}

	t.Run("the code is unique", func(t *testing.T) {
		_, err := store.CreateProduct(ctx, product("INSTALL", "Installation again", billing.ProductOneTime, "998432", "99.00"), "meena")
		if !errors.Is(err, billing.ErrProductCodeTaken) {
			t.Fatalf("want ErrProductCodeTaken, got %v", err)
		}
	})

	t.Run("the database refuses a malformed HSN/SAC even past the Go check", func(t *testing.T) {
		// Written straight to SQL, bypassing Normalise, to confirm the
		// CHECK is really there and not just the Go validation.
		_, err := pool.Exec(ctx, `
			INSERT INTO products (code, name, kind, price, hsn_sac, created_by)
			VALUES ('BAD', 'Bad', 'one_time', 10.00, 'abcd', 'meena')`)
		if err == nil {
			t.Fatal("the database accepted a non-numeric HSN/SAC")
		}
	})

	t.Run("updating keeps the code and the kind", func(t *testing.T) {
		updated, err := store.UpdateProduct(ctx, billing.Product{
			ID: install.ID, Code: install.Code, Kind: install.Kind,
			Name: "Installation and activation", HSNSAC: "998421",
			Price: decimal.RequireFromString("1750.00"),
		}, "ravi")
		if err != nil {
			t.Fatalf("update product: %v", err)
		}
		if updated.Code != "INSTALL" || updated.Kind != billing.ProductOneTime {
			t.Errorf("code/kind changed: %s / %s", updated.Code, updated.Kind)
		}
		if !updated.Price.Equal(decimal.RequireFromString("1750.00")) {
			t.Errorf("price is %s", updated.Price)
		}
		if updated.UpdatedBy != "ravi" || updated.UpdatedAt == nil {
			t.Error("the update was not attributed")
		}
	})

	t.Run("a product nothing has used can be deleted", func(t *testing.T) {
		spare, err := store.CreateProduct(ctx, product("SPARE", "Spare", billing.ProductRecurring, "8517", "10.00"), "meena")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteProduct(ctx, spare.ID); err != nil {
			t.Fatalf("delete unused product: %v", err)
		}
		if _, err := store.GetProduct(ctx, spare.ID); !errors.Is(err, billing.ErrProductNotFound) {
			t.Errorf("want ErrProductNotFound after deleting, got %v", err)
		}
	})

	t.Run("retiring hides it from attachment but keeps it attachable to nobody new", func(t *testing.T) {
		if err := store.SetProductActive(ctx, install.ID, false, "meena"); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		_, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: install.ID}, "meena")
		if !errors.Is(err, billing.ErrProductInactive) {
			t.Fatalf("want ErrProductInactive, got %v", err)
		}
		// A retired product is still listed, just not when filtering to
		// what is offered.
		offered, err := store.ListProducts(ctx, billing.ProductFilter{ActiveOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range offered {
			if p.ID == install.ID {
				t.Error("a retired product appeared in the offered list")
			}
		}
		if err := store.SetProductActive(ctx, install.ID, true, "meena"); err != nil {
			t.Fatalf("reactivate: %v", err)
		}
	})

	t.Run("a product subscribers hold cannot be deleted", func(t *testing.T) {
		if _, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: install.ID}, "meena"); err != nil {
			t.Fatalf("attach: %v", err)
		}
		if err := store.DeleteProduct(ctx, install.ID); !errors.Is(err, billing.ErrProductInUse) {
			t.Fatalf("want ErrProductInUse, got %v", err)
		}
	})
}

// An attachment's price is fixed when it is made. Raising the catalogue
// price must not re-price a subscriber who is already on it.
func TestFR_BIL_022_AttachmentPriceIsASnapshot(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "early", Balance: "0.00", MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "late", Balance: "0.00", MobileNumber: "+919800000002"})

	router, err := store.CreateProduct(ctx, product("ROUTER", "Router rental", billing.ProductRecurring, "8517", "150.00"), "meena")
	if err != nil {
		t.Fatal(err)
	}

	early, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: router.ID}, "meena")
	if err != nil {
		t.Fatal(err)
	}
	if !early.UnitPrice.Equal(decimal.RequireFromString("150.00")) {
		t.Fatalf("attached at %s, want the catalogue 150.00", early.UnitPrice)
	}

	// The catalogue price goes up.
	if _, err := store.UpdateProduct(ctx, billing.Product{ID: router.ID, Code: router.Code,
		Kind: router.Kind, Name: router.Name, HSNSAC: router.HSNSAC,
		Price: decimal.RequireFromString("250.00")}, "ravi"); err != nil {
		t.Fatal(err)
	}

	// The existing subscriber is untouched; a new one gets the new price.
	held, err := store.ListBillableProducts(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || !held[0].UnitPrice.Equal(decimal.RequireFromString("150.00")) {
		t.Errorf("the existing subscriber was re-priced: %+v", held)
	}
	late, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 2, ProductID: router.ID}, "meena")
	if err != nil {
		t.Fatal(err)
	}
	if !late.UnitPrice.Equal(decimal.RequireFromString("250.00")) {
		t.Errorf("a new attachment took %s, want the current 250.00", late.UnitPrice)
	}

	t.Run("attaching to a subscriber that does not exist is not-found, not a crash", func(t *testing.T) {
		_, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 999999, ProductID: router.ID}, "meena")
		if !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("an override beats the catalogue price", func(t *testing.T) {
		sp, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: router.ID,
			Quantity: 3, UnitPrice: decimal.RequireFromString("99.50")}, "meena")
		if err != nil {
			t.Fatal(err)
		}
		if !sp.UnitPrice.Equal(decimal.RequireFromString("99.50")) || sp.Quantity != 3 {
			t.Errorf("override not applied: %s × %d", sp.UnitPrice, sp.Quantity)
		}
		if !sp.Base().Equal(decimal.RequireFromString("298.50")) {
			t.Errorf("3 × 99.50 came to %s", sp.Base())
		}
	})
}

// An invoice is written with its lines, and the one-time charges it billed
// are closed in the same transaction.
func TestFR_BIL_022_InvoiceLinesAndOneTimeClosure(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	install, err := store.CreateProduct(ctx, product("INSTALL", "Installation", billing.ProductOneTime, "998432", "1500.00"), "meena")
	if err != nil {
		t.Fatal(err)
	}
	router, err := store.CreateProduct(ctx, product("ROUTER", "Router rental", billing.ProductRecurring, "8517", "150.00"), "meena")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: install.ID}, "meena"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: router.ID}, "meena"); err != nil {
		t.Fatal(err)
	}

	billable, err := store.ListBillableProducts(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(billable) != 2 {
		t.Fatalf("want 2 billable charges, got %d", len(billable))
	}
	// one_time sorts before recurring, so installation prints above the
	// ongoing rental — the order ListBillableProducts documents.
	if billable[0].Kind != billing.ProductOneTime {
		t.Errorf("billable order puts %s first", billable[0].Kind)
	}

	rate, err := store.GetActiveGstRate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	charges := []billing.LineCharge{billing.PlanCharge("Home 100M", decimal.RequireFromString("799.00"))}
	for _, sp := range billable {
		charges = append(charges, billing.ProductCharge(sp))
	}
	lines, hdr, err := billing.ComposeInvoice(charges, "TN", "TN", rate)
	if err != nil {
		t.Fatal(err)
	}
	hdr.SubscriberID = 1

	invoiceID, err := store.CreateInvoiceWithLines(ctx, hdr, lines)
	if err != nil {
		t.Fatalf("create invoice with lines: %v", err)
	}

	// The header the database holds equals the sum of the stored lines.
	got := scanString(ctx, t, pool, `
		SELECT (i.base_amount = l.base AND i.cgst_amount = l.cgst AND i.sgst_amount = l.sgst
		        AND i.igst_amount = l.igst AND i.total_amount = l.total)::text
		  FROM invoices i
		  JOIN (SELECT invoice_id, SUM(base_amount) base, SUM(cgst_amount) cgst,
		               SUM(sgst_amount) sgst, SUM(igst_amount) igst, SUM(total_amount) total
		          FROM invoice_lines GROUP BY invoice_id) l ON l.invoice_id = i.id
		 WHERE i.id = $1`, invoiceID)
	if got != "true" {
		t.Error("the stored invoice header does not equal the sum of its stored lines")
	}

	stored, err := store.ListInvoiceLines(ctx, invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("want 3 stored lines, got %d", len(stored))
	}
	for i, l := range stored {
		if l.LineNo != i+1 {
			t.Errorf("line %d stored with line_no %d", i, l.LineNo)
		}
	}
	if stored[0].Kind != billing.LineKindPlan || stored[0].ProductID != nil {
		t.Error("the plan line is not first, or names a product")
	}

	// The one-time charge is closed and points at the invoice that billed
	// it; the recurring one is untouched and still billable.
	t.Run("the one-time charge is closed, the recurring one is not", func(t *testing.T) {
		all, err := store.ListSubscriberProducts(ctx, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		var sawCharged, sawActive bool
		for _, sp := range all {
			switch sp.ProductCode {
			case "INSTALL":
				sawCharged = true
				if sp.Status != billing.AttachmentCharged {
					t.Errorf("the installation charge is %s, want charged", sp.Status)
				}
				if sp.InvoiceID == nil || *sp.InvoiceID != invoiceID {
					t.Error("the installation charge does not name the invoice that billed it")
				}
				if sp.ChargedAt == nil {
					t.Error("the installation charge has no charged_at")
				}
			case "ROUTER":
				sawActive = true
				if sp.Status != billing.AttachmentActive {
					t.Errorf("the rental is %s, want active", sp.Status)
				}
			}
		}
		if !sawCharged || !sawActive {
			t.Fatal("both charges should still be listed with history")
		}

		next, err := store.ListBillableProducts(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(next) != 1 || next[0].ProductCode != "ROUTER" {
			t.Errorf("next cycle should bill the rental only, got %+v", next)
		}
	})

	t.Run("a charged one-time attachment cannot be ended", func(t *testing.T) {
		all, _ := store.ListSubscriberProducts(ctx, 1, true)
		for _, sp := range all {
			if sp.ProductCode != "INSTALL" {
				continue
			}
			if err := store.EndAttachment(ctx, sp.ID, "changed my mind", "meena"); !errors.Is(err, billing.ErrAttachmentNotActive) {
				t.Fatalf("want ErrAttachmentNotActive, got %v", err)
			}
		}
	})

	t.Run("the invoice detail carries its lines", func(t *testing.T) {
		detail, err := store.GetInvoiceDetail(ctx, invoiceID)
		if err != nil {
			t.Fatal(err)
		}
		if detail == nil {
			t.Fatal("no detail")
		}
		if len(detail.Lines) != 3 {
			t.Errorf("the invoice detail carries %d lines, want 3", len(detail.Lines))
		}
	})
}

// The negative control for migration 058's deferred trigger: an invoice
// whose header disagrees with its lines must be refused at commit.
//
// Without this the per-line arithmetic would be guarded only by Go code,
// and any future path that wrote lines directly could file a tax invoice
// that does not add up.
func TestFR_BIL_022_HeaderThatDisagreesWithItsLinesIsRefused(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	rate, err := store.GetActiveGstRate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lines, hdr, err := billing.ComposeInvoice(
		[]billing.LineCharge{billing.PlanCharge("Home 100M", decimal.RequireFromString("799.00"))},
		"TN", "TN", rate)
	if err != nil {
		t.Fatal(err)
	}
	hdr.SubscriberID = 1

	// A header overstating the invoice by one rupee, with honest lines.
	bad := hdr
	bad.TotalAmount = bad.TotalAmount.Add(decimal.RequireFromString("1.00"))
	bad.BaseAmount = bad.BaseAmount.Add(decimal.RequireFromString("1.00"))

	if _, err := store.CreateInvoiceWithLines(ctx, bad, lines); err == nil {
		t.Fatal("the database accepted an invoice whose header does not equal its lines")
	}

	// And nothing was left behind: the whole transaction rolled back.
	if n := scanString(ctx, t, pool, `SELECT count(*)::text FROM invoices`); n != "0" {
		t.Errorf("%s invoice rows survived a refused write", n)
	}
	if n := scanString(ctx, t, pool, `SELECT count(*)::text FROM invoice_lines`); n != "0" {
		t.Errorf("%s invoice_lines rows survived a refused write", n)
	}

	// The honest version of the same invoice still goes in, which proves
	// the refusal above was the mismatch and not something else.
	if _, err := store.CreateInvoiceWithLines(ctx, hdr, lines); err != nil {
		t.Fatalf("the matching header was refused too: %v", err)
	}
}

// Two concurrent renewals must not both bill the same one-time charge.
// The guard is `AND status = 'active'` plus insisting exactly one row
// moved, so the loser's whole transaction rolls back.
func TestFR_BIL_022_OneTimeChargeIsBilledOnceUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	install, err := store.CreateProduct(ctx, product("INSTALL", "Installation", billing.ProductOneTime, "998432", "1500.00"), "meena")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: install.ID}, "meena"); err != nil {
		t.Fatal(err)
	}
	billable, err := store.ListBillableProducts(ctx, 1)
	if err != nil || len(billable) != 1 {
		t.Fatalf("billable: %v %d", err, len(billable))
	}
	rate, err := store.GetActiveGstRate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Both passes composed their lines from the same read — the race the
	// guard exists for.
	build := func() (billing.Invoice, []billing.InvoiceLine) {
		lines, hdr, err := billing.ComposeInvoice([]billing.LineCharge{
			billing.PlanCharge("Home 100M", decimal.RequireFromString("799.00")),
			billing.ProductCharge(billable[0]),
		}, "TN", "TN", rate)
		if err != nil {
			t.Fatal(err)
		}
		hdr.SubscriberID = 1
		return hdr, lines
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hdr, lines := build()
			if _, err := store.CreateInvoiceWithLines(ctx, hdr, lines); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d of 2 concurrent renewals billed the one-time charge, want exactly 1", succeeded)
	}
	if n := scanString(ctx, t, pool, `SELECT count(*)::text FROM invoices`); n != "1" {
		t.Errorf("%s invoices exist, want 1 — the installation fee was billed twice", n)
	}
	if n := scanString(ctx, t, pool,
		`SELECT count(*)::text FROM subscriber_products WHERE status = 'charged'`); n != "1" {
		t.Errorf("%s charges are marked charged, want 1", n)
	}
}

func TestFR_BIL_022_EndingAndListingCharges(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	router, err := store.CreateProduct(ctx, product("ROUTER", "Router rental", billing.ProductRecurring, "8517", "150.00"), "meena")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := store.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: router.ID}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a reason is required", func(t *testing.T) {
		if err := store.EndAttachment(ctx, sp.ID, "   ", "meena"); !errors.Is(err, billing.ErrProductInvalid) {
			t.Fatalf("want ErrProductInvalid, got %v", err)
		}
	})

	if err := store.EndAttachment(ctx, sp.ID, "router returned", "meena"); err != nil {
		t.Fatalf("end attachment: %v", err)
	}

	t.Run("it stops being billable and is not ended twice", func(t *testing.T) {
		billable, err := store.ListBillableProducts(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(billable) != 0 {
			t.Errorf("an ended charge is still billable: %+v", billable)
		}
		if err := store.EndAttachment(ctx, sp.ID, "again", "meena"); !errors.Is(err, billing.ErrAttachmentNotActive) {
			t.Fatalf("want ErrAttachmentNotActive, got %v", err)
		}
	})

	t.Run("history keeps it with its reason", func(t *testing.T) {
		all, err := store.ListSubscriberProducts(ctx, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 {
			t.Fatalf("want 1 historical charge, got %d", len(all))
		}
		if all[0].Status != billing.AttachmentEnded || all[0].EndReason != "router returned" {
			t.Errorf("ended charge reads %s / %q", all[0].Status, all[0].EndReason)
		}
		if all[0].EndedBy != "meena" || all[0].EndedAt == nil {
			t.Error("the ending was not attributed")
		}
		// Without history it is gone from the subscriber's current list.
		current, err := store.ListSubscriberProducts(ctx, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(current) != 0 {
			t.Errorf("an ended charge shows in the current list: %+v", current)
		}
	})

	t.Run("ending a charge that does not exist is reported as such", func(t *testing.T) {
		if err := store.EndAttachment(ctx, 999999, "whatever", "meena"); !errors.Is(err, billing.ErrAttachmentNotFound) {
			t.Fatalf("want ErrAttachmentNotFound, got %v", err)
		}
	})

	t.Run("who holds a product is listable with the subscriber's name", func(t *testing.T) {
		held, err := store.ListAttachmentsAcrossSubscribers(ctx, router.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(held) != 1 || held[0].Username != "acme" {
			t.Errorf("want one holder named acme, got %+v", held)
		}
	})
}

// GSTR-1's HSN summary has to see the invoice's real codes.
//
// Worth a database test and not just BuildReturn's unit tests: if the
// breakdown query returned nothing, BuildReturn would fall back to
// reporting everything under internet services — which is the behaviour
// that was correct until migration 058 and so looks entirely plausible in
// the output. The failure mode is a misclassified return that reads fine.
func TestFR_BIL_022_GSTR1SeesEachInvoiceLinesHSNCode(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "500.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	router, err := store.CreateProduct(ctx, product("ROUTER", "Router rental", billing.ProductRecurring, "8517", "150.00"), "meena")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachProduct(ctx, billing.AttachRequest{
		SubscriberID: 1, ProductID: router.ID, Quantity: 2}, "meena"); err != nil {
		t.Fatal(err)
	}
	billable, err := store.ListBillableProducts(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := store.GetActiveGstRate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lines, hdr, err := billing.ComposeInvoice([]billing.LineCharge{
		billing.PlanCharge("Home 100M", decimal.RequireFromString("500.00")),
		billing.ProductCharge(billable[0]),
	}, "TN", "TN", rate)
	if err != nil {
		t.Fatal(err)
	}
	hdr.SubscriberID = 1
	if _, err := store.CreateInvoiceWithLines(ctx, hdr, lines); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rows, err := store.ListInvoicesForGSTR1(ctx, now.Year(), now.Month())
	if err != nil {
		t.Fatalf("list invoices for GSTR-1: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 invoice in the period, got %d", len(rows))
	}
	if len(rows[0].HSNBreakdown) != 2 {
		t.Fatalf("want 2 HSN parts, got %d: %+v", len(rows[0].HSNBreakdown), rows[0].HSNBreakdown)
	}

	byCode := map[string]billing.HSNPart{}
	for _, p := range rows[0].HSNBreakdown {
		byCode[p.HSN] = p
	}
	svc, ok := byCode[billing.PlanHSNSAC]
	if !ok {
		t.Fatalf("no %s part: %+v", billing.PlanHSNSAC, rows[0].HSNBreakdown)
	}
	if !svc.TaxableValue.Equal(decimal.RequireFromString("500.00")) {
		t.Errorf("the plan part is %s, want 500.00", svc.TaxableValue)
	}
	goods, ok := byCode["8517"]
	if !ok {
		t.Fatalf("no 8517 part: %+v", rows[0].HSNBreakdown)
	}
	if !goods.TaxableValue.Equal(decimal.RequireFromString("300.00")) {
		t.Errorf("the rental part is %s, want 300.00 (2 × 150)", goods.TaxableValue)
	}
	if !goods.Quantity.Equal(decimal.RequireFromString("2")) {
		t.Errorf("the rental quantity is %s, want 2", goods.Quantity)
	}

	// And the whole return classifies it as two rows rather than one.
	ret := billing.BuildReturn(billing.Period{Year: now.Year(), Month: now.Month()},
		billing.Supplier{GSTIN: "33ABCDE1234F1Z5", State: "TN", Name: "Maaran Net"}, rows, nil)
	if len(ret.HSN) != 2 {
		t.Errorf("the return has %d HSN rows, want 2: %+v", len(ret.HSN), ret.HSN)
	}
}

// An invoice with no lines is still a valid invoice — every one raised
// before migration 058 is like that, and the trigger must not reject them.
func TestFR_BIL_022_LinelessInvoicesRemainValid(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme", Balance: "0.00", MobileNumber: "+919800000001"})

	id, err := store.CreateInvoice(ctx, billing.Invoice{
		SubscriberID: 1, BaseAmount: decimal.RequireFromString("799.00"),
		CgstAmount: decimal.RequireFromString("71.91"), SgstAmount: decimal.RequireFromString("71.91"),
		IgstAmount: decimal.Zero, TotalAmount: decimal.RequireFromString("942.82"),
		GstRateID: 1, GbIncluded: 100, GbUsed: decimal.Zero,
	})
	if err != nil {
		t.Fatalf("a lineless invoice was refused: %v", err)
	}
	lines, err := store.ListInvoiceLines(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("a lineless invoice reported %d lines", len(lines))
	}
	detail, err := store.GetInvoiceDetail(ctx, id)
	if err != nil || detail == nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Lines) != 0 {
		t.Errorf("detail reported %d lines", len(detail.Lines))
	}
	// And it renders: the template falls back to the header.
	if !decimal.RequireFromString(detail.TotalAmount).Equal(decimal.RequireFromString("942.82")) {
		t.Errorf("detail total is %s", detail.TotalAmount)
	}
}
