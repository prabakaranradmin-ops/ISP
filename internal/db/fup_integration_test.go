//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

const (
	quota3TB = int64(3_543_348_019_200)
)

// TestFR_FUP_001_FUPStore_BreachDetection verifies the scanner query selects exactly the
// subscribers who have breached quota and are not already throttled.
//
// FR-FUP-001 | INT-FUP-001
func TestFR_FUP_001_FUPStore_BreachDetection(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", quota3TB, "10M/10M", "799.00")
	seedPlan(ctx, t, pool, 2, "Unlimited", "200M/200M", 0, "", "1199.00")

	// 1: over quota, not yet throttled  -> must be selected
	// 2: over quota, already throttled  -> must not be re-selected
	// 3: under quota                    -> must not be selected
	// 4: unlimited plan                 -> must not be selected
	// 5: over quota but terminated      -> must not be selected
	// 6: over quota but offline         -> must not be selected
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "breach@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "throttled@isp", PlanID: 1, FUPActive: true})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "under@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "unlimited@isp", PlanID: 2})
	seedSubscriber(ctx, t, pool, 5, seedOpts{Username: "gone@isp", PlanID: 1, Status: "terminated"})
	seedSubscriber(ctx, t, pool, 6, seedOpts{Username: "offline@isp", PlanID: 1})

	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", quota3TB/2, quota3TB/2+1)
	seedSession(ctx, t, pool, 2, "s-2", "10.10.0.2", quota3TB, 1)
	seedSession(ctx, t, pool, 3, "s-3", "10.10.0.3", 1_000_000, 1_000_000)
	seedSession(ctx, t, pool, 4, "s-4", "10.10.0.4", quota3TB*2, 0)
	seedSession(ctx, t, pool, 5, "s-5", "10.10.0.5", quota3TB*2, 0)
	// subscriber 6 has no open session

	store := database.FUP()

	sessions, err := store.GetActiveSessionsAboveFUP(ctx)
	if err != nil {
		t.Fatalf("GetActiveSessionsAboveFUP: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want exactly 1 breaching session, got %d: %+v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.SubscriberID != 1 {
		t.Errorf("want subscriber 1, got %d", got.SubscriberID)
	}
	if got.Username != "breach@isp" {
		t.Errorf("username: got %q", got.Username)
	}
	if got.NasIP != "10.10.0.1" {
		t.Errorf("nas_ip: want 10.10.0.1, got %q", got.NasIP)
	}
	if got.FUPThreshold != quota3TB {
		t.Errorf("threshold: want %d, got %d", quota3TB, got.FUPThreshold)
	}
	if got.BytesUsed < quota3TB {
		t.Errorf("bytes_used must be at or above quota, got %d", got.BytesUsed)
	}
	if got.FUPActive {
		t.Error("selected subscriber must not already be throttled")
	}
}

// TestFR_FUP_004_FUPStore_WarningBand verifies the 80% warning query selects only
// subscribers between the warning threshold and their quota.
//
// FR-FUP-004 | INT-FUP-002
func TestFR_FUP_004_FUPStore_WarningBand(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", quota3TB, "10M/10M", "799.00")

	// 1: 82% -> warn
	// 2: 79% -> too early
	// 3: 105% -> already breached, belongs to the CoA path not the warning path
	// 4: 85% but already throttled -> no warning
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "warn@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "early@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "breached@isp"})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "already@isp", FUPActive: true})

	seedSession(ctx, t, pool, 1, "w-1", "10.10.0.1", quota3TB*82/100, 0)
	seedSession(ctx, t, pool, 2, "w-2", "10.10.0.2", quota3TB*79/100, 0)
	seedSession(ctx, t, pool, 3, "w-3", "10.10.0.3", quota3TB*105/100, 0)
	seedSession(ctx, t, pool, 4, "w-4", "10.10.0.4", quota3TB*85/100, 0)

	sessions, err := database.FUP().GetSessionsAtWarning(ctx, 80)
	if err != nil {
		t.Fatalf("GetSessionsAtWarning: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want exactly 1 session in the warning band, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].SubscriberID != 1 {
		t.Errorf("want subscriber 1 (82%%), got %d", sessions[0].SubscriberID)
	}
}

// TestFR_FUP_001_FUPStore_SetFUPActive verifies the throttle flag persists and removes the
// subscriber from the next breach scan.
//
// FR-FUP-001
func TestFR_FUP_001_FUPStore_SetFUPActive(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", quota3TB, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "toggle@isp"})
	seedSession(ctx, t, pool, 1, "t-1", "10.10.0.1", quota3TB+1, 0)

	store := database.FUP()

	before, err := store.GetActiveSessionsAboveFUP(ctx)
	if err != nil {
		t.Fatalf("scan before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("precondition: want 1 breaching session, got %d", len(before))
	}

	if err := store.SetFUPActive(ctx, 1, true); err != nil {
		t.Fatalf("SetFUPActive: %v", err)
	}

	after, err := store.GetActiveSessionsAboveFUP(ctx)
	if err != nil {
		t.Fatalf("scan after: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a throttled subscriber must not be re-selected, got %d", len(after))
	}

	t.Run("clearing the flag re-arms detection", func(t *testing.T) {
		if err := store.SetFUPActive(ctx, 1, false); err != nil {
			t.Fatalf("SetFUPActive false: %v", err)
		}
		again, err := store.GetActiveSessionsAboveFUP(ctx)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(again) != 1 {
			t.Errorf("want the subscriber selected again after clearing, got %d", len(again))
		}
	})

	t.Run("unknown subscriber reports not found", func(t *testing.T) {
		if err := store.SetFUPActive(ctx, 999999, true); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})
}

// TestFR_FUP_002_FUPStore_GetSubscriberNASSession verifies the CoA lookup returns the
// throttled profile once the subscriber is flagged, which is what makes the CoA
// actually reduce their speed.
//
// FR-FUP-002 | INT-FUP-003
func TestFR_FUP_002_FUPStore_GetSubscriberNASSession(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", quota3TB, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "coa@isp"})
	seedSession(ctx, t, pool, 1, "coa-session-1", "10.10.0.7", quota3TB+1, 0)

	store := database.FUP()

	nasIP, sessionID, rateLimit, planID, err := store.GetSubscriberNASSession(ctx, 1)
	if err != nil {
		t.Fatalf("GetSubscriberNASSession: %v", err)
	}
	if nasIP != "10.10.0.7" {
		t.Errorf("nas_ip: want 10.10.0.7, got %q", nasIP)
	}
	if sessionID != "coa-session-1" {
		t.Errorf("session_id: want coa-session-1, got %q", sessionID)
	}
	if rateLimit != "100M/100M" {
		t.Errorf("before throttling the full rate applies, got %q", rateLimit)
	}
	if planID != 1 {
		t.Errorf("plan_id: want 1, got %d", planID)
	}

	if err := store.SetFUPActive(ctx, 1, true); err != nil {
		t.Fatalf("SetFUPActive: %v", err)
	}
	_, _, rateLimit, _, err = store.GetSubscriberNASSession(ctx, 1)
	if err != nil {
		t.Fatalf("GetSubscriberNASSession after throttle: %v", err)
	}
	if rateLimit != "10M/10M" {
		t.Errorf("throttled subscriber must get the FUP profile, got %q", rateLimit)
	}

	t.Run("no live session reports not found", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "offline-coa@isp"})
		if _, _, _, _, err := store.GetSubscriberNASSession(ctx, 2); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("want ErrNotFound for a subscriber with no open session, got %v", err)
		}
	})
}

// TestFR_AAA_003_FUPStore_SessionLifecycle verifies accounting start, interim update and
// stop move a session through the partitioned history table.
//
// FR-AAA-003, FR-NET-001
func TestFR_AAA_003_FUPStore_SessionLifecycle(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", quota3TB, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "session@isp"})

	store := database.FUP()

	if err := store.StartSession(ctx, 1, "live-1", "10.10.0.9", "100.64.1.2", ""); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM subscriber_session_history WHERE stop_time IS NULL`); n != 1 {
		t.Fatalf("want 1 open session, got %d", n)
	}

	matched, err := store.UpdateSessionOctets(ctx, "live-1", 1_000_000, 2_000_000)
	if err != nil {
		t.Fatalf("UpdateSessionOctets: %v", err)
	}
	if !matched {
		t.Error("an interim update for an open session must report that it matched one")
	}
	used := countRows(ctx, t, pool, `SELECT input_octets + output_octets FROM subscriber_session_history WHERE session_id = 'live-1'`)
	if used != 3_000_000 {
		t.Errorf("usage: want 3000000, got %d", used)
	}

	stopped, err := store.StopSession(ctx, "live-1", 5_000_000, 6_000_000, "User-Request")
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if !stopped {
		t.Error("stopping an open session must report that it matched one")
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM subscriber_session_history WHERE stop_time IS NULL`); n != 0 {
		t.Errorf("session must be closed, %d still open", n)
	}
	cause := scanString(ctx, t, pool, `SELECT terminate_cause FROM subscriber_session_history WHERE session_id = 'live-1'`)
	if cause != "User-Request" {
		t.Errorf("terminate_cause: want User-Request, got %q", cause)
	}

	t.Run("a closed session no longer counts toward FUP", func(t *testing.T) {
		sessions, err := store.GetActiveSessionsAboveFUP(ctx)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(sessions) != 0 {
			t.Errorf("closed sessions must not appear in the FUP scan, got %d", len(sessions))
		}
	})
}

// TestFR_NOTIF_009_NotificationStore verifies dispatch logging and forward-only delivery
// status transitions.
//
// FR-NOTIF-009, FR-NOTIF-011 | INT-NOTIF-002, INT-NOTIF-003
func TestFR_NOTIF_009_NotificationStore(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "notify@isp", MobileNumber: "+919876543210"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "dnd@isp", DndOptOut: true})
	seedTemplate(ctx, t, pool, "TMPL-001", "fup_warning", "fup_warning")

	store := database.Notifications()

	t.Run("subscriber lookup carries DND flag and number", func(t *testing.T) {
		sub, err := store.GetSubscriber(ctx, 1)
		if err != nil {
			t.Fatalf("GetSubscriber: %v", err)
		}
		if sub.MobileNumber != "+919876543210" {
			t.Errorf("mobile_number: got %q", sub.MobileNumber)
		}
		if sub.DndOptOut {
			t.Error("subscriber 1 is not DND")
		}

		dnd, err := store.GetSubscriber(ctx, 2)
		if err != nil {
			t.Fatalf("GetSubscriber dnd: %v", err)
		}
		if !dnd.DndOptOut {
			t.Error("subscriber 2 must report dnd_opt_out")
		}
	})

	t.Run("dispatch is logged with the provider message id", func(t *testing.T) {
		err := store.CreateNotificationLog(ctx, notifications.NotificationLog{
			SubscriberID:      1,
			Channel:           "whatsapp",
			TemplateID:        "TMPL-001",
			TriggeredByEvent:  "fup_warning_80pct",
			ProviderMessageID: "wamid.abc123",
			DeliveryStatus:    "sent",
		})
		if err != nil {
			t.Fatalf("CreateNotificationLog: %v", err)
		}
		status := scanString(ctx, t, pool, `SELECT delivery_status FROM notification_log WHERE provider_message_id = 'wamid.abc123'`)
		if status != "sent" {
			t.Errorf("delivery_status: want sent, got %q", status)
		}
	})

	t.Run("an unregistered template is stored as NULL rather than losing the row", func(t *testing.T) {
		err := store.CreateNotificationLog(ctx, notifications.NotificationLog{
			SubscriberID:      1,
			Channel:           "sms",
			TemplateID:        "TMPL-DOES-NOT-EXIST",
			TriggeredByEvent:  "adhoc",
			ProviderMessageID: "sms-1",
			DeliveryStatus:    "sent",
		})
		if err != nil {
			t.Fatalf("CreateNotificationLog with unknown template: %v", err)
		}
		if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM notification_log WHERE provider_message_id = 'sms-1' AND template_id IS NULL`); n != 1 {
			t.Error("want the audit row kept with a NULL template_id")
		}
	})

	t.Run("suppression is recorded", func(t *testing.T) {
		err := store.CreateNotificationLog(ctx, notifications.NotificationLog{
			SubscriberID:     2,
			Channel:          "whatsapp",
			TemplateID:       "TMPL-001",
			TriggeredByEvent: "promotional_campaign",
			DeliveryStatus:   "suppressed_dnd",
		})
		if err != nil {
			t.Fatalf("CreateNotificationLog suppressed: %v", err)
		}
		if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM notification_log WHERE subscriber_id = 2 AND delivery_status = 'suppressed_dnd'`); n != 1 {
			t.Error("want a suppressed_dnd row for the DND subscriber")
		}
	})

	t.Run("delivery status advances forward only", func(t *testing.T) {
		if err := store.UpdateDeliveryStatus(ctx, "wamid.abc123", "delivered"); err != nil {
			t.Fatalf("UpdateDeliveryStatus delivered: %v", err)
		}
		if got := scanString(ctx, t, pool, `SELECT delivery_status FROM notification_log WHERE provider_message_id = 'wamid.abc123'`); got != "delivered" {
			t.Fatalf("want delivered, got %q", got)
		}

		if err := store.UpdateDeliveryStatus(ctx, "wamid.abc123", "read"); err != nil {
			t.Fatalf("UpdateDeliveryStatus read: %v", err)
		}
		if got := scanString(ctx, t, pool, `SELECT delivery_status FROM notification_log WHERE provider_message_id = 'wamid.abc123'`); got != "read" {
			t.Fatalf("want read, got %q", got)
		}

		// Meta delivers at-least-once and out of order; a late 'sent' must not
		// undo a 'read' that already arrived.
		if err := store.UpdateDeliveryStatus(ctx, "wamid.abc123", "sent"); err != nil {
			t.Fatalf("late duplicate must not error: %v", err)
		}
		if got := scanString(ctx, t, pool, `SELECT delivery_status FROM notification_log WHERE provider_message_id = 'wamid.abc123'`); got != "read" {
			t.Errorf("an out-of-order callback must not regress status, got %q", got)
		}
	})

	t.Run("a callback for an unknown message is not an error", func(t *testing.T) {
		if err := store.UpdateDeliveryStatus(ctx, "wamid.never-sent", "delivered"); err != nil {
			t.Errorf("unknown provider_message_id must not error, got %v", err)
		}
	})
}
