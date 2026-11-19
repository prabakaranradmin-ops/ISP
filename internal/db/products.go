package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Product catalogue, subscriber attachments and invoice lines — FR-BIL-022
// | migration 058. See internal/billing/product.go for the domain rules.

// uniqueViolation reports whether err is a Postgres unique-constraint
// violation naming the given constraint (any, if none is named).
func uniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// Two spellings of the same column list: one qualified for queries that
// join products as `p`, one bare for RETURNING clauses, which cannot carry
// a table alias.
// foreignKeyViolation reports whether err is a Postgres foreign-key
// violation naming the given constraint.
func foreignKeyViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

const (
	productColumns = `
	p.id, p.code, p.name, p.description, p.kind, p.price::text, p.hsn_sac,
	p.active, p.created_by, p.created_at, COALESCE(p.updated_by, ''), p.updated_at`

	productColumnsBare = `
	id, code, name, description, kind, price::text, hsn_sac,
	active, created_by, created_at, COALESCE(updated_by, ''), updated_at`
)

func scanProduct(row pgx.Row, p *billing.Product, attached *int) error {
	var price string
	dest := []any{&p.ID, &p.Code, &p.Name, &p.Description, &p.Kind, &price, &p.HSNSAC,
		&p.Active, &p.CreatedBy, &p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt}
	if attached != nil {
		dest = append(dest, attached)
	}
	if err := row.Scan(dest...); err != nil {
		return err
	}
	var err error
	p.Price, err = parseDecimal(price)
	return err
}

// CreateProduct adds a catalogue entry.
func (s *BillingStore) CreateProduct(ctx context.Context, in billing.Product, by string) (*billing.Product, error) {
	in, err := in.Normalise()
	if err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO products (code, name, description, kind, price, hsn_sac, active, created_by)
		VALUES ($1, $2, $3, $4, $5::numeric, $6, TRUE, $7)
		RETURNING ` + productColumnsBare

	var out billing.Product
	row := s.pool.QueryRow(ctx, q,
		in.Code, in.Name, in.Description, in.Kind, in.Price.String(), in.HSNSAC, by)
	if err := scanProduct(row, &out, nil); err != nil {
		if uniqueViolation(err, "products_code_key") {
			return nil, fmt.Errorf("%w: %s", billing.ErrProductCodeTaken, in.Code)
		}
		return nil, fmt.Errorf("db: create product %s: %w", in.Code, err)
	}
	return &out, nil
}

// UpdateProduct changes a catalogue entry's name, description, price or
// HSN/SAC code.
//
// The kind is not editable: a recurring product that became one-time, or
// the reverse, would change what every existing attachment means — the
// right move is a new product and ending the old attachments. The code is
// not editable either, since it is what operators and imports refer to.
//
// Changing the price never reprices existing subscribers: each attachment
// snapshotted its own unit_price when it was created (migration 058).
func (s *BillingStore) UpdateProduct(ctx context.Context, in billing.Product, by string) (*billing.Product, error) {
	in, err := in.Normalise()
	if err != nil {
		return nil, err
	}
	const q = `
		UPDATE products
		   SET name = $2, description = $3, price = $4::numeric, hsn_sac = $5,
		       updated_by = $6, updated_at = now()
		 WHERE id = $1
		RETURNING ` + productColumnsBare

	var out billing.Product
	row := s.pool.QueryRow(ctx, q,
		in.ID, in.Name, in.Description, in.Price.String(), in.HSNSAC, by)
	if err := scanProduct(row, &out, nil); err != nil {
		if isNoRows(err) {
			return nil, fmt.Errorf("db: product %d: %w", in.ID, billing.ErrProductNotFound)
		}
		return nil, fmt.Errorf("db: update product %d: %w", in.ID, err)
	}
	return &out, nil
}

// SetProductActive deactivates or reinstates a catalogue entry.
//
// Deactivating hides it from the attach form; it does not stop billing the
// subscribers who already hold it, exactly as deactivating a plan leaves
// existing subscribers on it (FR-CAT-001). Stopping a subscriber's charge
// is EndAttachment's job, one subscriber at a time, with a reason.
func (s *BillingStore) SetProductActive(ctx context.Context, id int, active bool, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE products SET active = $2, updated_by = $3, updated_at = now()
		 WHERE id = $1`, id, active, by)
	if err != nil {
		return fmt.Errorf("db: set product %d active=%v: %w", id, active, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: product %d: %w", id, billing.ErrProductNotFound)
	}
	return nil
}

// GetProduct loads one catalogue entry, with how many subscribers hold it.
func (s *BillingStore) GetProduct(ctx context.Context, id int) (*billing.Product, error) {
	const q = `
		SELECT ` + productColumns + `,
		       (SELECT COUNT(*) FROM subscriber_products sp
		         WHERE sp.product_id = p.id AND sp.status = 'active')
		  FROM products p WHERE p.id = $1`

	var out billing.Product
	if err := scanProduct(s.pool.QueryRow(ctx, q, id), &out, &out.Attached); err != nil {
		if isNoRows(err) {
			return nil, fmt.Errorf("db: product %d: %w", id, billing.ErrProductNotFound)
		}
		return nil, fmt.Errorf("db: get product %d: %w", id, err)
	}
	return &out, nil
}

// ListProducts returns the catalogue, with the number of subscribers
// currently holding each entry.
func (s *BillingStore) ListProducts(ctx context.Context, f billing.ProductFilter) ([]billing.Product, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	const q = `
		SELECT ` + productColumns + `,
		       (SELECT COUNT(*) FROM subscriber_products sp
		         WHERE sp.product_id = p.id AND sp.status = 'active')
		  FROM products p
		 WHERE ($1 = '' OR p.kind = $1)
		   AND (NOT $2::boolean OR p.active)
		 ORDER BY p.active DESC, p.kind, p.code
		 LIMIT $3`

	rows, err := s.pool.Query(ctx, q, f.Kind, f.ActiveOnly, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list products: %w", err)
	}
	defer rows.Close()

	out := make([]billing.Product, 0, 32)
	for rows.Next() {
		var p billing.Product
		if err := scanProduct(rows, &p, &p.Attached); err != nil {
			return nil, fmt.Errorf("db: scan product: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate products: %w", err)
	}
	return out, nil
}

// DeleteProduct removes a catalogue entry that was never used.
//
// Refused the moment any attachment references it, current or historical:
// an invoice line names its product, and deleting the product behind a
// line already filed on GSTR-1 would leave that document unexplainable.
// Deactivation is the ordinary way to retire a product.
func (s *BillingStore) DeleteProduct(ctx context.Context, id int) error {
	var used int
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM subscriber_products WHERE product_id = $1)
		     + (SELECT COUNT(*) FROM invoice_lines WHERE product_id = $1)`, id).Scan(&used); err != nil {
		return fmt.Errorf("db: check product %d usage: %w", id, err)
	}
	if used > 0 {
		return fmt.Errorf("%w: deactivate it instead", billing.ErrProductInUse)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: delete product %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: product %d: %w", id, billing.ErrProductNotFound)
	}
	return nil
}

// ── Attachments ─────────────────────────────────────────────────────────────

const attachmentColumns = `
	sp.id, sp.subscriber_id, sp.product_id, p.code, p.name, p.kind, p.hsn_sac,
	sp.quantity, sp.unit_price::text, sp.status, sp.attached_by, sp.attached_at,
	sp.ended_at, COALESCE(sp.ended_by, ''), COALESCE(sp.end_reason, ''),
	sp.invoice_id, sp.charged_at`

func scanAttachment(row pgx.Row, sp *billing.SubscriberProduct, username *string) error {
	var price string
	dest := []any{&sp.ID, &sp.SubscriberID, &sp.ProductID, &sp.ProductCode, &sp.ProductName,
		&sp.Kind, &sp.HSNSAC, &sp.Quantity, &price, &sp.Status, &sp.AttachedBy, &sp.AttachedAt,
		&sp.EndedAt, &sp.EndedBy, &sp.EndReason, &sp.InvoiceID, &sp.ChargedAt}
	if username != nil {
		dest = append(dest, username)
	}
	if err := row.Scan(dest...); err != nil {
		return err
	}
	var err error
	sp.UnitPrice, err = parseDecimal(price)
	return err
}

// AttachProduct sells a catalogue product to a subscriber.
//
// The price is snapshotted here — the override if staff gave one, the
// catalogue price otherwise — so a later catalogue change cannot reprice
// this subscriber. Deactivated products are refused: they stay billable
// for whoever already holds them, but nobody new may be put on one.
func (s *BillingStore) AttachProduct(ctx context.Context, req billing.AttachRequest, by string) (*billing.SubscriberProduct, error) {
	req, err := req.Normalise()
	if err != nil {
		return nil, err
	}

	var out billing.SubscriberProduct
	err = inTx(ctx, s.pool, func(tx pgx.Tx) error {
		sp, err := attachProductTx(ctx, tx, req, by)
		if err != nil {
			return err
		}
		out = *sp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// attachProductTx is AttachProduct's body, inside a caller's transaction.
//
// Shared rather than duplicated because allocating a static address
// attaches a product too (FR-NET-005 bills the address through
// FR-BIL-022), and a second copy of "look up the price, refuse a retired
// product, insert the attachment" would be a second place for those rules
// to drift from these.
func attachProductTx(ctx context.Context, tx pgx.Tx, req billing.AttachRequest, by string) (*billing.SubscriberProduct, error) {
	req, err := req.Normalise()
	if err != nil {
		return nil, err
	}
	var (
		priceTxt string
		active   bool
	)
	// FOR UPDATE: the price read here is the price written below, and
	// a concurrent price edit must not land between the two.
	err = tx.QueryRow(ctx, `
		SELECT price::text, active FROM products WHERE id = $1 FOR UPDATE`,
		req.ProductID).Scan(&priceTxt, &active)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: product %d: %w", req.ProductID, billing.ErrProductNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: attach product %d: load: %w", req.ProductID, err)
	}
	if !active {
		return nil, billing.ErrProductInactive
	}

	price := req.UnitPrice
	if !price.IsPositive() {
		if price, err = parseDecimal(priceTxt); err != nil {
			return nil, err
		}
	}

	var out billing.SubscriberProduct
	row := tx.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO subscriber_products
				(subscriber_id, product_id, quantity, unit_price, attached_by)
			VALUES ($1, $2, $3, $4::numeric, $5)
			RETURNING *
		)
		SELECT `+attachmentColumns+`
		  FROM ins sp JOIN products p ON p.id = sp.product_id`,
		req.SubscriberID, req.ProductID, req.Quantity, price.String(), by)
	if err := scanAttachment(row, &out, nil); err != nil {
		// No such subscriber: the FK rejects it. Reported as not-found
		// rather than as an internal failure, since an id that does
		// not exist is the caller's mistake and a 500 would send
		// someone looking at the database instead of the request.
		if foreignKeyViolation(err, "subscriber_products_subscriber_id_fkey") {
			return nil, fmt.Errorf("db: subscriber %d: %w", req.SubscriberID, ErrNotFound)
		}
		return nil, fmt.Errorf("db: attach product %d to subscriber %d: %w",
			req.ProductID, req.SubscriberID, err)
	}
	return &out, nil
}

// EndAttachment stops a recurring charge, with a reason.
//
// Only an active attachment can be ended. A one-time charge that has
// already been invoiced is closed by that invoice and refused here: there
// is nothing left to stop, and marking it ended would erase the link to
// the invoice that billed it.
func (s *BillingStore) EndAttachment(ctx context.Context, id int, reason, by string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: give a reason for ending the charge", billing.ErrProductInvalid)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscriber_products
		   SET status = 'ended', ended_at = now(), ended_by = $3, end_reason = $2
		 WHERE id = $1 AND status = 'active'`, id, reason, by)
	if err != nil {
		return fmt.Errorf("db: end attachment %d: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing changed: it either does not exist or is no longer active.
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM subscriber_products WHERE id = $1`, id).Scan(&status); err != nil {
		if isNoRows(err) {
			return fmt.Errorf("db: attachment %d: %w", id, billing.ErrAttachmentNotFound)
		}
		return fmt.Errorf("db: end attachment %d: %w", id, err)
	}
	return fmt.Errorf("%w: it is %s", billing.ErrAttachmentNotActive, status)
}

// ListSubscriberProducts returns one subscriber's charges. With history,
// ended and already-charged attachments are included too.
func (s *BillingStore) ListSubscriberProducts(ctx context.Context, subscriberID int, history bool) ([]billing.SubscriberProduct, error) {
	const q = `
		SELECT ` + attachmentColumns + `
		  FROM subscriber_products sp
		  JOIN products p ON p.id = sp.product_id
		 WHERE sp.subscriber_id = $1
		   AND ($2::boolean OR sp.status = 'active')
		 ORDER BY sp.status <> 'active', p.kind, p.code, sp.id`

	rows, err := s.pool.Query(ctx, q, subscriberID, history)
	if err != nil {
		return nil, fmt.Errorf("db: list products for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()
	return collectAttachments(rows, false)
}

// ListBillableProducts returns the charges to put on this subscriber's
// next invoice: every active attachment, recurring or not-yet-billed
// one-time.
//
// Ordered one_time before recurring (the kinds sort that way), then by
// code, so an installation charge prints above an ongoing rental and the
// document's line order is the same every time it is rendered.
func (s *BillingStore) ListBillableProducts(ctx context.Context, subscriberID int) ([]billing.SubscriberProduct, error) {
	const q = `
		SELECT ` + attachmentColumns + `
		  FROM subscriber_products sp
		  JOIN products p ON p.id = sp.product_id
		 WHERE sp.subscriber_id = $1 AND sp.status = 'active'
		 ORDER BY p.kind, p.code, sp.id`

	rows, err := s.pool.Query(ctx, q, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list billable products for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()
	return collectAttachments(rows, false)
}

// ListAttachmentsAcrossSubscribers returns attachments for one product,
// so an operator can see who holds it before deactivating it.
func (s *BillingStore) ListAttachmentsAcrossSubscribers(ctx context.Context, productID, limit int) ([]billing.SubscriberProduct, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	const q = `
		SELECT ` + attachmentColumns + `, sub.username
		  FROM subscriber_products sp
		  JOIN products p    ON p.id = sp.product_id
		  JOIN subscribers sub ON sub.id = sp.subscriber_id
		 WHERE sp.product_id = $1
		 ORDER BY sp.status <> 'active', sub.username, sp.id
		 LIMIT $2`

	rows, err := s.pool.Query(ctx, q, productID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list attachments for product %d: %w", productID, err)
	}
	defer rows.Close()
	return collectAttachments(rows, true)
}

func collectAttachments(rows pgx.Rows, withUsername bool) ([]billing.SubscriberProduct, error) {
	out := make([]billing.SubscriberProduct, 0, 16)
	for rows.Next() {
		var sp billing.SubscriberProduct
		var err error
		if withUsername {
			err = scanAttachment(rows, &sp, &sp.Username)
		} else {
			err = scanAttachment(rows, &sp, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("db: scan subscriber product: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate subscriber products: %w", err)
	}
	return out, nil
}

// ── Invoice lines ───────────────────────────────────────────────────────────

// CreateInvoiceWithLines writes an invoice and its lines, and closes every
// one-time charge the invoice billed, in one transaction.
//
// Atomicity is the point. A one-time charge marked charged without its
// invoice would be lost revenue; an invoice billing a one-time charge that
// stayed active would bill it again next cycle. Both are prevented by
// doing the three writes together, and by closing each attachment with
// `AND status = 'active'` and insisting exactly one row moved — so two
// concurrent renewals cannot both bill the same installation fee. The
// loser's transaction rolls back rather than double-charging.
//
// Migration 058's deferred trigger checks, at commit, that the header
// equals the sum of the lines.
func (s *BillingStore) CreateInvoiceWithLines(ctx context.Context, inv billing.Invoice, lines []billing.InvoiceLine) (int, error) {
	if len(lines) == 0 {
		return 0, billing.ErrNoInvoiceLines
	}

	var invoiceID int
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		// invoices.description is left NULL: it exists for a one-off charge
		// that has no lines to explain it (a converted proforma,
		// FR-BIL-018). An invoice with lines explains itself.
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoices (
				subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
				total_amount, gst_rate_id, gb_included, gb_used
			) VALUES ($1,$2::numeric,$3::numeric,$4::numeric,$5::numeric,$6::numeric,$7,$8,$9::numeric)
			RETURNING id`,
			inv.SubscriberID, inv.BaseAmount.String(), inv.CgstAmount.String(),
			inv.SgstAmount.String(), inv.IgstAmount.String(), inv.TotalAmount.String(),
			inv.GstRateID, inv.GbIncluded, inv.GbUsed.String(),
		).Scan(&invoiceID); err != nil {
			return fmt.Errorf("db: create invoice for subscriber %d: %w", inv.SubscriberID, err)
		}

		for _, l := range lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO invoice_lines (
					invoice_id, line_no, kind, product_id, description, hsn_sac,
					quantity, unit_price, base_amount, cgst_amount, sgst_amount,
					igst_amount, total_amount
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric,$10::numeric,$11::numeric,$12::numeric,$13::numeric)`,
				invoiceID, l.LineNo, l.Kind, l.ProductID, l.Description, l.HSNSAC,
				l.Quantity, l.UnitPrice.String(), l.BaseAmount.String(),
				l.CGSTAmount.String(), l.SGSTAmount.String(), l.IGSTAmount.String(),
				l.TotalAmount.String()); err != nil {
				return fmt.Errorf("db: invoice %d line %d: %w", invoiceID, l.LineNo, err)
			}

			if !l.OneTime || l.AttachmentID == nil {
				continue
			}
			tag, err := tx.Exec(ctx, `
				UPDATE subscriber_products
				   SET status = 'charged', invoice_id = $2, charged_at = now()
				 WHERE id = $1 AND status = 'active'`, *l.AttachmentID, invoiceID)
			if err != nil {
				return fmt.Errorf("db: close one-time charge %d: %w", *l.AttachmentID, err)
			}
			if tag.RowsAffected() != 1 {
				// Another renewal billed or staff ended it after this
				// invoice's lines were composed. Roll back rather than
				// bill it twice.
				return fmt.Errorf("db: one-time charge %d: %w", *l.AttachmentID, billing.ErrAttachmentNotActive)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return invoiceID, nil
}

// ListInvoiceLines returns one invoice's lines in printed order.
//
// Empty for every invoice raised before migration 058, which carried its
// whole value in the header. Callers render the header as a single line in
// that case — see billing.InvoiceData.Lines.
func (s *BillingStore) ListInvoiceLines(ctx context.Context, invoiceID int) ([]billing.InvoiceLine, error) {
	const q = `
		SELECT id, invoice_id, line_no, kind, product_id, description, hsn_sac,
		       quantity, unit_price::text, base_amount::text, cgst_amount::text,
		       sgst_amount::text, igst_amount::text, total_amount::text
		  FROM invoice_lines WHERE invoice_id = $1 ORDER BY line_no`

	rows, err := s.pool.Query(ctx, q, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("db: list lines for invoice %d: %w", invoiceID, err)
	}
	defer rows.Close()

	out := make([]billing.InvoiceLine, 0, 8)
	for rows.Next() {
		var (
			l                                   billing.InvoiceLine
			unit, base, cgst, sgst, igst, total string
		)
		if err := rows.Scan(&l.ID, &l.InvoiceID, &l.LineNo, &l.Kind, &l.ProductID,
			&l.Description, &l.HSNSAC, &l.Quantity, &unit, &base, &cgst, &sgst,
			&igst, &total); err != nil {
			return nil, fmt.Errorf("db: scan invoice line: %w", err)
		}
		var err error
		if l.UnitPrice, err = parseDecimal(unit); err != nil {
			return nil, err
		}
		if l.BaseAmount, err = parseDecimal(base); err != nil {
			return nil, err
		}
		if l.CGSTAmount, err = parseDecimal(cgst); err != nil {
			return nil, err
		}
		if l.SGSTAmount, err = parseDecimal(sgst); err != nil {
			return nil, err
		}
		if l.IGSTAmount, err = parseDecimal(igst); err != nil {
			return nil, err
		}
		if l.TotalAmount, err = parseDecimal(total); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate invoice lines: %w", err)
	}
	return out, nil
}
