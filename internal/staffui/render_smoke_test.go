package staffui

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// Template execution errors (an undefined field, a func called with the
// wrong type) only surface when a branch actually runs with real data —
// html/template.Must at package init only catches parse errors. These
// smoke tests exist because that gap has bitten this package before (see
// render.go's own comment on a mistyped health.SubscriberRecord field
// first showing up as a silently empty panel): every screen touched by the
// NAS/demo-data/speed-override/bulk-action/tabs work is rendered here with
// real data in each shape the template branches on, and a render.go
// failure (which reports 500) fails the test instead of only being
// noticed by an operator later.
func assertRendersOK(t *testing.T, h *Handler, name string, d pageData) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.render(rec, name, d)
	if rec.Code != 200 {
		t.Fatalf("render %q: got status %d, body: %s", name, rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "could not be rendered") {
		t.Fatalf("render %q: template execution failed, body: %s", name, rec.Body.String())
	}
}

func testSession() Session {
	return Session{StaffID: 1, Username: "owner", FullName: "Owner", Role: "isp_owner"}
}

func TestRenderSubscriberDetail(t *testing.T) {
	h := &Handler{}
	s := testSession()
	future := time.Now().Add(24 * time.Hour)

	cases := []struct {
		name string
		data subscriberDetailData
	}{
		{"empty", subscriberDetailData{}},
		{"full, no overrides, offline", subscriberDetailData{
			Subscriber:  &api.SubscriberRecord{ID: 1, Username: "demo_priya", Status: "active", CAFNumber: "CAF-1", MobileNumber: "+919800000001", RegisteredState: "TN"},
			Health:      &health.SubscriberRecord{WalletBalance: decimal.NewFromInt(500), OpenTickets: 1},
			Balance:     decimal.NewFromInt(500),
			Ledger:      []api.LedgerEntry{{ID: 1, EntryType: "credit", Amount: "500.00", BalanceAfter: "500.00", CreatedAt: time.Now()}},
			Tickets:     []portal.TicketEntry{{ID: 1, Category: "connectivity", Description: "slow", Status: "open", CreatedAt: time.Now()}},
			ShowBilling: true, ShowTickets: true, ShowSpeedOverride: true,
		}},
		{"online with active speed override", subscriberDetailData{
			Subscriber: &api.SubscriberRecord{
				ID: 2, Username: "demo_arjun", Status: "active",
				SpeedOverrideRateLimit: "100M/100M", SpeedOverrideExpiresAt: &future,
			},
			Session:           &portal.ActiveSession{AssignedIP: "10.0.0.5", GBUsed: decimal.NewFromInt(10), GBIncluded: decimal.NewFromInt(100), StartedAt: time.Now()},
			ShowSpeedOverride: true,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRendersOK(t, h, "subscriber_detail", pageData{Session: s, Data: tc.data})
		})
	}
}

func TestRenderSubscribers(t *testing.T) {
	h := &Handler{}
	s := testSession()

	cases := []struct {
		name string
		data subscribersData
	}{
		{"no results, no bulk", subscribersData{}},
		{"results, bulk actions shown", subscribersData{
			Results:         []revenue.SubscriberRow{{ID: 1, Username: "demo_priya"}, {ID: 2, Username: "demo_arjun"}},
			Total:           2,
			ShowBulkActions: true,
			Plans:           []Plan{{ID: 1, Name: "Demo_Home_50M"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRendersOK(t, h, "subscribers", pageData{Session: s, Data: tc.data})
		})
	}
}

func TestRenderNAS(t *testing.T) {
	h := &Handler{}
	assertRendersOK(t, h, "nas", pageData{Session: testSession(), Data: nasData{
		Devices: []nas.DeviceSummary{{ID: 1, IP: "203.0.113.10", Vendor: "mikrotik", CoAPort: 1700, PoDPort: 1700, AllowMAB: true}},
		Vendors: nas.Vendors(),
	}})
	assertRendersOK(t, h, "nas", pageData{Session: testSession(), Data: nasData{Vendors: nas.Vendors()}})
}

func TestRenderDemo(t *testing.T) {
	h := &Handler{}
	assertRendersOK(t, h, "demo", pageData{Session: testSession(), Data: demoData{}})
	assertRendersOK(t, h, "demo", pageData{Session: testSession(), Data: demoData{
		Status: DemoStatus{Subscribers: 5, Plans: 2, NASDevices: 1},
	}})
}

func TestRenderAccounts(t *testing.T) {
	h := &Handler{}
	assertRendersOK(t, h, "accounts", pageData{Session: testSession(), Data: accountsData{Roles: staffRoles}})
	assertRendersOK(t, h, "accounts", pageData{Session: testSession(), Data: accountsData{
		Roles: staffRoles,
		Accounts: []StaffAccount{
			{ID: 1, Username: "owner", FullName: "Owner", Role: "isp_owner", LeaAccess: true, Active: true},
			{ID: 2, Username: "old_csr", FullName: "Former CSR", Role: "csr", LeaAccess: false, Active: false},
		},
	}})
}

func TestRenderChangePassword(t *testing.T) {
	h := &Handler{}
	assertRendersOK(t, h, "change_password", pageData{Session: testSession()})
	assertRendersOK(t, h, "change_password", pageData{Session: testSession(), Error: "Current password is incorrect."})
}
