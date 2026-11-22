package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// InventoryStore serves CPE stock. Satisfies api.InventoryQuerier.
type InventoryStore struct{ pool dbPool }

var _ api.InventoryQuerier = (*InventoryStore)(nil)

// ── Device types ─────────────────────────────────────────────────────────────

// vendorName is how every read in this file resolves a supplier: the
// master's current name, falling back to the free text the row was written
// with (FR-INV-004 | migration 071).
//
// COALESCE rather than the joined name alone, so a row the backfill could not
// link — one whose vendor text was blank, or one a later hand-edit
// unlinked — still shows whatever it originally said instead of going blank.
// Losing the vendor from a purchase history is a worse outcome than showing a
// name that is not in the master yet.
const vendorName = `COALESCE(v.name, %s.vendor)`

// CreateDeviceType registers a CPE model and its reorder point.
//
// The vendor is selected from the master in the same statement as the insert,
// rather than checked first and then written: a separate check leaves a window
// in which the vendor is retired in between, and the insert would then carry a
// vendor the check had approved. It also means the name stored on the row is
// the master's own, so the two can never disagree at the moment of writing.
func (s *InventoryStore) CreateDeviceType(ctx context.Context, t inventory.DeviceType) (*inventory.DeviceType, error) {
	if t.VendorID == nil || *t.VendorID <= 0 {
		return nil, vendors.ErrRequired
	}
	const q = `
		INSERT INTO cpe_device_types (name, vendor, vendor_id, reorder_threshold)
		SELECT $1, v.name, v.id, $3
		  FROM vendors v
		 WHERE v.id = $2 AND v.active
		RETURNING id, name, vendor, vendor_id, reorder_threshold, created_at`

	var out inventory.DeviceType
	err := s.pool.QueryRow(ctx, q, t.Name, *t.VendorID, t.ReorderThreshold).
		Scan(&out.ID, &out.Name, &out.Vendor, &out.VendorID, &out.ReorderThreshold, &out.CreatedAt)
	if isNoRows(err) {
		// No row inserted means the SELECT found no usable vendor — the one
		// failure the unique-name violation below cannot explain.
		return nil, vendorWriteFailure(ctx, s.pool, *t.VendorID)
	}
	if err != nil {
		return nil, fmt.Errorf("db: create device type %q: %w", t.Name, err)
	}
	return &out, nil
}

// ListDeviceTypes returns every registered CPE model.
func (s *InventoryStore) ListDeviceTypes(ctx context.Context) ([]inventory.DeviceType, error) {
	q := fmt.Sprintf(`
		SELECT t.id, t.name, %s, t.vendor_id, t.reorder_threshold, t.created_at
		  FROM cpe_device_types t
		  LEFT JOIN vendors v ON v.id = t.vendor_id
		 ORDER BY t.name`, fmt.Sprintf(vendorName, "t"))

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list device types: %w", err)
	}
	defer rows.Close()

	var out []inventory.DeviceType
	for rows.Next() {
		var t inventory.DeviceType
		if err := rows.Scan(&t.ID, &t.Name, &t.Vendor, &t.VendorID,
			&t.ReorderThreshold, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan device type: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate device types: %w", err)
	}
	return out, nil
}

// ── Devices ──────────────────────────────────────────────────────────────────

const deviceColumns = `
	d.id, d.device_type_id, t.name, d.serial_number, COALESCE(d.mac_address, ''),
	d.status, COALESCE(d.location, ''), d.subscriber_id, d.issued_at,
	COALESCE(d.notes, ''), d.created_at, d.updated_at`

func scanDevice(row interface{ Scan(dest ...any) error }) (*inventory.Device, error) {
	var d inventory.Device
	err := row.Scan(
		&d.ID, &d.DeviceTypeID, &d.DeviceType, &d.SerialNumber, &d.MACAddress,
		&d.Status, &d.Location, &d.SubscriberID, &d.IssuedAt,
		&d.Notes, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateDevice adds one physical unit to stock.
func (s *InventoryStore) CreateDevice(ctx context.Context, d inventory.Device) (*inventory.Device, error) {
	const q = `
		WITH ins AS (
			INSERT INTO cpe_devices (device_type_id, serial_number, mac_address, location, notes)
			VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''))
			RETURNING *
		)
		SELECT ` + deviceColumns + `
		FROM ins d JOIN cpe_device_types t ON t.id = d.device_type_id`

	created, err := scanDevice(s.pool.QueryRow(ctx, q,
		d.DeviceTypeID, d.SerialNumber, d.MACAddress, d.Location, d.Notes))
	if err != nil {
		// Surfaced verbatim so a duplicate serial_number can be classified
		// as 409 rather than 500 — two warehouse entries for one physical
		// router is exactly what the unique index exists to catch.
		return nil, fmt.Errorf("db: create device %q: %w", d.SerialNumber, err)
	}
	return created, nil
}

// GetDeviceBySerial looks a device up by its physical identity. A missing
// row returns (nil, nil).
func (s *InventoryStore) GetDeviceBySerial(ctx context.Context, serial string) (*inventory.Device, error) {
	const q = `
		SELECT ` + deviceColumns + `
		FROM cpe_devices d JOIN cpe_device_types t ON t.id = d.device_type_id
		WHERE d.serial_number = $1`

	d, err := scanDevice(s.pool.QueryRow(ctx, q, serial))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get device %q: %w", serial, err)
	}
	return d, nil
}

// ListDevices lists stock, optionally filtered by status, type and holder.
func (s *InventoryStore) ListDevices(ctx context.Context, status *string, deviceTypeID, subscriberID *int) ([]inventory.Device, error) {
	const q = `
		SELECT ` + deviceColumns + `
		FROM cpe_devices d JOIN cpe_device_types t ON t.id = d.device_type_id
		WHERE ($1::text IS NULL OR d.status = $1)
		  AND ($2::int  IS NULL OR d.device_type_id = $2)
		  AND ($3::int  IS NULL OR d.subscriber_id = $3)
		ORDER BY t.name, d.serial_number`

	rows, err := s.pool.Query(ctx, q, status, deviceTypeID, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list devices: %w", err)
	}
	defer rows.Close()

	var out []inventory.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan device: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate devices: %w", err)
	}
	return out, nil
}

// IssueDevice atomically claims an in-stock device for a subscriber.
//
// The `AND d.status = 'in_stock'` predicate is the whole guarantee behind
// FR-INV-002: two concurrent issues of the same serial cannot both match,
// so one physical router is never handed to two subscribers. The loser gets
// (nil, nil) — "the claim did not land" — rather than an error, matching
// ClaimApprovalRequest's convention so the handler can answer 409.
func (s *InventoryStore) IssueDevice(ctx context.Context, serial string, subscriberID int) (*inventory.Device, error) {
	const q = `
		WITH upd AS (
			UPDATE cpe_devices
			SET status = 'issued', subscriber_id = $2, issued_at = NOW()
			WHERE serial_number = $1 AND status = 'in_stock'
			RETURNING *
		)
		SELECT ` + deviceColumns + `
		FROM upd d JOIN cpe_device_types t ON t.id = d.device_type_id`

	d, err := scanDevice(s.pool.QueryRow(ctx, q, serial, subscriberID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: issue device %q: %w", serial, err)
	}
	return d, nil
}

// ReturnDevice takes a device back from a subscriber. newStatus is normally
// 'returned' (pending inspection) or 'faulty'; subscriber_id is cleared
// either way, which chk_cpe_issued_has_subscriber requires for any status
// other than 'issued'.
func (s *InventoryStore) ReturnDevice(ctx context.Context, serial, newStatus string) (*inventory.Device, error) {
	const q = `
		WITH upd AS (
			UPDATE cpe_devices
			SET status = $2, subscriber_id = NULL, issued_at = NULL
			WHERE serial_number = $1 AND status = 'issued'
			RETURNING *
		)
		SELECT ` + deviceColumns + `
		FROM upd d JOIN cpe_device_types t ON t.id = d.device_type_id`

	d, err := scanDevice(s.pool.QueryRow(ctx, q, serial, newStatus))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: return device %q: %w", serial, err)
	}
	return d, nil
}

// ── Stock levels & purchases (FR-INV-003) ────────────────────────────────────

// GetStockLevels reports availability per device type, flagging those at or
// below their reorder threshold.
//
// One grouped query with FILTER rather than a join per status: counting
// three statuses through three joined subqueries would multiply rows the
// same way the franchise P&L query had to avoid (MDS §4.10).
func (s *InventoryStore) GetStockLevels(ctx context.Context, lowOnly bool) ([]inventory.StockLevel, error) {
	q := fmt.Sprintf(`
		SELECT t.id, t.name, %s, t.vendor_id, t.reorder_threshold,
		       COUNT(d.id) FILTER (WHERE d.status = 'in_stock') AS in_stock,
		       COUNT(d.id) FILTER (WHERE d.status = 'issued')   AS issued,
		       COUNT(d.id) FILTER (WHERE d.status = 'faulty')   AS faulty
		FROM cpe_device_types t
		LEFT JOIN vendors v ON v.id = t.vendor_id
		LEFT JOIN cpe_devices d ON d.device_type_id = t.id
		GROUP BY t.id, t.name, v.name, t.vendor, t.vendor_id, t.reorder_threshold
		ORDER BY t.name`, fmt.Sprintf(vendorName, "t"))

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: stock levels: %w", err)
	}
	defer rows.Close()

	var out []inventory.StockLevel
	for rows.Next() {
		var l inventory.StockLevel
		if err := rows.Scan(&l.DeviceTypeID, &l.DeviceType, &l.Vendor, &l.VendorID,
			&l.ReorderThreshold, &l.InStock, &l.Issued, &l.Faulty); err != nil {
			return nil, fmt.Errorf("db: scan stock level: %w", err)
		}
		l.IsLow = l.InStock <= l.ReorderThreshold
		if lowOnly && !l.IsLow {
			continue
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate stock levels: %w", err)
	}
	return out, nil
}

// RecordPurchase persists a vendor purchase record. The vendor comes from the
// master in the same statement as the insert — see CreateDeviceType on why
// that is one statement and not two.
func (s *InventoryStore) RecordPurchase(ctx context.Context, p inventory.Purchase) (*inventory.Purchase, error) {
	if p.VendorID == nil || *p.VendorID <= 0 {
		return nil, vendors.ErrRequired
	}
	const q = `
		INSERT INTO cpe_purchases (device_type_id, vendor, vendor_id, quantity, unit_cost, invoice_ref, purchased_by_username)
		SELECT $1, v.name, v.id, $3, $4::numeric, NULLIF($5,''), $6
		  FROM vendors v
		 WHERE v.id = $2 AND v.active
		RETURNING id, device_type_id, vendor, vendor_id, quantity, unit_cost::text,
		          COALESCE(invoice_ref, ''), purchased_by_username, purchased_at`

	var (
		out      inventory.Purchase
		unitCost string
	)
	err := s.pool.QueryRow(ctx, q,
		p.DeviceTypeID, *p.VendorID, p.Quantity, p.UnitCost.String(), p.InvoiceRef, p.PurchasedBy,
	).Scan(&out.ID, &out.DeviceTypeID, &out.Vendor, &out.VendorID, &out.Quantity, &unitCost,
		&out.InvoiceRef, &out.PurchasedBy, &out.PurchasedAt)
	if isNoRows(err) {
		return nil, vendorWriteFailure(ctx, s.pool, *p.VendorID)
	}
	if err != nil {
		return nil, fmt.Errorf("db: record purchase: %w", err)
	}
	if out.UnitCost, err = parseDecimal(unitCost); err != nil {
		return nil, err
	}
	out.UnitCostStr = out.UnitCost.StringFixed(2)
	out.TotalCostStr = out.UnitCost.Mul(decimalFromInt(out.Quantity)).StringFixed(2)
	return &out, nil
}

// ListPurchases returns purchase history, newest first, optionally for one
// device type.
func (s *InventoryStore) ListPurchases(ctx context.Context, deviceTypeID *int) ([]inventory.Purchase, error) {
	q := fmt.Sprintf(`
		SELECT p.id, p.device_type_id, %s, p.vendor_id, p.quantity, p.unit_cost::text,
		       COALESCE(p.invoice_ref, ''), p.purchased_by_username, p.purchased_at
		FROM cpe_purchases p
		LEFT JOIN vendors v ON v.id = p.vendor_id
		WHERE ($1::int IS NULL OR p.device_type_id = $1)
		ORDER BY p.purchased_at DESC`, fmt.Sprintf(vendorName, "p"))

	rows, err := s.pool.Query(ctx, q, deviceTypeID)
	if err != nil {
		return nil, fmt.Errorf("db: list purchases: %w", err)
	}
	defer rows.Close()

	var out []inventory.Purchase
	for rows.Next() {
		var (
			p        inventory.Purchase
			unitCost string
		)
		if err := rows.Scan(&p.ID, &p.DeviceTypeID, &p.Vendor, &p.VendorID, &p.Quantity, &unitCost,
			&p.InvoiceRef, &p.PurchasedBy, &p.PurchasedAt); err != nil {
			return nil, fmt.Errorf("db: scan purchase: %w", err)
		}
		if p.UnitCost, err = parseDecimal(unitCost); err != nil {
			return nil, err
		}
		p.UnitCostStr = p.UnitCost.StringFixed(2)
		p.TotalCostStr = p.UnitCost.Mul(decimalFromInt(p.Quantity)).StringFixed(2)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate purchases: %w", err)
	}
	return out, nil
}
