//go:build integration

// An announcement's reach must never widen after it is sent.
//
// A NULL segment means "no restriction on this dimension", so anything that
// leaves an announcement with no segment and no recipient list turns it into
// a message to the whole base: a banner meant for three customers in arrears
// shows on every subscriber's portal. Three routes led there, and each test
// below is one of them:
//
//   - a list announcement (the console's bulk "Notify these") shown as a
//     portal banner, which the portal read did not scope at all;
//   - a list announcement whose recipients were later deleted, because
//     announcement_recipients cascades and "has no recipient rows" was taken
//     to mean "is a segment announcement";
//   - deleting the plan or franchise a segment named, which nulled it
//     (migration 028's ON DELETE SET NULL).
package db_test

import (
	"context"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

func sendAnnouncement(ctx context.Context, t *testing.T, store interface {
	ClaimAnnouncementForSending(context.Context, int) (*notifications.Announcement, error)
	FinishAnnouncement(context.Context, int, string, int) error
}, id int) {
	t.Helper()
	claimed, err := store.ClaimAnnouncementForSending(ctx, id)
	if err != nil || claimed == nil {
		t.Fatalf("claim %d: %v (claimed %v)", id, err, claimed)
	}
	if err := store.FinishAnnouncement(ctx, id, notifications.AnnouncementSent, 1); err != nil {
		t.Fatalf("finish %d: %v", id, err)
	}
}

func portalBanner(title string) notifications.Announcement {
	return notifications.Announcement{
		Title: title, Body: "Please clear your dues to avoid disconnection.",
		Channels: []string{}, Class: "transactional", CreatedBy: "billing1",
		ShowInPortal: true,
	}
}

func TestAnnouncementReach_ListBannerIsShownOnlyToItsRecipients(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "owes@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "paid-up@isp"})

	store := database.Announcements()
	a := portalBanner("Payment overdue")
	a.SubscriberIDs = []int{1}
	created, err := store.CreateAnnouncement(ctx, a)
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}
	if !created.AddressedToList {
		t.Error("an announcement created with a recipient list must say so on its own row")
	}
	sendAnnouncement(ctx, t, store, created.ID)

	mine, err := store.ListPortalAnnouncements(ctx, 1)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements(1): %v", err)
	}
	if len(mine) != 1 {
		t.Errorf("the recipient should see the banner, got %d", len(mine))
	}
	theirs, err := store.ListPortalAnnouncements(ctx, 2)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements(2): %v", err)
	}
	if len(theirs) != 0 {
		t.Errorf("a banner addressed to subscriber 1 is showing on subscriber 2's portal: %+v", theirs)
	}
}

func TestAnnouncementReach_ListWhoseRecipientsAreGoneReachesNobody(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "bystander@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "leaving@isp"})

	store := database.Announcements()
	listed := portalBanner("Your router is being replaced")
	listed.Channels = []string{"sms"}
	listed.SubscriberIDs = []int{2}

	draft, err := store.CreateAnnouncement(ctx, listed)
	if err != nil {
		t.Fatalf("CreateAnnouncement (draft): %v", err)
	}
	sent, err := store.CreateAnnouncement(ctx, listed)
	if err != nil {
		t.Fatalf("CreateAnnouncement (sent): %v", err)
	}
	sendAnnouncement(ctx, t, store, sent.ID)

	// The only recipient goes; announcement_recipients cascades with them.
	if _, err := pool.Exec(ctx, `DELETE FROM subscribers WHERE id = 2`); err != nil {
		t.Fatalf("delete subscriber: %v", err)
	}

	ids, err := store.ListSegmentSubscriberIDs(ctx, draft.ID, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("ListSegmentSubscriberIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("sending a list announcement whose recipients are gone would message %v; it must reach nobody", ids)
	}

	banners, err := store.ListPortalAnnouncements(ctx, 1)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements: %v", err)
	}
	if len(banners) != 0 {
		t.Errorf("a list banner whose recipients are gone is showing on a bystander's portal: %+v", banners)
	}
}

func TestAnnouncementReach_ATargetedPlanOrFranchiseCannotBeDeleted(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedFranchise(ctx, t, pool, 1, "Chennai North", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Retired promo", "100M/100M", 0, "", "799.00")
	seedPlan(ctx, t, pool, 3, "Unused", "10M/10M", 0, "", "199.00")

	store := database.Announcements()
	byPlan := portalBanner("Promo ending")
	plan := 2
	byPlan.SegmentPlanID = &plan
	if _, err := store.CreateAnnouncement(ctx, byPlan); err != nil {
		t.Fatalf("CreateAnnouncement (plan): %v", err)
	}
	byFranchise := portalBanner("Area outage")
	franchise := 1
	byFranchise.SegmentFranchiseID = &franchise
	if _, err := store.CreateAnnouncement(ctx, byFranchise); err != nil {
		t.Fatalf("CreateAnnouncement (franchise): %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM plans WHERE id = 2`); err == nil {
		t.Error("deleting a plan an announcement is addressed to must be refused, not widen the announcement to everyone")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM franchises WHERE id = 1`); err == nil {
		t.Error("deleting a franchise an announcement is addressed to must be refused, not widen the announcement to everyone")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM plans WHERE id = 3`); err != nil {
		t.Errorf("a plan no announcement names must still be deletable: %v", err)
	}
}

func TestAnnouncementReach_AListCannotAlsoCarryASegment(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO announcements (title, body, channels, show_in_portal, created_by_username,
		                           addressed_to_list, segment_status)
		VALUES ('X', 'Y', ARRAY['sms']::TEXT[], FALSE, 'ops1', TRUE, 'active')`)
	if err == nil {
		t.Error("an announcement addressed to a list must not also carry a segment: which one decides its reach would be ambiguous")
	}
}

// Removing demo data deletes the demo plans. An announcement addressed to one
// of them used to survive with its segment nulled — addressed to everyone.
func TestAnnouncementReach_RemovingDemoDataTakesItsAnnouncementsWithIt(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Demo Fiber 100", "100M/100M", 0, "", "999.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "real-customer@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "demo-customer@isp", PlanID: 2})

	demo := database.Demo()
	if err := demo.MarkPlanDemo(ctx, 2); err != nil {
		t.Fatalf("MarkPlanDemo: %v", err)
	}
	if err := demo.MarkSubscriberDemo(ctx, 2); err != nil {
		t.Fatalf("MarkSubscriberDemo: %v", err)
	}

	store := database.Announcements()
	walkthrough := portalBanner("Upgrade to Demo Fiber 100")
	demoPlan := 2
	walkthrough.SegmentPlanID = &demoPlan
	created, err := store.CreateAnnouncement(ctx, walkthrough)
	if err != nil {
		t.Fatalf("CreateAnnouncement: %v", err)
	}
	sendAnnouncement(ctx, t, store, created.ID)

	// Addressed to everyone, for contrast: removal must leave it alone.
	everyone, err := store.CreateAnnouncement(ctx, portalBanner("Office closed on Monday"))
	if err != nil {
		t.Fatalf("CreateAnnouncement (everyone): %v", err)
	}
	sendAnnouncement(ctx, t, store, everyone.ID)

	if err := demo.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	banners, err := store.ListPortalAnnouncements(ctx, 1)
	if err != nil {
		t.Fatalf("ListPortalAnnouncements: %v", err)
	}
	for _, b := range banners {
		if b.ID == created.ID {
			t.Errorf("after removing demo data, the demo plan's banner shows on a real customer's portal")
		}
	}
	if len(banners) != 1 || banners[0].ID != everyone.ID {
		t.Errorf("the real customer should still see exactly the banner addressed to everyone, got %+v", banners)
	}
	if got, err := store.GetAnnouncement(ctx, created.ID); err != nil || got != nil {
		t.Errorf("the demo plan's announcement should be removed with the demo data, got %+v (err %v)", got, err)
	}
}

// The other direction, and the one no test covered: a real segment
// announcement must still fall through to its segment filters.
//
// Every subtest in TestFR_ANN_001_SegmentAddressesExactlyTheIntendedSubscribers
// passes announcement id 0 deliberately, so that no recipient row can match
// and the segment query is what is under test. The cost is that none of them
// resolves an announcement that exists, and the list/segment decision is made
// from the row. Dropping that decision — reading "no recipient rows" as "a
// list that reaches nobody" — makes every segment announcement send to zero
// subscribers, and the whole suite still passes, because pgx scans the empty
// recipient array into a non-nil empty slice rather than nil. This test is
// the one that fails.
func TestAnnouncementReach_ASegmentAnnouncementStillReachesItsSegment(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Super", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "on-basic@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "also-basic@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "on-super@isp", PlanID: 2})

	store := database.Announcements()

	everyone, err := store.CreateAnnouncement(ctx, portalBanner("Office closed on Monday"))
	if err != nil {
		t.Fatalf("CreateAnnouncement (everyone): %v", err)
	}
	if everyone.AddressedToList {
		t.Error("an announcement created without a recipient list must not be marked as addressed to one")
	}
	ids, err := store.ListSegmentSubscriberIDs(ctx, everyone.ID, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("ListSegmentSubscriberIDs (everyone): %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("a segment announcement with no filters addresses all 3 subscribers, got %d: %v", len(ids), ids)
	}

	byPlan := portalBanner("Your plan is being upgraded")
	basic := 1
	byPlan.SegmentPlanID = &basic
	narrowed, err := store.CreateAnnouncement(ctx, byPlan)
	if err != nil {
		t.Fatalf("CreateAnnouncement (plan): %v", err)
	}
	ids, err = store.ListSegmentSubscriberIDs(ctx, narrowed.ID, narrowed.SegmentFranchiseID, narrowed.SegmentPlanID, narrowed.SegmentStatus, narrowed.SegmentGroupID)
	if err != nil {
		t.Fatalf("ListSegmentSubscriberIDs (plan): %v", err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("a plan segment addresses exactly that plan's subscribers [1 2], got %v", ids)
	}
}
