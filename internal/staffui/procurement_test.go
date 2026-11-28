package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/procurement"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// The console's Procurement screen — CRD-EXP-007 (general purchase orders)
// and FR-INV-004 (the vendor named from the shared master).
//
// procurement.go was at 28%. Two defects were found writing this:
//
//   - A decision other than "approve" was a rejection, including no decision
//     at all. A rejection cannot be undone, so a form posted without its
//     select — a crafted request, or a template that lost the field — threw
//     the order away. The decision now has to be one of the two.
//   - A lookup that failed reported "Purchase order not found", so an
//     operator deciding an order during a database hiccup was told it did
//     not exist. The API already told the two apart.

var errPOStore = errors.New("store: purchase_orders unreachable")

type fakePOs struct {
	orders map[int]*procurement.PurchaseOrder

	gotNew      *procurement.NewPurchaseOrder
	gotStatus   *string
	gotDecision *struct {
		id      int
		approve bool
		by, why string
	}
	gotFulfil string

	listErr, createErr, getErr, decideErr, fulfilErr error
	alreadyDecided, wrongState                       bool
}

func newFakePOs() *fakePOs {
	return &fakePOs{orders: map[int]*procurement.PurchaseOrder{
		7: {ID: 7, Description: "Fibre splicer", Category: "hardware", Amount: "84000.00",
			Status: "pending", RequestedBy: "billing_admin_user"},
	}}
}

func (f *fakePOs) CreatePurchaseOrder(_ context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error) {
	f.gotNew = &po
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &procurement.PurchaseOrder{ID: 12, Description: po.Description, Status: "pending"}, nil
}

func (f *fakePOs) ListPurchaseOrders(_ context.Context, status *string) ([]procurement.PurchaseOrder, error) {
	f.gotStatus = status
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []procurement.PurchaseOrder
	for _, o := range f.orders {
		out = append(out, *o)
	}
	return out, nil
}

func (f *fakePOs) GetPurchaseOrder(_ context.Context, id int) (*procurement.PurchaseOrder, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.orders[id], nil
}

func (f *fakePOs) DecidePurchaseOrder(_ context.Context, id int, approve bool, by, why string) (*procurement.PurchaseOrder, error) {
	f.gotDecision = &struct {
		id      int
		approve bool
		by, why string
	}{id, approve, by, why}
	if f.decideErr != nil {
		return nil, f.decideErr
	}
	if f.alreadyDecided {
		return nil, nil
	}
	o := *f.orders[id]
	o.Status = "rejected"
	if approve {
		o.Status = "approved"
	}
	return &o, nil
}

func (f *fakePOs) UpdateFulfilment(_ context.Context, id int, status, _ string) (*procurement.PurchaseOrder, error) {
	f.gotFulfil = status
	if f.fulfilErr != nil {
		return nil, f.fulfilErr
	}
	if f.wrongState {
		return nil, nil
	}
	o := *f.orders[id]
	o.Status = status
	return &o, nil
}

func servePO(h *Handler, s Session, method, target, id string, form url.Values) (int, string) {
	var r *http.Request
	if form == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if id != "" {
		r.SetPathValue("id", id)
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(target, "/new"):
		h.CreatePurchaseOrderForm(w, r)
	case strings.HasSuffix(target, "/decide"):
		h.DecidePurchaseOrderForm(w, r)
	case strings.HasSuffix(target, "/fulfilment"):
		h.UpdateFulfilmentForm(w, r)
	default:
		h.Procurement(w, r)
	}
	return w.Code, visibleText(w.Body.String())
}

func poHandler(f *fakePOs) *Handler {
	return &Handler{procurement: f, vendors: &stubVendorStore{list: []vendors.Vendor{{ID: 3, Name: "Ingram", Active: true}}}}
}

func TestCRD_EXP_007_TheProcurementList(t *testing.T) {
	f := newFakePOs()
	_, body := servePO(poHandler(f), owner(), http.MethodGet, "/staff/procurement?status=pending", "", nil)
	if f.gotStatus == nil || *f.gotStatus != "pending" {
		t.Errorf("the status filter reached the store as %v", f.gotStatus)
	}
	for _, want := range []string{"Fibre splicer", "Ingram"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q: %s", want, firstLines(body))
		}
	}
	servePO(poHandler(f), owner(), http.MethodGet, "/staff/procurement", "", nil)
	if f.gotStatus != nil {
		t.Error("no filter reached the store as a value")
	}

	f.listErr = errPOStore
	if _, body := servePO(poHandler(f), owner(), http.MethodGet, "/staff/procurement", "", nil); !strings.Contains(body, "Could not load purchase orders") {
		t.Errorf("a failed list: %s", firstLines(body))
	}
	if _, body := servePO(&Handler{}, owner(), http.MethodGet, "/staff/procurement", "", nil); !strings.Contains(body, "not configured") {
		t.Errorf("unconfigured: %s", firstLines(body))
	}
}

func TestCRD_EXP_007_FilingAPurchaseOrder(t *testing.T) {
	f := newFakePOs()
	_, body := servePO(poHandler(f), sess("billing_admin"), http.MethodPost, "/staff/procurement/new", "", url.Values{
		"description": {"  OLT line card  "}, "vendor_id": {"3"}, "category": {"hardware"}, "amount": {"125000.5"},
	})
	if !strings.Contains(body, "Purchase order #12 filed") {
		t.Fatalf("no confirmation: %s", firstLines(body))
	}
	n := f.gotNew
	if n.Description != "OLT line card" || n.VendorID != 3 || n.Amount != "125000.50" || n.RequestedBy != "billing_admin_user" {
		t.Errorf("filed as %+v; requested_by must come from the session and the amount to two places", n)
	}

	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no description":   {url.Values{"vendor_id": {"3"}, "category": {"hardware"}, "amount": {"1"}}, "description is required"},
		"no vendor":        {url.Values{"description": {"x"}, "category": {"hardware"}, "amount": {"1"}}, "Choose a vendor"},
		"bad category":     {url.Values{"description": {"x"}, "vendor_id": {"3"}, "category": {"capex"}, "amount": {"1"}}, "Category must be one of"},
		"negative amount":  {url.Values{"description": {"x"}, "vendor_id": {"3"}, "category": {"other"}, "amount": {"-5"}}, "non-negative number"},
		"amount not a sum": {url.Values{"description": {"x"}, "vendor_id": {"3"}, "category": {"other"}, "amount": {"lots"}}, "non-negative number"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePOs()
			if _, body := servePO(poHandler(f), owner(), http.MethodPost, "/staff/procurement/new", "", tc.form); !strings.Contains(body, tc.want) {
				t.Errorf("want %q: %s", tc.want, firstLines(body))
			}
			if f.gotNew != nil {
				t.Error("an invalid order reached the store")
			}
		})
	}

	// A vendor retired since the page loaded is something the operator can
	// act on; a storage failure is not, and its text stays in the log.
	for err, want := range map[error]string{
		vendors.ErrInactive: "retired since this page loaded",
		errPOStore:          "Could not file that purchase order",
	} {
		f := newFakePOs()
		f.createErr = err
		_, body := servePO(poHandler(f), owner(), http.MethodPost, "/staff/procurement/new", "", url.Values{
			"description": {"x"}, "vendor_id": {"3"}, "category": {"other"}, "amount": {"0"},
		})
		if !strings.Contains(body, want) || strings.Contains(body, errPOStore.Error()) {
			t.Errorf("%v: %s", err, firstLines(body))
		}
	}
}

func TestCRD_EXP_007_DecidingAPurchaseOrder(t *testing.T) {
	f := newFakePOs()
	_, body := servePO(poHandler(f), owner(), http.MethodPost, "/staff/procurement/7/decide", "7", url.Values{"decision": {"approve"}})
	if !strings.Contains(body, "Purchase order #7 approved") {
		t.Errorf("approve: %s", firstLines(body))
	}
	if d := f.gotDecision; d == nil || !d.approve || d.by != "owner_user" {
		t.Errorf("decided as %+v; the decider comes from the session", d)
	}

	f = newFakePOs()
	_, body = servePO(poHandler(f), owner(), http.MethodPost, "/staff/procurement/7/decide", "7",
		url.Values{"decision": {"reject"}, "reason": {" over budget "}})
	if !strings.Contains(body, "Purchase order #7 rejected") || f.gotDecision.why != "over budget" {
		t.Errorf("reject: %s / %+v", firstLines(body), f.gotDecision)
	}
}

func TestCRD_EXP_007_DecisionRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		id    string
		form  url.Values
		setup func(*fakePOs)
		as    Session
		want  string
	}{
		"bad id":            {"x", url.Values{"decision": {"approve"}}, nil, owner(), "Invalid purchase order id"},
		"reject, no reason": {"7", url.Values{"decision": {"reject"}}, nil, owner(), "reason is required"},
		"no decision":       {"7", url.Values{"reason": {"looks fine"}}, nil, owner(), "Choose approve or reject"},
		"garbled decision":  {"7", url.Values{"decision": {"aprove"}, "reason": {"x"}}, nil, owner(), "Choose approve or reject"},
		"no such order":     {"99", url.Values{"decision": {"approve"}}, nil, owner(), "Purchase order not found"},
		"lookup failed":     {"7", url.Values{"decision": {"approve"}}, func(f *fakePOs) { f.getErr = errPOStore }, owner(), "Could not load that purchase order"},
		"own order":         {"7", url.Values{"decision": {"approve"}}, nil, sess("billing_admin"), "filed yourself"},
		"already decided":   {"7", url.Values{"decision": {"approve"}}, func(f *fakePOs) { f.alreadyDecided = true }, owner(), "already decided"},
		"store failed":      {"7", url.Values{"decision": {"approve"}}, func(f *fakePOs) { f.decideErr = errPOStore }, owner(), "Could not decide"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePOs()
			if tc.setup != nil {
				tc.setup(f)
			}
			_, body := servePO(poHandler(f), tc.as, http.MethodPost, "/staff/procurement/"+tc.id+"/decide", tc.id, tc.form)
			if !strings.Contains(body, tc.want) {
				t.Errorf("want %q: %s", tc.want, firstLines(body))
			}
			if strings.Contains(body, errPOStore.Error()) {
				t.Error("the store's error reached the page")
			}
			switch name {
			case "bad id", "reject, no reason", "no decision", "garbled decision", "no such order", "lookup failed", "own order":
				if f.gotDecision != nil {
					t.Errorf("the store was asked to decide: %+v", f.gotDecision)
				}
			}
		})
	}
}

func TestCRD_EXP_007_Fulfilment(t *testing.T) {
	f := newFakePOs()
	_, body := servePO(poHandler(f), owner(), http.MethodPost, "/staff/procurement/7/fulfilment", "7", url.Values{"status": {"received"}})
	if !strings.Contains(body, "Purchase order #7 marked received") || f.gotFulfil != "received" {
		t.Errorf("received: %s", firstLines(body))
	}
	for name, tc := range map[string]struct {
		id    string
		form  url.Values
		setup func(*fakePOs)
		want  string
	}{
		"bad id":      {"x", url.Values{"status": {"ordered"}}, nil, "Invalid purchase order id"},
		"bad status":  {"7", url.Values{"status": {"approved"}}, nil, "Status must be one of"},
		"wrong state": {"7", url.Values{"status": {"received"}}, func(f *fakePOs) { f.wrongState = true }, "No purchase order in a state"},
		"store":       {"7", url.Values{"status": {"cancelled"}}, func(f *fakePOs) { f.fulfilErr = errPOStore }, "Could not update"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePOs()
			if tc.setup != nil {
				tc.setup(f)
			}
			if _, body := servePO(poHandler(f), owner(), http.MethodPost, "/staff/procurement/"+tc.id+"/fulfilment", tc.id, tc.form); !strings.Contains(body, tc.want) {
				t.Errorf("want %q: %s", tc.want, firstLines(body))
			}
		})
	}
}

// Owner and billing admin only; an unconfigured deployment refuses writes.
func TestCRD_EXP_007_ProcurementGate(t *testing.T) {
	f := newFakePOs()
	for _, role := range []string{"csr", "technician", "noc_engineer", "lco"} {
		for _, target := range []string{"/staff/procurement", "/staff/procurement/new", "/staff/procurement/7/decide", "/staff/procurement/7/fulfilment"} {
			method := http.MethodPost
			if target == "/staff/procurement" {
				method = http.MethodGet
			}
			if code, _ := servePO(poHandler(f), sess(role), method, target, "7", url.Values{"decision": {"approve"}}); code != http.StatusForbidden {
				t.Errorf("%s reached %s: %d", role, target, code)
			}
		}
	}
	if f.gotDecision != nil || f.gotNew != nil {
		t.Error("a refused role reached the store")
	}
	for _, target := range []string{"/staff/procurement/new", "/staff/procurement/7/decide", "/staff/procurement/7/fulfilment"} {
		if code, _ := servePO(&Handler{}, owner(), http.MethodPost, target, "7", url.Values{}); code != http.StatusServiceUnavailable {
			t.Errorf("unconfigured %s: %d", target, code)
		}
	}
}
