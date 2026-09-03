//go:build integration

package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

// ── RadiusStore ─────────────────────────────────────────────────────────────

// TestFR_AAA_002_RadiusStore_GetSubscriberByUsername verifies the AAA lookup returns the
// plan's rate limit and the subscriber's throttle state.
//
// FR-AAA-002
func TestFR_AAA_002_RadiusStore_GetSubscriberByUsername(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "TN_Super_100M", "100M/100M", 3_543_348_019_200, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "throttled@isp", FUPActive: true})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "banned@isp", Status: "hard_suspended"})

	store := database.Radius()

	t.Run("active subscriber carries plan rate limit", func(t *testing.T) {
		sub, err := store.GetSubscriberByUsername(ctx, "alice@isp")
		if err != nil {
			t.Fatalf("GetSubscriberByUsername: %v", err)
		}
		if sub == nil {
			t.Fatal("want a subscriber, got nil")
		}
		if sub.ID != 1 || sub.Username != "alice@isp" {
			t.Errorf("identity: got id=%d username=%q", sub.ID, sub.Username)
		}
		if sub.Status != "active" {
			t.Errorf("status: want active, got %q", sub.Status)
		}
		if sub.RateLimitStr != "100M/100M" {
			t.Errorf("rate limit: want 100M/100M, got %q", sub.RateLimitStr)
		}
		if sub.FUPThrottle != "10M/10M" {
			t.Errorf("fup throttle: want 10M/10M, got %q", sub.FUPThrottle)
		}
		if sub.FUPActive {
			t.Error("fup_active must be false for a fresh subscriber")
		}
		if sub.PasswordHash == "" {
			t.Error("password hash must be loaded for bcrypt comparison")
		}
	})

	t.Run("throttled subscriber reports fup_active", func(t *testing.T) {
		sub, err := store.GetSubscriberByUsername(ctx, "throttled@isp")
		if err != nil {
			t.Fatalf("GetSubscriberByUsername: %v", err)
		}
		if sub == nil || !sub.FUPActive {
			t.Fatalf("want fup_active=true, got %+v", sub)
		}
	})

	t.Run("suspended subscriber still returned so handler can reject", func(t *testing.T) {
		sub, err := store.GetSubscriberByUsername(ctx, "banned@isp")
		if err != nil {
			t.Fatalf("GetSubscriberByUsername: %v", err)
		}
		if sub == nil {
			t.Fatal("suspended subscriber must be returned; the handler decides the reject")
		}
		if sub.Status != "hard_suspended" {
			t.Errorf("status: want hard_suspended, got %q", sub.Status)
		}
	})

	t.Run("unknown username is not an error", func(t *testing.T) {
		sub, err := store.GetSubscriberByUsername(ctx, "nobody@isp")
		if err != nil {
			t.Fatalf("an unknown username must not error, got: %v", err)
		}
		if sub != nil {
			t.Errorf("want nil for unknown username, got %+v", sub)
		}
	})
}

// ── APIStore ────────────────────────────────────────────────────────────────

// TestFR_SUB_001_APIStore_SubscriberLifecycle verifies create, read and partial update.
//
// FR-SUB-001
func TestFR_SUB_001_APIStore_SubscriberLifecycle(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "TN_Basic_50M", "50M/50M", 1_771_674_009_600, "5M/5M", "499.00")
	seedPlan(ctx, t, pool, 2, "TN_Super_100M", "100M/100M", 3_543_348_019_200, "10M/10M", "799.00")

	store := database.API()

	created, err := store.CreateSubscriber(ctx, api.SubscriberRecord{
		CAFNumber:       "CAF-2026-1001",
		Username:        "newsub@isp",
		MobileNumber:    "+919876500001",
		Email:           "new@example.com",
		PlanID:          1,
		RegisteredState: "TN",
		Status:          "active",
		DunningState:    "active",
		KYCStatus:       "pending",
		WalletBalance:   "250.00",
	}, "$2a$12$hashedpassword")
	if err != nil {
		t.Fatalf("CreateSubscriber: %v", err)
	}

	if created.ID == 0 {
		t.Error("created subscriber must carry a generated id")
	}
	if created.WalletBalance != "250.00" {
		t.Errorf("wallet_balance: want 250.00, got %q", created.WalletBalance)
	}
	if created.CreatedAt.IsZero() {
		t.Error("created_at must be populated")
	}

	t.Run("read back by id", func(t *testing.T) {
		got, err := store.GetSubscriberByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetSubscriberByID: %v", err)
		}
		if got == nil || got.Username != "newsub@isp" {
			t.Fatalf("want newsub@isp, got %+v", got)
		}
		if got.Email != "new@example.com" {
			t.Errorf("email: want new@example.com, got %q", got.Email)
		}
	})

	t.Run("read back by username", func(t *testing.T) {
		got, err := store.GetSubscriberByUsername(ctx, "newsub@isp")
		if err != nil {
			t.Fatalf("GetSubscriberByUsername: %v", err)
		}
		if got == nil || got.ID != created.ID {
			t.Fatalf("want id %d, got %+v", created.ID, got)
		}
	})

	t.Run("missing rows are not errors", func(t *testing.T) {
		byID, err := store.GetSubscriberByID(ctx, 999999)
		if err != nil || byID != nil {
			t.Errorf("want (nil, nil) for unknown id, got (%+v, %v)", byID, err)
		}
		byName, err := store.GetSubscriberByUsername(ctx, "ghost@isp")
		if err != nil || byName != nil {
			t.Errorf("want (nil, nil) for unknown username, got (%+v, %v)", byName, err)
		}
	})

	t.Run("partial update leaves unspecified fields alone", func(t *testing.T) {
		newPlan := 2
		updated, err := store.UpdateSubscriber(ctx, created.ID, &newPlan, nil, nil)
		if err != nil {
			t.Fatalf("UpdateSubscriber: %v", err)
		}
		if updated.PlanID != 2 {
			t.Errorf("plan_id: want 2, got %d", updated.PlanID)
		}
		if updated.Status != "active" {
			t.Errorf("status must be untouched when nil is passed, got %q", updated.Status)
		}

		suspended := "soft_suspended"
		updated, err = store.UpdateSubscriber(ctx, created.ID, nil, &suspended, nil)
		if err != nil {
			t.Fatalf("UpdateSubscriber status: %v", err)
		}
		if updated.Status != "soft_suspended" {
			t.Errorf("status: want soft_suspended, got %q", updated.Status)
		}
		if updated.PlanID != 2 {
			t.Errorf("plan_id must be untouched when nil is passed, got %d", updated.PlanID)
		}
	})

	t.Run("duplicate username is reported as a unique violation", func(t *testing.T) {
		_, err := store.CreateSubscriber(ctx, api.SubscriberRecord{
			CAFNumber:       "CAF-2026-1002",
			Username:        "newsub@isp", // already taken
			MobileNumber:    "+919876500002",
			PlanID:          1,
			RegisteredState: "TN",
		}, "$2a$12$hash")
		if err == nil {
			t.Fatal("expected a unique violation for a duplicate username")
		}
		// api.isUniqueViolation classifies on this substring to return 409.
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate") &&
			!strings.Contains(strings.ToLower(err.Error()), "unique") {
			t.Errorf("error must mention duplicate/unique so the API maps it to 409: %v", err)
		}
	})
}

// TestFR_SEC_002_APIStore_UpsertKYC verifies encrypted PII is stored once per subscriber
// and replaced rather than duplicated on re-submission.
//
// FR-SEC-002
func TestFR_SEC_002_APIStore_UpsertKYC(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "kyc@isp"})
	seedEncryptionKey(ctx, t, pool, "v1")
	seedEncryptionKey(ctx, t, pool, "v2")

	store := database.API()

	if err := store.UpsertKYC(ctx, 1, "v1:firstciphertext", "v1:panciphertext", "v1"); err != nil {
		t.Fatalf("first UpsertKYC: %v", err)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM kyc_verifications WHERE subscriber_id = 1`); n != 1 {
		t.Fatalf("want 1 KYC row, got %d", n)
	}

	// Re-submitting after a key rotation must replace, not accumulate.
	if err := store.UpsertKYC(ctx, 1, "v2:rotatedciphertext", "v2:rotatedpan", "v2"); err != nil {
		t.Fatalf("second UpsertKYC: %v", err)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM kyc_verifications WHERE subscriber_id = 1`); n != 1 {
		t.Errorf("re-submission must upsert, not duplicate: got %d rows", n)
	}

	got := scanString(ctx, t, pool, `SELECT aadhaar_encrypted FROM kyc_verifications WHERE subscriber_id = 1`)
	if got != "v2:rotatedciphertext" {
		t.Errorf("aadhaar ciphertext: want the rotated value, got %q", got)
	}
	version := scanString(ctx, t, pool, `SELECT key_version_id FROM kyc_verifications WHERE subscriber_id = 1`)
	if version != "v2" {
		t.Errorf("key_version_id: want v2, got %q", version)
	}

	t.Run("an empty field does not erase the stored ciphertext", func(t *testing.T) {
		if err := store.UpsertKYC(ctx, 1, "v2:aadhaaronly", "", "v2"); err != nil {
			t.Fatalf("UpsertKYC with empty PAN: %v", err)
		}
		pan := scanString(ctx, t, pool, `SELECT pan_encrypted FROM kyc_verifications WHERE subscriber_id = 1`)
		if pan != "v2:rotatedpan" {
			t.Errorf("PAN must survive an update that omits it, got %q", pan)
		}
	})
}

// ── HealthStore ─────────────────────────────────────────────────────────────

// TestFR_OBS_004_HealthStore_GetSubscriberWithMeta verifies the diagnostic view assembles
// account state, open-ticket count and last notification in one query.
//
// FR-OBS-004
func TestFR_OBS_004_HealthStore_GetSubscriberWithMeta(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	expiry := time.Now().Add(15 * 24 * time.Hour).UTC()
	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 3_543_348_019_200, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{
		Username: "diagnose@isp", Balance: "342.50", PlanExpiry: &expiry,
	})
	seedTemplate(ctx, t, pool, "TMPL-001", "fup_warning", "fup_warning")

	// Two open tickets and one closed: only the open ones should count.
	for _, status := range []string{"open", "in_progress", "resolved"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tickets (subscriber_id, category, description, status)
			VALUES (1, 'connectivity', 'test', $1)`, status); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_log (subscriber_id, channel, template_id, triggered_by_event, sent_at, delivery_status)
		VALUES (1, 'whatsapp', 'TMPL-001', 'fup_warning_80pct', NOW() - INTERVAL '2 hours', 'delivered'),
		       (1, 'whatsapp', 'TMPL-001', 'payment_received',  NOW() - INTERVAL '10 minutes', 'read')`); err != nil {
		t.Fatalf("seed notification log: %v", err)
	}

	rec, err := database.Health().GetSubscriberWithMeta(ctx, 1)
	if err != nil {
		t.Fatalf("GetSubscriberWithMeta: %v", err)
	}

	if rec.Username != "diagnose@isp" {
		t.Errorf("username: got %q", rec.Username)
	}
	if rec.Status != "active" {
		t.Errorf("status: want active, got %q", rec.Status)
	}
	assertDecimalEqual(t, "wallet_balance", rec.WalletBalance, "342.50")
	if rec.OpenTickets != 2 {
		t.Errorf("open_tickets: want 2 (open + in_progress), got %d", rec.OpenTickets)
	}
	if rec.LastNotifEvent != "payment_received" {
		t.Errorf("last notification must be the most recent, got %q", rec.LastNotifEvent)
	}
	if rec.LastNotifAt == nil {
		t.Error("last notification timestamp must be populated")
	}
	if rec.PlanExpiry == nil {
		t.Error("plan_expiry must be populated")
	}

	t.Run("unknown subscriber reports not found", func(t *testing.T) {
		if _, err := database.Health().GetSubscriberWithMeta(ctx, 999999); err == nil {
			t.Error("want an error for an unknown subscriber")
		}
	})
}

// ── PortalStore ─────────────────────────────────────────────────────────────

// TestFR_SUB_001_PortalStore_ScopingAndProfile verifies portal reads are confined to one
// subscriber and expose only self-service fields.
//
// FR-SUB-001, FR-SUB-005
func TestFR_SUB_001_PortalStore_ScopingAndProfile(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	expiry := time.Now().Add(20 * 24 * time.Hour).UTC()
	seedPlan(ctx, t, pool, 1, "100 Mbps Unlimited", "100M/100M", 3_543_348_019_200, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice@isp", Balance: "250.00", PlanExpiry: &expiry})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "bob@isp", Balance: "99.00"})
	seedTemplate(ctx, t, pool, "TMPL-001", "fup_warning", "fup_warning")
	seedTemplate(ctx, t, pool, "TMPL-004", "service_suspended", "soft_suspension")

	store := database.Portal()

	t.Run("login lookup returns only credentials", func(t *testing.T) {
		auth, err := store.GetSubscriberByUsername(ctx, "alice@isp")
		if err != nil {
			t.Fatalf("GetSubscriberByUsername: %v", err)
		}
		if auth == nil || auth.ID != 1 {
			t.Fatalf("want subscriber 1, got %+v", auth)
		}
		if auth.PasswordHash == "" {
			t.Error("password hash must be returned for bcrypt comparison")
		}
	})

	t.Run("unknown user returns nil so login can run a dummy compare", func(t *testing.T) {
		auth, err := store.GetSubscriberByUsername(ctx, "ghost@isp")
		if err != nil || auth != nil {
			t.Errorf("want (nil, nil), got (%+v, %v)", auth, err)
		}
	})

	t.Run("profile carries plan name and balance", func(t *testing.T) {
		profile, err := store.GetSubscriberByID(ctx, 1)
		if err != nil {
			t.Fatalf("GetSubscriberByID: %v", err)
		}
		if profile == nil {
			t.Fatal("want a profile, got nil")
		}
		if profile.PlanName != "100 Mbps Unlimited" {
			t.Errorf("plan_name: got %q", profile.PlanName)
		}
		assertDecimalEqual(t, "wallet_balance", profile.WalletBalance, "250.00")
		if profile.PlanExpiry == nil {
			t.Error("plan_expiry must be populated")
		}
	})

	t.Run("notification history is scoped to the subscriber", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO notification_log (subscriber_id, channel, template_id, triggered_by_event, sent_at, delivery_status)
			VALUES (1, 'whatsapp', 'TMPL-001', 'fup_warning_80pct', NOW() - INTERVAL '3 hours', 'delivered'),
			       (1, 'sms',      NULL,       'payment_reminder',  NOW() - INTERVAL '1 hour',  'sent'),
			       (2, 'whatsapp', 'TMPL-004', 'soft_suspension',   NOW(),                      'read')`); err != nil {
			t.Fatalf("seed notification log: %v", err)
		}

		alice, err := store.ListNotifications(ctx, 1, 50)
		if err != nil {
			t.Fatalf("ListNotifications: %v", err)
		}
		if len(alice) != 2 {
			t.Fatalf("alice must see exactly her 2 rows, got %d", len(alice))
		}
		// Newest first.
		if alice[0].Channel != "sms" {
			t.Errorf("want newest-first ordering, got %q first", alice[0].Channel)
		}
		for _, n := range alice {
			if n.TemplateName == "service_suspended" {
				t.Error("alice received bob's notification")
			}
		}

		bob, err := store.ListNotifications(ctx, 2, 50)
		if err != nil {
			t.Fatalf("ListNotifications for bob: %v", err)
		}
		if len(bob) != 1 {
			t.Errorf("bob must see exactly his 1 row, got %d", len(bob))
		}
	})

	t.Run("ticket history is scoped and creation attributes correctly", func(t *testing.T) {
		created, err := store.CreateTicket(ctx, portal.TicketCreateRequest{
			SubscriberID: 1, Category: "connectivity", Description: "No internet since morning",
		})
		if err != nil {
			t.Fatalf("CreateTicket: %v", err)
		}
		if created.ID == 0 || created.Status != "open" {
			t.Errorf("created ticket: %+v", created)
		}

		if _, err := store.CreateTicket(ctx, portal.TicketCreateRequest{
			SubscriberID: 2, Category: "billing", Description: "Wrong invoice",
		}); err != nil {
			t.Fatalf("CreateTicket for bob: %v", err)
		}

		alice, err := store.ListTickets(ctx, 1)
		if err != nil {
			t.Fatalf("ListTickets: %v", err)
		}
		if len(alice) != 1 || alice[0].Description != "No internet since morning" {
			t.Errorf("alice must see only her ticket, got %+v", alice)
		}
	})

	t.Run("an invalid ticket category is rejected by the schema", func(t *testing.T) {
		_, err := store.CreateTicket(ctx, portal.TicketCreateRequest{
			SubscriberID: 1, Category: "not_a_category", Description: "x",
		})
		if err == nil {
			t.Error("expected the tickets category CHECK to reject an unknown category")
		}
	})
}

// TestFR_SUB_003_PortalStore_GetPlanRenewalInfo verifies the join that
// renewalProcessor.ApplyRenewal (cmd/api/main.go) uses to compute where a
// renewal should extend plan_expiry to.
//
// FR-SUB-003 | MDS §4.9 — portal one-tap renewal
func TestFR_SUB_003_PortalStore_GetPlanRenewalInfo(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	// seedPlan hardcodes validity_days = 30.
	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	store := database.Portal()

	t.Run("returns the plan's validity_days and the subscriber's current plan_expiry", func(t *testing.T) {
		expiry := time.Now().Add(15 * 24 * time.Hour).UTC()
		seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "renewal-info-1@isp", PlanExpiry: &expiry})

		validityDays, currentExpiry, err := store.GetPlanRenewalInfo(ctx, 1)
		if err != nil {
			t.Fatalf("GetPlanRenewalInfo: %v", err)
		}
		if validityDays != 30 {
			t.Errorf("validity_days: want 30, got %d", validityDays)
		}
		if currentExpiry == nil {
			t.Fatal("want a non-nil current expiry")
		}
		if !currentExpiry.Truncate(time.Second).Equal(expiry.Truncate(time.Second)) {
			t.Errorf("current_expiry: want %v, got %v", expiry, *currentExpiry)
		}
	})

	t.Run("a subscriber with no plan_expiry set yet returns a nil pointer", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "renewal-info-2@isp"})

		_, currentExpiry, err := store.GetPlanRenewalInfo(ctx, 2)
		if err != nil {
			t.Fatalf("GetPlanRenewalInfo: %v", err)
		}
		if currentExpiry != nil {
			t.Errorf("want a nil current_expiry, got %v", *currentExpiry)
		}
	})

	t.Run("an unknown subscriber id returns an error", func(t *testing.T) {
		if _, _, err := store.GetPlanRenewalInfo(ctx, 999); err == nil {
			t.Error("expected an error for an unknown subscriber id")
		}
	})
}

// ── Lifecycle (FR-LC-001..002) ──────────────────────────────────────────────

// TestFR_LC_001_APIStore_GetPlanChangeInfo verifies the two plans' price and
// validity are read correctly (not swapped) and that an unknown subscriber
// vs. an unknown plan id are distinguishable — the handler needs 404 for one
// and 422 for the other.
func TestFR_LC_001_APIStore_GetPlanChangeInfo(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Old", "50M/50M", 0, "", "500.00") // validity_days = 30 (seedPlan's fixed value)
	if _, err := pool.Exec(ctx, `
		INSERT INTO plans (id, name, rate_limit_string, volume_gb, fup_threshold_bytes, price, validity_days)
		VALUES (2, 'New', '100M/100M', 3300, 0, 1000.00, 60)`); err != nil {
		t.Fatalf("seed plan 2: %v", err)
	}
	expiry := time.Now().Add(10 * 24 * time.Hour).UTC()
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "planchange@isp", PlanExpiry: &expiry})

	store := database.API()

	t.Run("both plans' price and validity are read, not swapped", func(t *testing.T) {
		info, err := store.GetPlanChangeInfo(ctx, 1, 2)
		if err != nil {
			t.Fatalf("GetPlanChangeInfo: %v", err)
		}
		if info == nil {
			t.Fatal("want a non-nil result for a known subscriber and plan")
		}
		if info.Username != "planchange@isp" {
			t.Errorf("username: got %q", info.Username)
		}
		if !info.OldPrice.Equal(mustDecimal(t, "500.00")) || info.OldValidityDays != 30 {
			t.Errorf("old plan: want price=500.00 validity=30, got price=%s validity=%d", info.OldPrice, info.OldValidityDays)
		}
		if !info.NewPrice.Equal(mustDecimal(t, "1000.00")) || info.NewValidityDays != 60 {
			t.Errorf("new plan: want price=1000.00 validity=60, got price=%s validity=%d", info.NewPrice, info.NewValidityDays)
		}
		if info.CurrentExpiry == nil || !info.CurrentExpiry.Truncate(time.Second).Equal(expiry.Truncate(time.Second)) {
			t.Errorf("current_expiry: want %v, got %v", expiry, info.CurrentExpiry)
		}
	})

	t.Run("unknown subscriber returns (nil, nil)", func(t *testing.T) {
		info, err := store.GetPlanChangeInfo(ctx, 999999, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info != nil {
			t.Errorf("want nil for an unknown subscriber, got %+v", info)
		}
	})

	t.Run("unknown plan returns ErrInvalidPlan", func(t *testing.T) {
		_, err := store.GetPlanChangeInfo(ctx, 1, 999999)
		if !errors.Is(err, api.ErrInvalidPlan) {
			t.Errorf("want ErrInvalidPlan for an unknown new_plan_id, got %v", err)
		}
	})
}

// TestFR_LC_001_APIStore_SetSubscriberPlan verifies plan_id and plan_expiry
// move together in the one statement the proration handler relies on.
func TestFR_LC_001_APIStore_SetSubscriberPlan(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Old", "50M/50M", 0, "", "500.00")
	if _, err := pool.Exec(ctx, `
		INSERT INTO plans (id, name, rate_limit_string, volume_gb, fup_threshold_bytes, price, validity_days)
		VALUES (2, 'New', '100M/100M', 3300, 0, 1000.00, 60)`); err != nil {
		t.Fatalf("seed plan 2: %v", err)
	}
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "setplan@isp"})

	store := database.API()
	newExpiry := time.Now().Add(45 * 24 * time.Hour).UTC()

	rec, err := store.SetSubscriberPlan(ctx, 1, 2, newExpiry)
	if err != nil {
		t.Fatalf("SetSubscriberPlan: %v", err)
	}
	if rec == nil {
		t.Fatal("want a non-nil record")
	}
	if rec.PlanID != 2 {
		t.Errorf("plan_id: want 2, got %d", rec.PlanID)
	}
	if rec.PlanExpiry == nil || !rec.PlanExpiry.Truncate(time.Second).Equal(newExpiry.Truncate(time.Second)) {
		t.Errorf("plan_expiry: want %v, got %v", newExpiry, rec.PlanExpiry)
	}

	t.Run("unknown subscriber returns (nil, nil)", func(t *testing.T) {
		rec, err := store.SetSubscriberPlan(ctx, 999999, 2, newExpiry)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec != nil {
			t.Errorf("want nil for an unknown subscriber, got %+v", rec)
		}
	})
}

// TestFR_LC_002_APIStore_TerminateSubscriber verifies termination sets status
// only — plan_id and wallet_balance, which a refund or historical reporting
// may still need to read, must survive termination untouched.
func TestFR_LC_002_APIStore_TerminateSubscriber(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "terminate@isp", Balance: "250.00"})

	store := database.API()
	rec, err := store.TerminateSubscriber(ctx, 1)
	if err != nil {
		t.Fatalf("TerminateSubscriber: %v", err)
	}
	if rec == nil {
		t.Fatal("want a non-nil record")
	}
	if rec.Status != "terminated" {
		t.Errorf("status: want terminated, got %q", rec.Status)
	}
	if rec.PlanID != 1 {
		t.Errorf("plan_id must survive termination unchanged, got %d", rec.PlanID)
	}
	if rec.WalletBalance != "250.00" {
		t.Errorf("wallet_balance must survive termination unchanged, want 250.00, got %s", rec.WalletBalance)
	}

	t.Run("unknown subscriber returns (nil, nil)", func(t *testing.T) {
		rec, err := store.TerminateSubscriber(ctx, 999999)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec != nil {
			t.Errorf("want nil for an unknown subscriber, got %+v", rec)
		}
	})
}

// TestSubscribers_MobileNumberE164Constraint verifies migration 020's
// DB-level defense-in-depth for E.164 phone format: a malformed number is
// rejected by the database itself, not just by application-level validation
// (internal/api's CreateSubscriber, internal/notifications' send paths) —
// so any future code path that inserts directly still cannot store one.
//
// DoD Phase 2 Step 4 | DBD §6.2
func TestSubscribers_MobileNumberE164Constraint(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")

	t.Run("a non-E.164 mobile_number is rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO subscribers (caf_number, username, password_hash, mobile_number, plan_id, status, registered_state)
			VALUES ('CAF-BAD', 'bad-phone@isp', 'h', '9876543210', 1, 'active', 'TN')`) // missing leading +
		if err == nil {
			t.Fatal("expected the chk_subscribers_mobile_e164 constraint to reject a number missing '+'")
		}
	})

	t.Run("a valid E.164 mobile_number is accepted", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO subscribers (caf_number, username, password_hash, mobile_number, plan_id, status, registered_state)
			VALUES ('CAF-GOOD', 'good-phone@isp', 'h', '+919876543210', 1, 'active', 'TN')`)
		if err != nil {
			t.Fatalf("a valid E.164 number must be accepted: %v", err)
		}
	})
}

// TestFranchises_MobileNumberE164Constraint is the same constraint on the
// franchises table.
func TestFranchises_MobileNumberE164Constraint(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO franchises (name, owner_name, mobile_number, commission_rate_pct, status)
		VALUES ('Bad Franchise', 'Owner', 'not-a-phone', 10.00, 'active')`)
	if err == nil {
		t.Fatal("expected the chk_franchises_mobile_e164 constraint to reject a malformed number")
	}
}
