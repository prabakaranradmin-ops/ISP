//go:build integration

// The catalogue service layer: bandwidth profiles (FR-CAT-002), subscriber
// groups (FR-GRP-001) and fair-use policies with their top-ups (FR-CAT-003).
//
// These three had no tests at all. They are the layer between the console's
// HTTP handlers and the stores, and almost all of what they add over a
// passthrough is the part worth testing: refusing to act when the store is
// not wired, and writing the audit trail. Both are invisible when they are
// wrong — an unwired store that panics instead of reporting takes the request
// with it, and a missing audit entry is only noticed when somebody asks who
// changed a tariff and nothing can answer.
package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// errStore is what every store below returns when a test wants the failure
// path. Distinct text so a leaked message is identifiable.
var errStore = errors.New("store: the catalogue table is unreachable")

// auditing runs fn with a recording sink and returns what was recorded.
func auditing(t *testing.T, fn func(ctx context.Context)) []middleware.AuditEntry {
	t.Helper()
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })
	fn(middleware.WithSubject(context.Background(), "meena"))
	return sink.entries()
}

// ── Bandwidth profiles ──────────────────────────────────────────────────────

type stubBandwidth struct {
	err     error
	profile nas.BandwidthProfile
	// by records the actor the handler passed down, which is the thing the
	// store writes into created_by/updated_by.
	by        string
	setPlan   [2]int
	cleared   bool
	deleted   int
	retired   *bool
	listedAll bool
}

func (s *stubBandwidth) CreateBandwidthProfile(_ context.Context, in nas.BandwidthProfile, by string) (*nas.BandwidthProfile, error) {
	s.by = by
	if s.err != nil {
		return nil, s.err
	}
	in.ID = 7
	return &in, nil
}

func (s *stubBandwidth) UpdateBandwidthProfile(_ context.Context, id int, in nas.BandwidthProfile, by string) (*nas.BandwidthProfile, error) {
	s.by = by
	if s.err != nil {
		return nil, s.err
	}
	in.ID = id
	in.InUseByPlans = 4
	return &in, nil
}

func (s *stubBandwidth) SetBandwidthProfileActive(_ context.Context, _ int, active bool, by string) error {
	s.by = by
	s.retired = &active
	return s.err
}

func (s *stubBandwidth) DeleteBandwidthProfile(_ context.Context, id int) error {
	s.deleted = id
	return s.err
}

func (s *stubBandwidth) ListBandwidthProfiles(_ context.Context, includeRetired bool) ([]nas.BandwidthProfile, error) {
	s.listedAll = includeRetired
	if s.err != nil {
		return nil, s.err
	}
	return []nas.BandwidthProfile{s.profile}, nil
}

func (s *stubBandwidth) BandwidthProfileByID(_ context.Context, id int) (*nas.BandwidthProfile, error) {
	if s.err != nil {
		return nil, s.err
	}
	p := s.profile
	p.ID = id
	return &p, nil
}

func (s *stubBandwidth) SetPlanBandwidthProfile(_ context.Context, planID int, profileID *int, by string) error {
	s.by = by
	if profileID == nil {
		s.cleared = true
		s.setPlan = [2]int{planID, 0}
	} else {
		s.setPlan = [2]int{planID, *profileID}
	}
	return s.err
}

func bandwidthHandler(s api.BandwidthProfileStore) *api.Handler {
	return api.NewHandler(api.HandlerDeps{Bandwidth: s})
}

func home50M() nas.BandwidthProfile {
	return nas.BandwidthProfile{Shaping: nas.Shaping{
		Name: "Home 50M", MaxDownBPS: 50_000_000, MaxUpBPS: 50_000_000,
	}}
}

// Nothing wired is reported, not panicked. A deployment that has not
// configured profiles still serves every other route, and the console shows
// the reason rather than a blank screen.
func TestFR_CAT_002_AnUnwiredProfileStoreIsReported(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{})
	ctx := context.Background()
	one := 1

	for name, call := range map[string]func() error{
		"create":         func() error { _, err := h.CreateBandwidthProfile(ctx, home50M()); return err },
		"update":         func() error { _, err := h.UpdateBandwidthProfile(ctx, 1, home50M()); return err },
		"setActive":      func() error { return h.SetBandwidthProfileActive(ctx, 1, false) },
		"delete":         func() error { return h.DeleteBandwidthProfile(ctx, 1) },
		"list":           func() error { _, err := h.ListBandwidthProfiles(ctx, false); return err },
		"byID":           func() error { _, err := h.BandwidthProfileByID(ctx, 1); return err },
		"setPlanProfile": func() error { return h.SetPlanBandwidthProfile(ctx, 1, &one) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, api.ErrBandwidthProfilesUnwired) {
				t.Errorf("want ErrBandwidthProfilesUnwired, got %v", err)
			}
		})
	}
}

func TestFR_CAT_002_CreatingAProfileIsAuditedWithWhoDidIt(t *testing.T) {
	s := &stubBandwidth{}
	h := bandwidthHandler(s)

	var got *nas.BandwidthProfile
	entries := auditing(t, func(ctx context.Context) {
		var err error
		got, err = h.CreateBandwidthProfile(ctx, home50M())
		if err != nil {
			t.Fatalf("create: %v", err)
		}
	})

	if got == nil || got.ID != 7 {
		t.Fatalf("the store's row should come back, got %+v", got)
	}
	if s.by != "meena" {
		t.Errorf("the store was told the actor is %q; created_by would be wrong", s.by)
	}
	if len(entries) != 1 {
		t.Fatalf("want one audit entry, got %d", len(entries))
	}
	if entries[0].Action != "bandwidth_profile.created" {
		t.Errorf("action = %q", entries[0].Action)
	}
	if entries[0].Target != "Home 50M" {
		t.Errorf("target = %q; the trail should name the profile", entries[0].Target)
	}
}

// Editing a profile re-shapes every plan on it, so the trail records how many
// — the number an operator needs to judge the blast radius afterwards.
func TestFR_CAT_002_AnEditRecordsHowManyPlansItReshapes(t *testing.T) {
	h := bandwidthHandler(&stubBandwidth{})

	entries := auditing(t, func(ctx context.Context) {
		if _, err := h.UpdateBandwidthProfile(ctx, 3, home50M()); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	if len(entries) != 1 {
		t.Fatalf("want one audit entry, got %d", len(entries))
	}
	if entries[0].Detail["plans_affected"] != 4 {
		t.Errorf("plans_affected = %v, want 4 — without it the trail says a profile "+
			"changed but not what that changed", entries[0].Detail["plans_affected"])
	}
}

// A store failure is the store's error, and it is not audited: the trail must
// not record a change that did not happen.
func TestFR_CAT_002_AFailedWriteIsNotAudited(t *testing.T) {
	h := bandwidthHandler(&stubBandwidth{err: errStore})

	for name, call := range map[string]func(context.Context) error{
		"create":    func(ctx context.Context) error { _, err := h.CreateBandwidthProfile(ctx, home50M()); return err },
		"update":    func(ctx context.Context) error { _, err := h.UpdateBandwidthProfile(ctx, 1, home50M()); return err },
		"setActive": func(ctx context.Context) error { return h.SetBandwidthProfileActive(ctx, 1, false) },
		"delete":    func(ctx context.Context) error { return h.DeleteBandwidthProfile(ctx, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			entries := auditing(t, func(ctx context.Context) {
				if err := call(ctx); !errors.Is(err, errStore) {
					t.Errorf("want the store's error, got %v", err)
				}
			})
			if len(entries) != 0 {
				t.Errorf("a failed write was audited: %+v", entries)
			}
		})
	}
}

// Clearing a plan's profile is a different event from setting one, because
// "this plan no longer has a shaped ceiling" is the change somebody will come
// looking for.
func TestFR_CAT_002_SettingAndClearingAPlansProfileAreDistinctEvents(t *testing.T) {
	s := &stubBandwidth{}
	h := bandwidthHandler(s)
	id := 9

	set := auditing(t, func(ctx context.Context) {
		if err := h.SetPlanBandwidthProfile(ctx, 2, &id); err != nil {
			t.Fatal(err)
		}
	})
	if s.setPlan != [2]int{2, 9} {
		t.Errorf("store got plan=%d profile=%d", s.setPlan[0], s.setPlan[1])
	}
	if len(set) != 1 {
		t.Fatalf("want one entry for setting, got %d", len(set))
	}

	cleared := auditing(t, func(ctx context.Context) {
		if err := h.SetPlanBandwidthProfile(ctx, 2, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !s.cleared {
		t.Error("the store was not told to clear the profile")
	}
	if len(cleared) != 1 {
		t.Fatalf("want one entry for clearing, got %d", len(cleared))
	}
	if set[0].Action == cleared[0].Action && set[0].Detail["profile_id"] == cleared[0].Detail["profile_id"] {
		t.Errorf("setting and clearing are indistinguishable in the trail: %q / %q",
			set[0].Action, cleared[0].Action)
	}
}

// Retired profiles are excluded unless asked for. The flag has to reach the
// store, or the console's "show retired" toggle does nothing.
func TestFR_CAT_002_RetiredProfilesAreExcludedUnlessAskedFor(t *testing.T) {
	s := &stubBandwidth{profile: home50M()}
	h := bandwidthHandler(s)
	ctx := context.Background()

	if _, err := h.ListBandwidthProfiles(ctx, false); err != nil {
		t.Fatal(err)
	}
	if s.listedAll {
		t.Error("includeRetired=false did not reach the store")
	}
	if _, err := h.ListBandwidthProfiles(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !s.listedAll {
		t.Error("includeRetired=true did not reach the store")
	}
}

// ── Subscriber groups ───────────────────────────────────────────────────────

type stubGroups struct {
	err     error
	group   revenue.SubscriberGroup
	by      string
	changed int
}

func (s *stubGroups) CreateSubscriberGroup(_ context.Context, in revenue.SubscriberGroup, by string) (*revenue.SubscriberGroup, error) {
	s.by = by
	if s.err != nil {
		return nil, s.err
	}
	in.ID = 11
	return &in, nil
}

func (s *stubGroups) RenameSubscriberGroup(_ context.Context, _ int, _ revenue.SubscriberGroup, by string) error {
	s.by = by
	return s.err
}

func (s *stubGroups) SetSubscriberGroupActive(_ context.Context, _ int, _ bool, by string) error {
	s.by = by
	return s.err
}

func (s *stubGroups) DeleteSubscriberGroup(_ context.Context, _ int) error { return s.err }

func (s *stubGroups) ListSubscriberGroups(_ context.Context, _ bool) ([]revenue.SubscriberGroup, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []revenue.SubscriberGroup{s.group}, nil
}

func (s *stubGroups) AddToGroup(_ context.Context, _ int, ids []int, by string) (int, error) {
	s.by = by
	if s.err != nil {
		return 0, s.err
	}
	s.changed = len(ids)
	return len(ids), nil
}

func (s *stubGroups) RemoveFromGroup(_ context.Context, _ int, ids []int) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.changed = len(ids)
	return len(ids), nil
}

func (s *stubGroups) GroupsForSubscriber(_ context.Context, _ int) ([]revenue.SubscriberGroup, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []revenue.SubscriberGroup{s.group}, nil
}

func TestFR_GRP_001_AnUnwiredGroupStoreIsReported(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{})
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"create": func() error {
			_, err := h.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"})
			return err
		},
		"rename":    func() error { return h.RenameSubscriberGroup(ctx, 1, revenue.SubscriberGroup{Name: "x"}) },
		"setActive": func() error { return h.SetSubscriberGroupActive(ctx, 1, false) },
		"delete":    func() error { return h.DeleteSubscriberGroup(ctx, 1) },
		"list":      func() error { _, err := h.ListSubscriberGroups(ctx, false); return err },
		"add":       func() error { _, err := h.AddToGroup(ctx, 1, []int{1}); return err },
		"remove":    func() error { _, err := h.RemoveFromGroup(ctx, 1, []int{1}); return err },
		"forSub":    func() error { _, err := h.GroupsForSubscriber(ctx, 1); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, api.ErrSubscriberGroupsUnwired) {
				t.Errorf("want ErrSubscriberGroupsUnwired, got %v", err)
			}
		})
	}
}

// Membership changes are audited with the count, because a group is what an
// announcement is sent to: "who was in this group when that message went out"
// is answered from the trail.
func TestFR_GRP_001_MembershipChangesRecordHowMany(t *testing.T) {
	s := &stubGroups{}
	h := api.NewHandler(api.HandlerDeps{Groups: s})

	added := auditing(t, func(ctx context.Context) {
		n, err := h.AddToGroup(ctx, 11, []int{1, 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("added %d, want 3", n)
		}
	})
	if len(added) != 1 {
		t.Fatalf("want one audit entry, got %d", len(added))
	}

	removed := auditing(t, func(ctx context.Context) {
		if _, err := h.RemoveFromGroup(ctx, 11, []int{1}); err != nil {
			t.Fatal(err)
		}
	})
	if len(removed) != 1 {
		t.Fatalf("want one audit entry for removal, got %d", len(removed))
	}
	if added[0].Action == removed[0].Action {
		t.Errorf("adding and removing members are the same action in the trail: %q",
			added[0].Action)
	}
}

func TestFR_GRP_001_CreatingAGroupPassesTheActorAndIsAudited(t *testing.T) {
	s := &stubGroups{}
	h := api.NewHandler(api.HandlerDeps{Groups: s})

	entries := auditing(t, func(ctx context.Context) {
		g, err := h.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "Anna Nagar"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if g.ID != 11 {
			t.Errorf("id = %d", g.ID)
		}
	})
	if s.by != "meena" {
		t.Errorf("the store was told the actor is %q", s.by)
	}
	if len(entries) != 1 {
		t.Fatalf("want one audit entry, got %d", len(entries))
	}
}

// ── Fair-use policies and top-ups ───────────────────────────────────────────

type stubFUP struct {
	err      error
	policy   fup.Policy
	by       string
	topup    fup.Topup
	gotTopup fup.Topup
}

func (s *stubFUP) SaveFUPPolicy(_ context.Context, p fup.Policy, by string) (*fup.Policy, error) {
	s.by = by
	if s.err != nil {
		return nil, s.err
	}
	p.ID = 5
	p.InUseByPlans = 2
	return &p, nil
}

func (s *stubFUP) SetFUPPolicyActive(_ context.Context, _ int, _ bool, by string) error {
	s.by = by
	return s.err
}

func (s *stubFUP) DeleteFUPPolicy(_ context.Context, _ int) error { return s.err }

func (s *stubFUP) ListFUPPolicies(_ context.Context, _ bool) ([]fup.Policy, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []fup.Policy{s.policy}, nil
}

func (s *stubFUP) FUPPolicyByID(_ context.Context, id int) (*fup.Policy, error) {
	if s.err != nil {
		return nil, s.err
	}
	p := s.policy
	p.ID = id
	return &p, nil
}

func (s *stubFUP) SetPlanFUPPolicy(_ context.Context, _ int, _ *int, by string) error {
	s.by = by
	return s.err
}

func (s *stubFUP) AddDataTopup(_ context.Context, t fup.Topup, by string) (*fup.Topup, error) {
	s.by = by
	s.gotTopup = t
	if s.err != nil {
		return nil, s.err
	}
	t.ID = 21
	return &t, nil
}

func (s *stubFUP) ListDataTopups(_ context.Context, _ int) ([]fup.Topup, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []fup.Topup{s.topup}, nil
}

func (s *stubFUP) ActiveTopupBytes(_ context.Context, _ int) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return 50 << 30, nil
}

func policy() fup.Policy {
	return fup.Policy{Name: "Home FUP", Tiers: []fup.Tier{
		{ThresholdBytes: 500 << 30, ThrottleRate: "2M/2M"},
		{ThresholdBytes: 1000 << 30, ThrottleRate: "512k/512k"},
	}}
}

func TestFR_CAT_003_AnUnwiredPolicyStoreIsReported(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{})
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"save":        func() error { _, err := h.SaveFUPPolicy(ctx, policy()); return err },
		"setActive":   func() error { return h.SetFUPPolicyActive(ctx, 1, false) },
		"delete":      func() error { return h.DeleteFUPPolicy(ctx, 1) },
		"list":        func() error { _, err := h.ListFUPPolicies(ctx, false); return err },
		"setPlan":     func() error { return h.SetPlanFUPPolicy(ctx, 1, nil) },
		"grantTopup":  func() error { _, err := h.GrantDataTopup(ctx, 1, 1<<30, time.Now(), nil); return err },
		"listTopups":  func() error { _, err := h.ListDataTopups(ctx, 1); return err },
		"activeBytes": func() error { _, err := h.ActiveTopupBytes(ctx, 1); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, api.ErrFUPPoliciesUnwired) {
				t.Errorf("want ErrFUPPoliciesUnwired, got %v", err)
			}
		})
	}
}

// The tiers are the change. The stored row says what the policy looks like
// now; only the trail can answer which edit throttled a customer harder.
func TestFR_CAT_003_SavingAPolicyAuditsEveryTier(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{FUPPolicies: &stubFUP{}})

	entries := auditing(t, func(ctx context.Context) {
		if _, err := h.SaveFUPPolicy(ctx, policy()); err != nil {
			t.Fatalf("save: %v", err)
		}
	})
	if len(entries) != 1 {
		t.Fatalf("want one audit entry, got %d", len(entries))
	}
	tiers, ok := entries[0].Detail["tiers"]
	if !ok {
		t.Fatalf("the audit detail carries no tiers: %+v", entries[0].Detail)
	}
	if list, ok := tiers.([]map[string]any); ok && len(list) != 2 {
		t.Errorf("audited %d tiers, want 2", len(list))
	}
}

// A top-up must record who granted it. Without a subject the grant is refused
// rather than written anonymously — "who gave this customer 50GB" is the
// first question when one turns up unexplained.
func TestFR_CAT_003_AnAnonymousTopupIsRefused(t *testing.T) {
	s := &stubFUP{}
	h := api.NewHandler(api.HandlerDeps{FUPPolicies: s})

	// No subject on the context at all.
	_, err := h.GrantDataTopup(context.Background(), 1, 50<<30, time.Now().Add(720*time.Hour), nil)
	if err == nil {
		t.Fatal("a top-up with no actor must be refused")
	}
	if s.gotTopup.Bytes != 0 {
		t.Errorf("the refused top-up reached the store anyway: %+v", s.gotTopup)
	}
}

// A paid top-up and a goodwill grant must not look alike in the trail.
func TestFR_CAT_003_ThePaidFlagDistinguishesASaleFromAGoodwillGrant(t *testing.T) {
	s := &stubFUP{}
	h := api.NewHandler(api.HandlerDeps{FUPPolicies: s})
	txn := 404
	expires := time.Now().Add(720 * time.Hour)

	free := auditing(t, func(ctx context.Context) {
		if _, err := h.GrantDataTopup(ctx, 1, 50<<30, expires, nil); err != nil {
			t.Fatal(err)
		}
	})
	paid := auditing(t, func(ctx context.Context) {
		if _, err := h.GrantDataTopup(ctx, 1, 50<<30, expires, &txn); err != nil {
			t.Fatal(err)
		}
	})

	if len(free) != 1 || len(paid) != 1 {
		t.Fatalf("want one entry each, got %d and %d", len(free), len(paid))
	}
	if free[0].Detail["paid"] != false {
		t.Errorf("a goodwill grant is recorded as paid=%v", free[0].Detail["paid"])
	}
	if paid[0].Detail["paid"] != true {
		t.Errorf("a sale is recorded as paid=%v", paid[0].Detail["paid"])
	}
	if paid[0].Detail["transaction_id"] != txn {
		t.Errorf("transaction_id = %v, want %d — the trail should reach the payment",
			paid[0].Detail["transaction_id"], txn)
	}
}

func TestFR_CAT_003_TopupReadsPassThroughAndReportStoreFailures(t *testing.T) {
	ok := api.NewHandler(api.HandlerDeps{FUPPolicies: &stubFUP{topup: fup.Topup{ID: 21, Bytes: 1 << 30}}})
	bad := api.NewHandler(api.HandlerDeps{FUPPolicies: &stubFUP{err: errStore}})
	ctx := context.Background()

	if list, err := ok.ListDataTopups(ctx, 1); err != nil || len(list) != 1 {
		t.Errorf("list: %v, %d rows", err, len(list))
	}
	if n, err := ok.ActiveTopupBytes(ctx, 1); err != nil || n != 50<<30 {
		t.Errorf("active bytes: %v, %d", err, n)
	}
	if _, err := bad.ListDataTopups(ctx, 1); !errors.Is(err, errStore) {
		t.Errorf("want the store's error, got %v", err)
	}
	if _, err := bad.ActiveTopupBytes(ctx, 1); !errors.Is(err, errStore) {
		t.Errorf("want the store's error, got %v", err)
	}
}
