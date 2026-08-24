package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/procurement"
)

// ProcurementStore serves purchase-order requests (CRD-EXP-007).
type ProcurementStore struct{ pool dbPool }

// Procurement exposes the purchase-order store.
func (d *DB) Procurement() *ProcurementStore { return &ProcurementStore{pool: d.pool} }

func scanPurchaseOrder(row interface{ Scan(dest ...any) error }) (*procurement.PurchaseOrder, error) {
	var po procurement.PurchaseOrder
	var decidedBy, decisionReason *string
	if err := row.Scan(&po.ID, &po.Description, &po.Vendor, &po.Category, &po.Amount,
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

const purchaseOrderColumns = `
	id, description, vendor, category, amount::text, status, requested_by,
	decided_by, decision_reason, created_at, decided_at, received_at`

// CreatePurchaseOrder files a new request, always in status 'requested' —
// there is no way to create one pre-approved, matching the schema's own
// default and chk_po_distinct_approver's assumption that a decision always
// comes after the request exists.
func (s *ProcurementStore) CreatePurchaseOrder(ctx context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error) {
	q := fmt.Sprintf(`
		INSERT INTO purchase_orders (description, vendor, category, amount, requested_by)
		VALUES ($1, $2, $3, $4::numeric, $5)
		RETURNING %s`, purchaseOrderColumns)

	row := s.pool.QueryRow(ctx, q, po.Description, po.Vendor, po.Category, po.Amount, po.RequestedBy)
	created, err := scanPurchaseOrder(row)
	if err != nil {
		return nil, fmt.Errorf("db: create purchase order: %w", err)
	}
	return created, nil
}

// ListPurchaseOrders returns requests, optionally narrowed by status, newest
// first.
func (s *ProcurementStore) ListPurchaseOrders(ctx context.Context, status *string) ([]procurement.PurchaseOrder, error) {
	q := fmt.Sprintf(`
		SELECT %s FROM purchase_orders
		WHERE ($1::text IS NULL OR status = $1::text)
		ORDER BY created_at DESC`, purchaseOrderColumns)

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
	q := fmt.Sprintf(`SELECT %s FROM purchase_orders WHERE id = $1`, purchaseOrderColumns)
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
		UPDATE purchase_orders SET
			status = $2, decided_by = $3, decision_reason = $4, decided_at = NOW()
		WHERE id = $1 AND status = '%s'
		RETURNING %s`, procurement.StatusRequested, purchaseOrderColumns)

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
func (s *ProcurementStore) UpdateFulfilment(ctx context.Context, id int, status string) (*procurement.PurchaseOrder, error) {
	receivedAtSet := ""
	if status == procurement.StatusReceived {
		receivedAtSet = ", received_at = NOW()"
	}
	q := fmt.Sprintf(`
		UPDATE purchase_orders SET status = $2%s
		WHERE id = $1 AND status IN ('%s', '%s')
		RETURNING %s`, receivedAtSet, procurement.StatusApproved, procurement.StatusRequested, purchaseOrderColumns)

	po, err := scanPurchaseOrder(s.pool.QueryRow(ctx, q, id, status))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update purchase order %d fulfilment: %w", id, err)
	}
	return po, nil
}
