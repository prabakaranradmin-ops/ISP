//go:build integration

// IP pools and static addresses — FR-NET-005. Routes and role gates; the
// allocation behaviour is proven against a real database in internal/db.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

type stubIPPools struct {
	created   nas.Pool
	allocated nas.AllocateRequest
	released  int
	reason    string
	err       error
}

func stubPool() nas.Pool {
	r, _ := nas.ParseAddrRange("203.0.113.0/29", "") //nolint:errcheck // a literal
	return nas.Pool{ID: 1, Name: "Customer statics", Purpose: nas.PoolStatic, Range: r,
		CreatedBy: "meena", CreatedAt: time.Now()}
}

func (s *stubIPPools) CreatePool(_ context.Context, in nas.Pool, _ string) (*nas.Pool, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	s.created = in.Normalise()
	out := in.Normalise()
	out.ID = 1
	return &out, nil
}

func (s *stubIPPools) GetPool(_ context.Context, id int) (*nas.Pool, error) {
	if id != 1 {
		return nil, nas.ErrPoolNotFound
	}
	p := stubPool()
	return &p, nil
}

func (s *stubIPPools) ListPools(context.Context, nas.PoolFilter) ([]nas.Pool, error) {
	return []nas.Pool{stubPool()}, nil
}

func (s *stubIPPools) DeletePool(context.Context, int) error { return s.err }

func (s *stubIPPools) AddExclusion(_ context.Context, poolID int, r nas.AddrRange, reason, _ string) (*nas.PoolExclusion, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &nas.PoolExclusion{ID: 2, PoolID: poolID, Range: r, Reason: reason}, nil
}

func (s *stubIPPools) RemoveExclusion(context.Context, int) error { return s.err }

func (s *stubIPPools) AllocateStaticIP(_ context.Context, req nas.AllocateRequest, _ string) (*nas.StaticAssignment, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if s.err != nil {
		return nil, s.err
	}
	s.allocated = req
	addr := req.Address
	if !addr.IsValid() {
		addr = netip.MustParseAddr("203.0.113.2")
	}
	return &nas.StaticAssignment{ID: 9, PoolID: req.PoolID, PoolName: "Customer statics",
		SubscriberID: req.SubscriberID, Username: "biz", Address: addr,
		Status: nas.AssignmentActive, AssignedBy: "meena", AssignedAt: time.Now()}, nil
}

// ReleaseStaticIP returns the released row, as the store does: subscriber 1,
// "biz", who held 203.0.113.4.
func (s *stubIPPools) ReleaseStaticIP(_ context.Context, id int, reason, _ string) (*nas.StaticAssignment, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.released, s.reason = id, reason
	return &nas.StaticAssignment{ID: id, PoolName: "Customer statics", SubscriberID: 1, Username: "biz",
		Address: netip.MustParseAddr("203.0.113.4"), Status: "released", ReleaseReason: reason}, nil
}

func (s *stubIPPools) GetSubscriberStaticIP(context.Context, int) (*nas.StaticAssignment, error) {
	return nil, nil
}

func (s *stubIPPools) ListSubscriberStaticIPs(context.Context, int) ([]nas.StaticAssignment, error) {
	return []nas.StaticAssignment{{ID: 9, PoolName: "Customer statics", SubscriberID: 1,
		Username: "biz", Address: netip.MustParseAddr("203.0.113.4"),
		Status: nas.AssignmentActive, AssignedBy: "meena", AssignedAt: time.Now()}}, nil
}

func (s *stubIPPools) ListPoolAssignments(context.Context, int) ([]nas.StaticAssignment, error) {
	return s.ListSubscriberStaticIPs(context.Background(), 1)
}

func ipPoolHarness(t *testing.T, store api.IPPoolStore) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}), IPPools: store})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func TestFR_NET_005_IPPoolAPIGates(t *testing.T) {
	body := `{"name":"Customer statics","purpose":"static","first_address":"203.0.113.0/29"}`

	t.Run("NOC and the owner manage pools", func(t *testing.T) {
		for _, role := range []string{"noc_engineer", "isp_owner"} {
			store := &stubIPPools{}
			rec := httptestJSON(t, ipPoolHarness(t, store), "POST", "/api/v1/ip-pools", role, body)
			if rec.Code != 201 {
				t.Fatalf("%s: got %d: %s", role, rec.Code, rec.Body.String())
			}
			if store.created.Name != "Customer statics" {
				t.Errorf("%s: store got %+v", role, store.created)
			}
			if store.created.Range.Last.String() != "203.0.113.7" {
				t.Errorf("%s: the CIDR block became %s", role, store.created.Range)
			}
		}
	})

	t.Run("billing staff and a CSR cannot touch pools", func(t *testing.T) {
		mux := ipPoolHarness(t, &stubIPPools{})
		for _, role := range []string{"billing_admin", "csr", "technician"} {
			if rec := httptestGet(t, mux, "/api/v1/ip-pools", role); rec.Code != 403 {
				t.Errorf("%s listing pools: got %d, want 403", role, rec.Code)
			}
			if rec := httptestJSON(t, mux, "POST", "/api/v1/ip-pools", role, body); rec.Code != 403 {
				t.Errorf("%s creating a pool: got %d, want 403", role, rec.Code)
			}
		}
	})

	// Assigning is both a network act and a billing one, so NOC and
	// billing both do it — the nocOrBilling gate.
	t.Run("NOC and billing both assign and release", func(t *testing.T) {
		for _, role := range []string{"noc_engineer", "billing_admin", "isp_owner"} {
			store := &stubIPPools{}
			mux := ipPoolHarness(t, store)
			rec := httptestJSON(t, mux, "POST", "/api/v1/subscribers/1/static-ip", role, `{"pool_id":1}`)
			if rec.Code != 201 {
				t.Errorf("%s assigning: got %d: %s", role, rec.Code, rec.Body.String())
			}
			rec = httptestJSON(t, mux, "POST", "/api/v1/static-ips/9/release", role, `{"reason":"downgraded"}`)
			if rec.Code != 204 {
				t.Errorf("%s releasing: got %d", role, rec.Code)
			}
			if store.released != 9 || store.reason != "downgraded" {
				t.Errorf("%s: store got %d / %q", role, store.released, store.reason)
			}
		}
	})

	t.Run("a CSR and a technician cannot assign or release", func(t *testing.T) {
		mux := ipPoolHarness(t, &stubIPPools{})
		for _, role := range []string{"csr", "technician"} {
			if rec := httptestJSON(t, mux, "POST", "/api/v1/subscribers/1/static-ip", role, `{"pool_id":1}`); rec.Code != 403 {
				t.Errorf("%s assigning: got %d, want 403", role, rec.Code)
			}
			if rec := httptestJSON(t, mux, "POST", "/api/v1/static-ips/9/release", role, `{"reason":"x"}`); rec.Code != 403 {
				t.Errorf("%s releasing: got %d, want 403", role, rec.Code)
			}
		}
	})

	// "What is my IP" is a front-desk question, so reading one
	// subscriber's address is staff-wide even though pools are not.
	t.Run("every staff role can read a subscriber's address", func(t *testing.T) {
		mux := ipPoolHarness(t, &stubIPPools{})
		for _, role := range []string{"noc_engineer", "billing_admin", "csr", "technician", "isp_owner"} {
			if rec := httptestGet(t, mux, "/api/v1/subscribers/1/static-ip", role); rec.Code != 200 {
				t.Errorf("%s: got %d", role, rec.Code)
			}
		}
	})

	t.Run("an unconfigured store is a 503, not a 500", func(t *testing.T) {
		h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
			Wallet: billing.NewWalletService(&stubWallet{})})
		m := http.NewServeMux()
		h.RegisterRoutes(m, itJWTSecret)
		if rec := httptestGet(t, m, "/api/v1/ip-pools", "isp_owner"); rec.Code != 503 {
			t.Errorf("got %d", rec.Code)
		}
	})
}

func TestFR_NET_005_IPPoolAPIValidation(t *testing.T) {
	mux := ipPoolHarness(t, &stubIPPools{})

	t.Run("a malformed range is a 422 with the offending value named", func(t *testing.T) {
		rec := httptestJSON(t, mux, "POST", "/api/v1/ip-pools", "noc_engineer",
			`{"name":"X","purpose":"static","first_address":"the office"}`)
		if rec.Code != 422 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "the office") {
			t.Errorf("the message does not name the bad value: %s", rec.Body.String())
		}
	})

	t.Run("an unknown purpose is refused", func(t *testing.T) {
		rec := httptestJSON(t, mux, "POST", "/api/v1/ip-pools", "noc_engineer",
			`{"name":"X","purpose":"whatever","first_address":"203.0.113.1"}`)
		if rec.Code != 422 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("a mixed-family range is refused", func(t *testing.T) {
		rec := httptestJSON(t, mux, "POST", "/api/v1/ip-pools", "noc_engineer",
			`{"name":"X","purpose":"static","first_address":"203.0.113.1","last_address":"2001:db8::1"}`)
		if rec.Code != 422 {
			t.Errorf("got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a bad address on an assignment is a 422", func(t *testing.T) {
		rec := httptestJSON(t, mux, "POST", "/api/v1/subscribers/1/static-ip", "noc_engineer",
			`{"pool_id":1,"address":"nope"}`)
		if rec.Code != 422 {
			t.Errorf("got %d", rec.Code)
		}
	})

	conflicts := map[string]error{
		"ERR_ADDRESS_TAKEN":    nas.ErrAddressTaken,
		"ERR_POOL_EXHAUSTED":   nas.ErrPoolExhausted,
		"ERR_ALREADY_ASSIGNED": nas.ErrSubscriberHasAddress,
		"ERR_ADDRESS_EXCLUDED": nas.ErrAddressExcluded,
		"ERR_POOL_NOT_STATIC":  nas.ErrPoolNotStatic,
	}
	for code, err := range conflicts {
		t.Run(code+" is a 409", func(t *testing.T) {
			rec := httptestJSON(t, ipPoolHarness(t, &stubIPPools{err: err}), "POST",
				"/api/v1/subscribers/1/static-ip", "noc_engineer", `{"pool_id":1}`)
			if rec.Code != 409 {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), code) {
				t.Errorf("want %s, body is %s", code, rec.Body.String())
			}
		})
	}

	t.Run("an address outside the pool is a 422, not a 409", func(t *testing.T) {
		// A caller's mistake about the input, not a conflict with state.
		rec := httptestJSON(t, ipPoolHarness(t, &stubIPPools{err: nas.ErrAddressOutsidePool}),
			"POST", "/api/v1/subscribers/1/static-ip", "noc_engineer", `{"pool_id":1,"address":"198.51.100.1"}`)
		if rec.Code != 422 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("a pool still in use is a 409 that says to keep it", func(t *testing.T) {
		rec := httptestJSON(t, ipPoolHarness(t, &stubIPPools{err: nas.ErrPoolInUse}),
			"DELETE", "/api/v1/ip-pools/1", "noc_engineer", "")
		if rec.Code != 409 {
			t.Fatalf("got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "keep the pool") {
			t.Errorf("unhelpful message: %s", rec.Body.String())
		}
	})
}

// The JSON a client reads, including the address as a bare address rather
// than an INET value with a mask.
func TestFR_NET_005_IPPoolJSONShape(t *testing.T) {
	rec := httptestGet(t, ipPoolHarness(t, &stubIPPools{}), "/api/v1/ip-pools/1", "noc_engineer")
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["first_address"] != "203.0.113.0" || got["last_address"] != "203.0.113.7" {
		t.Errorf("addresses are %v / %v", got["first_address"], got["last_address"])
	}
	if got["size"] != "8" {
		t.Errorf("size is %v, want \"8\"", got["size"])
	}
	if got["purpose"] != nas.PoolStatic {
		t.Errorf("purpose is %v", got["purpose"])
	}

	rec = httptestGet(t, ipPoolHarness(t, &stubIPPools{}), "/api/v1/subscribers/1/static-ip", "csr")
	var list map[string][]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list["addresses"]) != 1 {
		t.Fatalf("got %d addresses", len(list["addresses"]))
	}
	if list["addresses"][0]["address"] != "203.0.113.4" {
		t.Errorf("address is %v, want a bare 203.0.113.4", list["addresses"][0]["address"])
	}
}
