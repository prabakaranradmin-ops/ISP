//go:build integration

// The rest of the catalogue service layer: the lifecycle transitions and the
// read paths that catalogue_service_test.go's first pass did not reach.
//
// Split into its own file to keep each readable. Shares that file's stubs and
// the auditing helper.
package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Retiring, restoring and deleting must be three distinct events. Each is one
// store call plus one audit entry — code that reads as obviously right, which
// is why none of it was tested.
func TestFR_CAT_002_RetireRestoreAndDeleteAreThreeDistinctEvents(t *testing.T) {
	s := &stubBandwidth{}
	h := bandwidthHandler(s)

	retired := auditing(t, func(ctx context.Context) {
		if err := h.SetBandwidthProfileActive(ctx, 7, false); err != nil {
			t.Fatal(err)
		}
	})
	restored := auditing(t, func(ctx context.Context) {
		if err := h.SetBandwidthProfileActive(ctx, 7, true); err != nil {
			t.Fatal(err)
		}
	})
	deleted := auditing(t, func(ctx context.Context) {
		if err := h.DeleteBandwidthProfile(ctx, 7); err != nil {
			t.Fatal(err)
		}
	})

	for name, got := range map[string][]middleware.AuditEntry{
		"retire": retired, "restore": restored, "delete": deleted,
	} {
		if len(got) != 1 {
			t.Fatalf("%s: want one audit entry, got %d", name, len(got))
		}
		if got[0].Target != "7" {
			t.Errorf("%s: target = %q, want the profile id", name, got[0].Target)
		}
	}

	seen := map[string]bool{}
	for _, e := range [][]middleware.AuditEntry{retired, restored, deleted} {
		if seen[e[0].Action] {
			t.Errorf("%q is used for more than one of retire, restore and delete", e[0].Action)
		}
		seen[e[0].Action] = true
	}
	if s.deleted != 7 {
		t.Errorf("the store was asked to delete profile %d", s.deleted)
	}
}

func TestFR_CAT_002_AProfileReadsBackByID(t *testing.T) {
	h := bandwidthHandler(&stubBandwidth{profile: home50M()})
	p, err := h.BandwidthProfileByID(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || p.ID != 7 || p.Name != "Home 50M" {
		t.Errorf("got %+v", p)
	}

	bad := bandwidthHandler(&stubBandwidth{err: errStore})
	if _, err := bad.BandwidthProfileByID(context.Background(), 7); !errors.Is(err, errStore) {
		t.Errorf("want the store's error, got %v", err)
	}
}

// The trail carries the burst settings when there are any, and the rendered
// NAS attribute either way. The rendered string is what a router actually
// received; the numbers alone do not answer "what speed was this customer
// getting last Tuesday".
func TestFR_CAT_002_TheAuditDetailCarriesBurstAndTheRenderedAttribute(t *testing.T) {
	burst := home50M()
	burst.BurstDownBPS = 100_000_000
	burst.BurstUpBPS = 100_000_000
	burst.BurstThresholdDown = 40_000_000
	burst.BurstThresholdUp = 40_000_000
	burst.BurstTimeSeconds = 8

	h := bandwidthHandler(&stubBandwidth{})
	withBurst := auditing(t, func(ctx context.Context) {
		if _, err := h.CreateBandwidthProfile(ctx, burst); err != nil {
			t.Fatal(err)
		}
	})
	plain := auditing(t, func(ctx context.Context) {
		if _, err := h.CreateBandwidthProfile(ctx, home50M()); err != nil {
			t.Fatal(err)
		}
	})

	if len(withBurst) != 1 || len(plain) != 1 {
		t.Fatalf("want one entry each, got %d and %d", len(withBurst), len(plain))
	}
	if withBurst[0].Detail["burst_down_bps"] != int64(100_000_000) {
		t.Errorf("burst_down_bps = %v", withBurst[0].Detail["burst_down_bps"])
	}
	if withBurst[0].Detail["burst_time_seconds"] != 8 {
		t.Errorf("burst_time_seconds = %v", withBurst[0].Detail["burst_time_seconds"])
	}
	if _, ok := plain[0].Detail["burst_down_bps"]; ok {
		t.Error("a profile with no burst recorded burst fields anyway")
	}
	for _, e := range [][]middleware.AuditEntry{withBurst, plain} {
		_, rendered := e[0].Detail["rate_limit"]
		_, failed := e[0].Detail["rate_limit_error"]
		if !rendered && !failed {
			t.Errorf("the trail records neither the rendered attribute nor why it could "+
				"not be rendered: %+v", e[0].Detail)
		}
	}
}

func TestFR_CAT_003_PolicyLifecycleIsAudited(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{FUPPolicies: &stubFUP{policy: policy()}})
	id := 5

	for name, call := range map[string]func(context.Context) error{
		"retire":  func(ctx context.Context) error { return h.SetFUPPolicyActive(ctx, 5, false) },
		"restore": func(ctx context.Context) error { return h.SetFUPPolicyActive(ctx, 5, true) },
		"delete":  func(ctx context.Context) error { return h.DeleteFUPPolicy(ctx, 5) },
		"attach":  func(ctx context.Context) error { return h.SetPlanFUPPolicy(ctx, 2, &id) },
		"detach":  func(ctx context.Context) error { return h.SetPlanFUPPolicy(ctx, 2, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			entries := auditing(t, func(ctx context.Context) {
				if err := call(ctx); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			})
			if len(entries) != 1 {
				t.Fatalf("%s: want one audit entry, got %d", name, len(entries))
			}
		})
	}

	if list, err := h.ListFUPPolicies(context.Background(), true); err != nil || len(list) != 1 {
		t.Errorf("list: %v, %d rows", err, len(list))
	}
}

func TestFR_CAT_003_AFailedPolicyWriteIsNotAudited(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{FUPPolicies: &stubFUP{err: errStore}})

	for name, call := range map[string]func(context.Context) error{
		"save":      func(ctx context.Context) error { _, err := h.SaveFUPPolicy(ctx, policy()); return err },
		"setActive": func(ctx context.Context) error { return h.SetFUPPolicyActive(ctx, 1, false) },
		"delete":    func(ctx context.Context) error { return h.DeleteFUPPolicy(ctx, 1) },
		"setPlan":   func(ctx context.Context) error { return h.SetPlanFUPPolicy(ctx, 1, nil) },
		"list":      func(ctx context.Context) error { _, err := h.ListFUPPolicies(ctx, false); return err },
		"grant": func(ctx context.Context) error {
			_, err := h.GrantDataTopup(ctx, 1, 1<<30, time.Now(), nil)
			return err
		},
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

func TestFR_GRP_001_GroupLifecycleIsAuditedAndReadsPassThrough(t *testing.T) {
	s := &stubGroups{group: revenue.SubscriberGroup{ID: 11, Name: "Anna Nagar", Members: 42}}
	h := api.NewHandler(api.HandlerDeps{Groups: s})

	for name, call := range map[string]func(context.Context) error{
		"rename": func(ctx context.Context) error {
			return h.RenameSubscriberGroup(ctx, 11, revenue.SubscriberGroup{Name: "Anna Nagar West"})
		},
		"retire":  func(ctx context.Context) error { return h.SetSubscriberGroupActive(ctx, 11, false) },
		"restore": func(ctx context.Context) error { return h.SetSubscriberGroupActive(ctx, 11, true) },
		"delete":  func(ctx context.Context) error { return h.DeleteSubscriberGroup(ctx, 11) },
	} {
		t.Run(name, func(t *testing.T) {
			entries := auditing(t, func(ctx context.Context) {
				if err := call(ctx); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			})
			if len(entries) != 1 {
				t.Fatalf("%s: want one audit entry, got %d", name, len(entries))
			}
		})
	}

	ctx := context.Background()
	if list, err := h.ListSubscriberGroups(ctx, true); err != nil || len(list) != 1 {
		t.Errorf("list: %v, %d rows", err, len(list))
	}
	if list, err := h.GroupsForSubscriber(ctx, 1); err != nil || len(list) != 1 {
		t.Errorf("groups for subscriber: %v, %d rows", err, len(list))
	}
}

func TestFR_GRP_001_EveryGroupCallReportsAStoreFailure(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{Groups: &stubGroups{err: errStore}})
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"create": func() error {
			_, err := h.CreateSubscriberGroup(ctx, revenue.SubscriberGroup{Name: "x"})
			return err
		},
		"rename": func() error { return h.RenameSubscriberGroup(ctx, 1, revenue.SubscriberGroup{Name: "x"}) },
		"retire": func() error { return h.SetSubscriberGroupActive(ctx, 1, false) },
		"delete": func() error { return h.DeleteSubscriberGroup(ctx, 1) },
		"list":   func() error { _, err := h.ListSubscriberGroups(ctx, false); return err },
		"add":    func() error { _, err := h.AddToGroup(ctx, 1, []int{1}); return err },
		"remove": func() error { _, err := h.RemoveFromGroup(ctx, 1, []int{1}); return err },
		"forSub": func() error { _, err := h.GroupsForSubscriber(ctx, 1); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errStore) {
				t.Errorf("want the store's error, got %v", err)
			}
		})
	}
}
