package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/procurement"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// ProcurementStore serves purchase-order requests (CRD-EXP-007).
type ProcurementStore struct{ pool dbPool }

// Procurement exposes the purchase-order store.
func (d *DB) Procurement() *ProcurementStore { return &ProcurementStore{pool: d.pool} }

func scanPurchaseOrder(row interface{ Scan(dest ...any) error }) (*procurement.PurchaseOrder, error) {
	var po procurement.PurchaseOrder
	var decidedBy, decisionReason *string
	if err := row.Scan(&po.ID, &po.Description, &po.Vendor, &po.VendorID, &po.Category, &po.Amount,
		&po.Status, &po.RequestedBy, &decidedBy, &decisionReason,
		&po.CreatedAt, &po.DecidedAt, &po.ReceivedAt); err != nil {
		return nil, err
	}
	if decidedBy != nil {
		po.DecidedBy = *decidedBy
	}
	if decisionReason != nil {
		po.DecisionReason = *decisionReason
	}
	return &po, nil
}

// purchaseOrderColumns reads an order with its supplier resolved from the
// shared vendor master (FR-INV-004 | migration 071): `o` is the order and `v`
// the LEFT JOINed vendors row, so correcting a misspelled vendor name in the
// master corrects it on every order at once.
//
// COALESCE onto o.vendor for the rows migration 071's backfill could not
// link — an order whose vendor text was blank — so no order ever loses its
// supplier from the screen.
//
// Every write below wraps its INSERT or UPDATE in a CTE and selects through
// this list rather than using a bare RETURNING. RETURNING cannot see a joined
// table, so it would hand back the name stored on the row instead of the
// master's current one, and a rename would make the response to a decision
// disagree with the list the operator is looking at.
const purchaseOrderColumns = `
	o.id, o.description, COALESCE(v.name, o.vendor), o.vendor_id, o.category,
	o.amount::text, o.status, o.requested_by, o.decided_by, o.decision_reason,
	o.created_at, o.decided_at, o.received_at`

// purchaseOrderVendorJoin resolves the master for a relation aliased `o`.
const purchaseOrderVendorJoin = ` LEFT JOIN vendors v ON v.id = o.vendor_id`

// CreatePurchaseOrder files a new request, always in status 'requested' —
// there is no way to create one pre-approved, matching the schema's own
// default and chk_po_distinct_approver's assumption that a decision always
// comes after the request exists.
// The vendor is selected from the master in the same statement as the insert
// rather than checked beforehand, so an order cannot be filed against a
// vendor retired between the check and the write, and the name stored on the
// row is the master's own.
func (s *ProcurementStore) CreatePurchaseOrder(ctx context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error) {
	if po.VendorID <= 0 {
		return nil, vendors.ErrRequired
	}
	q := fmt.Sprintf(`
		WITH ins AS (
			INSERT INTO purchase_orders (description, vendor, vendor_id, category, amount, requested_by)
			SELECT $1, v.name, v.id, $3, $4::numeric, $5
			  FROM vendors v
			 WHERE v.id = $2 AND v.active
			RETURNING *
		)
		SELECT %s FROM ins o%s`, purchaseOrderColumns, purchaseOrderVendorJoin)

	row := s.pool.QueryRow(ctx, q, po.Description, po.VendorID, po.Category, po.Amount, po.RequestedBy)
	created, err := scanPurchaseOrder(row)
	if isNoRows(err) {
		return nil, vendorWriteFailure(ctx, s.pool, po.VendorID)
	}
	if err != nil {
		return nil, fmt.Errorf("db: create purchase order: %w", err)
	}
	return created, nil
}

// ListPurchaseOrders returns requests, optionally narrowed by status, newest
// first.
func (s *ProcurementStore) ListPurchaseOrders(ctx context.Context, status *string) ([]procurement.PurchaseOrder, error) {
	q := fmt.Sprintf(`
		SELECT %s FROM purchase_orders o%s
		WHERE ($1::text IS NULL OR o.status = $1::text)
		ORDER BY o.created_at DESC`, purchaseOrderColumns, purchaseOrderVendorJoin)

	rows, err := s.pool.Query(ctx, q, status)
	if err != nil {
		return nil, fmt.Errorf("db: list purchase orders: %w", err)
	}
	defer rows.Close()

	out := make([]procurement.PurchaseOrder, 0, 8)
	for rows.Next() {
		po, err := scanPurchaseOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan purchase order row: %w", err)
		}
		out = append(out, *po)
	}
	return out, rows.Err()
}

// GetPurchaseOrder returns one request, or nil when no such id exists.
func (s *ProcurementStore) GetPurchaseOrder(ctx context.Context, id int) (*procurement.PurchaseOrder, error) {
	q := fmt.Sprintf(`SELECT %s FROM purchase_orders o%s WHERE o.id = $1`,
		purchaseOrderColumns, purchaseOrderVendorJoin)
	po, err := scanPurchaseOrder(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get purchase order %d: %w", id, err)
	}
	return po, nil
}

// DecidePurchaseOrder approves or rejects a still-pending request atomically
// — the WHERE clause only matches a row still in 'requested', so a decision
// racing another one (or arriving after the request was already decided)
// cannot land twice. Returns nil when the claim did not land, the same
// "someone else already decided this, or it never existed" signal
// workflow.ClaimApprovalRequest uses for the same reason.
//
// approve controls the resulting status (approved/rejected); the
// distinct-approver rule is enforced by chk_po_distinct_approver, not
// re-checked here — a violation surfaces as a constraint error, which the
// caller maps to a clear message rather than this store re-deriving the
// same rule the schema already owns.
func (s *ProcurementStore) DecidePurchaseOrder(ctx context.Context, id int, approve bool, decidedBy, reason string) (*procurement.PurchaseOrder, error) {
	newStatus := procurement.StatusRejected
	if approve {
		newStatus = procurement.StatusApproved
	}
	q := fmt.Sprintf(`
		WITH upd AS (
			UPDATE purchase_orders SET
				status = $2, decided_by = $3, decision_reason = $4, decided_at = NOW()
			WHERE id = $1 AND status = '%s'
			RETURNING *
		)
		SELECT %s FROM upd o%s`,
		procurement.StatusRequested, purchaseOrderColumns, purchaseOrderVendorJoin)

	po, err := scanPurchaseOrder(s.pool.QueryRow(ctx, q, id, newStatus, decidedBy, reason))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: decide purchase order %d: %w", id, err)
	}
	return po, nil
}

// UpdateFulfilment moves an approved order through ordered/received, or
// cancels it. Only a still-approved (or, for cancellation, still-requested)
// order can transition — matching the domain's own lifecycle: an order that
// was rejected cannot later be marked received.
//
// A transition to received additionally posts the GL entry CRD-EXP-006
// Phase 2 promised ("accounts payable falls naturally out of Phase 2's
// procurement posting"): Dr Operating Expenses, Cr Accounts Payable, in the
// same transaction as the status change. Every category (hardware,
// services, other) expenses to the same account rather than capitalising
// hardware as a fixed asset — this codebase has no depreciation schedule or
// asset register anywhere, and adding one is its own design question, not
// a wire-up decision. actor is the acknowledging staff member's username,
// recorded as the GL entry's created_by.
func (s *ProcurementStore) UpdateFulfilment(ctx context.Context, id int, status, actor string) (*procurement.PurchaseOrder, error) {
	receivedAtSet := ""
	if status == procurement.StatusReceived {
		receivedAtSet = ", received_at = NOW()"
	}
	q := fmt.Sprintf(`
		WITH upd AS (
			UPDATE purchase_orders SET status = $2%s
			WHERE id = $1 AND status IN ('%s', '%s')
			RETURNING *
		)
		SELECT %s FROM upd o%s`, receivedAtSet,
		procurement.StatusApproved, procurement.StatusRequested,
		purchaseOrderColumns, purchaseOrderVendorJoin)

	var po *procurement.PurchaseOrder
	err := inTx(ctx, s.pool, func(dbTx pgx.Tx) error {
		var err error
		po, err = scanPurchaseOrder(dbTx.QueryRow(ctx, q, id, status))
		if isNoRows(err) {
			po = nil
			return nil
		}
		if err != nil {
			return fmt.Errorf("db: update purchase order %d fulfilment: %w", id, err)
		}
		if status != procurement.StatusReceived {
			return nil
		}

		var journalID int
		if err := dbTx.QueryRow(ctx, `
			INSERT INTO gl_journal_entries (description, source_type, source_id, created_by)
			VALUES ($1, 'purchase_order', $2, $3)
			RETURNING id`,
			fmt.Sprintf("Purchase order #%d received — %s (%s)", po.ID, po.Description, po.Vendor),
			po.ID, actor,
		).Scan(&journalID); err != nil {
			return fmt.Errorf("db: insert purchase order GL journal entry: %w", err)
		}

		const insertLine = `
			INSERT INTO gl_journal_lines (journal_entry_id, account_id, debit, credit)
			VALUES ($1, $2, $3::numeric, $4::numeric)`

		// Both codes predate this path (migration 043), so this is the
		// defensive case rather than the live hazard the wallet and
		// commission postings face — resolved the same way regardless, so a
		// chart of accounts edited by hand cannot turn into a not-null
		// violation nobody can trace. See glAccountID.
		expenseID, err := glAccountID(ctx, dbTx, "5100")
		if err != nil {
			return err
		}
		payableID, err := glAccountID(ctx, dbTx, "2000")
		if err != nil {
			return err
		}
		if _, err := dbTx.Exec(ctx, insertLine, journalID, expenseID, po.Amount, "0"); err != nil {
			return fmt.Errorf("db: insert purchase order expense line: %w", err)
		}
		if _, err := dbTx.Exec(ctx, insertLine, journalID, payableID, "0", po.Amount); err != nil {
			return fmt.Errorf("db: insert purchase order payable line: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return po, nil
}
