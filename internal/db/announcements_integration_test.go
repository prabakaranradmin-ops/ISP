//go:build integration

// Announcement persistence tests — FR-ANN-001..002 | MDS §4.17.
//
// Two properties carry the weight: the segment query must address exactly
// the subscribers it claims to (a broadcast aimed at one franchise landing
// on the whole base is not a cosmetic bug), and the send claim must be
// atomic so a double-click cannot broadcast twice.
package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

func draftAnnouncement(channels []string) notifications.Announcement {
	return notifications.Announcement{
		Title: "Scheduled maintenance", Body: "Service will be briefly interrupted.",
		Channels: channels, Class: "marketing", CreatedBy: "ops1",
	}
}

func TestFR_ANN_001_CreateAndReadAnnouncement(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Announcements()

	created, err := store.CreateAnnouncement(ctx, draftAnnouncement([]string{"sms", "email"}))
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}
	if created.Status != notifications.AnnouncementDraft {
		t.Errorf("a new announcement must start as a draft, got %q", created.Status)
	}
	if len(created.Channels) != 2 {
		t.Errorf("channels round trip: got %v", created.Channels)
	}
	if created.RecipientCount != 0 {
		t.Errorf("recipient_count should start at 0, got %d", created.RecipientCount)
	}

	got, err := store.GetAnnouncement(ctx, created.ID)
	if err != nil || got == nil {
		t.Fatalf("GetAnnouncement: %v (got %v)", err, got)
	}

	t.Run("unknown id returns (nil, nil)", func(t *testing.T) {
		a, err := store.GetAnnouncement(ctx, 999999)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a != nil {
			t.Errorf("want nil, got %+v", a)
		}
	})
}

// TestFR_ANN_001_SchemaRejectsAnnouncementWithNoDestination: a broadcast
// addressed to nothing would report success having reached nobody.
func TestFR_ANN_001_SchemaRejectsAnnouncementWithNoDestination(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO announcements (title, body, channels, show_in_portal, created_by_username)
		VALUES ('X', 'Y', ARRAY[]::TEXT[], FALSE, 'ops1')`)
	if err == nil {
		t.Fatal("chk_announcement_has_destination must reject an announcement with no channel and no banner")
	}
}

// TestFR_ANN_001_SchemaRejectsUnknownChannel keeps 'portal' and typos out of
// the channels array — a value the dispatcher would reject at delivery time,
// long after the operator thought the broadcast was configured.
func TestFR_ANN_001_SchemaRejectsUnknownChannel(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO announcements (title, body, channels, created_by_username)
		VALUES ('X', 'Y', ARRAY['portal']::TEXT[], 'ops1')`)
	if err == nil {
		t.Fatal("chk_announcement_channels must reject a channel that is not dispatchable")
	}
}

// TestFR_ANN_001_SegmentAddressesExactlyTheIntendedSubscribers is the test
// that matters most: each filter must narrow the audience, and an unset
// filter must not.
func TestFR_ANN_001_SegmentAddressesExactlyTheIntendedSubscribers(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Chennai North", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "Chennai South", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Super", "100M/100M", 0, "", "799.00")

	f1, f2 := 1, 2
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp", FranchiseID: &f1, PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b@isp", FranchiseID: &f1, PlanID: 2})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "c@isp", FranchiseID: &f2, PlanID: 1})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "d@isp", PlanID: 1, Status: "hard_suspended"})
	// Terminated subscribers have left: broadcasting to them is at best
	// noise and at worst a compliance problem.
	seedSubscriber(ctx, t, pool, 5, seedOpts{Username: "e@isp", PlanID: 1, Status: "terminated"})

	store := database.Announcements()

	// ListSegmentSubscriberIDs checks announcement_recipients first and only
	// falls through to the segment filters below when that table holds
	// nothing for the id (migration 041 — an announcement may instead name
	// an explicit console-selected list). No announcement has id 0, so no
	// recipient row can ever match it: these subtests are about the segment
	// query, and this keeps them on that path. The explicit-recipient path
	// is covered separately by
	// TestFR_ANN_001_ExplicitRecipientsOverrideSegmentFilters.
	const segmentOnly = 0

	t.Run("no filters addresses everyone except terminated", func(t *testing.T) {
		ids, err := store.ListSegmentSubscriberIDs(ctx, segmentOnly, nil, nil, nil)
		if err != nil {
			t.Fatalf("ListSegmentSubscriberIDs: %v", err)
		}
		if len(ids) != 4 {
			t.Errorf("want 4 (5 minus the terminated one), got %d: %v", len(ids), ids)
		}
		for _, id := range ids {
			if id == 5 {
				t.Error("a terminated subscriber must never be in a broadcast segment")
			}
		}
	})

	t.Run("franchise filter narrows to that partner", func(t *testing.T) {
		ids, err := store.ListSegmentSubscriberIDs(ctx, segmentOnly, &f1, nil, nil)
		if err != nil {
			t.Fatalf("ListSegmentSubscriberIDs: %v", err)
		}
		if len(ids) != 2 {
			t.Errorf("franchise 1 should have 2 subscribers, got %d: %v", len(ids), ids)
		}
	})

	t.Run("plan filter narrows to that plan", func(t *testing.T) {
		planID := 2
		ids, err := store.ListSegmentSubscriberIDs(ctx, segmentOnly, nil, &planID, nil)
		if err != nil {
			t.Fatalf("ListSegmentSubscriberIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != 2 {
			t.Errorf("plan 2 should have exactly subscriber 2, got %v", ids)
		}
	})

	t.Run("status filter narrows to that status", func(t *testing.T) {
		status := "hard_suspended"
		ids, err := store.ListSegmentSubscriberIDs(ctx, segmentOnly, nil, nil, &status)
		if err != nil {
			t.Fatalf("ListSegmentSubscriberIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != 4 {
			t.Errorf("want exactly the suspended subscriber, got %v", ids)
		}
	})

	t.Run("filters compose rather than override one another", func(t *testing.T) {
		planID := 1
		ids, err := store.ListSegmentSubscriberIDs(ctx, segmentOnly, &f1, &planID, nil)
		if err != nil {
			t.Fatalf("ListSegmentSubscriberIDs: %v", err)
		}
		if len(ids) != 1 || ids[0] != 1 {
			t.Errorf("franchise 1 + plan 1 should be exactly subscriber 1, got %v", ids)
		}
	})
}

// TestFR_ANN_001_ConcurrentSendsClaimExactlyOnce is the double-broadcast
// guard. Without the conditional claim, a double-click sends the same
// message to the whole segment twice.
func TestFR_ANN_001_ConcurrentSendsClaimExactlyOnce(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Announcements()

	created, err := store.CreateAnnouncement(ctx, draftAnnouncement([]string{"sms"}))
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, err := store.ClaimAnnouncementForSending(ctx, created.ID)
			if err != nil {
				t.Errorf("ClaimAnnouncementForSending: %v", err)
				return
			}
			if claimed != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("DOUBLE BROADCAST: %d of %d concurrent sends were allowed to proceed, want exactly 1", winners, racers)
	}
}

func TestFR_ANN_001_ClaimingANonDraftDoesNotLand(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Announcements()

	created, err := store.CreateAnnouncement(ctx, draftAnnouncement([]string{"sms"}))
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}
	if _, err := store.ClaimAnnouncementForSending(ctx, created.ID); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := store.FinishAnnouncement(ctx, created.ID, notifications.AnnouncementSent, 12); err != nil {
		t.Fatalf("FinishAnnouncement: %v", err)
	}

	again, err := store.ClaimAnnouncementForSending(ctx, created.ID)
	if err != nil {
		t.Fatalf("re-claiming a sent announcement must not error: %v", err)
	}
	if again != nil {
		t.Error("an already-sent announcement must not be re-claimable")
	}

	final, err := store.GetAnnouncement(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAnnouncement: %v", err)
	}
	if final.Status != notifications.AnnouncementSent || final.RecipientCount != 12 {
		t.Errorf("outcome not recorded: status=%q recipients=%d", final.Status, final.RecipientCount)
	}
	if final.SentAt == nil {
		t.Error("a sent announcement must be timestamped")
	}
}

// TestFR_ANN_001_PortalBannersAreSegmentScoped: a banner aimed at one
// franchise must not appear on every subscriber's dashboard.
func TestFR_ANN_001_PortalBannersAreSegmentScoped(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Chennai North", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "Chennai South", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	f1, f2 := 1, 2
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp", FranchiseID: &f1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "theirs@isp", FranchiseID: &f2})

	store := database.Announcements()

	// One targeted at franchise 1, one for everybody.
	targeted := draftAnnouncement([]string{"sms"})
	targeted.ShowInPortal = true
	targeted.SegmentFranchiseID = &f1
	t1, err := store.CreateAnnouncement(ctx, targeted)
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}

	global := draftAnnouncement([]string{"sms"})
	global.ShowInPortal = true
	g1, err := store.CreateAnnouncement(ctx, global)
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}

	for _, id := range []int{t1.ID, g1.ID} {
		if _, err := store.ClaimAnnouncementForSending(ctx, id); err != nil {
			t.Fatalf("claim %d: %v", id, err)
		}
		if err := store.FinishAnnouncement(ctx, id, notifications.AnnouncementSent, 1); err != nil {
			t.Fatalf("finish %d: %v", id, err)
		}
	}

	mine, err := store.ListPortalAnnouncements(ctx, 1)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements(1): %v", err)
	}
	if len(mine) != 2 {
		t.Errorf("franchise 1's subscriber should see both the targeted and the global banner, got %d", len(mine))
	}

	theirs, err := store.ListPortalAnnouncements(ctx, 2)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements(2): %v", err)
	}
	if len(theirs) != 1 {
		t.Errorf("franchise 2's subscriber should see only the global banner, got %d: %+v", len(theirs), theirs)
	}
}

// TestFR_ANN_001_DraftBannersAreNotVisible: a banner must not appear until
// it has actually been sent, or drafting one publishes it.
func TestFR_ANN_001_DraftBannersAreNotVisible(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "reader@isp"})

	store := database.Announcements()
	a := draftAnnouncement([]string{"sms"})
	a.ShowInPortal = true
	if _, err := store.CreateAnnouncement(ctx, a); err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}

	banners, err := store.ListPortalAnnouncements(ctx, 1)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements: %v", err)
	}
	if len(banners) != 0 {
		t.Errorf("a draft banner must not be visible to subscribers, got %d", len(banners))
	}
}

// ── FR-NOTIF-013: push token storage ────────────────────────────────────────

func TestFR_NOTIF_013_PushTokenRegistrationIsIdempotentPerDevice(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "app@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "other@isp"})

	store := database.Notifications()

	if err := store.RegisterPushToken(ctx, 1, "device-token-abc", "android"); err != nil {
		t.Fatalf("RegisterPushToken: %v", err)
	}
	// The same physical device re-registering after a reinstall must update,
	// not accumulate — duplicates would each receive a copy of every
	// notification.
	if err := store.RegisterPushToken(ctx, 1, "device-token-abc", "android"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	tokens, err := store.ListPushTokens(ctx, 1)
	if err != nil {
		t.Fatalf("ListPushTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("want 1 token after re-registering the same device, got %d", len(tokens))
	}

	t.Run("a device moving to another subscriber follows it", func(t *testing.T) {
		if err := store.RegisterPushToken(ctx, 2, "device-token-abc", "android"); err != nil {
			t.Fatalf("re-register under a new owner: %v", err)
		}
		oldOwner, err := store.ListPushTokens(ctx, 1)
		if err != nil {
			t.Fatalf("ListPushTokens(1): %v", err)
		}
		if len(oldOwner) != 0 {
			t.Errorf("the previous owner must no longer receive that device's notifications, got %v", oldOwner)
		}
		newOwner, err := store.ListPushTokens(ctx, 2)
		if err != nil {
			t.Fatalf("ListPushTokens(2): %v", err)
		}
		if len(newOwner) != 1 {
			t.Errorf("the new owner should have the token, got %v", newOwner)
		}
	})

	t.Run("a subscriber with no device returns empty, not an error", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "nodevice@isp"})
		tokens, err := store.ListPushTokens(ctx, 3)
		if err != nil {
			t.Fatalf("ListPushTokens: %v", err)
		}
		if len(tokens) != 0 {
			t.Errorf("want no tokens, got %v", tokens)
		}
	})
}

// TestFR_NOTIF_012_NotificationLogAcceptsEmailAndPush verifies migration
// 028's widened CHECK — without it FR-NOTIF-009's "every notification
// creates a log record" would be unsatisfiable for push.
func TestFR_NOTIF_012_NotificationLogAcceptsEmailAndPush(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "log@isp"})

	store := database.Notifications()
	for _, channel := range []string{"whatsapp", "sms", "email", "push"} {
		err := store.CreateNotificationLog(ctx, notifications.NotificationLog{
			SubscriberID: 1, Channel: channel, TriggeredByEvent: "test",
			DeliveryStatus: "sent",
		})
		if err != nil {
			t.Errorf("channel %q must be loggable: %v", channel, err)
		}
	}

	t.Run("failure_reason is persisted", func(t *testing.T) {
		if err := store.CreateNotificationLog(ctx, notifications.NotificationLog{
			SubscriberID: 1, Channel: "email", TriggeredByEvent: "test",
			DeliveryStatus: "failed", FailureReason: "subscriber has no email address",
		}); err != nil {
			t.Fatalf("CreateNotificationLog: %v", err)
		}
		reason := scanString(ctx, t, pool,
			`SELECT COALESCE(failure_reason,'') FROM notification_log
			  WHERE delivery_status='failed' ORDER BY id DESC LIMIT 1`)
		if reason != "subscriber has no email address" {
			t.Errorf("failure_reason = %q, want the recorded reason", reason)
		}
	})
}
