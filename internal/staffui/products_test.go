package staffui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Product catalogue console — FR-BIL-022.

type fakeProducts struct {
	err       error
	created   []billing.Product
	updated   []billing.Product
	actor     string
	activeSet map[int]bool
	deleted   int
	attached  []billing.AttachRequest
	endedID   int
	endReason string
	list      []billing.Product
	held      []billing.SubscriberProduct
	gotFilter billing.ProductFilter
}

func newFakeProducts() *fakeProducts {
	return &fakeProducts{activeSet: map[int]bool{}}
}

func (f *fakeProducts) CreateProduct(ctx context.Context, in billing.Product) (*billing.Product, error) {
	norm, err := in.Normalise()
	if err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.created, f.actor = append(f.created, norm), middleware.SubjectFromContext(ctx)
	out := norm
	out.ID = len(f.created)
	return &out, nil
}

func (f *fakeProducts) UpdateProduct(ctx context.Context, in billing.Product) (*billing.Product, error) {
	norm, err := in.Normalise()
	if err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.updated, f.actor = append(f.updated, norm), middleware.SubjectFromContext(ctx)
	return &norm, nil
}

func (f *fakeProducts) SetProductActive(ctx context.Context, id int, active bool) error {
	if f.err != nil {
		return f.err
	}
	f.activeSet[id], f.actor = active, middleware.SubjectFromContext(ctx)
	return nil
}

func (f *fakeProducts) DeleteProduct(_ context.Context, id int) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = id
	return nil
}

func (f *fakeProducts) GetProduct(_ context.Context, id int) (*billing.Product, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &billing.Product{ID: id, Code: "ROUTER", Name: "Router rental",
		Kind: billing.ProductRecurring, HSNSAC: "8517",
		Price: decimal.RequireFromString("150.00"), Active: true}, nil
}

func (f *fakeProducts) ListProducts(_ context.Context, flt billing.ProductFilter) ([]billing.Product, error) {
	f.gotFilter = flt
	return f.list, nil
}

func (f *fakeProducts) AttachProduct(ctx context.Context, req billing.AttachRequest) (*billing.SubscriberProduct, error) {
	norm, err := req.Normalise()
	if err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.attached, f.actor = append(f.attached, norm), middleware.SubjectFromContext(ctx)
	return &billing.SubscriberProduct{ID: 42, SubscriberID: norm.SubscriberID}, nil
}

func (f *fakeProducts) EndAttachment(ctx context.Context, id int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return billing.ErrProductInvalid
	}
	if f.err != nil {
		return f.err
	}
	f.endedID, f.endReason, f.actor = id, reason, middleware.SubjectFromContext(ctx)
	return nil
}

func (f *fakeProducts) ListSubscriberProducts(context.Context, int, bool) ([]billing.SubscriberProduct, error) {
	return f.held, nil
}

func (f *fakeProducts) ListProductSubscribers(context.Context, int, int) ([]billing.SubscriberProduct, error) {
	return f.held, nil
}

// servePR drives one console handler with a session of the given role.
func servePR(h *Handler, role, method, target string, form url.Values, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if method == "GET" {
		r = httptest.NewRequest("GET", target, nil)
	} else {
		r = httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /staff/products/{id}/..., /staff/subscribers/{id}/products and
	// /staff/subscriber-products/{id}/end all carry the id third.
	if len(parts) > 2 {
		r.SetPathValue("id", parts[2])
	}
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func TestFR_BIL_022_ProductCatalogueScreenGates(t *testing.T) {
	t.Run("a CSR may read the catalogue but sees no management controls", func(t *testing.T) {
		f := newFakeProducts()
		f.list = []billing.Product{{ID: 7, Code: "ROUTER", Name: "Router rental",
			Kind: billing.ProductRecurring, HSNSAC: "8517",
			Price: decimal.RequireFromString("150.00"), Active: true, CreatedAt: time.Now()}}
		h := &Handler{products: f}
		w := servePR(h, "csr", "GET", "/staff/products", nil, h.Products)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "ROUTER") || !strings.Contains(body, "Router rental") {
			t.Error("the catalogue did not render")
		}
		// The management forms must not be in the markup at all for a CSR:
		// hiding a button while leaving the form posted-to is how a gate
		// gets bypassed.
		if strings.Contains(body, "/staff/products/7/active") || strings.Contains(body, "Add a product") {
			t.Error("a CSR was served the management controls")
		}
	})

	t.Run("billing sees the management controls", func(t *testing.T) {
		f := newFakeProducts()
		f.list = []billing.Product{{ID: 7, Code: "ROUTER", Name: "Router rental",
			Kind: billing.ProductRecurring, HSNSAC: "8517",
			Price: decimal.RequireFromString("150.00"), Active: true, CreatedAt: time.Now()}}
		h := &Handler{products: f}
		body := servePR(h, "billing_admin", "GET", "/staff/products", nil, h.Products).Body.String()
		if !strings.Contains(body, "/staff/products/7/active") || !strings.Contains(body, "Add a product") {
			t.Error("billing was not served the management controls")
		}
	})

	t.Run("a CSR cannot post any write action", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		cases := []struct {
			name   string
			target string
			fn     func(http.ResponseWriter, *http.Request)
			form   url.Values
		}{
			{"create", "/staff/products", h.CreateProductForm,
				url.Values{"code": {"X1"}, "name": {"X"}, "kind": {"one_time"}, "price": {"10.00"}, "hsn_sac": {"998432"}}},
			{"update", "/staff/products/7", h.UpdateProductForm,
				url.Values{"name": {"X"}, "price": {"10.00"}, "hsn_sac": {"998432"}}},
			{"retire", "/staff/products/7/active", h.SetProductActiveForm, url.Values{"active": {"false"}}},
			{"delete", "/staff/products/7/delete", h.DeleteProductForm, nil},
			{"attach", "/staff/subscribers/5/products", h.AttachProductForm, url.Values{"product_id": {"7"}}},
			{"end", "/staff/subscriber-products/42/end", h.EndAttachmentForm, url.Values{"reason": {"x"}, "subscriber_id": {"5"}}},
		}
		for _, c := range cases {
			w := servePR(h, "csr", "POST", c.target, c.form, c.fn)
			if w.Code != 403 {
				t.Errorf("%s: got %d, want 403", c.name, w.Code)
			}
		}
		if len(f.created) != 0 || len(f.attached) != 0 || f.endedID != 0 || f.deleted != 0 || len(f.activeSet) != 0 {
			t.Error("a CSR's refused request still reached the store")
		}
	})

	t.Run("an unconfigured catalogue says so rather than failing", func(t *testing.T) {
		h := &Handler{}
		w := servePR(h, "isp_owner", "GET", "/staff/products", nil, h.Products)
		if w.Code != 503 {
			t.Errorf("got %d, want 503", w.Code)
		}
	})
}

// The create form's "Adds a connection" box reaches the store, and an
// edit cannot change it: the form for editing offers no such box, and the
// stored value is what is kept (FR-AAA-002).
func TestFR_BIL_022_TheAddsAConnectionBox(t *testing.T) {
	f := newFakeProducts()
	h := &Handler{products: f}
	form := url.Values{"code": {"extra-conn"}, "name": {"Extra connection"},
		"kind": {"recurring"}, "price": {"199.00"}, "hsn_sac": {"998432"}, "adds_connections": {"1"}}
	if w := servePR(h, "billing_admin", "POST", "/staff/products", form, h.CreateProductForm); w.Code != 303 {
		t.Fatalf("got %d", w.Code)
	}
	if len(f.created) != 1 || !f.created[0].AddsConnections {
		t.Fatalf("store got %+v", f.created)
	}
	// Posted to an edit, the box is ignored; the fake's stored product (7)
	// does not add connections, so the update must not either.
	form = url.Values{"name": {"Renamed"}, "price": {"150.00"}, "hsn_sac": {"8517"}, "adds_connections": {"1"}}
	servePR(h, "billing_admin", "POST", "/staff/products/7", form, h.UpdateProductForm)
	if len(f.updated) != 1 || f.updated[0].AddsConnections {
		t.Errorf("an edit changed what the product grants: %+v", f.updated)
	}
}

func TestFR_BIL_022_ProductFormsReachTheStore(t *testing.T) {
	t.Run("creating a product, attributed and upper-cased", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		form := url.Values{"code": {"router-rent"}, "name": {"Router rental"},
			"kind": {"recurring"}, "price": {"150.00"}, "hsn_sac": {"8517"}}
		w := servePR(h, "billing_admin", "POST", "/staff/products", form, h.CreateProductForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.created) != 1 || f.created[0].Code != "ROUTER-RENT" {
			t.Fatalf("store got %+v", f.created)
		}
		if f.actor != "billing_admin_user" {
			t.Errorf("attributed to %q", f.actor)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "p=created") {
			t.Errorf("redirect %q", loc)
		}
	})

	t.Run("a non-numeric price comes back worded, and nothing is saved", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		form := url.Values{"code": {"X1"}, "name": {"X"}, "kind": {"one_time"},
			"price": {"free"}, "hsn_sac": {"998432"}}
		w := servePR(h, "billing_admin", "POST", "/staff/products", form, h.CreateProductForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.Contains(loc, "err=") {
			t.Errorf("redirect %q carries no reason", loc)
		}
		if len(f.created) != 0 {
			t.Error("an unparseable price still reached the store")
		}
	})

	t.Run("updating keeps the stored code and kind, whatever the form says", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		// The form tries to change both, as a hand-built POST could.
		form := url.Values{"code": {"HACKED"}, "kind": {"one_time"},
			"name": {"Router rental plus"}, "price": {"250.00"}, "hsn_sac": {"8517"}}
		w := servePR(h, "isp_owner", "POST", "/staff/products/7", form, h.UpdateProductForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.updated) != 1 {
			t.Fatalf("store got %+v", f.updated)
		}
		if f.updated[0].Code != "ROUTER" || f.updated[0].Kind != billing.ProductRecurring {
			t.Errorf("the form changed the code or kind: %s / %s", f.updated[0].Code, f.updated[0].Kind)
		}
		if !f.updated[0].Price.Equal(decimal.RequireFromString("250.00")) {
			t.Errorf("price reached the store as %s", f.updated[0].Price)
		}
	})

	t.Run("retiring and reinstating", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		if w := servePR(h, "isp_owner", "POST", "/staff/products/7/active",
			url.Values{"active": {"false"}}, h.SetProductActiveForm); w.Code != 303 {
			t.Fatalf("retire: got %d", w.Code)
		}
		if active, ok := f.activeSet[7]; !ok || active {
			t.Errorf("retire reached the store as %v/%v", active, ok)
		}
		if w := servePR(h, "isp_owner", "POST", "/staff/products/7/active",
			url.Values{"active": {"true"}}, h.SetProductActiveForm); w.Code != 303 {
			t.Fatalf("reinstate: got %d", w.Code)
		}
		if !f.activeSet[7] {
			t.Error("reinstate did not reach the store")
		}
	})

	t.Run("a product in use cannot be deleted, and the operator is told why", func(t *testing.T) {
		f := newFakeProducts()
		f.err = billing.ErrProductInUse
		h := &Handler{products: f}
		w := servePR(h, "isp_owner", "POST", "/staff/products/7/delete", nil, h.DeleteProductForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "p=inuse") {
			t.Errorf("redirect %q", loc)
		}
	})
}

func TestFR_BIL_022_AttachAndEndFromTheSubscriberPage(t *testing.T) {
	t.Run("attaching with an override, and back to that subscriber", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		form := url.Values{"product_id": {"7"}, "quantity": {"3"}, "unit_price": {"99.50"}}
		w := servePR(h, "billing_admin", "POST", "/staff/subscribers/5/products", form, h.AttachProductForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.attached) != 1 {
			t.Fatalf("store got %+v", f.attached)
		}
		got := f.attached[0]
		if got.SubscriberID != 5 || got.ProductID != 7 || got.Quantity != 3 ||
			!got.UnitPrice.Equal(decimal.RequireFromString("99.50")) {
			t.Errorf("store got %+v", got)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/staff/subscribers/5?msg=") {
			t.Errorf("redirect %q — attaching should return to the subscriber", loc)
		}
	})

	t.Run("an omitted override is left for the catalogue price", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		w := servePR(h, "billing_admin", "POST", "/staff/subscribers/5/products",
			url.Values{"product_id": {"7"}}, h.AttachProductForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.attached[0].UnitPrice.IsPositive() {
			t.Errorf("an omitted override became %s", f.attached[0].UnitPrice)
		}
		if f.attached[0].Quantity != 1 {
			t.Errorf("quantity defaulted to %d", f.attached[0].Quantity)
		}
	})

	t.Run("ending a charge carries its reason and returns to the subscriber", func(t *testing.T) {
		f := newFakeProducts()
		h := &Handler{products: f}
		form := url.Values{"reason": {"router returned"}, "subscriber_id": {"5"}}
		w := servePR(h, "billing_admin", "POST", "/staff/subscriber-products/42/end", form, h.EndAttachmentForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.endedID != 42 || f.endReason != "router returned" {
			t.Errorf("store got %d / %q", f.endedID, f.endReason)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/staff/subscribers/5?msg=") {
			t.Errorf("redirect %q", loc)
		}
	})
}

// The charges card on the subscriber page — the panel staff actually use to
// attach and end a charge. Rendered directly, because a template error here
// would otherwise only show up as a blank area on a page nobody tested.
func TestFR_BIL_022_SubscriberPageChargesCard(t *testing.T) {
	charges := []billing.SubscriberProduct{
		{ID: 42, SubscriberID: 5, ProductCode: "ROUTER", ProductName: "Router rental",
			Kind: billing.ProductRecurring, HSNSAC: "8517", Quantity: 2,
			UnitPrice: decimal.RequireFromString("150.00"), Status: billing.AttachmentActive,
			AttachedBy: "meena", AttachedAt: time.Now()},
		{ID: 43, SubscriberID: 5, ProductCode: "INSTALL", ProductName: "Installation",
			Kind: billing.ProductOneTime, HSNSAC: "998432", Quantity: 1,
			UnitPrice: decimal.RequireFromString("1500.00"), Status: billing.AttachmentCharged,
			AttachedBy: "meena", AttachedAt: time.Now(), InvoiceID: intPtr(9)},
	}
	available := []billing.Product{{ID: 7, Code: "ROUTER", Name: "Router rental",
		Kind: billing.ProductRecurring, HSNSAC: "8517",
		Price: decimal.RequireFromString("150.00"), Active: true}}

	render := func(role string, manage bool) string {
		rec := httptest.NewRecorder()
		s := testSession()
		s.Role = role
		(&Handler{}).render(rec, "subscriber_detail", pageData{Session: s, Data: subscriberDetailData{
			Subscriber:  &api.SubscriberRecord{ID: 5, Username: "acme", Status: "active"},
			ShowBilling: true, ShowCharges: true, ShowManageCharges: manage,
			Charges: charges, AvailableProducts: available,
		}})
		return rec.Body.String()
	}

	t.Run("billing sees the charges, the amounts and both forms", func(t *testing.T) {
		body := render("billing_admin", true)
		for _, want := range []string{
			"Router rental", "Installation", "8517", "998432",
			"300.00",  // 2 × 150, the amount actually billed
			"1500.00", // the charged installation; money() has no separators
			"INV-000009",
			// The link to the invoice that billed it. InvoiceID is a *int,
			// and html/template indirects it — asserted so a change to a
			// plain field, or to the template, cannot start printing a
			// pointer address into the URL.
			`href="/staff/invoices/9/pdf"`,
			`action="/staff/subscribers/5/products"`,
			`action="/staff/subscriber-products/42/end"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the charges card does not show %q", want)
			}
		}
		// A charge already billed offers no End button — there is nothing
		// left to stop, and ending it would erase the invoice link.
		if strings.Contains(body, `action="/staff/subscriber-products/43/end"`) {
			t.Error("an already-charged one-time charge offers an End form")
		}
	})

	t.Run("a CSR sees the charges but neither form", func(t *testing.T) {
		body := render("csr", false)
		if !strings.Contains(body, "Router rental") {
			t.Error("a CSR cannot see the charges at all")
		}
		if strings.Contains(body, `action="/staff/subscribers/5/products"`) ||
			strings.Contains(body, `action="/staff/subscriber-products/42/end"`) {
			t.Error("a CSR was served the management forms")
		}
	})
}

func intPtr(i int) *int { return &i }
