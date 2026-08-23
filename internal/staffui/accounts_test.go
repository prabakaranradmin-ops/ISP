package staffui

import (
	"context"
	"net/http/httptest"
	"testing"
)

// stubStaffStore backs only what blockLastOwnerLockout reads (ListStaff);
// every other method is unused by these tests and just satisfies the
// interface.
type stubStaffStore struct {
	accounts []StaffAccount
}

func (s *stubStaffStore) GetStaffByUsername(context.Context, string) (*StaffAccount, error) {
	return nil, nil
}
func (s *stubStaffStore) TouchStaffLogin(context.Context, int) error { return nil }
func (s *stubStaffStore) ListStaff(context.Context) ([]StaffAccount, error) {
	return s.accounts, nil
}
func (s *stubStaffStore) CreateStaff(context.Context, string, string, string, string, bool) (*StaffAccount, error) {
	return nil, nil
}
func (s *stubStaffStore) UpdateStaff(context.Context, int, *string, *bool, *bool) (*StaffAccount, error) {
	return nil, nil
}
func (s *stubStaffStore) SetStaffPassword(context.Context, int, string) error { return nil }

// TestBlockLastOwnerLockout is the property the owner-only Staff Accounts
// screen exists to never violate: there must always be at least one active
// isp_owner account, or that screen itself becomes permanently unreachable
// with no SQL-free way back in.
func TestBlockLastOwnerLockout(t *testing.T) {
	r := httptest.NewRequest("POST", "/staff/accounts/1/update", nil)

	cases := []struct {
		name        string
		roster      []StaffAccount
		id          int
		newRole     string
		newActive   bool
		wantBlocked bool
	}{
		{
			name:   "deactivating the only owner is blocked",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 2, Role: "csr", Active: true}},
			id:     1, newRole: "isp_owner", newActive: false,
			wantBlocked: true,
		},
		{
			name:   "demoting the only owner is blocked",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 2, Role: "csr", Active: true}},
			id:     1, newRole: "csr", newActive: true,
			wantBlocked: true,
		},
		{
			name:   "editing the only owner but keeping them active owner is allowed",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 2, Role: "csr", Active: true}},
			id:     1, newRole: "isp_owner", newActive: true,
			wantBlocked: false,
		},
		{
			name:   "deactivating a non-owner is never blocked",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 2, Role: "csr", Active: true}},
			id:     2, newRole: "csr", newActive: false,
			wantBlocked: false,
		},
		{
			name:   "promoting a non-owner to owner is never blocked",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 2, Role: "csr", Active: true}},
			id:     2, newRole: "isp_owner", newActive: true,
			wantBlocked: false,
		},
		{
			name:   "deactivating one of two active owners is allowed",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 3, Role: "isp_owner", Active: true}},
			id:     1, newRole: "isp_owner", newActive: false,
			wantBlocked: false,
		},
		{
			name:   "an already-deactivated owner losing owner role changes nothing",
			roster: []StaffAccount{{ID: 1, Role: "isp_owner", Active: true}, {ID: 4, Role: "isp_owner", Active: false}},
			id:     4, newRole: "csr", newActive: false,
			wantBlocked: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{staff: &stubStaffStore{accounts: tc.roster}}
			blocked, msg := h.blockLastOwnerLockout(r, tc.id, tc.newRole, tc.newActive)
			if blocked != tc.wantBlocked {
				t.Errorf("blocked = %v (msg %q), want %v", blocked, msg, tc.wantBlocked)
			}
			if blocked && msg == "" {
				t.Error("a blocked update must explain why")
			}
		})
	}
}

func TestIsStaffRole(t *testing.T) {
	for _, r := range staffRoles {
		if !isStaffRole(r) {
			t.Errorf("isStaffRole(%q) = false, want true (it's in staffRoles)", r)
		}
	}
	// Franchise-scoped roles exist in the DB's CHECK constraint (migration
	// 024) but are deliberately out of scope for this screen — no UI here
	// collects the franchise_id they'd require.
	for _, r := range []string{"franchise_admin", "franchise_staff", "lco", "", "made_up_role"} {
		if isStaffRole(r) {
			t.Errorf("isStaffRole(%q) = true, want false", r)
		}
	}
}
