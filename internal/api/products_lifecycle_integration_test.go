//go:build integration

// The product catalogue's edit, retire, delete and charge-ending paths, and
// what each says on failure — FR-BIL-022.
//
// products_integration_test.go covers the role gates, creation and
// attachment. What it left (products.go at 63%): updating a product, which
// must keep the code and kind it was created with whatever the body says;
// retiring and reinstating; deleting one subscribers hold; ending a charge;
// the two subscriber listings; and every error mapping in writeProductError.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

var errProductStore = errors.New("store: products unreachable")

// recProducts records the calls the shared stub does not, and fails on
// demand.
type recProducts struct {
	*stubProducts

	gotActive  *bool
	gotDeleted int
	gotFilter  billing.ProductFilter
	gotHistory bool
	gotLimit   int
	gotUpdated *billing.Product
	activeErr  error
	deleteErr  error
	getErr     error
	listErr    error
	updateErr  error
}

func newRecProducts() *recProducts { return &recProducts{stubProducts: &stubProducts{}} }

func (r *recProducts) SetProductActive(_ context.Context, _ int, active bool, _ string) error {
	if r.activeErr != nil {
		return r.activeErr
	}
	r.gotActive = &active
	return nil
}

func (r *recProducts) DeleteProduct(_ context.Context, id int) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	r.gotDeleted = id
	return nil
}

func (r *recProducts) GetProduct(ctx context.Context, id int) (*billing.Product, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	p, err := r.stubProducts.GetProduct(ctx, id)
	if p != nil {
		upd := time.Now()
		p.UpdatedAt, p.UpdatedBy = &upd, "ravi"
	}
	return p, err
}

func (r *recProducts) UpdateProduct(ctx context.Context, in billing.Product, by string) (*billing.Product, error) {
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	r.gotUpdated = &in
	return r.stubProducts.UpdateProduct(ctx, in, by)
}

func (r *recProducts) ListProducts(ctx context.Context, f billing.ProductFilter) ([]billing.Product, error) {
	r.gotFilter = f
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubProducts.ListProducts(ctx, f)
}

func (r *recProducts) ListSubscriberProducts(ctx context.Context, id int, history bool) ([]billing.SubscriberProduct, error) {
	r.gotHistory = history
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubProducts.ListSubscriberProducts(ctx, id, history)
}

func (r *recProducts) ListAttachmentsAcrossSubscribers(ctx context.Context, id, limit int) ([]billing.SubscriberProduct, error) {
	r.gotLimit = limit
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubProducts.ListAttachmentsAcrossSubscribers(ctx, id, limit)
}

func ownerJSON(t *testing.T, store api.ProductStore, method, path, body string) (int, string) {
	t.Helper()
	rec := httptestJSON(t, productHarness(t, store), method, path, "isp_owner", body)
	return rec.Code, rec.Body.String()
}

// The code and kind are fixed after creation: an update keeps the stored
// ones whatever the body says, rather than half-applying them.
func TestFR_BIL_022_AnUpdateKeepsTheCodeAndKind(t *testing.T) {
	s := newRecProducts()
	code, body := ownerJSON(t, s, http.MethodPut, "/api/v1/products/7",
		`{"code":"SOMETHING_ELSE","kind":"one_time","name":"Router rental (dual band)","price":"175","hsn_sac":"8517"}`)
	if code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	if s.gotUpdated == nil || s.gotUpdated.Code != "ROUTER" || s.gotUpdated.Kind != billing.ProductRecurring {
		t.Errorf("the store was asked to change the code or kind: %+v", s.gotUpdated)
	}
	if s.gotUpdated.Name != "Router rental (dual band)" || s.gotUpdated.Price.StringFixed(2) != "175.00" {
		t.Errorf("the editable fields did not reach the store: %+v", s.gotUpdated)
	}

	for name, tc := range map[string]struct {
		path, body string
		setup      func(*recProducts)
		code       int
		errCode    string
	}{
		"bad id":          {"/api/v1/products/x", `{}`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"malformed":       {"/api/v1/products/7", `{"name":`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"price not a sum": {"/api/v1/products/7", `{"name":"x","price":"cheap"}`, nil, http.StatusUnprocessableEntity, "ERR_VALIDATION"},
		"no such product": {"/api/v1/products/99", `{"name":"x","price":"1","hsn_sac":"8517"}`, nil, http.StatusNotFound, "ERR_NOT_FOUND"},
		"lookup failed":   {"/api/v1/products/7", `{"name":"x","price":"1","hsn_sac":"8517"}`, func(r *recProducts) { r.getErr = errProductStore }, http.StatusInternalServerError, "ERR_INTERNAL"},
		"no name":         {"/api/v1/products/7", `{"name":"","price":"1","hsn_sac":"8517"}`, nil, http.StatusUnprocessableEntity, "ERR_VALIDATION"},
		"store failed":    {"/api/v1/products/7", `{"name":"x","price":"1","hsn_sac":"8517"}`, func(r *recProducts) { r.updateErr = errProductStore }, http.StatusInternalServerError, "ERR_INTERNAL"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newRecProducts()
			if tc.setup != nil {
				tc.setup(s)
			}
			code, body := ownerJSON(t, s, http.MethodPut, tc.path, tc.body)
			if code != tc.code || !strings.Contains(body, tc.errCode) {
				t.Errorf("got %d %s, want %d %s", code, body, tc.code, tc.errCode)
			}
			if strings.Contains(body, errProductStore.Error()) {
				t.Error("the store's error reached the client")
			}
		})
	}
}

func TestFR_BIL_022_RetireReinstateAndDelete(t *testing.T) {
	for _, active := range []bool{false, true} {
		s := newRecProducts()
		body := `{"active":false}`
		if active {
			body = `{"active":true}`
		}
		if code, out := ownerJSON(t, s, http.MethodPost, "/api/v1/products/7/active", body); code != http.StatusNoContent {
			t.Fatalf("active=%v: %d %s", active, code, out)
		}
		if s.gotActive == nil || *s.gotActive != active {
			t.Errorf("active=%v reached the store as %v", active, s.gotActive)
		}
	}

	s := newRecProducts()
	if code, _ := ownerJSON(t, s, http.MethodDelete, "/api/v1/products/7", ""); code != http.StatusNoContent || s.gotDeleted != 7 {
		t.Errorf("delete: %d, deleted %d", code, s.gotDeleted)
	}

	for name, tc := range map[string]struct {
		method, path, body string
		setup              func(*recProducts)
		code               int
		errCode            string
	}{
		"retire, bad id":       {http.MethodPost, "/api/v1/products/x/active", `{}`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"retire, malformed":    {http.MethodPost, "/api/v1/products/7/active", `{`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"retire, missing":      {http.MethodPost, "/api/v1/products/9/active", `{"active":false}`, func(r *recProducts) { r.activeErr = billing.ErrProductNotFound }, http.StatusNotFound, "ERR_NOT_FOUND"},
		"delete, bad id":       {http.MethodDelete, "/api/v1/products/x", "", nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"delete, held":         {http.MethodDelete, "/api/v1/products/7", "", func(r *recProducts) { r.deleteErr = billing.ErrProductInUse }, http.StatusConflict, "ERR_PRODUCT_IN_USE"},
		"delete, store failed": {http.MethodDelete, "/api/v1/products/7", "", func(r *recProducts) { r.deleteErr = errProductStore }, http.StatusInternalServerError, "ERR_INTERNAL"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newRecProducts()
			if tc.setup != nil {
				tc.setup(s)
			}
			code, body := ownerJSON(t, s, tc.method, tc.path, tc.body)
			if code != tc.code || !strings.Contains(body, tc.errCode) {
				t.Errorf("got %d %s, want %d %s", code, body, tc.code, tc.errCode)
			}
		})
	}
}

func TestFR_BIL_022_ReadingProducts(t *testing.T) {
	s := newRecProducts()
	code, body := ownerJSON(t, s, http.MethodGet, "/api/v1/products/7", "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(body), &p)
	if p["price"] != "150.00" || p["code"] != "ROUTER" || p["updated_by"] != "ravi" || p["updated_at"] == nil {
		t.Errorf("product = %v", p)
	}
	if code, _ := ownerJSON(t, s, http.MethodGet, "/api/v1/products/99", ""); code != http.StatusNotFound {
		t.Errorf("a missing product: %d", code)
	}
	if code, _ := ownerJSON(t, s, http.MethodGet, "/api/v1/products/x", ""); code != http.StatusBadRequest {
		t.Errorf("a bad id: %d", code)
	}

	ownerJSON(t, s, http.MethodGet, "/api/v1/products?kind=recurring&active_only=true", "")
	if s.gotFilter.Kind != billing.ProductRecurring || !s.gotFilter.ActiveOnly {
		t.Errorf("the list filter reached the store as %+v", s.gotFilter)
	}
	if code, body := ownerJSON(t, s, http.MethodGet, "/api/v1/products?kind=monthly", ""); code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown kind: %d %s", code, body)
	}

	code, body = ownerJSON(t, s, http.MethodGet, "/api/v1/products/7/subscribers?limit=20", "")
	if code != http.StatusOK || !strings.Contains(body, `"username":"acme"`) || !strings.Contains(body, `"amount":"150.00"`) {
		t.Errorf("product subscribers: %d %s", code, body)
	}
	if s.gotLimit != 20 {
		t.Errorf("limit reached the store as %d", s.gotLimit)
	}
	if code, _ := ownerJSON(t, s, http.MethodGet, "/api/v1/products/x/subscribers", ""); code != http.StatusBadRequest {
		t.Errorf("subscribers, bad id: %d", code)
	}

	code, body = ownerJSON(t, s, http.MethodGet, "/api/v1/subscribers/1/products?history=true", "")
	if code != http.StatusOK || !s.gotHistory || !strings.Contains(body, `"products"`) {
		t.Errorf("subscriber products: %d history=%v %s", code, s.gotHistory, body)
	}
	ownerJSON(t, s, http.MethodGet, "/api/v1/subscribers/1/products", "")
	if s.gotHistory {
		t.Error("history defaulted to true")
	}
	if code, _ := ownerJSON(t, s, http.MethodGet, "/api/v1/subscribers/x/products", ""); code != http.StatusBadRequest {
		t.Errorf("subscriber products, bad id: %d", code)
	}

	s.listErr = errProductStore
	for _, path := range []string{"/api/v1/products", "/api/v1/products/7/subscribers", "/api/v1/subscribers/1/products"} {
		code, body := ownerJSON(t, s, http.MethodGet, path, "")
		if code != http.StatusInternalServerError || strings.Contains(body, errProductStore.Error()) {
			t.Errorf("%s failing: %d %s", path, code, body)
		}
	}
}

func TestFR_BIL_022_EndingACharge(t *testing.T) {
	s := newRecProducts()
	if code, body := ownerJSON(t, s, http.MethodPost, "/api/v1/subscriber-products/42/end", `{"reason":"router returned"}`); code != http.StatusNoContent {
		t.Fatalf("end: %d %s", code, body)
	}
	if s.endedID != 42 || s.endReason != "router returned" {
		t.Errorf("ended %d with %q", s.endedID, s.endReason)
	}
	for name, tc := range map[string]struct {
		path, body string
		err        error
		code       int
		errCode    string
	}{
		"bad id":         {"/api/v1/subscriber-products/x/end", `{}`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"malformed":      {"/api/v1/subscriber-products/42/end", `{`, nil, http.StatusBadRequest, "ERR_BAD_REQUEST"},
		"already closed": {"/api/v1/subscriber-products/42/end", `{"reason":"x"}`, billing.ErrAttachmentNotActive, http.StatusConflict, "ERR_CHARGE_CLOSED"},
		"no such charge": {"/api/v1/subscriber-products/42/end", `{"reason":"x"}`, billing.ErrAttachmentNotFound, http.StatusNotFound, "ERR_NOT_FOUND"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newRecProducts()
			s.endErr = tc.err
			code, body := ownerJSON(t, s, http.MethodPost, tc.path, tc.body)
			if code != tc.code || !strings.Contains(body, tc.errCode) {
				t.Errorf("got %d %s, want %d %s", code, body, tc.code, tc.errCode)
			}
		})
	}

	// A retired product cannot be newly attached, and says how to fix it.
	s = newRecProducts()
	s.attachErr = billing.ErrProductInactive
	if code, body := ownerJSON(t, s, http.MethodPost, "/api/v1/subscribers/1/products", `{"product_id":7,"quantity":1}`); code != http.StatusConflict || !strings.Contains(body, "ERR_PRODUCT_INACTIVE") {
		t.Errorf("attach a retired product: %d %s", code, body)
	}
	if code, _ := ownerJSON(t, s, http.MethodPost, "/api/v1/subscribers/x/products", `{}`); code != http.StatusBadRequest {
		t.Errorf("attach, bad id: %d", code)
	}
	if code, _ := ownerJSON(t, s, http.MethodPost, "/api/v1/subscribers/1/products", `{`); code != http.StatusBadRequest {
		t.Errorf("attach, malformed: %d", code)
	}
}

// No product store at all is a 503 on every route, not a panic.
func TestFR_BIL_022_AnUnconfiguredCatalogueSaysSo(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/products", ""},
		{http.MethodGet, "/api/v1/products/7", ""},
		{http.MethodPost, "/api/v1/products", `{"code":"X","name":"x","kind":"one_time","price":"1","hsn_sac":"8517"}`},
		{http.MethodPost, "/api/v1/products/7/active", `{"active":false}`},
		{http.MethodDelete, "/api/v1/products/7", ""},
		{http.MethodPost, "/api/v1/subscriber-products/42/end", `{"reason":"x"}`},
	} {
		rec := httptestJSON(t, mux, tc.method, tc.path, "isp_owner", tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
