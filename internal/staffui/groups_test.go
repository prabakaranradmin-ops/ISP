package staffui

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Named subscriber groups in the console — FR-GRP-001.

type fakeGroups struct {
	list []revenue.SubscriberGroup
	err  error

	created       revenue.SubscriberGroup
	renamedTo     revenue.SubscriberGroup
	renamedID     int
	activeID      int
	active        bool
	deletedID     int
	memberOp      string
	memberIDs     []int
	memberN       int
	who           string
	calls         int
	listedRetired bool
}

func (f *fakeGroups) CreateSubscriberGroup(ctx context.Context, in revenue.SubscriberGroup) (*revenue.SubscriberGroup, error) {
	f.calls++
	f.created, f.who = in, middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	in.ID = 1
	return &in, nil
}

func (f *fakeGroups) RenameSubscriberGroup(ctx context.Context, id int, in revenue.SubscriberGroup) error {
	f.calls++
	f.renamedID, f.renamedTo, f.who = id, in, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeGroups) SetSubscriberGroupActive(ctx context.Context, id int, active bool) error {
	f.calls++
	f.activeID, f.active, f.who = id, active, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeGroups) DeleteSubscriberGroup(ctx context.Context, id int) error {
	f.calls++
	f.deletedID, f.who = id, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeGroups) ListSubscriberGroups(_ context.Context, includeRetired bool) ([]revenue.SubscriberGroup, error) {
	f.listedRetired = includeRetired
	if f.err != nil {
		return nil, f.err
	}
	out := make([]revenue.SubscriberGroup, 0, len(f.list))
	for _, g := range f.list {
		if includeRetired || g.Active {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeGroups) AddToGroup(ctx context.Context, groupID int, ids []int) (int, error) {
	f.calls++
	f.memberOp, f.memberIDs, f.who = "add", ids, middleware.SubjectFromContext(ctx)
	f.activeID = groupID
	return f.memberN, f.err
}

func (f *fakeGroups) RemoveFromGroup(ctx context.Context, groupID int, ids []int) (int, error) {
	f.calls++
	f.memberOp, f.memberIDs, f.who = "remove", ids, middleware.SubjectFromContext(ctx)
	f.activeID = groupID
	return f.memberN, f.err
}

func twoGroups() []revenue.SubscriberGroup {
	now := time.Now()
	return []revenue.SubscriberGroup{
		{ID: 4, Name: "Anna Nagar", Description: "Feeder cable A", Active: true, Members: 42,
			CreatedBy: "csr_user", CreatedAt: time.Now().Add(-72 * time.Hour)},
		{ID: 5, Name: "Old survey", Active: false, Members: 3,
			CreatedAt: time.Now().Add(-500 * time.Hour), UpdatedBy: "owner", UpdatedAt: &now},
	}
}

func groupScreen() (*Handler, *fakeGroups) {
	f := &fakeGroups{list: twoGroups()}
	return &Handler{groups: f}, f
}

// ── Reach ───────────────────────────────────────────────────────────────────

// The section registry and the handler's own gate must agree. If the section
// let a role onto the screen and canManageGroups then refused every control,
// the screen would be a dead end; if the reverse, the registry would be
// advertising reach the handler does not enforce.
func TestFR_GRP_001_TheSectionAndTheHandlerAgreeOnWhoCanManageGroups(t *testing.T) {
	for _, role := range []string{"isp_owner", "billing_admin", "csr", "noc_engineer",
		"technician", "lco", "franchise_admin", "intern", ""} {
		var inSection bool
		for _, sec := range AllowedSections(role, false) {
			if sec.Key == "groups" {
				inSection = true
			}
		}
		canManage := canManageGroups(role)
		if inSection != canManage {
			t.Errorf("role %q: in the Subscriber Groups section = %v, but canManageGroups = %v",
				role, inSection, canManage)
		}
	}
}

func TestFR_GRP_001_AnUnwiredDeploymentSaysSoRatherThanPanicking(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/groups", nil, h.Groups)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}

// ── Screen ──────────────────────────────────────────────────────────────────

// Rendered with real data, in each shape the template branches on. A Go
// template resolves field names at run time, so a wrong name compiles
// cleanly and fails only here.
func TestFR_GRP_001_TheScreenRendersWithRealData(t *testing.T) {
	h, _ := groupScreen()

	for _, tc := range []struct {
		name string
		data groupData
	}{
		{"live and retired groups, manageable", groupData{
			Groups: twoGroups(), IncludeRetired: true, CanManage: true}},
		{"read-only viewer", groupData{Groups: twoGroups(), CanManage: false}},
		{"no groups at all", groupData{CanManage: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := h.page(testSession(), "Subscriber groups", "groups")
			d.Data = tc.data
			assertRendersOK(t, h, "groups", d)
		})
	}
}

// <no value> is what a Go template prints for a field that does not exist, so
// its absence is the assertion that every name in the template resolved.
func TestFR_GRP_001_TheScreenHasNoUnresolvedFields(t *testing.T) {
	h, _ := groupScreen()
	w := servePR(h, "isp_owner", "GET", "/staff/groups?retired=true", nil, h.Groups)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<no value>") {
		t.Error("the screen rendered <no value>, so a template field name does not resolve")
	}
	for _, want := range []string{"Anna Nagar", "Feeder cable A", "42", "Old survey", "retired"} {
		if !strings.Contains(body, want) {
			t.Errorf("the screen is missing %q", want)
		}
	}
	// The member count links into the directory's own group filter, so the
	// two views of a group cannot drift apart.
	if !strings.Contains(body, "/staff/subscribers?group_id=4") {
		t.Error("the member count does not link to the directory filtered to that group")
	}
}

func TestFR_GRP_001_RetiredGroupsAreHiddenUnlessAsked(t *testing.T) {
	h, f := groupScreen()

	servePR(h, "isp_owner", "GET", "/staff/groups", nil, h.Groups)
	if f.listedRetired {
		t.Error("the default list asked for retired groups too")
	}
	servePR(h, "isp_owner", "GET", "/staff/groups?retired=true", nil, h.Groups)
	if !f.listedRetired {
		t.Error("?retired=true did not ask for the retired groups")
	}
}

// ── Changes ─────────────────────────────────────────────────────────────────

func TestFR_GRP_001_AGroupIsCreatedWithTheOperatorsIdentity(t *testing.T) {
	h, f := groupScreen()
	w := servePR(h, "csr", "POST", "/staff/groups",
		url.Values{"name": {"  Anna Nagar  "}, "description": {"Feeder A"}}, h.CreateGroupForm)

	if w.Code != 303 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if f.created.Name != "Anna Nagar" {
		t.Errorf("the name reached the service as %q; it should be trimmed", f.created.Name)
	}
	if f.created.Description != "Feeder A" {
		t.Errorf("the description reached the service as %q", f.created.Description)
	}
	// Without the subject on the context the audit entry would name nobody.
	if f.who == "" {
		t.Error("the operator's identity did not reach the service, so the audit entry would name nobody")
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=created") {
		t.Errorf("redirected to %q", loc)
	}
}

func TestFR_GRP_001_AnEmptyNameIsRefusedBeforeTheService(t *testing.T) {
	h, f := groupScreen()
	w := servePR(h, "csr", "POST", "/staff/groups",
		url.Values{"name": {"   "}}, h.CreateGroupForm)

	if w.Code != 303 {
		t.Fatalf("got %d", w.Code)
	}
	if f.calls != 0 {
		t.Error("a nameless group reached the service")
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Errorf("the operator was not told what was wrong: %q", loc)
	}
}

func TestFR_GRP_001_ADuplicateNameIsReportedAsSuch(t *testing.T) {
	h, f := groupScreen()
	f.err = revenue.ErrGroupNameTaken
	w := servePR(h, "csr", "POST", "/staff/groups", url.Values{"name": {"Anna Nagar"}}, h.CreateGroupForm)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=duplicate") {
		t.Errorf("redirected to %q, want the duplicate-name message", loc)
	}
}

// A group an announcement is addressed to cannot be deleted, and the operator
// is told to retire it instead: deleting it would change who that
// announcement reaches.
func TestFR_GRP_001_AGroupInUseIsNotDeleted(t *testing.T) {
	h, f := groupScreen()
	f.err = revenue.ErrGroupInUse
	w := servePR(h, "isp_owner", "POST", "/staff/groups/4/delete", nil, h.DeleteGroupForm)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=inuse") {
		t.Errorf("redirected to %q, want the in-use message", loc)
	}
	if !strings.Contains(groupMessages["inuse"], "Retire it instead") {
		t.Error("the in-use message does not tell the operator what to do instead")
	}
}

func TestFR_GRP_001_RetiringAndRestoringAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		posted string
		want   bool
		code   string
	}{
		{"false", false, "m=retired"},
		{"true", true, "m=restored"},
	} {
		h, f := groupScreen()
		w := servePR(h, "isp_owner", "POST", "/staff/groups/4/active",
			url.Values{"active": {tc.posted}}, h.SetGroupActiveForm)
		if f.active != tc.want || f.activeID != 4 {
			t.Errorf("active=%q reached the service as id=%d active=%v", tc.posted, f.activeID, f.active)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, tc.code) {
			t.Errorf("active=%q redirected to %q", tc.posted, loc)
		}
	}
}

func TestFR_GRP_001_ABadGroupIdIsRefusedCleanly(t *testing.T) {
	for _, bad := range []string{"notanumber", "0", "-1"} {
		h, f := groupScreen()
		w := servePR(h, "isp_owner", "POST", "/staff/groups/"+bad+"/delete", nil, h.DeleteGroupForm)
		if w.Code != 400 {
			t.Errorf("id %q got %d, want 400", bad, w.Code)
		}
		if f.calls != 0 {
			t.Errorf("id %q reached the service", bad)
		}
	}
}

func TestFR_GRP_001_OnlyThePermittedRolesCanChangeAGroup(t *testing.T) {
	for _, role := range []string{"isp_owner", "billing_admin", "csr", "noc_engineer"} {
		h, f := groupScreen()
		w := servePR(h, role, "POST", "/staff/groups", url.Values{"name": {"X"}}, h.CreateGroupForm)
		if w.Code != 303 || f.calls != 1 {
			t.Errorf("%s got %d with %d service call(s)", role, w.Code, f.calls)
		}
	}
	for _, role := range []string{"technician", "lco", "intern"} {
		h, f := groupScreen()
		w := servePR(h, role, "POST", "/staff/groups", url.Values{"name": {"X"}}, h.CreateGroupForm)
		if w.Code != 403 {
			t.Errorf("%s got %d, want 403", role, w.Code)
		}
		if f.calls != 0 {
			t.Errorf("%s reached the service despite the refusal", role)
		}
	}
}

// ── Membership ──────────────────────────────────────────────────────────────

// However the list arrived — out of a spreadsheet column, a message, or both.
func TestFR_GRP_001_APastedIDListIsParsedInEveryShapeItArrivesIn(t *testing.T) {
	want := []int{1012, 1013, 1014}
	for _, raw := range []string{
		"1012,1013,1014",
		"1012, 1013, 1014",
		"1012\n1013\n1014",
		"1012\r\n1013\r\n1014\r\n",
		"1012; 1013;1014",
		"  1012\t1013  1014  ",
		// A duplicate in the paste is one membership, not two.
		"1012,1013,1012,1014",
	} {
		got, err := parseSubscriberIDList(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%q parsed to %v, want %v", raw, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q parsed to %v, want %v", raw, got, want)
				break
			}
		}
	}
}

func TestFR_GRP_001_ANonIDInThePasteIsRefusedRatherThanSkipped(t *testing.T) {
	// Skipping it silently would make a mistyped list look like it worked,
	// and a mistyped id is how somebody ends up in the wrong group.
	for _, raw := range []string{"1012, abc", "1012, 0", "1012, -3", "", "   ", "\n"} {
		if _, err := parseSubscriberIDList(raw); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
}

func TestFR_GRP_001_AnOversizedPasteIsRefused(t *testing.T) {
	ids := make([]string, 2001)
	for i := range ids {
		ids[i] = "1"
	}
	if _, err := parseSubscriberIDList(strings.Join(ids, ",")); err == nil {
		t.Error("a paste of 2001 ids was accepted")
	}
}

// The count added is reported, not assumed: adding is idempotent and skips
// ids that are not subscribers, so "17 of 20" is the honest answer and the
// three missing are what the operator needs to chase.
func TestFR_GRP_001_TheMembershipCountIsReportedHonestly(t *testing.T) {
	h, f := groupScreen()
	f.memberN = 17
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = string(rune('0'+i%10)) + "01"
	}
	// 20 distinct ids.
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		b.WriteString("10")
		b.WriteString(strings.TrimSpace(strings.Repeat(" ", 0)))
		b.WriteString(itoa(i))
	}

	w := servePR(h, "csr", "POST", "/staff/groups/4/members",
		url.Values{"subscriber_ids": {b.String()}, "action": {"add"}}, h.GroupMembersForm)
	if w.Code != 303 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if f.memberOp != "add" || len(f.memberIDs) != 20 {
		t.Fatalf("the service was asked to %q %d ids", f.memberOp, len(f.memberIDs))
	}
	loc, _ := url.QueryUnescape(w.Header().Get("Location"))
	if !strings.Contains(loc, "17 of 20 added to the group") {
		t.Errorf("the operator was told %q", loc)
	}
	if !strings.Contains(loc, "already in it, or are not subscriber ids") {
		t.Error("the operator is not told why three were skipped")
	}
}

func TestFR_GRP_001_MembersCanBeRemovedAsWellAsAdded(t *testing.T) {
	h, f := groupScreen()
	f.memberN = 2
	w := servePR(h, "csr", "POST", "/staff/groups/4/members",
		url.Values{"subscriber_ids": {"1012,1013"}, "action": {"remove"}}, h.GroupMembersForm)
	if w.Code != 303 {
		t.Fatalf("got %d", w.Code)
	}
	if f.memberOp != "remove" {
		t.Errorf("the service was asked to %q", f.memberOp)
	}
	loc, _ := url.QueryUnescape(w.Header().Get("Location"))
	if !strings.Contains(loc, "2 of 2 removed from the group") {
		t.Errorf("the operator was told %q", loc)
	}
}

// ── The directory filter ────────────────────────────────────────────────────

func TestFR_GRP_001_TheDirectoryOffersTheGroupFilter(t *testing.T) {
	h, _ := groupScreen()
	h.console = &fakeConsole{}

	w := servePR(h, "isp_owner", "GET", "/staff/subscribers?group_id=4", nil, h.Subscribers)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<no value>") {
		t.Error("the directory rendered <no value>, so a template field name does not resolve")
	}
	if !strings.Contains(body, "Anna Nagar") {
		t.Error("the group filter does not offer the live groups")
	}
	// Retired groups are not offered: a retired group is out of the filters
	// by definition.
	if strings.Contains(body, "Old survey") {
		t.Error("a retired group is offered in the directory filter")
	}
	if got := h.console.(*fakeConsole).gotFilt.GroupID; got != 4 {
		t.Errorf("the filter reached the store as group %d, want 4", got)
	}
}

// An id naming a retired or deleted group is reported, not applied. Applying
// it would show an empty list, which reads as "this group has no members".
func TestFR_GRP_001_AnUnknownGroupFilterIsReportedNotApplied(t *testing.T) {
	h, _ := groupScreen()
	h.console = &fakeConsole{}

	w := servePR(h, "isp_owner", "GET", "/staff/subscribers?group_id=999", nil, h.Subscribers)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "retired or no longer exists") {
		t.Error("an unknown group filter was applied silently")
	}
	if got := h.console.(*fakeConsole).gotFilt.GroupID; got != 0 {
		t.Errorf("the unknown group reached the store as %d; it should have been dropped", got)
	}
}

// ── The reports screen ──────────────────────────────────────────────────────

type fakeRosterStore struct {
	rows   []reporting.SubscriberRosterRow
	gotGrp *int
	calls  int
	err    error
}

func (f *fakeRosterStore) PlanMix(context.Context, *int) ([]reporting.PlanMixRow, error) {
	return nil, f.err
}
func (f *fakeRosterStore) GrowthMonthly(context.Context, int, *int) ([]reporting.GrowthRow, error) {
	return nil, f.err
}
func (f *fakeRosterStore) TicketResolution(context.Context, int, *int) ([]reporting.TicketResolutionRow, error) {
	return nil, f.err
}
func (f *fakeRosterStore) FranchiseCollection(context.Context, int, *int) ([]reporting.CollectionRow, error) {
	return nil, f.err
}
func (f *fakeRosterStore) SubscriberRoster(_ context.Context, group, _ *int) ([]reporting.SubscriberRosterRow, error) {
	f.calls++
	f.gotGrp = group
	return f.rows, f.err
}

func TestFR_GRP_001_TheReportsScreenScopesTheRosterToAGroup(t *testing.T) {
	h, _ := groupScreen()
	store := &fakeRosterStore{rows: []reporting.SubscriberRosterRow{{
		SubscriberID: 7, Username: "priya@isp", Status: "active", PlanName: "Fibre 100",
		MobileNumber: "+919876543210", Groups: []string{"Anna Nagar", "Corporate"},
		CreatedAt: time.Now().Add(-100 * time.Hour),
	}}}
	h.reporting = store

	w := servePR(h, "isp_owner", "GET", "/staff/reports?report=subscribers&group_id=4", nil, h.Reports)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if store.gotGrp == nil || *store.gotGrp != 4 {
		t.Fatalf("the group reached the store as %v, want 4", store.gotGrp)
	}
	body := w.Body.String()
	if strings.Contains(body, "<no value>") {
		t.Error("the roster rendered <no value>, so a template field name does not resolve")
	}
	// The mobile number appears as &#43;91…: html/template escapes a leading
	// plus defensively (it is significant in UTF-7), so the rendered page
	// never contains the literal "+919876543210".
	for _, want := range []string{"priya@isp", "Fibre 100", "&#43;919876543210", "Anna Nagar, Corporate"} {
		if !strings.Contains(body, want) {
			t.Errorf("the roster is missing %q", want)
		}
	}
}

// The screen must not offer a group selector on a report that cannot honour
// one, and must say so if an id arrives for such a report anyway.
func TestFR_GRP_001_TheAggregateReportsOfferNoGroupSelector(t *testing.T) {
	for _, report := range []string{reporting.ReportPlanMix, reporting.ReportGrowth,
		reporting.ReportTicketResolution, reporting.ReportCollection} {
		h, _ := groupScreen()
		store := &fakeRosterStore{}
		h.reporting = store

		w := servePR(h, "isp_owner", "GET", "/staff/reports?report="+report+"&group_id=4", nil, h.Reports)
		if w.Code != 200 {
			t.Fatalf("%s: got %d: %s", report, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "cannot be filtered by group") {
			t.Errorf("%s: a group filter was dropped without telling the operator", report)
		}
		if store.calls != 0 {
			t.Errorf("%s: the roster query ran for a different report", report)
		}
	}
}

func TestFR_GRP_001_TheRosterScreenSurvivesAStoreFailure(t *testing.T) {
	h, _ := groupScreen()
	h.reporting = &fakeRosterStore{err: errors.New("boom")}
	w := servePR(h, "isp_owner", "GET", "/staff/reports?report=subscribers", nil, h.Reports)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Could not load that report") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}

// itoa avoids importing strconv for one call in a test helper.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
