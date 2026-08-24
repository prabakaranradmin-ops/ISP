package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// StaffStore serves staff account lookups for the operations console.
type StaffStore struct {
	pool dbPool
}

// NewStaffStore constructs a StaffStore.
func NewStaffStore(pool dbPool) *StaffStore { return &StaffStore{pool: pool} }

// GetStaffByUsername returns an active staff account, or nil when there is no
// such account or it has been deactivated.
//
// Deactivated accounts are indistinguishable from missing ones to the caller
// on purpose: a login form that answered "this account is disabled" would
// confirm to an outsider that the username is real.
func (s *StaffStore) GetStaffByUsername(ctx context.Context, username string) (*staffui.StaffAccount, error) {
	const q = `
		SELECT id, username, password_hash, full_name, role, lea_access, franchise_id
		  FROM staff_users
		 WHERE username = $1 AND active`

	var a staffui.StaffAccount
	err := s.pool.QueryRow(ctx, q, username).Scan(
		&a.ID, &a.Username, &a.PasswordHash, &a.FullName, &a.Role, &a.LeaAccess, &a.FranchiseID)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get staff %q: %w", username, err)
	}
	return &a, nil
}

// ListStaff returns every staff account, active and deactivated alike, for
// the owner-only account-management screen — the deactivated ones have to
// stay visible or there would be no way to find and reactivate one.
// password_hash is deliberately not selected: see staffui.StaffAccount's
// own comment on why.
func (s *StaffStore) ListStaff(ctx context.Context) ([]staffui.StaffAccount, error) {
	const q = `
		SELECT id, username, full_name, role, lea_access, active, franchise_id
		  FROM staff_users
		 ORDER BY username`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list staff: %w", err)
	}
	defer rows.Close()

	out := make([]staffui.StaffAccount, 0, 8)
	for rows.Next() {
		var a staffui.StaffAccount
		if err := rows.Scan(&a.ID, &a.Username, &a.FullName, &a.Role, &a.LeaAccess, &a.Active, &a.FranchiseID); err != nil {
			return nil, fmt.Errorf("db: scan staff row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateStaff registers a new console operator. franchiseID must be non-nil
// exactly when role is franchise-scoped (lco/franchise_admin/
// franchise_staff) — staffui.franchiseIDForRole enforces that before this is
// ever called; chk_staff_franchise_binding enforces it again here as the
// real backstop.
func (s *StaffStore) CreateStaff(ctx context.Context, username, fullName, passwordHash, role string, leaAccess bool, franchiseID *int) (*staffui.StaffAccount, error) {
	const q = `
		INSERT INTO staff_users (username, full_name, password_hash, role, lea_access, franchise_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, username, full_name, role, lea_access, active, franchise_id`

	var a staffui.StaffAccount
	err := s.pool.QueryRow(ctx, q, username, fullName, passwordHash, role, leaAccess, franchiseID).
		Scan(&a.ID, &a.Username, &a.FullName, &a.Role, &a.LeaAccess, &a.Active, &a.FranchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: create staff %q: %w", username, err)
	}
	return &a, nil
}

// UpdateStaff applies a partial update, returning nil when no such account.
//
// franchise_id's own CASE is not a COALESCE like the other three columns:
// when the (possibly just-changed) role is franchise-scoped, an explicit
// franchiseID rebinds the account and a nil one leaves the existing binding
// alone: exactly COALESCE's usual meaning, but conditional on the role,
// because the unconditional form would let a role change to isp_owner keep
// a stale franchise_id and violate chk_staff_franchise_binding on the very
// next admin action, whatever it happened to be, rather than on this one
// that actually caused it. When the role is ISP-wide, franchise_id is
// forced to NULL outright, regardless of what was passed — the same rule
// staffui.franchiseIDForRole already applies before this is ever called,
// enforced again here as the real backstop.
func (s *StaffStore) UpdateStaff(ctx context.Context, id int, role *string, leaAccess, active *bool, franchiseID *int) (*staffui.StaffAccount, error) {
	const q = `
		UPDATE staff_users SET
			role         = COALESCE($2, role),
			lea_access   = COALESCE($3, lea_access),
			active       = COALESCE($4, active),
			franchise_id = CASE
				WHEN COALESCE($2, role) IN ('lco', 'franchise_admin', 'franchise_staff')
					THEN COALESCE($5, franchise_id)
				ELSE NULL
			END
		WHERE id = $1
		RETURNING id, username, full_name, role, lea_access, active, franchise_id`

	var a staffui.StaffAccount
	err := s.pool.QueryRow(ctx, q, id, role, leaAccess, active, franchiseID).
		Scan(&a.ID, &a.Username, &a.FullName, &a.Role, &a.LeaAccess, &a.Active, &a.FranchiseID)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update staff %d: %w", id, err)
	}
	return &a, nil
}

// SetStaffPassword replaces an account's password hash — used both by an
// owner resetting someone else's password and by a staff member changing
// their own.
func (s *StaffStore) SetStaffPassword(ctx context.Context, id int, passwordHash string) error {
	const q = `UPDATE staff_users SET password_hash = $2 WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, passwordHash)
	if err != nil {
		return fmt.Errorf("db: set password for staff %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: staff %d: %w", id, ErrNotFound)
	}
	return nil
}

// TouchStaffLogin records a successful sign-in.
//
// Failures are the caller's to ignore: this is an audit convenience, and
// refusing a login because the timestamp could not be written would trade a
// working console for a bookkeeping detail.
func (s *StaffStore) TouchStaffLogin(ctx context.Context, staffID int) error {
	const q = `UPDATE staff_users SET last_login_at = $2 WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, staffID, time.Now()); err != nil {
		return fmt.Errorf("db: touch staff login %d: %w", staffID, err)
	}
	return nil
}

// Staff exposes the staff account store.
func (d *DB) Staff() *StaffStore { return NewStaffStore(d.pool) }
