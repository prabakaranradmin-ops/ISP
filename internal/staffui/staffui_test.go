package staffui_test

import (
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// The console's authorisation table is the thing most worth pinning: it decides
// what five different job roles can see about a customer's money, connection
// and legal record. It is asserted here against expectations written out
// literally, not derived from the table under test, because a test that reads
// the same list it is checking would agree with any mistake in it.

func TestFR_SEC_005_AllowedSections_MatchesTheAPIRoleMatrix(t *testing.T) {
	cases := []struct {
		role      string
		leaAccess bool
		want      []string
	}{
		// Dashboard leads every ISP-wide list because Home redirects to the
		// first allowed section; that order is part of what is pinned here.
		{"isp_owner", true, []string{"dashboard", "subscribers", "leads", "billing", "tickets", "invoices", "revenue", "catalogue", "products", "bandwidth", "groups", "fair-use", "sessions", "cpe", "nas", "ip-pools", "auth-log", "plan-attributes", "network", "lea", "demo", "accounts", "documents", "audit", "franchise", "procurement", "ledger", "dunning", "diagnostics", "billing-runs", "gateway-log", "collections", "inventory", "reports", "announcements", "logs", "tasks"}},
		{"noc_engineer", true, []string{"dashboard", "subscribers", "groups", "sessions", "cpe", "nas", "ip-pools", "auth-log", "plan-attributes", "network", "lea", "inventory", "logs", "tasks"}},
		{"billing_admin", false, []string{"dashboard", "subscribers", "leads", "billing", "invoices", "catalogue", "products", "bandwidth", "groups", "fair-use", "sessions", "documents", "procurement", "ledger", "dunning", "diagnostics", "billing-runs", "gateway-log", "collections", "inventory", "reports", "announcements", "logs", "tasks"}},
		// Catalogue is deliberately absent for csr and technician: editing a
		// tariff re-prices every subscriber on it and a GST change alters
		// every invoice raised afterwards, which is not reach either role
		// needs to answer a call or fix a line.
		//
		// Gateway Log (FR-PAY-001) is billing tier, same reach as the
		// ledger: every row is money arriving from the gateway, and the
		// ones that matter are where it arrived and reached nobody.
		//
		// Auth Log (FR-NET-004) is deliberately wider than IP Pools: "why
		// can't this customer get online" is the commonest support call,
		// and the answer is in it. Reading why an authentication failed is
		// diagnosis, not reach over the network's address space — which is
		// why a technician and a CSR have it and neither has IP Pools.
		//
		// Audit Trail (FR-OBS-006) is owner-only and appears for no other
		// role: the trail names every action every operator has taken, so
		// it is a record about the staff as much as about the subscribers.
		//
		// IP Pools (FR-NET-005) is NOC tier, like Routers: a pool
		// describes the network's address space. Billing staff are absent
		// from the section even though they may assign a subscriber an
		// address — that action is gated inside the subscriber's page
		// (canAssignStaticIP), not by this list, because it belongs to the
		// customer rather than to the network estate.
		//
		// Products & Charges (FR-BIL-022) is present for csr and absent for
		// technician, which looks inconsistent with Catalogue beside it and
		// is not: a CSR fielding "why is my bill higher this month" has to
		// be able to read the charges on the account. Changing the
		// catalogue, and attaching or ending a charge, are gated again
		// inside the screen to owner/billing (canManageProducts), so what a
		// CSR gains here is reading, not pricing.
		//
		// Subscriber Groups (FR-GRP-001) is the widest of the Subscribers-tier
		// screens: owner, billing, csr and noc_engineer, and absent for
		// technician. A group is a label on a set of customers, not a price or
		// a speed, and the people who know which customers belong together are
		// the front desk and the NOC. A technician is left out because their
		// reach is the job in front of them rather than the shape of the base —
		// and a group is what an announcement is addressed to.
		{"csr", false, []string{"dashboard", "subscribers", "leads", "billing", "tickets", "invoices", "products", "groups", "sessions", "auth-log", "logs", "tasks"}},
		{"technician", false, []string{"dashboard", "subscribers", "leads", "tickets", "sessions", "cpe", "auth-log", "collections", "inventory", "tasks"}},
		// Franchise-scoped roles get their own restricted pair, never the
		// ISP-wide sections above (not even the Dashboard, whose counts span
		// every partner's subscribers) — a franchise partner's reach is My
		// Subscribers/My P&L only.
		{"lco", false, []string{"my-subscribers", "my-pnl"}},
		{"franchise_admin", false, []string{"my-subscribers", "my-pnl"}},
		{"franchise_staff", false, []string{"my-subscribers", "my-pnl"}},
		// An unknown role gets nothing. Defaulting to any access would make a
		// typo in a role name a privilege grant.
		{"intern", false, nil},
		{"", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.role+"/lea="+boolStr(tc.leaAccess), func(t *testing.T) {
			got := keys(staffui.AllowedSections(tc.role, tc.leaAccess))
			if !equal(got, tc.want) {
				t.Errorf("role %q: want %v, got %v", tc.role, tc.want, got)
			}
		})
	}
}

// TestFR_SEC_005_LEASectionRequiresTheClaimNotJustTheRole is the property SecD
// §9.3 exists for: reach over law-enforcement lookups must never arrive as a
// side effect of a job title. A NOC engineer or owner without the claim must
// not see the section, and no other role sees it even holding the claim.
func TestFR_SEC_005_LEASectionRequiresTheClaimNotJustTheRole(t *testing.T) {
	for _, role := range []string{"noc_engineer", "isp_owner"} {
		if has(staffui.AllowedSections(role, false), "lea") {
			t.Errorf("%s without lea_access must not reach the LEA section", role)
		}
		if !has(staffui.AllowedSections(role, true), "lea") {
			t.Errorf("%s with lea_access should reach the LEA section", role)
		}
	}
	// The claim alone is not enough either — the role still has to qualify.
	for _, role := range []string{"csr", "technician", "billing_admin"} {
		if has(staffui.AllowedSections(role, true), "lea") {
			t.Errorf("%s must not reach the LEA section even holding lea_access", role)
		}
	}
}

// TestFR_SEC_005_NoRoleSeesEverythingByAccident — only the owner is meant to
// have the full console. If a change ever widens another role to everything,
// this fails rather than the widening going unnoticed.
func TestFR_SEC_005_NoRoleSeesEverythingByAccident(t *testing.T) {
	full := len(staffui.AllowedSections("isp_owner", true))
	for _, role := range []string{"noc_engineer", "billing_admin", "csr", "technician"} {
		if n := len(staffui.AllowedSections(role, true)); n >= full {
			t.Errorf("%s reaches %d sections; only isp_owner (%d) should have the full console", role, n, full)
		}
	}
}

func keys(ss []staffui.Section) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Key)
	}
	return out
}

func has(ss []staffui.Section, key string) bool {
	for _, s := range ss {
		if s.Key == key {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
