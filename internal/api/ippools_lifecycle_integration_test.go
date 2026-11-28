//go:build integration

// IP pools and static addresses: the paths ippools_integration_test.go left —
// FR-NET-005.
//
// That file covers the role gates, creation and the JSON shape. What it left
// (ippools.go at 67%): the cache invalidation that makes an allocated or
// released address reach the NAS, the list filters, exclusions, deleting a
// pool in use, and every error mapping in writeIPPoolError.
//
// The invalidation is the part worth defending. A released static address
// that stays in the auth cache keeps being handed to its former holder, and
// the release path has to find who that was before it releases, because
// there is no by-id getter: it walks the static pools' assignments.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

var errPoolStore = errors.New("store: ip_pools unreachable")

type recIPPools struct {
	*stubIPPools

	gotFilter     nas.PoolFilter
	removed       int
	listErr       error
	assignmentErr error
	deleteErr     error
	removeErr     error
	allocErr      error
	releaseErr    error
	exclusionErr  error
}

func newRecIPPools() *recIPPools { return &recIPPools{stubIPPools: &stubIPPools{}} }

func (r *recIPPools) ListPools(ctx context.Context, f nas.PoolFilter) ([]nas.Pool, error) {
	r.gotFilter = f
	if r.listErr != nil {
		return nil, r.listErr
	}
	p := stubPool()
	ex, _ := nas.ParseAddrRange("203.0.113.6", "203.0.113.7") //nolint:errcheck // a literal
	p.Exclusions = []nas.PoolExclusion{{ID: 4, Range: ex, Reason: "router and gateway"}}
	return []nas.Pool{p}, nil
}

func (r *recIPPools) ListPoolAssignments(ctx context.Context, id int) ([]nas.StaticAssignment, error) {
	if r.assignmentErr != nil {
		return nil, r.assignmentErr
	}
	return r.stubIPPools.ListPoolAssignments(ctx, id)
}

func (r *recIPPools) DeletePool(context.Context, int) error { return r.deleteErr }
func (r *recIPPools) RemoveExclusion(_ context.Context, id int) error {
	if r.removeErr != nil {
		return r.removeErr
	}
	r.removed = id
	return nil
}

func (r *recIPPools) AddExclusion(ctx context.Context, poolID int, rng nas.AddrRange, reason, by string) (*nas.PoolExclusion, error) {
	if r.exclusionErr != nil {
		return nil, r.exclusionErr
	}
	return r.stubIPPools.AddExclusion(ctx, poolID, rng, reason, by)
}

func (r *recIPPools) AllocateStaticIP(ctx context.Context, req nas.AllocateRequest, by string) (*nas.StaticAssignment, error) {
	if r.allocErr != nil {
		return nil, r.allocErr
	}
	return r.stubIPPools.AllocateStaticIP(ctx, req, by)
}

func (r *recIPPools) ReleaseStaticIP(ctx context.Context, id int, reason, by string) error {
	if r.releaseErr != nil {
		return r.releaseErr
	}
	return r.stubIPPools.ReleaseStaticIP(ctx, id, reason, by)
}

// failingSubCache fails every invalidation.
type failingSubCache struct{ calls int }

func (f *failingSubCache) InvalidateSubscriber(context.Context, string) error {
	f.calls++
	return errors.New("redis: connection refused")
}

func poolMux(t *testing.T, store api.IPPoolStore, cache api.SubscriberCacheInvalidator) *http.ServeMux {
	t.Helper()
	deps := api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}), IPPools: store}
	if cache != nil {
		deps.SubCache = cache
	}
	h := api.NewHandler(deps)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func nocJSON(t *testing.T, mux *http.ServeMux, method, path, body string) (int, string) {
	t.Helper()
	rec := httptestJSON(t, mux, method, path, "isp_owner", body)
	return rec.Code, rec.Body.String()
}

// Allocating and releasing both reach the auth cache, for the right person.
func TestFR_NET_005_AnAddressChangeReachesTheAuthCache(t *testing.T) {
	cache := &stubSubCache{}
	store := newRecIPPools()
	mux := poolMux(t, store, cache)

	if code, body := nocJSON(t, mux, http.MethodPost, "/api/v1/subscribers/1/static-ip", `{"pool_id":1}`); code != http.StatusCreated {
		t.Fatalf("allocate: %d %s", code, body)
	}
	// The release finds the holder (assignment 9, "biz") before releasing.
	if code, body := nocJSON(t, mux, http.MethodPost, "/api/v1/static-ips/9/release", `{"reason":"moved office"}`); code != http.StatusNoContent {
		t.Fatalf("release: %d %s", code, body)
	}
	if len(cache.invalidated) != 2 || cache.invalidated[0] != "biz" || cache.invalidated[1] != "biz" {
		t.Errorf("invalidated %v; want the holder on both the allocation and the release", cache.invalidated)
	}
	if store.released != 9 || store.reason != "moved office" {
		t.Errorf("released %d with %q", store.released, store.reason)
	}
	// The walk only looks at static pools: dhcp and cgnat hand out nothing
	// that could be released here.
	if store.gotFilter.Purpose != nas.PoolStatic {
		t.Errorf("the holder lookup walked %q pools", store.gotFilter.Purpose)
	}

	// An id nobody holds still releases (the store decides), and
	// invalidates nobody.
	cache.invalidated = nil
	if code, _ := nocJSON(t, mux, http.MethodPost, "/api/v1/static-ips/77/release", `{"reason":"x"}`); code != http.StatusNoContent {
		t.Errorf("release of an unlisted id: %d", code)
	}
	if len(cache.invalidated) != 0 {
		t.Errorf("an unlisted id invalidated %v", cache.invalidated)
	}
}

// A cache that cannot be reached does not undo the address change, and the
// lookup failing does not block the release.
func TestFR_NET_005_ACacheFailureDoesNotFailTheChange(t *testing.T) {
	cache := &failingSubCache{}
	mux := poolMux(t, newRecIPPools(), cache)
	if code, _ := nocJSON(t, mux, http.MethodPost, "/api/v1/subscribers/1/static-ip", `{"pool_id":1}`); code != http.StatusCreated {
		t.Errorf("allocate with the cache down: %d", code)
	}
	if cache.calls != 1 {
		t.Errorf("the invalidation was not attempted")
	}

	store := newRecIPPools()
	store.assignmentErr = errPoolStore
	if code, _ := nocJSON(t, poolMux(t, store, &stubSubCache{}), http.MethodPost, "/api/v1/static-ips/9/release", `{"reason":"x"}`); code != http.StatusNoContent {
		t.Errorf("release with the holder lookup failing: %d", code)
	}
	if store.released != 9 {
		t.Error("the lookup failing stopped the release")
	}
}

func TestFR_NET_005_PoolListFilters(t *testing.T) {
	store := newRecIPPools()
	mux := poolMux(t, store, nil)
	code, body := nocJSON(t, mux, http.MethodGet, "/api/v1/ip-pools?purpose=static&nas_id=3&limit=10", "")
	if code != http.StatusOK || store.gotFilter.Purpose != nas.PoolStatic || store.gotFilter.NASID != 3 || store.gotFilter.Limit != 10 {
		t.Errorf("filters: %d %+v", code, store.gotFilter)
	}
	var out struct {
		Pools []struct {
			Exclusions []struct{ Reason string } `json:"exclusions"`
		} `json:"pools"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Pools) != 1 || len(out.Pools[0].Exclusions) != 1 {
		t.Errorf("the pool's exclusions are missing from the list: %s", body)
	}
	for _, q := range []string{"purpose=public", "nas_id=0", "nas_id=router1", "nas_id=-2"} {
		if code, _ := nocJSON(t, mux, http.MethodGet, "/api/v1/ip-pools?"+q, ""); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", q, code)
		}
	}
	store.listErr = errPoolStore
	if code, body := nocJSON(t, mux, http.MethodGet, "/api/v1/ip-pools", ""); code != http.StatusInternalServerError || strings.Contains(body, errPoolStore.Error()) {
		t.Errorf("a failing list: %d %s", code, body)
	}
}

func TestFR_NET_005_PoolsAndExclusions(t *testing.T) {
	store := newRecIPPools()
	mux := poolMux(t, store, nil)

	if code, body := nocJSON(t, mux, http.MethodGet, "/api/v1/ip-pools/1", ""); code != http.StatusOK || !strings.Contains(body, `"first_address":"203.0.113.0"`) {
		t.Errorf("get: %d %s", code, body)
	}
	if code, body := nocJSON(t, mux, http.MethodGet, "/api/v1/ip-pools/1/assignments", ""); code != http.StatusOK || !strings.Contains(body, `"address":"203.0.113.4"`) {
		t.Errorf("assignments: %d %s", code, body)
	}

	code, body := nocJSON(t, mux, http.MethodPost, "/api/v1/ip-pools/1/exclusions",
		`{"first_address":"203.0.113.1","last_address":"203.0.113.1","reason":"gateway"}`)
	if code != http.StatusCreated || !strings.Contains(body, `"reason":"gateway"`) {
		t.Errorf("add exclusion: %d %s", code, body)
	}
	if code, _ := nocJSON(t, mux, http.MethodDelete, "/api/v1/ip-pool-exclusions/4", ""); code != http.StatusNoContent || store.removed != 4 {
		t.Errorf("remove exclusion: %d removed %d", code, store.removed)
	}
	if code, _ := nocJSON(t, mux, http.MethodDelete, "/api/v1/ip-pools/1", ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}

	for name, tc := range map[string]struct {
		method, path, body string
		setup              func(*recIPPools)
		code               int
		errCode            string
	}{
		"get, bad id":           {http.MethodGet, "/api/v1/ip-pools/x", "", nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"get, no such pool":     {http.MethodGet, "/api/v1/ip-pools/2", "", nil, http.StatusNotFound, "ERR_NOT_FOUND"},
		"assignments, bad id":   {http.MethodGet, "/api/v1/ip-pools/x/assignments", "", nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"delete, bad id":        {http.MethodDelete, "/api/v1/ip-pools/x", "", nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"delete, in use":        {http.MethodDelete, "/api/v1/ip-pools/1", "", func(r *recIPPools) { r.deleteErr = nas.ErrPoolInUse }, http.StatusConflict, "ERR_POOL_IN_USE"},
		"exclusion, bad id":     {http.MethodPost, "/api/v1/ip-pools/x/exclusions", `{}`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"exclusion, malformed":  {http.MethodPost, "/api/v1/ip-pools/1/exclusions", `{`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"exclusion, bad range":  {http.MethodPost, "/api/v1/ip-pools/1/exclusions", `{"first_address":"203.0.113.9","last_address":"203.0.113.1"}`, nil, http.StatusUnprocessableEntity, "ERR_VALIDATION"},
		"exclusion, outside":    {http.MethodPost, "/api/v1/ip-pools/1/exclusions", `{"first_address":"198.51.100.1","last_address":"198.51.100.1"}`, func(r *recIPPools) { r.exclusionErr = nas.ErrAddressOutsidePool }, http.StatusUnprocessableEntity, "not in the pool"},
		"remove, bad id":        {http.MethodDelete, "/api/v1/ip-pool-exclusions/x", "", nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"remove, no such range": {http.MethodDelete, "/api/v1/ip-pool-exclusions/4", "", func(r *recIPPools) { r.removeErr = nas.ErrPoolNotFound }, http.StatusNotFound, "ERR_NOT_FOUND"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRecIPPools()
			if tc.setup != nil {
				tc.setup(store)
			}
			code, body := nocJSON(t, poolMux(t, store, nil), tc.method, tc.path, tc.body)
			if code != tc.code || !strings.Contains(body, tc.errCode) {
				t.Errorf("got %d %s, want %d %s", code, body, tc.code, tc.errCode)
			}
		})
	}
}

func TestFR_NET_005_AllocationAndReleaseRefusals(t *testing.T) {
	// An IPv4-mapped IPv6 form of an address is the IPv4 address: stored
	// mapped, it would compare unequal to the same address everywhere else.
	store := newRecIPPools()
	if code, body := nocJSON(t, poolMux(t, store, nil), http.MethodPost, "/api/v1/subscribers/1/static-ip",
		`{"pool_id":1,"address":"::ffff:203.0.113.4"}`); code != http.StatusCreated {
		t.Fatalf("a mapped address: %d %s", code, body)
	}
	if store.allocated.Address != netip.MustParseAddr("203.0.113.4") {
		t.Errorf("stored as %v, want the plain IPv4 address", store.allocated.Address)
	}

	for name, tc := range map[string]struct {
		path, body string
		alloc      error
		release    error
		code       int
		errCode    string
	}{
		"allocate, bad id":      {"/api/v1/subscribers/x/static-ip", `{}`, nil, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"allocate, malformed":   {"/api/v1/subscribers/1/static-ip", `{`, nil, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"allocate, not an IP":   {"/api/v1/subscribers/1/static-ip", `{"pool_id":1,"address":"203.0.113"}`, nil, nil, http.StatusUnprocessableEntity, "ERR_VALIDATION"},
		"allocate, exhausted":   {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, nas.ErrPoolExhausted, nil, http.StatusConflict, "ERR_POOL_EXHAUSTED"},
		"allocate, taken":       {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, nas.ErrAddressTaken, nil, http.StatusConflict, "ERR_ADDRESS_TAKEN"},
		"allocate, excluded":    {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, nas.ErrAddressExcluded, nil, http.StatusConflict, "ERR_ADDRESS_EXCLUDED"},
		"allocate, not static":  {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, nas.ErrPoolNotStatic, nil, http.StatusConflict, "ERR_POOL_NOT_STATIC"},
		"allocate, has one":     {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, nas.ErrSubscriberHasAddress, nil, http.StatusConflict, "ERR_ALREADY_ASSIGNED"},
		"allocate, name taken":  {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, nas.ErrPoolNameTaken, nil, http.StatusConflict, "ERR_DUPLICATE"},
		"allocate, store down":  {"/api/v1/subscribers/1/static-ip", `{"pool_id":1}`, errPoolStore, nil, http.StatusInternalServerError, "ERR_INTERNAL"},
		"release, bad id":       {"/api/v1/static-ips/x/release", `{}`, nil, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"release, malformed":    {"/api/v1/static-ips/9/release", `{`, nil, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"release, already done": {"/api/v1/static-ips/9/release", `{"reason":"x"}`, nil, nas.ErrAssignmentNotActive, http.StatusConflict, "ERR_ALREADY_RELEASED"},
		"release, no such one":  {"/api/v1/static-ips/9/release", `{"reason":"x"}`, nil, nas.ErrAssignmentNotFound, http.StatusNotFound, "ERR_NOT_FOUND"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRecIPPools()
			store.allocErr, store.releaseErr = tc.alloc, tc.release
			cache := &stubSubCache{}
			code, body := nocJSON(t, poolMux(t, store, cache), http.MethodPost, tc.path, tc.body)
			if code != tc.code || !strings.Contains(body, tc.errCode) {
				t.Errorf("got %d %s, want %d %s", code, body, tc.code, tc.errCode)
			}
			if strings.Contains(body, errPoolStore.Error()) {
				t.Error("the store's error reached the client")
			}
			if len(cache.invalidated) != 0 {
				t.Errorf("a refused change invalidated %v", cache.invalidated)
			}
		})
	}

	if code, _ := nocJSON(t, poolMux(t, newRecIPPools(), nil), http.MethodGet, "/api/v1/subscribers/x/static-ip", ""); code != http.StatusBadRequest {
		t.Errorf("list addresses, bad id: %d", code)
	}
}

// No pool store at all is a 503 on every route, not a panic.
func TestFR_NET_005_UnconfiguredPoolsSaySo(t *testing.T) {
	mux := poolMux(t, nil, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/ip-pools", ""},
		{http.MethodGet, "/api/v1/ip-pools/1", ""},
		{http.MethodDelete, "/api/v1/ip-pools/1", ""},
		{http.MethodGet, "/api/v1/ip-pools/1/assignments", ""},
		{http.MethodPost, "/api/v1/ip-pools/1/exclusions", `{"first_address":"203.0.113.1","last_address":"203.0.113.1"}`},
		{http.MethodDelete, "/api/v1/ip-pool-exclusions/4", ""},
		{http.MethodPost, "/api/v1/subscribers/1/static-ip", `{"pool_id":1}`},
		{http.MethodGet, "/api/v1/subscribers/1/static-ip", ""},
		{http.MethodPost, "/api/v1/static-ips/9/release", `{"reason":"x"}`},
	} {
		if code, body := nocJSON(t, mux, tc.method, tc.path, tc.body); code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, code, body)
		}
	}
}
