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
		{"isp_owner", true, []string{"subscribers", "billing", "tickets", "revenue", "catalogue", "nas", "lea", "demo", "accounts", "franchise", "procurement", "ledger", "inventory", "reports", "tasks"}},
		{"noc_engineer", true, []string{"subscribers", "nas", "lea", "inventory", "tasks"}},
		{"billing_admin", false, []string{"subscribers", "billing", "catalogue", "procurement", "ledger", "inventory", "reports", "tasks"}},
		// Catalogue is deliberately absent for csr and technician: editing a
		// tariff re-prices every subscriber on it and a GST change alters
		// every invoice raised afterwards, which is not reach either role
		// needs to answer a call or fix a line.
		{"csr", false, []string{"subscribers", "billing", "tickets", "tasks"}},
		{"technician", false, []string{"subscribers", "tickets", "inventory", "tasks"}},
		// Franchise-scoped roles get their own restricted pair, never the
		// ISP-wide sections above (not even Subscribers or Tickets) — a
		// franchise partner's reach is My Subscribers/My P&L only.
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
