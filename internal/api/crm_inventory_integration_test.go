//go:build integration

// CRM and CPE inventory endpoint tests — FR-CRM-001..003, FR-INV-001..003 |
// MDS §4.16.
//
// The behaviour worth defending most here is the onboarding handoff: a
// conversion must carry the prospect's identity across (not whatever the
// caller retyped), must not be doable twice, and must survive the warehouse
// being empty — a subscriber who has paid should exist whether or not a
// router was available.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/crm"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
)

// ── Stub lead store ──────────────────────────────────────────────────────────

// stubLeads is an in-memory LeadQuerier. ConvertLead holds a mutex across
// the check-and-claim so it reproduces the real store's atomic conditional
// UPDATE; a stub that let two conversions both land would make the race
// test pass against an implementation that could never behave that way.
type stubLeads struct {
	mu     sync.Mutex
	byID   map[int]*crm.Lead
	nextID int

	convertedSubscribers int
	createErr            error
}

func newStubLeads() *stubLeads {
	return &stubLeads{byID: map[int]*crm.Lead{}, nextID: 1}
}

func (s *stubLeads) seed(l crm.Lead) *crm.Lead {
	s.mu.Lock()
	defer s.mu.Unlock()
	l.ID = s.nextID
	s.nextID++
	if l.Status == "" {
		l.Status = crm.StatusNew
	}
	s.byID[l.ID] = &l
	return &l
}

func (s *stubLeads) CreateLead(_ context.Context, l crm.Lead) (*crm.Lead, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return s.seed(l), nil
}

func (s *stubLeads) GetLead(_ context.Context, id int) (*crm.Lead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *l
	return &cp, nil
}

func (s *stubLeads) ListLeads(_ context.Context, status, source *string, franchiseID *int) ([]crm.Lead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []crm.Lead
	for _, l := range s.byID {
		if status != nil && l.Status != *status {
			continue
		}
		if source != nil && l.Source != *source {
			continue
		}
		if franchiseID != nil && (l.FranchiseID == nil || *l.FranchiseID != *franchiseID) {
			continue
		}
		out = append(out, *l)
	}
	return out, nil
}

func (s *stubLeads) UpdateLead(_ context.Context, id int, status, assignedTo, notes, lostReason *string) (*crm.Lead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	if status != nil {
		l.Status = *status
	}
	if assignedTo != nil {
		l.AssignedTo = *assignedTo
	}
	cp := *l
	return &cp, nil
}

func (s *stubLeads) ConvertLead(_ context.Context, leadID int, sub api.SubscriberRecord, _ string) (*crm.Lead, *api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.byID[leadID]
	if !ok || l.Status == crm.StatusConverted {
		return nil, nil, nil // the conditional UPDATE matched no row
	}
	s.convertedSubscribers++
	subID := 1000 + s.convertedSubscribers
	l.Status = crm.StatusConverted
	l.ConvertedSubscriberID = &subID
	sub.ID = subID
	cp := *l
	return &cp, &sub, nil
}

func (s *stubLeads) GetFunnel(_ context.Context, _ *int) (*crm.FunnelReport, error) {
	return &crm.FunnelReport{TotalLeads: 4, ConvertedLeads: 1, ConversionRatePct: "25.00"}, nil
}

// ── Stub inventory store ─────────────────────────────────────────────────────

type stubInventory struct {
	mu      sync.Mutex
	devices map[string]*inventory.Device
	types   []inventory.DeviceType
	levels  []inventory.StockLevel

	issueErr error
}

func newStubInventory() *stubInventory {
	return &stubInventory{devices: map[string]*inventory.Device{}}
}

func (s *stubInventory) seedDevice(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[serial] = &inventory.Device{
		ID: len(s.devices) + 1, SerialNumber: serial,
		DeviceTypeID: 1, DeviceType: "Archer C6", Status: inventory.StatusInStock,
	}
}

func (s *stubInventory) CreateDeviceType(_ context.Context, t inventory.DeviceType) (*inventory.DeviceType, error) {
	t.ID = len(s.types) + 1
	s.types = append(s.types, t)
	return &t, nil
}

func (s *stubInventory) ListDeviceTypes(context.Context) ([]inventory.DeviceType, error) {
	return s.types, nil
}

func (s *stubInventory) CreateDevice(_ context.Context, d inventory.Device) (*inventory.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.devices[d.SerialNumber]; exists {
		return nil, errDuplicateSerial
	}
	d.ID = len(s.devices) + 1
	d.Status = inventory.StatusInStock
	s.devices[d.SerialNumber] = &d
	return &d, nil
}

func (s *stubInventory) GetDeviceBySerial(_ context.Context, serial string) (*inventory.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[serial]
	if !ok {
		return nil, nil
	}
	cp := *d
	return &cp, nil
}

func (s *stubInventory) ListDevices(_ context.Context, status *string, _, subscriberID *int) ([]inventory.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []inventory.Device
	for _, d := range s.devices {
		if status != nil && d.Status != *status {
			continue
		}
		if subscriberID != nil && (d.SubscriberID == nil || *d.SubscriberID != *subscriberID) {
			continue
		}
		out = append(out, *d)
	}
	return out, nil
}

func (s *stubInventory) IssueDevice(_ context.Context, serial string, subscriberID int) (*inventory.Device, error) {
	if s.issueErr != nil {
		return nil, s.issueErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[serial]
	if !ok || d.Status != inventory.StatusInStock {
		return nil, nil // not available
	}
	d.Status = inventory.StatusIssued
	d.SubscriberID = &subscriberID
	cp := *d
	return &cp, nil
}

func (s *stubInventory) ReturnDevice(_ context.Context, serial, newStatus string) (*inventory.Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[serial]
	if !ok || d.Status != inventory.StatusIssued {
		return nil, nil
	}
	d.Status = newStatus
	d.SubscriberID = nil
	cp := *d
	return &cp, nil
}

func (s *stubInventory) GetStockLevels(_ context.Context, lowOnly bool) ([]inventory.StockLevel, error) {
	if !lowOnly {
		return s.levels, nil
	}
	var out []inventory.StockLevel
	for _, l := range s.levels {
		if l.IsLow {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *stubInventory) RecordPurchase(_ context.Context, p inventory.Purchase) (*inventory.Purchase, error) {
	p.ID = 1
	p.UnitCostStr = p.UnitCost.StringFixed(2)
	p.TotalCostStr = p.UnitCost.Mul(decimal.NewFromInt(int64(p.Quantity))).StringFixed(2)
	return &p, nil
}

func (s *stubInventory) ListPurchases(context.Context, *int) ([]inventory.Purchase, error) {
	return nil, nil
}

var errDuplicateSerial = &serialError{}

type serialError struct{}

func (e *serialError) Error() string { return "duplicate key value violates unique constraint" }

// ── Harness ──────────────────────────────────────────────────────────────────

type crmHarness struct {
	mux     *http.ServeMux
	leads   *stubLeads
	inv     *stubInventory
	tasks   *stubFieldTasks
	lifecy  *stubLifecycle
	appr    *stubApprovals
	vendors *stubVendors
}

func newCRMHarness(t *testing.T) *crmHarness {
	t.Helper()
	h := &crmHarness{
		leads: newStubLeads(), inv: newStubInventory(),
		tasks: newStubFieldTasks(), lifecy: &stubLifecycle{}, appr: newStubApprovals(),
		vendors: newStubVendors(),
	}
	// A purchase and a device type name a supplier from the shared master
	// since FR-INV-004, so the harness seeds the one these tests buy from.
	// Seeded rather than the tests being rewritten to send vendor_id: they
	// send a vendor *name*, which is exactly the pre-master request shape a
	// deployed client still sends, and keeping them that way is what proves
	// the compatibility path resolves it instead of refusing it.
	h.vendors.seedActive(vendors.Vendor{Name: "Ingram"})
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Leads: h.leads, Inventory: h.inv, FieldTasks: h.tasks,
		Lifecycle: h.lifecy, Approvals: h.appr, Vendors: h.vendors,
	})
	h.mux = http.NewServeMux()
	handler.RegisterRoutes(h.mux, itJWTSecret)
	return h
}

func (h *crmHarness) do(t *testing.T, method, path, body, role, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, subject))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// ── FR-CRM-001: pipeline ────────────────────────────────────────────────────

func TestFR_CRM_001_CreateLeadValidates(t *testing.T) {
	h := newCRMHarness(t)

	cases := []struct{ name, body string }{
		{"missing name", `{"mobile_number":"+919876500111","source":"walk_in"}`},
		{"non-E.164 mobile", `{"full_name":"X","mobile_number":"9876500111","source":"walk_in"}`},
		{"unknown source", `{"full_name":"X","mobile_number":"+919876500111","source":"telepathy"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/api/v1/leads", tc.body, "csr", "sales1")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFR_CRM_001_CreateAndListLead(t *testing.T) {
	h := newCRMHarness(t)

	rec := h.do(t, http.MethodPost, "/api/v1/leads",
		`{"full_name":"Ravi Kumar","mobile_number":"+919876500111","source":"referral"}`, "csr", "sales1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	var lead crm.Lead
	if err := json.Unmarshal(rec.Body.Bytes(), &lead); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if lead.Status != crm.StatusNew {
		t.Errorf("a new lead must start new, got %q", lead.Status)
	}

	list := h.do(t, http.MethodGet, "/api/v1/leads", ``, "csr", "sales1")
	if list.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", list.Code)
	}
}

// TestFR_CRM_001_UpdateCannotReachConverted: reaching converted also
// requires creating a subscriber, so the plain update path must refuse it
// rather than produce a converted lead pointing at nobody.
func TestFR_CRM_001_UpdateCannotReachConverted(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{FullName: "Ravi", MobileNumber: "+919876500111", Source: "walk_in"})

	rec := h.do(t, http.MethodPatch, "/api/v1/leads/1", `{"status":"converted"}`, "csr", "sales1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 setting status=converted directly, got %d — %s", rec.Code, rec.Body.String())
	}
	lead, _ := h.leads.GetLead(context.Background(), 1)
	if lead.Status == crm.StatusConverted {
		t.Error("the lead must not have been marked converted")
	}
}

// ── FR-CRM-002: the conversion handoff ──────────────────────────────────────

// TestFR_CRM_002_ConversionCarriesProspectIdentityOver is the carry-over the
// requirement asks for: identity comes from the lead, not from whatever the
// caller retyped into the conversion request.
func TestFR_CRM_002_ConversionCarriesProspectIdentityOver(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{
		FullName: "Ravi Kumar", MobileNumber: "+919876500111",
		Email: "ravi@example.com", Source: "referral",
	})

	body := `{"caf_number":"CAF-9001","username":"ravi@isp","password":"s3cret",
	          "plan_id":2,"registered_state":"TN"}`
	rec := h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, "billing_admin", "admin1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Subscriber api.SubscriberRecord `json:"subscriber"`
		Lead       crm.Lead             `json:"lead"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Subscriber.MobileNumber != "+919876500111" {
		t.Errorf("subscriber mobile = %q, want the lead's +919876500111", resp.Subscriber.MobileNumber)
	}
	if resp.Subscriber.Email != "ravi@example.com" {
		t.Errorf("subscriber email = %q, want the lead's", resp.Subscriber.Email)
	}
	if resp.Lead.Status != crm.StatusConverted {
		t.Errorf("lead status = %q, want converted", resp.Lead.Status)
	}
	if resp.Lead.ConvertedSubscriberID == nil {
		t.Error("the converted lead must point at the subscriber it produced")
	}
}

// TestFR_CRM_002_ConcurrentConversionsYieldOneSubscriber is the API-level
// race: whatever the concurrency, one prospect becomes one customer.
func TestFR_CRM_002_ConcurrentConversionsYieldOneSubscriber(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{FullName: "Ravi", MobileNumber: "+919876500111", Source: "website"})

	const racers = 8
	var wg sync.WaitGroup
	codes := make([]int, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := `{"caf_number":"CAF-900` + string(rune('a'+i)) + `","username":"ravi` +
				string(rune('a'+i)) + `@isp","password":"s3cret","plan_id":2,"registered_state":"TN"}`
			codes[i] = h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, "billing_admin", "admin1").Code
		}(i)
	}
	close(start)
	wg.Wait()

	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if created != 1 {
		t.Errorf("want exactly 1 conversion out of %d, got %d", racers, created)
	}
	if conflicts != racers-1 {
		t.Errorf("want %d conflicts, got %d", racers-1, conflicts)
	}
	if h.leads.convertedSubscribers != 1 {
		t.Errorf("DUPLICATE SUBSCRIBERS: %d created from one lead", h.leads.convertedSubscribers)
	}
}

func TestFR_CRM_002_ConvertingAConvertedLeadIs409(t *testing.T) {
	h := newCRMHarness(t)
	subID := 1001
	h.leads.seed(crm.Lead{
		FullName: "Ravi", MobileNumber: "+919876500111", Source: "walk_in",
		Status: crm.StatusConverted, ConvertedSubscriberID: &subID,
	})

	body := `{"caf_number":"CAF-1","username":"x@isp","password":"p","plan_id":2,"registered_state":"TN"}`
	rec := h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, "billing_admin", "admin1")
	if rec.Code != http.StatusConflict {
		t.Errorf("want 409, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestFR_CRM_002_ConvertingALostLeadIsRefused: silently resurrecting a
// prospect somebody deliberately closed would lose that decision.
func TestFR_CRM_002_ConvertingALostLeadIsRefused(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{
		FullName: "Ravi", MobileNumber: "+919876500111", Source: "walk_in", Status: crm.StatusLost,
	})

	body := `{"caf_number":"CAF-1","username":"x@isp","password":"p","plan_id":2,"registered_state":"TN"}`
	rec := h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, "billing_admin", "admin1")
	if rec.Code != http.StatusConflict {
		t.Errorf("want 409 converting a lost lead, got %d — %s", rec.Code, rec.Body.String())
	}
}

// ── The handoff: conversion + CPE (FR-CRM-002 × FR-INV-002) ─────────────────

func TestFR_INV_002_ConversionIssuesTheNamedCPE(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{FullName: "Ravi", MobileNumber: "+919876500111", Source: "walk_in"})
	h.inv.seedDevice("SN-1001")

	body := `{"caf_number":"CAF-1","username":"ravi@isp","password":"p","plan_id":2,
	          "registered_state":"TN","cpe_serial":"SN-1001"}`
	rec := h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, "billing_admin", "admin1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Subscriber api.SubscriberRecord `json:"subscriber"`
		CPEIssued  string               `json:"cpe_issued"`
		CPEWarning string               `json:"cpe_warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CPEIssued != "SN-1001" {
		t.Errorf("cpe_issued = %q, want SN-1001", resp.CPEIssued)
	}
	if resp.CPEWarning != "" {
		t.Errorf("unexpected warning: %q", resp.CPEWarning)
	}

	device, _ := h.inv.GetDeviceBySerial(context.Background(), "SN-1001")
	if device.Status != inventory.StatusIssued {
		t.Errorf("device status = %q, want issued", device.Status)
	}
	if device.SubscriberID == nil || *device.SubscriberID != resp.Subscriber.ID {
		t.Errorf("device must be linked to the new subscriber (FR-INV-002), got %v", device.SubscriberID)
	}
}

// TestFR_INV_002_UnavailableCPEDoesNotFailTheConversion is the design
// decision this handoff turns on: the subscriber exists and can be billed
// whether or not the warehouse could supply a router.
func TestFR_INV_002_UnavailableCPEDoesNotFailTheConversion(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{FullName: "Ravi", MobileNumber: "+919876500111", Source: "walk_in"})
	// Deliberately no device seeded — the warehouse is empty.

	body := `{"caf_number":"CAF-1","username":"ravi@isp","password":"p","plan_id":2,
	          "registered_state":"TN","cpe_serial":"SN-NOPE"}`
	rec := h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, "billing_admin", "admin1")

	if rec.Code != http.StatusCreated {
		t.Fatalf("an unavailable CPE must NOT fail the conversion; got %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Subscriber api.SubscriberRecord `json:"subscriber"`
		Lead       crm.Lead             `json:"lead"`
		CPEIssued  string               `json:"cpe_issued"`
		CPEWarning string               `json:"cpe_warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Subscriber.ID == 0 {
		t.Error("the subscriber must have been created")
	}
	if resp.Lead.Status != crm.StatusConverted {
		t.Error("the lead must still be marked converted")
	}
	if resp.CPEIssued != "" {
		t.Errorf("no device should be reported as issued, got %q", resp.CPEIssued)
	}
	// The failure has to be visible, or staff would never know to chase it.
	if resp.CPEWarning == "" {
		t.Error("an un-issued CPE must be reported back as a warning")
	}
}

// ── FR-INV: inventory endpoints ─────────────────────────────────────────────

func TestFR_INV_002_IssueAndReturnDevice(t *testing.T) {
	h := newCRMHarness(t)
	h.inv.seedDevice("SN-2001")

	issued := h.do(t, http.MethodPost, "/api/v1/cpe/devices/SN-2001/issue", `{"subscriber_id":7}`, "technician", "tech1")
	if issued.Code != http.StatusOK {
		t.Fatalf("issue: want 200, got %d — %s", issued.Code, issued.Body.String())
	}

	// A second issue of the same physical device must be refused.
	again := h.do(t, http.MethodPost, "/api/v1/cpe/devices/SN-2001/issue", `{"subscriber_id":8}`, "technician", "tech1")
	if again.Code != http.StatusConflict {
		t.Errorf("double issue: want 409, got %d — %s", again.Code, again.Body.String())
	}

	returned := h.do(t, http.MethodPost, "/api/v1/cpe/devices/SN-2001/return", `{"status":"returned"}`, "technician", "tech1")
	if returned.Code != http.StatusOK {
		t.Fatalf("return: want 200, got %d — %s", returned.Code, returned.Body.String())
	}
}

func TestFR_INV_002_IssuingAnUnknownSerialIs404(t *testing.T) {
	h := newCRMHarness(t)
	rec := h.do(t, http.MethodPost, "/api/v1/cpe/devices/SN-GHOST/issue", `{"subscriber_id":7}`, "technician", "tech1")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestFR_INV_001_ReturnRefusesInStock: hardware coming back goes to
// 'returned' pending inspection, never straight onto the shelf — the stock
// count reorder alerts trust must only include inspected devices.
func TestFR_INV_001_ReturnRefusesInStock(t *testing.T) {
	h := newCRMHarness(t)
	h.inv.seedDevice("SN-3001")
	h.do(t, http.MethodPost, "/api/v1/cpe/devices/SN-3001/issue", `{"subscriber_id":7}`, "technician", "tech1")

	rec := h.do(t, http.MethodPost, "/api/v1/cpe/devices/SN-3001/return", `{"status":"in_stock"}`, "technician", "tech1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 returning straight to in_stock, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestFR_INV_003_RecordPurchaseValidatesAndComputesTotal(t *testing.T) {
	h := newCRMHarness(t)

	rec := h.do(t, http.MethodPost, "/api/v1/cpe/purchases",
		`{"device_type_id":1,"vendor":"Ingram","quantity":3,"unit_cost":"1799.99"}`, "billing_admin", "admin1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	var p inventory.Purchase
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.TotalCostStr != "5399.97" {
		t.Errorf("total_cost = %q, want 5399.97", p.TotalCostStr)
	}

	t.Run("rejects a non-positive quantity", func(t *testing.T) {
		bad := h.do(t, http.MethodPost, "/api/v1/cpe/purchases",
			`{"device_type_id":1,"vendor":"Ingram","quantity":0,"unit_cost":"10.00"}`, "billing_admin", "admin1")
		if bad.Code != http.StatusUnprocessableEntity {
			t.Errorf("want 422, got %d", bad.Code)
		}
	})

	t.Run("rejects a malformed unit cost", func(t *testing.T) {
		bad := h.do(t, http.MethodPost, "/api/v1/cpe/purchases",
			`{"device_type_id":1,"vendor":"Ingram","quantity":1,"unit_cost":"free"}`, "billing_admin", "admin1")
		if bad.Code != http.StatusUnprocessableEntity {
			t.Errorf("want 422, got %d", bad.Code)
		}
	})
}

func TestFR_INV_003_LowStockFilter(t *testing.T) {
	h := newCRMHarness(t)
	h.inv.levels = []inventory.StockLevel{
		{DeviceTypeID: 1, DeviceType: "Archer C6", InStock: 10, ReorderThreshold: 5, IsLow: false},
		{DeviceTypeID: 2, DeviceType: "Splitter", InStock: 1, ReorderThreshold: 5, IsLow: true},
	}

	all := h.do(t, http.MethodGet, "/api/v1/cpe/stock", ``, "technician", "tech1")
	var allLevels []inventory.StockLevel
	if err := json.Unmarshal(all.Body.Bytes(), &allLevels); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(allLevels) != 2 {
		t.Errorf("unfiltered stock = %d rows, want 2", len(allLevels))
	}

	low := h.do(t, http.MethodGet, "/api/v1/cpe/stock?low_only=true", ``, "technician", "tech1")
	var lowLevels []inventory.StockLevel
	if err := json.Unmarshal(low.Body.Bytes(), &lowLevels); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(lowLevels) != 1 || lowLevels[0].DeviceType != "Splitter" {
		t.Errorf("low_only returned %+v, want just the Splitter", lowLevels)
	}
}

// ── Termination → CPE recovery task (MDS §4.16 × FR-WFL-002) ────────────────

// TestFR_INV_001_TerminationOpensACPERecoveryTask verifies the integration
// point: an approved termination must not silently restock hardware that is
// still in the customer's flat — it opens a task to go and get it.
func TestFR_INV_001_TerminationOpensACPERecoveryTask(t *testing.T) {
	h := newCRMHarness(t)
	h.inv.seedDevice("SN-4001")
	if _, err := h.inv.IssueDevice(context.Background(), "SN-4001", 9); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	h.appr.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9,
		Reason: "relocated", RequestedBy: "alice",
	})

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	tasks, err := h.tasks.ListFieldTasks(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ListFieldTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 CPE recovery task after termination, got %d", len(tasks))
	}
	if !strings.Contains(tasks[0].Title, "SN-4001") {
		t.Errorf("the recovery task must name the device, got %q", tasks[0].Title)
	}

	// The device stays issued until someone physically confirms it is back.
	device, _ := h.inv.GetDeviceBySerial(context.Background(), "SN-4001")
	if device.Status != inventory.StatusIssued {
		t.Errorf("device status = %q — it must stay issued until recovered, "+
			"or the stock count claims hardware is on the shelf that is not", device.Status)
	}
}

// ── Authorization ───────────────────────────────────────────────────────────

// TestFR_CRM_002_ConversionRestrictedToBillingTier: conversion creates a
// billable subscriber, so it sits on the same tier as POST /subscribers even
// though the rest of the pipeline is open to sales roles.
func TestFR_CRM_002_ConversionRestrictedToBillingTier(t *testing.T) {
	h := newCRMHarness(t)
	h.leads.seed(crm.Lead{FullName: "Ravi", MobileNumber: "+919876500111", Source: "walk_in"})

	body := `{"caf_number":"CAF-1","username":"x@isp","password":"p","plan_id":2,"registered_state":"TN"}`
	for _, role := range []string{"csr", "technician"} {
		rec := h.do(t, http.MethodPost, "/api/v1/leads/1/convert", body, role, "someone")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s converting: want 403, got %d", role, rec.Code)
		}
	}
	// ...while the same roles can still work the pipeline.
	if rec := h.do(t, http.MethodGet, "/api/v1/leads", ``, "csr", "sales1"); rec.Code != http.StatusOK {
		t.Errorf("csr listing leads: want 200, got %d", rec.Code)
	}
}

// TestFR_CRM_001_FranchiseScopedCallerSeesOnlyTheirOwnLeads applies the same
// per-partner isolation the franchise endpoints have (CRD-FRN-001) to the
// sales pipeline.
func TestFR_CRM_001_FranchiseScopedCallerSeesOnlyTheirOwnLeads(t *testing.T) {
	h := newCRMHarness(t)
	f1, f2 := 1, 2
	h.leads.seed(crm.Lead{FullName: "Mine", MobileNumber: "+919876500111", Source: "franchise", FranchiseID: &f1})
	h.leads.seed(crm.Lead{FullName: "Theirs", MobileNumber: "+919876500222", Source: "franchise", FranchiseID: &f2})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/leads", nil)
	req.Header.Set("Authorization", "Bearer "+itFranchiseToken(t, "franchise_admin", 1))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var list []crm.Lead
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].FullName != "Mine" {
		t.Errorf("franchise 1 saw %+v, want only their own lead", list)
	}

	// And cannot reach the other partner's lead directly by id.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/leads/2", nil)
	req2.Header.Set("Authorization", "Bearer "+itFranchiseToken(t, "franchise_admin", 1))
	rec2 := httptest.NewRecorder()
	h.mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("cross-franchise lead read: want 403, got %d", rec2.Code)
	}
}

func TestCRMInventoryRoutes_UnconfiguredReturns503(t *testing.T) {
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		// Leads and Inventory left nil.
	})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, itJWTSecret)

	for _, tc := range []struct{ method, path, role string }{
		{http.MethodGet, "/api/v1/leads", "csr"},
		{http.MethodGet, "/api/v1/leads/funnel", "csr"},
		{http.MethodGet, "/api/v1/cpe/devices", "technician"},
		{http.MethodGet, "/api/v1/cpe/stock", "technician"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+itRoleToken(t, tc.role, false))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: want 503 when unconfigured, got %d", tc.path, rec.Code)
		}
	}
}
