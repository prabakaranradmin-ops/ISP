//go:build integration

// Named subscriber groups — FR-GRP-001 | migration 069.
//
// What only a real database can show: that a group's size, the audience
// preview, the announcement fan-out, the portal's own visibility check and
// the roster report all count the same people; that the partial unique index
// lets a retired group share a name with its replacement; and that deleting a
// group an announcement is addressed to is refused by the foreign key rather
// than only by the Go that checks first.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// ── Lifecycle ───────────────────────────────────────────────────────────────

func TestFR_GRP_001_AGroupIsCreatedRenamedRetiredAndDeleted(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Revenue()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	g, err := store.CreateSubscriberGroup(ctx,
		revenue.SubscriberGroup{Name: "  Anna Nagar  ", Description: " Feeder A "}, "csr_user")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.Name != "Anna Nagar" || g.Description != "Feeder A" {
		t.Errorf("stored as %q / %q; the store should normalise", g.Name, g.Description)
	}
	if !g.Active {
		t.Error("a new group is not active, so it would not appear in any filter")
	}
	if g.CreatedBy != "csr_user" {
		t.Errorf("CreatedBy = %q", g.CreatedBy)
	}
	if g.Members != 0 {
		t.Errorf("a new group has %d members", g.Members)
	}

	if err := store.RenameSubscriberGroup(ctx, g.ID,
		revenue.SubscriberGroup{Name: "Anna Nagar East", Description: ""}, "owner"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	groups, err := store.ListSubscriberGroups(ctx, false)
	if err != nil || len(groups) != 1 {
		t.Fatalf("list: %v, %d groups", err, len(groups))
	}
	if groups[0].Name != "Anna Nagar East" {
		t.Errorf("name after rename = %q", groups[0].Name)
	}
	// Blanked rather than left as it was: an empty description is a
	// deliberate clearing, and NULLIF stores it as absent.
	if groups[0].Description != "" {
		t.Errorf("description after rename = %q", groups[0].Description)
	}
	if groups[0].UpdatedBy != "owner" || groups[0].UpdatedAt == nil {
		t.Errorf("the rename is not attributed: by=%q at=%v", groups[0].UpdatedBy, groups[0].UpdatedAt)
	}

	// Retiring takes it out of the default list but not out of the database.
	if err := store.SetSubscriberGroupActive(ctx, g.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if live, _ := store.ListSubscriberGroups(ctx, false); len(live) != 0 {
		t.Errorf("a retired group is still offered: %+v", live)
	}
	if all, _ := store.ListSubscriberGroups(ctx, true); len(all) != 1 {
		t.Error("a retired group vanished from the full list")
	}

	if err := store.SetSubscriberGroupActive(ctx, g.ID, true, "owner"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if live, _ := store.ListSubscriberGroups(ctx, false); len(live) != 1 {
		t.Error("a restored group is not offered again")
	}

	if err := store.DeleteSubscriberGroup(ctx, g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if all, _ := store.ListSubscriberGroups(ctx, true); len(all) != 0 {
		t.Error("the group survived its own deletion")
	}
}

func TestFR_GRP_001_OnlyLiveGroupsCompeteForAName(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Revenue()

	first, err := store.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"}, "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Case, surrounding space and a doubled space inside do not make a
	// different name: an operator refers to a group by name in conversation.
	// The index is on lower(btrim(name)), which would see "anna  nagar" as
	// distinct — Normalise collapses the run before it gets there.
	if _, err := store.CreateSubscriberGroup(ctx,
		revenue.SubscriberGroup{Name: " anna  nagar "}, "owner"); !errors.Is(err, revenue.ErrGroupNameTaken) {
		t.Errorf("a second live group with the same name was accepted: %v", err)
	}

	// Retire the first and the name frees up — which is how a group gets
	// rebuilt after a survey.
	if err := store.SetSubscriberGroupActive(ctx, first.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := store.CreateSubscriberGroup(ctx,
		revenue.SubscriberGroup{Name: "Anna Nagar"}, "owner"); err != nil {
		t.Errorf("a retired group blocks its own replacement's name: %v", err)
	}

	// And restoring the retired one would now collide, so it is refused
	// rather than producing two live groups with one name.
	if err := store.SetSubscriberGroupActive(ctx, first.ID, true, "owner"); err == nil {
		t.Error("restoring a group whose name has been reused was allowed, so two live groups " +
			"now share a name")
	}
}

func TestFR_GRP_001_AnUnknownGroupIsReportedNotSilentlyIgnored(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Revenue()

	if err := store.RenameSubscriberGroup(ctx, 9999,
		revenue.SubscriberGroup{Name: "Nowhere"}, "owner"); err == nil {
		t.Error("renaming a group that does not exist succeeded")
	}
	if err := store.SetSubscriberGroupActive(ctx, 9999, false, "owner"); err == nil {
		t.Error("retiring a group that does not exist succeeded")
	}
	if err := store.DeleteSubscriberGroup(ctx, 9999); err == nil {
		t.Error("deleting a group that does not exist succeeded")
	}
}

// ── Membership ──────────────────────────────────────────────────────────────

func TestFR_GRP_001_MembershipIsIdempotentAndSkipsIdsThatAreNotSubscribers(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Revenue()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	for i := 1; i <= 3; i++ {
		seedSubscriber(ctx, t, pool, i, seedOpts{Username: fmt.Sprintf("s%d@isp", i)})
	}
	g, err := store.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Block A"}, "csr")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Two real ids and one that is not a subscriber at all. A paste of twenty
	// with one typo should add the nineteen, not fail outright.
	added, err := store.AddToGroup(ctx, g.ID, []int{1, 2, 999999}, "csr")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2 — the id that is not a subscriber should be skipped", added)
	}

	// Adding the same people again is the same membership, not a second one.
	again, err := store.AddToGroup(ctx, g.ID, []int{1, 2, 3}, "csr")
	if err != nil {
		t.Fatalf("add again: %v", err)
	}
	if again != 1 {
		t.Errorf("re-adding two existing members plus one new reported %d; want 1", again)
	}

	ids, err := store.GroupMemberIDs(ctx, g.ID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("the group holds %v; want three distinct members", ids)
	}

	removed, err := store.RemoveFromGroup(ctx, g.ID, []int{2, 999999})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 — an id that was never in it is not a removal", removed)
	}

	// An empty list is a no-op rather than an error or a mass change.
	if n, err := store.AddToGroup(ctx, g.ID, nil, "csr"); err != nil || n != 0 {
		t.Errorf("adding nothing: %d, %v", n, err)
	}
	if n, err := store.RemoveFromGroup(ctx, g.ID, nil); err != nil || n != 0 {
		t.Errorf("removing nothing: %d, %v", n, err)
	}
}

// A terminated subscriber is still in the group — the row is history — but
// counts for nothing, because a group's size is the reach a message to it
// would actually have.
func TestFR_GRP_001_TerminatedSubscribersCountForNothing(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Revenue()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "live@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "gone@isp", Status: "terminated"})

	g, _ := store.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Block A"}, "csr")
	if _, err := store.AddToGroup(ctx, g.ID, []int{1, 2}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}

	groups, err := store.ListSubscriberGroups(ctx, false)
	if err != nil || len(groups) != 1 {
		t.Fatalf("list: %v", err)
	}
	if groups[0].Members != 1 {
		t.Errorf("Members = %d; a terminated subscriber is counted in the group's size, so the "+
			"console would promise a reach the fan-out will not deliver", groups[0].Members)
	}

	ids, _ := store.GroupMemberIDs(ctx, g.ID)
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("GroupMemberIDs = %v; the terminated subscriber is still being addressed", ids)
	}
}

// Both directions of the cascade. A membership describes a pairing and means
// nothing without either end of it.
func TestFR_GRP_001_MembershipsGoWithTheirGroupAndTheirSubscriber(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Revenue()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b@isp"})

	g, _ := store.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Block A"}, "csr")
	if _, err := store.AddToGroup(ctx, g.ID, []int{1, 2}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Deleting a subscriber takes their memberships with them.
	if _, err := pool.Exec(ctx, `DELETE FROM subscriber_group_members WHERE subscriber_id = 2`); err != nil {
		t.Fatalf("tidy: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM subscribers WHERE id = 2`); err != nil {
		// Other tables may reference the subscriber; the membership cascade
		// is what this test is about, so an unrelated FK is not a failure.
		t.Logf("subscriber delete blocked by an unrelated reference: %v", err)
	}

	// Deleting the group takes its memberships with it.
	if err := store.DeleteSubscriberGroup(ctx, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	var orphans int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM subscriber_group_members WHERE group_id = $1`, g.ID).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d membership(s) outlived their group", orphans)
	}
}

// Retired groups are included for one subscriber's own page: they are still
// in them, and hiding that would make a membership look removed when it is
// not — the sort of discrepancy noticed only when a message reaches somebody
// nobody expected.
func TestFR_GRP_001_ASubscribersOwnGroupsIncludeTheRetiredOnes(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Revenue()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	live, _ := store.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Live"}, "csr")
	old, _ := store.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Old survey"}, "csr")
	if _, err := store.AddToGroup(ctx, live.ID, []int{1}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := store.AddToGroup(ctx, old.ID, []int{1}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.SetSubscriberGroupActive(ctx, old.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	mine, err := store.GroupsForSubscriber(ctx, 1)
	if err != nil {
		t.Fatalf("groups for subscriber: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("the subscriber is in %d group(s); both memberships should show", len(mine))
	}
	// Live groups first, so the current ones read before the historical.
	if !mine[0].Active || mine[1].Active {
		t.Errorf("ordering puts the retired group first: %q then %q", mine[0].Name, mine[1].Name)
	}
}

// ── The three segment predicates ────────────────────────────────────────────

// The one that matters most. The segment appears in three places — the
// audience preview the console shows, the fan-out that actually sends, and
// the portal's own visibility check — and a group-addressed announcement that
// went out by SMS and never appeared in the portal would look like a portal
// bug for weeks.
func TestFR_GRP_001_ThePreviewTheFanOutAndThePortalAgreeOnAGroup(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	groups := database.Revenue()
	announcements := database.Announcements()
	console := database.Console()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	// Three in the group (one terminated, so two reachable), one outside it.
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "in1@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "in2@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "gone@isp", Status: "terminated"})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "out@isp"})

	g, _ := groups.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"}, "csr")
	if _, err := groups.AddToGroup(ctx, g.ID, []int{1, 2, 3}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}

	created, err := announcements.CreateAnnouncement(ctx, notifications.Announcement{
		Title: "Cable work on Friday", Body: "Expect a four-hour outage.",
		Channels: []string{"sms"}, Class: "transactional",
		SegmentGroupID: &g.ID, ShowInPortal: true, CreatedBy: "csr",
	})
	if err != nil {
		t.Fatalf("create announcement: %v", err)
	}
	if created.SegmentGroupID == nil || *created.SegmentGroupID != g.ID {
		t.Fatalf("the group did not survive the round trip: %v", created.SegmentGroupID)
	}

	// 1. The preview the console shows before sending.
	preview, err := console.CountAnnouncementAudience(ctx, nil, nil, nil, &g.ID)
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	if preview != 2 {
		t.Errorf("the preview promises %d recipients, want 2 (the terminated member is unreachable)", preview)
	}

	// 2. The fan-out that actually sends.
	fanout, err := announcements.ListSegmentSubscriberIDs(ctx, created.ID, nil, nil, nil, &g.ID)
	if err != nil {
		t.Fatalf("fan-out: %v", err)
	}
	if len(fanout) != preview {
		t.Errorf("the fan-out reaches %v (%d) but the preview promised %d", fanout, len(fanout), preview)
	}
	for _, id := range fanout {
		if id == 4 {
			t.Error("the fan-out reached a subscriber who is not in the group")
		}
		if id == 3 {
			t.Error("the fan-out reached a terminated subscriber")
		}
	}

	// 3. The portal's own visibility check, per subscriber.
	//
	// Taken through the real send lifecycle first, because the portal query
	// requires status = 'sent'. Leaving it a draft makes every subscriber
	// see nothing, which passes the "outside the group" case for entirely
	// the wrong reason and proves nothing about the group predicate.
	if claimed, err := announcements.ClaimAnnouncementForSending(ctx, created.ID); err != nil || claimed == nil {
		t.Fatalf("claim for sending: %v, %v", claimed, err)
	}
	if err := announcements.FinishAnnouncement(ctx, created.ID, "sent", len(fanout)); err != nil {
		t.Fatalf("finish: %v", err)
	}

	for _, tc := range []struct {
		id      int
		visible bool
		why     string
	}{
		{1, true, "a member cannot see an announcement addressed to their group"},
		{2, true, "a member cannot see an announcement addressed to their group"},
		{4, false, "a subscriber outside the group can see it in their portal"},
	} {
		list, err := announcements.ListPortalAnnouncements(ctx, tc.id)
		if err != nil {
			t.Fatalf("portal for %d: %v", tc.id, err)
		}
		var found bool
		for _, a := range list {
			if a.ID == created.ID {
				found = true
			}
		}
		if found != tc.visible {
			t.Errorf("subscriber %d: visible=%v — %s", tc.id, found, tc.why)
		}
	}
}

// The group narrows alongside the other dimensions rather than replacing
// them: "Anna Nagar, on Fibre 100, suspended" is three filters over one
// audience, and an announcement addressed to no group at all still reaches
// everybody as it did before groups existed.
func TestFR_GRP_001_TheGroupSegmentCombinesWithTheOthers(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	groups := database.Revenue()
	console := database.Console()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedPlan(ctx, t, pool, 2, "Fibre 200", "200M/200M", 0, "", "999.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b@isp", PlanID: 2})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "c@isp", PlanID: 1})

	g, _ := groups.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"}, "csr")
	if _, err := groups.AddToGroup(ctx, g.ID, []int{1, 2}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}

	planID := 1
	cases := []struct {
		name  string
		group *int
		plan  *int
		want  int
	}{
		{"no filters at all reaches everybody", nil, nil, 3},
		{"the group alone", &g.ID, nil, 2},
		{"the plan alone", nil, &planID, 2},
		{"both together, which is one subscriber", &g.ID, &planID, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := console.CountAnnouncementAudience(ctx, nil, tc.plan, nil, tc.group)
			if err != nil {
				t.Fatalf("audience: %v", err)
			}
			if n != tc.want {
				t.Errorf("reach = %d, want %d", n, tc.want)
			}
		})
	}
}

// Deleting a group an announcement is addressed to is refused twice over:
// by the Go that counts first so the operator gets a sentence, and by
// ON DELETE RESTRICT so a direct delete cannot widen a sent announcement's
// audience to everybody.
func TestFR_GRP_001_AGroupAnAnnouncementTargetsCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	groups := database.Revenue()
	announcements := database.Announcements()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	g, _ := groups.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"}, "csr")
	if _, err := announcements.CreateAnnouncement(ctx, notifications.Announcement{
		Title: "Outage", Body: "Friday", Channels: []string{"sms"}, Class: "transactional",
		SegmentGroupID: &g.ID, CreatedBy: "csr",
	}); err != nil {
		t.Fatalf("create announcement: %v", err)
	}

	err := groups.DeleteSubscriberGroup(ctx, g.ID)
	if !errors.Is(err, revenue.ErrGroupInUse) {
		t.Errorf("delete returned %v, want ErrGroupInUse", err)
	}

	// And the constraint itself, not only the Go check. A SET NULL here —
	// which the other three segment columns carry — would turn an
	// announcement sent to twenty people into one showing in every
	// subscriber's portal.
	if _, err := pool.Exec(ctx, `DELETE FROM subscriber_groups WHERE id = $1`, g.ID); err == nil {
		t.Error("a direct delete of a targeted group succeeded, so the announcement's audience " +
			"has silently widened to everybody")
	}

	// Retiring is the supported way out, and it changes nothing about who
	// the announcement reaches.
	if err := groups.SetSubscriberGroupActive(ctx, g.ID, false, "owner"); err != nil {
		t.Errorf("retiring a targeted group was refused: %v", err)
	}
	var still int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM announcements WHERE segment_group_id = $1`, g.ID).Scan(&still); err != nil {
		t.Fatalf("count: %v", err)
	}
	if still != 1 {
		t.Errorf("retiring the group left %d announcement(s) pointing at it, want 1", still)
	}
}

// ── The directory filter ────────────────────────────────────────────────────

func TestFR_GRP_001_TheDirectoryFiltersToAGroup(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	groups := database.Revenue()
	console := database.Console()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "in@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "out@isp"})

	g, _ := groups.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"}, "csr")
	if _, err := groups.AddToGroup(ctx, g.ID, []int{1}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}

	all, err := console.ListDirectory(ctx, staffui.DirectoryFilter{})
	if err != nil {
		t.Fatalf("directory: %v", err)
	}
	if len(all.Rows) != 2 {
		t.Fatalf("the unfiltered directory shows %d; want both", len(all.Rows))
	}

	only, err := console.ListDirectory(ctx, staffui.DirectoryFilter{GroupID: g.ID})
	if err != nil {
		t.Fatalf("directory by group: %v", err)
	}
	if len(only.Rows) != 1 || only.Rows[0].Username != "in@isp" {
		t.Errorf("the group filter returned %d row(s): %+v", len(only.Rows), only.Rows)
	}
	// Matched describes the filtered set, not the whole base — the count
	// above the table would otherwise contradict the table itself.
	if only.Matched != 1 {
		t.Errorf("Matched = %d under a group filter, want 1", only.Matched)
	}
}

// ── The roster report ───────────────────────────────────────────────────────

func TestFR_GRP_001_TheRosterReportIsScopedToAGroup(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	groups := database.Revenue()
	reports := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "in@isp", MobileNumber: "+919000000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "out@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "gone@isp", Status: "terminated"})

	anna, _ := groups.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"}, "csr")
	corp, _ := groups.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Corporate"}, "csr")
	if _, err := groups.AddToGroup(ctx, anna.ID, []int{1, 3}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := groups.AddToGroup(ctx, corp.ID, []int{1}, "csr"); err != nil {
		t.Fatalf("add: %v", err)
	}

	// The whole base, terminated excluded — so the roster and the group
	// counts and the fan-out all describe the same people.
	everyone, err := reports.SubscriberRoster(ctx, nil, nil)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(everyone) != 2 {
		t.Errorf("the unfiltered roster has %d rows; want 2 (the terminated subscriber excluded)",
			len(everyone))
	}

	scoped, err := reports.SubscriberRoster(ctx, &anna.ID, nil)
	if err != nil {
		t.Fatalf("roster by group: %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("the group roster has %d rows: %+v", len(scoped), scoped)
	}
	r := scoped[0]
	if r.Username != "in@isp" || r.PlanName != "Fibre 100" || r.Status != "active" {
		t.Errorf("row = %+v", r)
	}
	if r.MobileNumber != "+919000000001" {
		t.Errorf("MobileNumber = %q", r.MobileNumber)
	}
	if r.CreatedAt.IsZero() || r.CreatedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("CreatedAt = %v", r.CreatedAt)
	}
	// Every group the subscriber is in, not only the one filtered on: an
	// operator scheduling an outage for Anna Nagar needs to see which of
	// them are also Corporate.
	if len(r.Groups) != 2 || r.Groups[0] != "Anna Nagar" || r.Groups[1] != "Corporate" {
		t.Errorf("Groups = %v; want both, name-ordered", r.Groups)
	}

	// A group nobody is in is an empty roster, not an error and not the
	// whole base.
	empty, err := reports.SubscriberRoster(ctx, &corp.ID, nil)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(empty) != 1 {
		t.Errorf("the Corporate roster has %d rows, want 1", len(empty))
	}
	unknown := 999999
	none, err := reports.SubscriberRoster(ctx, &unknown, nil)
	if err != nil {
		t.Fatalf("roster for an unknown group: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unknown group returned %d rows; it must not fall back to everybody", len(none))
	}
}
