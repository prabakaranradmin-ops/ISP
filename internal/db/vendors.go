package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// The shared vendor master — FR-INV-004 | migration 071.
// The domain rules live in internal/vendors.
//
// There is deliberately no DeleteVendor. A vendor's name and GSTIN are what
// a purchase invoice was issued under; deleting the row would leave every
// purchase pointing at nothing, or — worse, had the column been ON DELETE SET
// NULL — at nobody, with the only surviving record of who was paid being the
// free text the master was supposed to replace. Vendors are retired instead
// (SetVendorActive), which stops them appearing in pickers while leaving
// every historical row intact and readable.

// VendorStore serves the vendor master.
type VendorStore struct{ pool dbPool }

// Asserted at compile time, as the inventory, CRM and announcement stores do.
// This is the check that catches a method put on the wrong store: the mistake
// is invisible until something tries to wire the two together, and by then the
// error names an interface rather than the method that is in the wrong place.
var _ api.VendorStore = (*VendorStore)(nil)

// Vendors exposes the vendor master store.
func (d *DB) Vendors() *VendorStore { return &VendorStore{pool: d.pool} }

const vendorColumns = `
	v.id, v.name, COALESCE(v.contact_person, ''), COALESCE(v.phone, ''),
	COALESCE(v.email, ''), COALESCE(v.gstin, ''), COALESCE(v.address, ''),
	COALESCE(v.notes, ''), v.active`

// vendorUsage counts what points at a vendor and totals what has been spent
// with it.
//
// Only a received purchase order counts towards spend: a requested or
// rejected one is not money that went anywhere, and counting it would make
// this column disagree with the general ledger for every order still awaiting
// a decision — which is exactly when somebody would be looking at it.
//
// Cast to numeric(16,2) rather than left as the bare sum so the string is
// always fixed-2dp, including the zero a vendor with no purchases produces:
// "0" and "0.00" in the same column would read as two different kinds of
// nothing.
const vendorUsage = `
	(SELECT count(*) FROM cpe_device_types t WHERE t.vendor_id = v.id),
	(SELECT count(*) FROM cpe_purchases p WHERE p.vendor_id = v.id),
	(SELECT count(*) FROM purchase_orders o WHERE o.vendor_id = v.id),
	((COALESCE((SELECT sum(p.quantity * p.unit_cost) FROM cpe_purchases p
	             WHERE p.vendor_id = v.id), 0)
	+ COALESCE((SELECT sum(o.amount) FROM purchase_orders o
	             WHERE o.vendor_id = v.id AND o.status = 'received'), 0)
	 )::numeric(16,2))::text`

func scanVendor(row pgx.Row, v *vendors.Vendor) error {
	return row.Scan(&v.ID, &v.Name, &v.ContactPerson, &v.Phone, &v.Email,
		&v.GSTIN, &v.Address, &v.Notes, &v.Active)
}

// vendorConflict maps the master's two unique indexes onto the domain errors
// that say which one was hit. Both are "this vendor already exists", but they
// need different wording: a name clash is usually a second person adding the
// same supplier, while a GSTIN clash means one legal entity has been entered
// twice under two different names — which is the duplicate worth merging.
func vendorConflict(err error) error {
	switch {
	case uniqueViolation(err, "uq_vendors_name_active"):
		return vendors.ErrNameTaken
	case uniqueViolation(err, "uq_vendors_gstin_active"):
		return vendors.ErrGSTINTaken
	}
	return err
}

// CreateVendor adds a supplier to the master.
func (s *VendorStore) CreateVendor(ctx context.Context, in vendors.Vendor, by string) (*vendors.Vendor, error) {
	in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO vendors (name, contact_person, phone, email, gstin, address, notes, created_by)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''),
		        NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''))
		RETURNING id, name, COALESCE(contact_person, ''), COALESCE(phone, ''),
		          COALESCE(email, ''), COALESCE(gstin, ''), COALESCE(address, ''),
		          COALESCE(notes, ''), active`

	var out vendors.Vendor
	row := s.pool.QueryRow(ctx, q, in.Name, in.ContactPerson, in.Phone, in.Email,
		in.GSTIN, in.Address, in.Notes, by)
	if err := scanVendor(row, &out); err != nil {
		return nil, fmt.Errorf("db: create vendor: %w", vendorConflict(err))
	}
	return &out, nil
}

// UpdateVendor replaces a vendor's details. Every field is written, including
// the empty ones — this backs an edit form, and a form that silently ignored
// a cleared field would make it impossible to remove a contact who has left.
//
// A retired vendor can still be edited: correcting the GSTIN on a supplier
// you no longer buy from is exactly what somebody reconciling last year's
// input tax credit needs to do.
func (s *VendorStore) UpdateVendor(ctx context.Context, id int, in vendors.Vendor, by string) (*vendors.Vendor, error) {
	in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		UPDATE vendors SET
			name = $2, contact_person = NULLIF($3, ''), phone = NULLIF($4, ''),
			email = NULLIF($5, ''), gstin = NULLIF($6, ''), address = NULLIF($7, ''),
			notes = NULLIF($8, ''), updated_by = NULLIF($9, '')
		WHERE id = $1
		RETURNING id, name, COALESCE(contact_person, ''), COALESCE(phone, ''),
		          COALESCE(email, ''), COALESCE(gstin, ''), COALESCE(address, ''),
		          COALESCE(notes, ''), active`

	var out vendors.Vendor
	row := s.pool.QueryRow(ctx, q, id, in.Name, in.ContactPerson, in.Phone, in.Email,
		in.GSTIN, in.Address, in.Notes, by)
	err := scanVendor(row, &out)
	if isNoRows(err) {
		return nil, vendors.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: update vendor %d: %w", id, vendorConflict(err))
	}
	return &out, nil
}

// SetVendorActive retires or restores a vendor.
//
// Restoring can collide: the partial unique indexes only cover active rows,
// so while this vendor was retired somebody may have created another under
// the same name or GSTIN. That surfaces as the same ErrNameTaken/ErrGSTINTaken
// a create would give, which is the honest answer — there is now a live
// vendor by that name, and which of the two should stay is a question for the
// operator, not for this function to guess.
func (s *VendorStore) SetVendorActive(ctx context.Context, id int, active bool, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE vendors SET active = $2, updated_by = NULLIF($3, '')
		 WHERE id = $1 AND active <> $2`, id, active, by)
	if err != nil {
		return fmt.Errorf("db: set vendor %d active=%v: %w", id, active, vendorConflict(err))
	}
	if tag.RowsAffected() != 1 {
		// Either no such vendor, or it is already in the state asked for.
		// Told apart so "already retired" does not read as "no such vendor",
		// which would send an operator looking for a row that is right in
		// front of them.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT true FROM vendors WHERE id = $1`, id).Scan(&exists); isNoRows(err) {
			return vendors.ErrNotFound
		} else if err != nil {
			return fmt.Errorf("db: confirm vendor %d exists: %w", id, err)
		}
		return nil // already in the requested state; nothing to do
	}
	return nil
}

// GetVendor returns one vendor, or vendors.ErrNotFound.
func (s *VendorStore) GetVendor(ctx context.Context, id int) (*vendors.Vendor, error) {
	q := fmt.Sprintf(`SELECT %s FROM vendors v WHERE v.id = $1`, vendorColumns)
	var out vendors.Vendor
	err := scanVendor(s.pool.QueryRow(ctx, q, id), &out)
	if isNoRows(err) {
		return nil, vendors.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: get vendor %d: %w", id, err)
	}
	return &out, nil
}

// ListActiveVendors returns the vendors a new purchase may name, for the
// pickers. Name-ordered case-insensitively, because an operator scanning a
// dropdown for "chennai networks" should not have to know it was entered
// lower-case to find it below the Zs.
func (s *VendorStore) ListActiveVendors(ctx context.Context) ([]vendors.Vendor, error) {
	q := fmt.Sprintf(`SELECT %s FROM vendors v WHERE v.active ORDER BY lower(v.name)`, vendorColumns)
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list active vendors: %w", err)
	}
	defer rows.Close()

	out := make([]vendors.Vendor, 0, 16)
	for rows.Next() {
		var v vendors.Vendor
		if err := scanVendor(rows, &v); err != nil {
			return nil, fmt.Errorf("db: scan vendor row: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVendors returns the master list with usage counts. Retired vendors come
// last rather than being hidden: they are where the duplicates to merge and
// the GSTINs to correct are, and a screen that hid them would hide the work.
func (s *VendorStore) ListVendors(ctx context.Context) ([]vendors.VendorWithUsage, error) {
	q := fmt.Sprintf(`
		SELECT %s, %s
		  FROM vendors v
		 ORDER BY v.active DESC, lower(v.name)`, vendorColumns, vendorUsage)

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list vendors: %w", err)
	}
	defer rows.Close()

	out := make([]vendors.VendorWithUsage, 0, 16)
	for rows.Next() {
		var r vendors.VendorWithUsage
		if err := rows.Scan(&r.ID, &r.Name, &r.ContactPerson, &r.Phone, &r.Email,
			&r.GSTIN, &r.Address, &r.Notes, &r.Active,
			&r.Usage.DeviceTypes, &r.Usage.Purchases, &r.Usage.PurchaseOrders,
			&r.Usage.SpendStr); err != nil {
			return nil, fmt.Errorf("db: scan vendor usage row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveVendorByName finds an active vendor by name, matched the way the
// master's unique index keys it — case-insensitively, with runs of whitespace
// collapsed.
//
// This exists for the JSON API's compatibility path, where a client still
// sends a vendor name rather than an id. It is not a back door to free text:
// an unknown name is refused with ErrNotFound rather than creating a vendor,
// because a typo that silently created "Chennai Netwoks" would reintroduce
// the exact duplication this requirement removes — and it would do it through
// the one path nobody is watching.
func (s *VendorStore) ResolveVendorByName(ctx context.Context, name string) (*vendors.Vendor, error) {
	key := vendors.NormalisedKey(name)
	if key == "" {
		return nil, vendors.ErrNotFound
	}
	q := fmt.Sprintf(`
		SELECT %s FROM vendors v
		 WHERE v.active
		   AND lower(regexp_replace(btrim(v.name), '\s+', ' ', 'g')) = $1`, vendorColumns)

	var out vendors.Vendor
	err := scanVendor(s.pool.QueryRow(ctx, q, key), &out)
	if isNoRows(err) {
		return nil, vendors.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: resolve vendor %q: %w", name, err)
	}
	return &out, nil
}

// vendorWriteFailure explains why an insert that selected its vendor from the
// master matched no row. Called only on the failure path, so the happy path
// stays one statement: checking the vendor first and then inserting would
// leave a window in which it is retired in between, and the insert would
// then carry a vendor the check had approved.
//
// Returns ErrNotFound or ErrInactive — the two reasons, told apart, because
// "no such vendor" sends an operator looking for a typo while "retired" sends
// them to the master to restore it.
func vendorWriteFailure(ctx context.Context, pool dbPool, vendorID int) error {
	var active bool
	err := pool.QueryRow(ctx, `SELECT active FROM vendors WHERE id = $1`, vendorID).Scan(&active)
	if isNoRows(err) {
		return vendors.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("db: check vendor %d: %w", vendorID, err)
	}
	if !active {
		return vendors.ErrInactive
	}
	// The vendor is there and active, so the insert failed for a reason that
	// has nothing to do with it — a bad device_type_id, most likely. Returned
	// rather than swallowed: a nil here would report success on a write that
	// did not happen.
	return fmt.Errorf("db: vendor %d is usable but the write matched no row", vendorID)
}
