//go:build integration

// Procurement's HTTP surface — FR-INV-003 (vendor purchase records) and
// FR-WFL-001 (second-approver sign-off, which a purchase order needs for the
// same reason a wallet credit does).
//
// internal/api/procurement.go was at 0%: 95 statements. A purchase order is
// money leaving the business, and the control on it is the same shape as the
// one on wallet approvals: the person who asked cannot be the person who
// signs. It is enforced twice — chk_po_distinct_approver at the schema and a
// check here against the caller's own identity, so a self-approval comes back
// as a clear 403 rather than an opaque constraint violation — and neither was
// tested.
package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/procurement"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

type fakeProcurement struct {
	orders  []procurement.PurchaseOrder
	one     *procurement.PurchaseOrder
	decided *procurement.PurchaseOrder

	got            procurement.NewPurchaseOrder
	gotStatus      *string
	decidedBy      string
	decidedApprove bool
	decidedReason  string
	fulfilStatus   string
	fulfilActor    string

	createErr         error
	listErr           error
	getErr            error
	decideErr         error
	updateErr         error
	returnNilOnDecide bool
	returnNilOnUpdate bool
}

func (f *fakeProcurement) CreatePurchaseOrder(_ context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error) {
	f.got = po
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := po.VendorID
	return &procurement.PurchaseOrder{
		ID: 11, Description: po.Description, Vendor: "Chennai Networks", VendorID: &id,
		Category: po.Category, Amount: po.Amount, Status: "pending", RequestedBy: po.RequestedBy,
	}, nil
}

func (f *fakeProcurement) ListPurchaseOrders(_ context.Context, status *string) ([]procurement.PurchaseOrder, error) {
	f.gotStatus = status
	return f.orders, f.listErr
}

func (f *fakeProcurement) GetPurchaseOrder(_ context.Context, _ int) (*procurement.PurchaseOrder, error) {
	return f.one, f.getErr
}

func (f *fakeProcurement) DecidePurchaseOrder(_ context.Context, id int, approve bool, decidedBy, reason string) (*procurement.PurchaseOrder, error) {
	f.decidedBy, f.decidedApprove, f.decidedReason = decidedBy, approve, reason
	if f.decideErr != nil {
		return nil, f.decideErr
	}
	if f.returnNilOnDecide {
		return nil, nil
	}
	if f.decided != nil {
		return f.decided, nil
	}
	return &procurement.PurchaseOrder{ID: id, Status: "approved", DecidedBy: decidedBy}, nil
}

func (f *fakeProcurement) UpdateFulfilment(_ context.Context, id int, status, actor string) (*procurement.PurchaseOrder, error) {
	f.fulfilStatus, f.fulfilActor = status, actor
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.returnNilOnUpdate {
		return nil, nil
	}
	return &procurement.PurchaseOrder{ID: id, Status: status}, nil
}

// procCall dispatches to the handler the path implies, as a given subject.
func procCall(h *api.Handler, method, target, body, subject string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if subject != "" {
		r = r.WithContext(middleware.WithSubject(r.Context(), subject))
	}

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "orders" && len(parts) > i+1 {
			r.SetPathValue("id", parts[i+1])
		}
	}

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/decide"):
		h.DecidePurchaseOrder(w, r)
	case strings.HasSuffix(path, "/fulfilment"):
		h.UpdateFulfilmentStatus(w, r)
	case method == http.MethodPost:
		h.CreatePurchaseOrder(w, r)
	default:
		h.ListPurchaseOrders(w, r)
	}
	return w
}

func procHandler(p api.ProcurementQuerier, v api.VendorStore) *api.Handler {
	return api.NewHandler(api.HandlerDeps{Procurement: p, Vendors: v})
}

func vendorsWithOne(t *testing.T) *stubVendors {
	t.Helper()
	sv := newStubVendors()
	sv.seed(vendors.Vendor{Name: "Chennai Networks", Active: true})
	return sv
}

func pendingOrder(requestedBy string) *procurement.PurchaseOrder {
	return &procurement.PurchaseOrder{
		ID: 11, Description: "8 ONTs", Vendor: "Chennai Networks",
		Category: "hardware", Amount: "48000.00", Status: "pending",
		RequestedBy: requestedBy,
	}
}

// The requester cannot decide their own purchase order. Enforced here against
// the caller's identity so it is a 403 with a code a client can branch on,
// rather than the schema constraint surfacing as a 500.
func TestFR_WFL_001_ThePurchaseOrderRequesterCannotDecideTheirOwnOrder(t *testing.T) {
	f := &fakeProcurement{one: pendingOrder("ravi")}
	h := procHandler(f, nil)

	rec := procCall(h, http.MethodPost, "/api/v1/procurement/orders/11/decide",
		`{"approve":true}`, "ravi")
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_SELF_APPROVAL") {
		t.Errorf("the refusal should carry a code a client can branch on, got %s", rec.Body.String())
	}
	if f.decidedBy != "" {
		t.Errorf("the decision reached the store anyway, as %q", f.decidedBy)
	}
}

// Somebody else may, and the row records who.
func TestFR_WFL_001_AnotherApproverMayDecideAPurchaseOrderAndIsRecorded(t *testing.T) {
	f := &fakeProcurement{one: pendingOrder("ravi")}
	h := procHandler(f, nil)

	rec := procCall(h, http.MethodPost, "/api/v1/procurement/orders/11/decide",
		`{"approve":true}`, "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.decidedBy != "meena" {
		t.Errorf("the store recorded %q as the decider", f.decidedBy)
	}
	if !f.decidedApprove {
		t.Error("an approval reached the store as a rejection")
	}
}

// A rejection needs a reason; an approval does not. The reason is the only
// record of why somebody's spend was declined.
func TestFR_WFL_001_APurchaseOrderRejectionNeedsAReason(t *testing.T) {
	f := &fakeProcurement{one: pendingOrder("ravi")}
	h := procHandler(f, nil)

	rec := procCall(h, http.MethodPost, "/api/v1/procurement/orders/11/decide",
		`{"approve":false}`, "meena")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for a reasonless rejection, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.decidedBy != "" {
		t.Error("a reasonless rejection reached the store")
	}

	rec = procCall(h, http.MethodPost, "/api/v1/procurement/orders/11/decide",
		`{"approve":false,"reason":"quote is above the approved budget"}`, "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.decidedReason != "quote is above the approved budget" {
		t.Errorf("the stored reason is %q", f.decidedReason)
	}
}

// An order already decided is a conflict, not a silent second decision.
func TestFR_WFL_001_AnAlreadyDecidedPurchaseOrderIsAConflict(t *testing.T) {
	f := &fakeProcurement{one: pendingOrder("ravi"), returnNilOnDecide: true}
	rec := procCall(procHandler(f, nil), http.MethodPost,
		"/api/v1/procurement/orders/11/decide", `{"approve":true}`, "meena")
	if rec.Code != http.StatusConflict {
		t.Errorf("want 409, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_ALREADY_DECIDED") {
		t.Errorf("want a distinguishable code, got %s", rec.Body.String())
	}
}

// A missing order is 404, and a lookup failure is 500 — distinguishable,
// because one means "no such spend request" and the other means "ask again".
func TestFR_WFL_001_AMissingAndABrokenPurchaseOrderAreDifferentAnswers(t *testing.T) {
	missing := procCall(procHandler(&fakeProcurement{one: nil}, nil), http.MethodPost,
		"/api/v1/procurement/orders/11/decide", `{"approve":true}`, "meena")
	if missing.Code != http.StatusNotFound {
		t.Errorf("want 404 for a missing order, got %d", missing.Code)
	}

	broken := procCall(procHandler(&fakeProcurement{getErr: errors.New("store down")}, nil),
		http.MethodPost, "/api/v1/procurement/orders/11/decide", `{"approve":true}`, "meena")
	if broken.Code != http.StatusInternalServerError {
		t.Errorf("want 500 when the lookup fails, got %d", broken.Code)
	}
	if strings.Contains(broken.Body.String(), "store down") {
		t.Errorf("the store's own error reached the caller: %s", broken.Body.String())
	}
}

// An order names a vendor from the shared master. A free-text name is
// accepted only as a lookup key, and an unknown one is refused rather than
// created — which is the moment vendor names would start diverging again.
func TestFR_INV_004_AnUnknownVendorNameIsRefusedNotCreated(t *testing.T) {
	sv := vendorsWithOne(t)
	f := &fakeProcurement{}
	h := procHandler(f, sv)

	rec := procCall(h, http.MethodPost, "/api/v1/procurement/orders",
		`{"description":"8 ONTs","vendor":"Some Other Supplier","category":"hardware","amount":"48000"}`,
		"ravi")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vendor master") {
		t.Errorf("the message should say to add it to the master first, got %s", rec.Body.String())
	}
	if f.got.Description != "" {
		t.Errorf("the order reached the store with an unresolved vendor: %+v", f.got)
	}
}

// A known name resolves to its id.
func TestFR_INV_004_AKnownVendorNameResolvesToItsID(t *testing.T) {
	sv := vendorsWithOne(t)
	f := &fakeProcurement{}

	rec := procCall(procHandler(f, sv), http.MethodPost, "/api/v1/procurement/orders",
		`{"description":"8 ONTs","vendor":"Chennai Networks","category":"hardware","amount":"48000"}`,
		"ravi")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.got.VendorID == 0 {
		t.Error("the order reached the store with no vendor id")
	}
}

// With neither an id nor a name, the caller is told where to find one.
func TestFR_INV_004_AnOrderWithNoVendorSaysWhereToFindOne(t *testing.T) {
	f := &fakeProcurement{}
	rec := procCall(procHandler(f, vendorsWithOne(t)), http.MethodPost,
		"/api/v1/procurement/orders",
		`{"description":"8 ONTs","category":"hardware","amount":"48000"}`, "ravi")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/api/v1/vendors") {
		t.Errorf("the message should point at the vendor listing, got %s", rec.Body.String())
	}
}

// The amount is normalised to two places before it is stored, so the same
// spend cannot appear as 48000 and 48000.00 in two rows.
func TestFR_INV_003_ThePurchaseAmountIsNormalisedToTwoPlaces(t *testing.T) {
	f := &fakeProcurement{}
	rec := procCall(procHandler(f, nil), http.MethodPost, "/api/v1/procurement/orders",
		`{"description":"8 ONTs","vendor_id":1,"category":"hardware","amount":"48000"}`, "ravi")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.got.Amount != "48000.00" {
		t.Errorf("amount stored as %q, want 48000.00", f.got.Amount)
	}
	if f.got.RequestedBy != "ravi" {
		t.Errorf("requested_by = %q; it must come from the token", f.got.RequestedBy)
	}
}

func TestFR_INV_003_PurchaseOrderValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":            {`{`, http.StatusBadRequest},
		"no description":      {`{"vendor_id":1,"category":"hardware","amount":"1"}`, http.StatusUnprocessableEntity},
		"unknown category":    {`{"description":"x","vendor_id":1,"category":"furniture","amount":"1"}`, http.StatusUnprocessableEntity},
		"no category":         {`{"description":"x","vendor_id":1,"amount":"1"}`, http.StatusUnprocessableEntity},
		"amount not a number": {`{"description":"x","vendor_id":1,"category":"hardware","amount":"lots"}`, http.StatusUnprocessableEntity},
		"negative amount":     {`{"description":"x","vendor_id":1,"category":"hardware","amount":"-1"}`, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeProcurement{}
			rec := procCall(procHandler(f, nil), http.MethodPost,
				"/api/v1/procurement/orders", tc.body, "ravi")
			if rec.Code != tc.want {
				t.Errorf("want %d, got %d — %s", tc.want, rec.Code, rec.Body.String())
			}
			if f.got.Description != "" {
				t.Errorf("invalid input reached the store: %+v", f.got)
			}
		})
	}
}

// Fulfilment accepts only the three states, and an inapplicable transition is
// a conflict rather than a success that changed nothing.
func TestFR_INV_003_FulfilmentTransitions(t *testing.T) {
	for _, status := range []string{procurement.StatusOrdered, procurement.StatusReceived,
		procurement.StatusCancelled} {
		f := &fakeProcurement{}
		rec := procCall(procHandler(f, nil), http.MethodPost,
			"/api/v1/procurement/orders/11/fulfilment",
			`{"status":"`+status+`"}`, "meena")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d — %s", status, rec.Code, rec.Body.String())
		}
		if f.fulfilStatus != status {
			t.Errorf("the store got status %q, want %q", f.fulfilStatus, status)
		}
		if f.fulfilActor != "meena" {
			t.Errorf("the store recorded %q as the actor", f.fulfilActor)
		}
	}

	f := &fakeProcurement{}
	if rec := procCall(procHandler(f, nil), http.MethodPost,
		"/api/v1/procurement/orders/11/fulfilment", `{"status":"delivered"}`, "meena"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an unknown status, got %d", rec.Code)
	}
	if f.fulfilStatus != "" {
		t.Error("an unknown status reached the store")
	}

	conflict := procCall(procHandler(&fakeProcurement{returnNilOnUpdate: true}, nil),
		http.MethodPost, "/api/v1/procurement/orders/11/fulfilment",
		`{"status":"received"}`, "meena")
	if conflict.Code != http.StatusConflict {
		t.Errorf("want 409 when the transition does not apply, got %d", conflict.Code)
	}
}

// The listing filters by status when asked and returns an empty array rather
// than null, which a client that iterates would otherwise crash on.
func TestFR_INV_003_ThePurchaseListingFiltersAndNeverAnswersNull(t *testing.T) {
	f := &fakeProcurement{orders: nil}
	h := procHandler(f, nil)

	rec := procCall(h, http.MethodGet, "/api/v1/procurement/orders?status=pending", "", "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if f.gotStatus == nil || *f.gotStatus != "pending" {
		t.Errorf("the status filter did not reach the store: %v", f.gotStatus)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("an empty listing answered %q, want []", body)
	}

	f.gotStatus = nil
	if rec := procCall(h, http.MethodGet, "/api/v1/procurement/orders", "", "meena"); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if f.gotStatus != nil {
		t.Errorf("no filter was asked for but the store got %q", *f.gotStatus)
	}
}

// Every endpoint reports an unconfigured deployment.
func TestFR_INV_003_AnUnconfiguredProcurementDeploymentIsReported(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{})
	for name, tc := range map[string]struct {
		method, target, body string
	}{
		"create":     {http.MethodPost, "/api/v1/procurement/orders", `{"description":"x"}`},
		"list":       {http.MethodGet, "/api/v1/procurement/orders", ""},
		"decide":     {http.MethodPost, "/api/v1/procurement/orders/11/decide", `{"approve":true}`},
		"fulfilment": {http.MethodPost, "/api/v1/procurement/orders/11/fulfilment", `{"status":"received"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := procCall(h, tc.method, tc.target, tc.body, "meena")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A malformed id is refused before anything is read.
func TestFR_INV_003_AMalformedPurchaseOrderIdIsRefused(t *testing.T) {
	f := &fakeProcurement{one: pendingOrder("ravi")}
	h := procHandler(f, nil)

	for _, target := range []string{
		"/api/v1/procurement/orders/not-a-number/decide",
		"/api/v1/procurement/orders/not-a-number/fulfilment",
	} {
		rec := procCall(h, http.MethodPost, target, `{"approve":true,"status":"received"}`, "meena")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", target, rec.Code)
		}
	}
	if f.decidedBy != "" || f.fulfilStatus != "" {
		t.Error("a malformed id reached the store")
	}
}

// Requesting, deciding and fulfilling are each audited, with the decision
// recording who made it — the trail an auditor reads for spend.
func TestFR_INV_003_EveryProcurementStepIsAudited(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	h := procHandler(&fakeProcurement{one: pendingOrder("ravi")}, nil)
	procCall(h, http.MethodPost, "/api/v1/procurement/orders",
		`{"description":"8 ONTs","vendor_id":1,"category":"hardware","amount":"48000"}`, "ravi")
	procCall(h, http.MethodPost, "/api/v1/procurement/orders/11/decide", `{"approve":true}`, "meena")
	procCall(h, http.MethodPost, "/api/v1/procurement/orders/11/fulfilment",
		`{"status":"received"}`, "meena")

	byAction := map[string]map[string]any{}
	for _, e := range sink.entries() {
		byAction[e.Action] = e.Detail
	}
	for _, want := range []string{"procurement.request", "procurement.decide", "procurement.fulfilment"} {
		if _, ok := byAction[want]; !ok {
			t.Errorf("%q was not audited; recorded: %v", want, byAction)
		}
	}
	if d := byAction["procurement.decide"]; d != nil && d["decided_by"] != "meena" {
		t.Errorf("the decision trail records decided_by=%v, want meena", d["decided_by"])
	}
}
