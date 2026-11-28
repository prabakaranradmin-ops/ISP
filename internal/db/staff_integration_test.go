//go:build integration

// The staff account store against a real database — FR-SEC-002 (role-based
// access), SecD §9.3 (LEA access is a separate grant).
//
// staff.go was at 0% after a full integration run: the store behind console
// sign-in, account creation and the LEA flag had never executed against
// PostgreSQL. Its comments make three promises worth holding it to, and
// none was checked: a deactivated account looks exactly like a missing one
// to the sign-in path; a role change to an ISP-wide role drops the franchise
// binding instead of tripping chk_staff_franchise_binding on some later,
// unrelated edit; and LEA access is never granted as a side effect of a role
// change.
package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/db"
)

func intp(n int) *int       { return &n }
func boolp(b bool) *bool    { return &b }
func strp(s string) *string { return &s }

func TestFR_SEC_002_StaffAccountsRoundTrip(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	s := d.Staff()

	created, err := s.CreateStaff(ctx, "meena", "Meena R", "$2a$12$hash-of-meena", "csr", false, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created.Active || created.Role != "csr" || created.LeaAccess || created.FranchiseID != nil {
		t.Errorf("created %+v", created)
	}
	if created.PasswordHash != "" {
		t.Error("CreateStaff returned the password hash; nothing that lists or echoes an account should carry it")
	}

	got, err := s.GetStaffByUsername(ctx, "meena")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.PasswordHash != "$2a$12$hash-of-meena" || got.ID != created.ID {
		t.Errorf("sign-in lookup %+v; it is the one read that needs the hash", got)
	}
	if missing, err := s.GetStaffByUsername(ctx, "nobody"); err != nil || missing != nil {
		t.Errorf("an unknown username: %v %v", missing, err)
	}

	// Usernames are unique: a second account cannot be created under one.
	if _, err := s.CreateStaff(ctx, "meena", "Someone Else", "$2a$12$x", "csr", false, nil); err == nil {
		t.Error("a second account was created under an existing username")
	}

	if err := s.SetStaffPassword(ctx, created.ID, "$2a$12$new-hash"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetStaffByUsername(ctx, "meena"); got.PasswordHash != "$2a$12$new-hash" {
		t.Errorf("password not replaced: %q", got.PasswordHash)
	}
	if err := s.SetStaffPassword(ctx, 9999, "$2a$12$x"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("setting a missing account's password: %v", err)
	}

	before := time.Now().Add(-time.Second)
	if err := s.TouchStaffLogin(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	var last time.Time
	if err := d.Pool().QueryRow(ctx, `SELECT last_login_at FROM staff_users WHERE id = $1`, created.ID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last.Before(before) {
		t.Errorf("last_login_at = %v", last)
	}
}

// A deactivated account is indistinguishable from a missing one at sign-in,
// so the login form cannot confirm a username is real — and it stays on the
// management list, or nobody could find it to reactivate it.
func TestFR_SEC_002_ADeactivatedAccountCannotSignIn(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	s := d.Staff()
	a, err := s.CreateStaff(ctx, "ravi", "Ravi K", "$2a$12$h", "technician", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateStaff(ctx, a.ID, nil, nil, boolp(false), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetStaffByUsername(ctx, "ravi"); err != nil || got != nil {
		t.Errorf("a deactivated account signed in: %+v %v", got, err)
	}

	list, err := s.ListStaff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range list {
		if row.PasswordHash != "" {
			t.Errorf("ListStaff carried %s's password hash", row.Username)
		}
		if row.Username == "ravi" {
			found = true
			if row.Active {
				t.Error("ravi is listed as active")
			}
		}
	}
	if !found {
		t.Error("a deactivated account vanished from the list, so it could never be reactivated")
	}

	if _, err := s.UpdateStaff(ctx, a.ID, nil, nil, boolp(true), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetStaffByUsername(ctx, "ravi"); got == nil {
		t.Error("a reactivated account cannot sign in")
	}
	if got, err := s.UpdateStaff(ctx, 9999, strp("csr"), nil, nil, nil); err != nil || got != nil {
		t.Errorf("updating a missing account: %+v %v", got, err)
	}
}

// Franchise-scoped roles need a franchise, ISP-wide roles must not have one,
// and a role change moves the binding with it instead of leaving a stale one
// for chk_staff_franchise_binding to trip over on the next, unrelated edit.
func TestFR_SEC_002_TheFranchiseBindingFollowsTheRole(t *testing.T) {
	d, pool := newTestDB(t)
	ctx := context.Background()
	seedFranchise(ctx, t, pool, 2, "Anna Nagar Networks", "10.00", "active")
	seedFranchise(ctx, t, pool, 3, "Velachery Fibre", "10.00", "active")
	s := d.Staff()

	if _, err := s.CreateStaff(ctx, "lco_nobind", "x", "$2a$12$h", "lco", false, nil); err == nil {
		t.Error("an LCO account was created with no franchise")
	}
	if _, err := s.CreateStaff(ctx, "owner_bound", "x", "$2a$12$h", "isp_owner", false, intp(2)); err == nil {
		t.Error("an ISP-wide account was created bound to a franchise")
	}

	lco, err := s.CreateStaff(ctx, "anna_lco", "Anna", "$2a$12$h", "lco", false, intp(2))
	if err != nil {
		t.Fatalf("create lco: %v", err)
	}
	if lco.FranchiseID == nil || *lco.FranchiseID != 2 {
		t.Fatalf("lco bound to %v", lco.FranchiseID)
	}

	// Staying franchise-scoped with no new franchise keeps the binding.
	got, err := s.UpdateStaff(ctx, lco.ID, strp("franchise_staff"), nil, nil, nil)
	if err != nil || got.FranchiseID == nil || *got.FranchiseID != 2 {
		t.Errorf("a scoped-to-scoped role change lost the binding: %+v %v", got, err)
	}
	// An explicit franchise rebinds.
	got, err = s.UpdateStaff(ctx, lco.ID, nil, nil, nil, intp(3))
	if err != nil || got.FranchiseID == nil || *got.FranchiseID != 3 {
		t.Errorf("rebinding: %+v %v", got, err)
	}
	// A move to an ISP-wide role drops it, even if a franchise is passed.
	got, err = s.UpdateStaff(ctx, lco.ID, strp("csr"), nil, nil, intp(3))
	if err != nil {
		t.Fatalf("scoped to ISP-wide: %v — the stale binding should have been cleared, not tripped the constraint", err)
	}
	if got.FranchiseID != nil {
		t.Errorf("an ISP-wide account kept franchise %d", *got.FranchiseID)
	}
	// And a later unrelated edit does not trip the constraint.
	if _, err := s.UpdateStaff(ctx, lco.ID, nil, nil, boolp(false), nil); err != nil {
		t.Errorf("an unrelated edit after a role change failed: %v", err)
	}
}

// LEA access is its own grant. No role change confers it, and only an
// explicit lea_access change moves it.
func TestSecD_9_3_ARoleChangeNeverGrantsLEAAccess(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	s := d.Staff()
	a, err := s.CreateStaff(ctx, "noc1", "NOC One", "$2a$12$h", "csr", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"noc_engineer", "isp_owner", "billing_admin"} {
		got, err := s.UpdateStaff(ctx, a.ID, strp(role), nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.LeaAccess {
			t.Errorf("becoming %s granted LEA access", role)
		}
	}
	got, err := s.UpdateStaff(ctx, a.ID, nil, boolp(true), nil, nil)
	if err != nil || !got.LeaAccess {
		t.Errorf("an explicit grant did not land: %+v %v", got, err)
	}
	// And a later role change does not take it away silently either.
	got, err = s.UpdateStaff(ctx, a.ID, strp("noc_engineer"), nil, nil, nil)
	if err != nil || !got.LeaAccess {
		t.Errorf("a role change revoked an explicit LEA grant: %+v %v", got, err)
	}
	if signin, _ := s.GetStaffByUsername(ctx, "noc1"); signin == nil || !signin.LeaAccess {
		t.Error("the sign-in lookup does not carry the LEA grant into the session")
	}
}
