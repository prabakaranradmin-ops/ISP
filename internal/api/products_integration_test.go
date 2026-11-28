//go:build integration

// Product catalogue — FR-BIL-022. Routes and role gates; the catalogue's
// behaviour is proven against a real database in internal/db.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// stubProducts records what the handler asked for and answers plausibly.
type stubProducts struct {
	created   billing.Product
	attached  billing.AttachRequest
	endedID   int
	endReason string
	createErr error
	attachErr error
	endErr    error
}

func sampleProduct() *billing.Product {
	return &billing.Product{ID: 7, Code: "ROUTER", Name: "Router rental",
		Kind: billing.ProductRecurring, HSNSAC: "8517",
		Price: decimal.RequireFromString("150.00"), Active: true,
		CreatedBy: "meena", CreatedAt: time.Now()}
}

func (s *stubProducts) CreateProduct(_ context.Context, in billing.Product, _ string) (*billing.Product, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	// Normalise is the store's job in production; mimic the part the test
	// asserts on (the upper-cased code) so the handler is not credited
	// with work it does not do.
	norm, err := in.Normalise()
	if err != nil {
		return nil, err
	}
	s.created = norm
	out := sampleProduct()
	out.Code, out.Name, out.Kind, out.Price, out.HSNSAC = norm.Code, norm.Name, norm.Kind, norm.Price, norm.HSNSAC
	return out, nil
}

func (s *stubProducts) UpdateProduct(_ context.Context, in billing.Product, _ string) (*billing.Product, error) {
	norm, err := in.Normalise()
	if err != nil {
		return nil, err
	}
	s.created = norm
	return sampleProduct(), nil
}

func (s *stubProducts) SetProductActive(context.Context, int, bool, string) error { return nil }
func (s *stubProducts) DeleteProduct(context.Context, int) error                  { return nil }

func (s *stubProducts) GetProduct(_ context.Context, id int) (*billing.Product, error) {
	if id != 7 {
		return nil, billing.ErrProductNotFound
	}
	return sampleProduct(), nil
}

func (s *stubProducts) ListProducts(context.Context, billing.ProductFilter) ([]billing.Product, error) {
	return []billing.Product{*sampleProduct()}, nil
}

func (s *stubProducts) AttachProduct(_ context.Context, req billing.AttachRequest, _ string) (*billing.SubscriberProduct, error) {
	if s.attachErr != nil {
		return nil, s.attachErr
	}
	norm, err := req.Normalise()
	if err != nil {
		return nil, err
	}
	s.attached = norm
	price := norm.UnitPrice
	if !price.IsPositive() {
		price = decimal.RequireFromString("150.00")
	}
	return &billing.SubscriberProduct{ID: 42, SubscriberID: norm.SubscriberID, ProductID: norm.ProductID,
		ProductCode: "ROUTER", ProductName: "Router rental", Kind: billing.ProductRecurring,
		HSNSAC: "8517", Quantity: norm.Quantity, UnitPrice: price,
		Status: billing.AttachmentActive, AttachedBy: "meena", AttachedAt: time.Now()}, nil
}

func (s *stubProducts) EndAttachment(_ context.Context, id int, reason, _ string) error {
	if s.endErr != nil {
		return s.endErr
	}
	s.endedID, s.endReason = id, reason
	return nil
}

func (s *stubProducts) ListSubscriberProducts(context.Context, int, bool) ([]billing.SubscriberProduct, error) {
	return []billing.SubscriberProduct{{ID: 42, SubscriberID: 1, ProductCode: "ROUTER",
		ProductName: "Router rental", Kind: billing.ProductRecurring, HSNSAC: "8517", Quantity: 1,
		UnitPrice: decimal.RequireFromString("150.00"), Status: billing.AttachmentActive}}, nil
}

func (s *stubProducts) ListAttachmentsAcrossSubscribers(context.Context, int, int) ([]billing.SubscriberProduct, error) {
	return []billing.SubscriberProduct{{ID: 42, SubscriberID: 1, Username: "acme", ProductCode: "ROUTER",
		ProductName: "Router rental", Kind: billing.ProductRecurring, HSNSAC: "8517", Quantity: 1,
		UnitPrice: decimal.RequireFromString("150.00"), Status: billing.AttachmentActive}}, nil
}

func productHarness(t *testing.T, store api.ProductStore) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}), Products: store})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func httptestJSON(t *testing.T, mux *http.ServeMux, method, path, role, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, role+"_user"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_BIL_022_ProductAPIGates(t *testing.T) {
	t.Run("billing, the owner and the front desk can read the catalogue", func(t *testing.T) {
		for _, role := range []string{"isp_owner", "billing_admin", "csr"} {
			rec := httptestGet(t, productHarness(t, &stubProducts{}), "/api/v1/products", role)
			if rec.Code != 200 {
				t.Errorf("%s reading the catalogue: got %d", role, rec.Code)
			}
		}
	})

	t.Run("a technician cannot read the catalogue", func(t *testing.T) {
		rec := httptestGet(t, productHarness(t, &stubProducts{}), "/api/v1/products", "technician")
		if rec.Code != 403 {
			t.Errorf("got %d, want 403", rec.Code)
		}
	})

	t.Run("the front desk cannot change the catalogue or attach a charge", func(t *testing.T) {
		mux := productHarness(t, &stubProducts{})
		cases := []struct{ method, path, body string }{
			{"POST", "/api/v1/products", `{"code":"X1","name":"X","kind":"one_time","price":"10.00","hsn_sac":"998432"}`},
			{"PUT", "/api/v1/products/7", `{"name":"X","price":"10.00","hsn_sac":"998432"}`},
			{"DELETE", "/api/v1/products/7", ``},
			{"POST", "/api/v1/products/7/active", `{"active":false}`},
			{"POST", "/api/v1/subscribers/1/products", `{"product_id":7}`},
			{"POST", "/api/v1/subscriber-products/42/end", `{"reason":"returned"}`},
		}
		for _, c := range cases {
			rec := httptestJSON(t, mux, c.method, c.path, "csr", c.body)
			if rec.Code != 403 {
				t.Errorf("csr %s %s: got %d, want 403", c.method, c.path, rec.Code)
			}
		}
	})

	t.Run("billing can change the catalogue", func(t *testing.T) {
		store := &stubProducts{}
		rec := httptestJSON(t, productHarness(t, store), "POST", "/api/v1/products", "billing_admin",
			`{"code":"router-rent","name":"Router rental","kind":"recurring","price":"150.00","hsn_sac":"8517"}`)
		if rec.Code != 201 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if store.created.Code != "ROUTER-RENT" {
			t.Errorf("the code reached the store as %q", store.created.Code)
		}
	})
}

// An extra-connection product (FR-AAA-002) is created with the flag, must be
// recurring, and keeps the flag through every edit: taking it off would
// silently take a connection away from everyone who holds the product.
func TestFR_BIL_022_TheAddsAConnectionFlagIsSetOnceAndKept(t *testing.T) {
	store := &stubProducts{}
	mux := productHarness(t, store)
	rec := httptestJSON(t, mux, "POST", "/api/v1/products", "billing_admin",
		`{"code":"extra-conn","name":"Extra connection","kind":"recurring","price":"199.00","hsn_sac":"998432","adds_connections":true}`)
	if rec.Code != 201 || !store.created.AddsConnections {
		t.Fatalf("create: %d %s, stored %+v", rec.Code, rec.Body.String(), store.created)
	}

	rec = httptestJSON(t, mux, "POST", "/api/v1/products", "billing_admin",
		`{"code":"one-off","name":"x","kind":"one_time","price":"199.00","hsn_sac":"998432","adds_connections":true}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "has to be recurring") {
		t.Errorf("a one-time product that adds connections: %d %s", rec.Code, rec.Body.String())
	}

	// The stored product (7) does not add connections; an edit cannot turn
	// it on, as it could not turn an existing one off.
	rec = httptestJSON(t, mux, "PUT", "/api/v1/products/7", "billing_admin",
		`{"name":"Renamed","price":"150.00","hsn_sac":"998432","adds_connections":true}`)
	if rec.Code != 200 || store.created.AddsConnections {
		t.Errorf("an edit changed what the product grants: %d, stored %+v", rec.Code, store.created)
	}
}

func TestFR_BIL_022_ProductAPIValidation(t *testing.T) {
	mux := productHarness(t, &stubProducts{})

	t.Run("a non-numeric price is a 422, not a 500", func(t *testing.T) {
		rec := httptestJSON(t, mux, "POST", "/api/v1/products", "billing_admin",
			`{"code":"X1","name":"X","kind":"one_time","price":"free","hsn_sac":"998432"}`)
		if rec.Code != 422 {
			t.Errorf("got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a bad HSN/SAC is refused with a sentence an operator can act on", func(t *testing.T) {
		rec := httptestJSON(t, mux, "POST", "/api/v1/products", "billing_admin",
			`{"code":"X1","name":"X","kind":"one_time","price":"10.00","hsn_sac":"99"}`)
		if rec.Code != 422 {
			t.Fatalf("got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "4 to 8 digits") {
			t.Errorf("unhelpful message: %s", rec.Body.String())
		}
	})

	t.Run("a duplicate code is a 409", func(t *testing.T) {
		m := productHarness(t, &stubProducts{createErr: billing.ErrProductCodeTaken})
		rec := httptestJSON(t, m, "POST", "/api/v1/products", "billing_admin",
			`{"code":"X1","name":"X","kind":"one_time","price":"10.00","hsn_sac":"998432"}`)
		if rec.Code != 409 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("attaching a retired product is a 409", func(t *testing.T) {
		m := productHarness(t, &stubProducts{attachErr: billing.ErrProductInactive})
		rec := httptestJSON(t, m, "POST", "/api/v1/subscribers/1/products", "billing_admin", `{"product_id":7}`)
		if rec.Code != 409 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("ending a charge already billed is a 409", func(t *testing.T) {
		m := productHarness(t, &stubProducts{endErr: billing.ErrAttachmentNotActive})
		rec := httptestJSON(t, m, "POST", "/api/v1/subscriber-products/42/end", "billing_admin", `{"reason":"x"}`)
		if rec.Code != 409 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("an unconfigured catalogue is a 503, not a 500", func(t *testing.T) {
		h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
			Wallet: billing.NewWalletService(&stubWallet{})})
		m := http.NewServeMux()
		h.RegisterRoutes(m, itJWTSecret)
		if rec := httptestGet(t, m, "/api/v1/products", "isp_owner"); rec.Code != 503 {
			t.Errorf("got %d", rec.Code)
		}
	})
}

func TestFR_BIL_022_AttachDefaultsAndOverrides(t *testing.T) {
	t.Run("an omitted price means the catalogue price and quantity defaults to 1", func(t *testing.T) {
		store := &stubProducts{}
		rec := httptestJSON(t, productHarness(t, store), "POST", "/api/v1/subscribers/1/products",
			"billing_admin", `{"product_id":7}`)
		if rec.Code != 201 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if store.attached.Quantity != 1 {
			t.Errorf("quantity reached the store as %d", store.attached.Quantity)
		}
		if store.attached.UnitPrice.IsPositive() {
			t.Errorf("an omitted price became %s instead of being left to the catalogue", store.attached.UnitPrice)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["unit_price"] != "150.00" || got["amount"] != "150.00" {
			t.Errorf("response reports %v / %v", got["unit_price"], got["amount"])
		}
	})

	t.Run("a supplied price and quantity reach the store", func(t *testing.T) {
		store := &stubProducts{}
		rec := httptestJSON(t, productHarness(t, store), "POST", "/api/v1/subscribers/1/products",
			"billing_admin", `{"product_id":7,"quantity":3,"unit_price":"99.50"}`)
		if rec.Code != 201 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if store.attached.Quantity != 3 || !store.attached.UnitPrice.Equal(decimal.RequireFromString("99.50")) {
			t.Errorf("store got %d × %s", store.attached.Quantity, store.attached.UnitPrice)
		}
	})

	t.Run("a reason reaches EndAttachment", func(t *testing.T) {
		store := &stubProducts{}
		rec := httptestJSON(t, productHarness(t, store), "POST", "/api/v1/subscriber-products/42/end",
			"billing_admin", `{"reason":"router returned"}`)
		if rec.Code != 204 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if store.endedID != 42 || store.endReason != "router returned" {
			t.Errorf("store got %d / %q", store.endedID, store.endReason)
		}
	})
}
