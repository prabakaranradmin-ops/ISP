package db

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// IP pools and static addresses — FR-NET-005 | migration 060. See
// internal/nas/ippool.go for the domain rules.

// NASStore gains the pool methods; they belong with the router estate
// rather than with billing, even though an assignment can carry a charge.

const poolColumns = `
	p.id, p.nas_id, COALESCE(host(n.ip), ''), COALESCE(n.description, ''),
	p.name, p.purpose, host(p.first_address), host(p.last_address),
	p.description, p.created_by, p.created_at,
	COALESCE(p.updated_by, ''), p.updated_at`

const poolFrom = `
	FROM ip_pools p
	LEFT JOIN nas_devices n ON n.id = p.nas_id`

func scanPool(row pgx.Row, p *nas.Pool, assigned *int) error {
	var first, last string
	dest := []any{&p.ID, &p.NASID, &p.NASIP, &p.NASDescription,
		&p.Name, &p.Purpose, &first, &last,
		&p.Description, &p.CreatedBy, &p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt}
	if assigned != nil {
		dest = append(dest, assigned)
	}
	if err := row.Scan(dest...); err != nil {
		return err
	}
	r, err := nas.ParseAddrRange(first, last)
	if err != nil {
		return fmt.Errorf("db: pool %d has an unreadable range %q–%q: %w", p.ID, first, last, err)
	}
	p.Range = r
	return nil
}

// CreatePool adds an IP pool.
func (s *NASStore) CreatePool(ctx context.Context, in nas.Pool, by string) (*nas.Pool, error) {
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		WITH ins AS (
			INSERT INTO ip_pools (nas_id, name, purpose, first_address, last_address,
			                      description, created_by)
			VALUES ($1, $2, $3, $4::inet, $5::inet, $6, $7)
			RETURNING *
		)
		SELECT ` + poolColumns + `
		  FROM ins p
		  LEFT JOIN nas_devices n ON n.id = p.nas_id`

	var out nas.Pool
	row := s.pool.QueryRow(ctx, q, in.NASID, in.Name, in.Purpose,
		in.Range.First.String(), in.Range.Last.String(), in.Description, by)
	if err := scanPool(row, &out, nil); err != nil {
		switch {
		case uniqueViolation(err, "uq_ip_pool_name"):
			return nil, fmt.Errorf("%w: %s", nas.ErrPoolNameTaken, in.Name)
		case foreignKeyViolation(err, "ip_pools_nas_id_fkey"):
			return nil, fmt.Errorf("%w: no router with that id", nas.ErrPoolInvalid)
		}
		return nil, fmt.Errorf("db: create IP pool %q: %w", in.Name, err)
	}
	return &out, nil
}

// GetPool loads one pool with its exclusions and how many addresses from it
// are held.
func (s *NASStore) GetPool(ctx context.Context, id int) (*nas.Pool, error) {
	const q = `
		SELECT ` + poolColumns + `,
		       (SELECT COUNT(*) FROM static_ip_assignments a
		         WHERE a.pool_id = p.id AND a.status = 'active')` + poolFrom + `
		 WHERE p.id = $1`

	var out nas.Pool
	if err := scanPool(s.pool.QueryRow(ctx, q, id), &out, &out.Assigned); err != nil {
		if isNoRows(err) {
			return nil, fmt.Errorf("db: IP pool %d: %w", id, nas.ErrPoolNotFound)
		}
		return nil, fmt.Errorf("db: get IP pool %d: %w", id, err)
	}
	excl, err := s.ListExclusions(ctx, id)
	if err != nil {
		return nil, err
	}
	out.Exclusions = excl
	return &out, nil
}

// ListPools returns the pools, with their exclusions and usage.
//
// Exclusions come along because a pool's capacity is meaningless without
// them, and because the allocator needs them: fetching them per pool
// afterwards would be a query per row on a screen that exists to show all
// of them at once.
func (s *NASStore) ListPools(ctx context.Context, f nas.PoolFilter) ([]nas.Pool, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	const q = `
		SELECT ` + poolColumns + `,
		       (SELECT COUNT(*) FROM static_ip_assignments a
		         WHERE a.pool_id = p.id AND a.status = 'active')` + poolFrom + `
		 WHERE ($1 = 0 OR p.nas_id = $1)
		   AND ($2 = '' OR p.purpose = $2)
		 ORDER BY p.purpose, p.name
		 LIMIT $3`

	rows, err := s.pool.Query(ctx, q, f.NASID, f.Purpose, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list IP pools: %w", err)
	}
	pools := make([]nas.Pool, 0, 16)
	for rows.Next() {
		var p nas.Pool
		if err := scanPool(rows, &p, &p.Assigned); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: scan IP pool: %w", err)
		}
		pools = append(pools, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate IP pools: %w", err)
	}

	for i := range pools {
		excl, err := s.ListExclusions(ctx, pools[i].ID)
		if err != nil {
			return nil, err
		}
		pools[i].Exclusions = excl
	}
	return pools, nil
}

// DeletePool removes a pool nothing holds an address from.
func (s *NASStore) DeletePool(ctx context.Context, id int) error {
	var held int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM static_ip_assignments
		 WHERE pool_id = $1 AND status = 'active'`, id).Scan(&held); err != nil {
		return fmt.Errorf("db: check IP pool %d usage: %w", id, err)
	}
	if held > 0 {
		return fmt.Errorf("%w: release them first", nas.ErrPoolInUse)
	}
	// Released assignments reference the pool too (ON DELETE RESTRICT), so
	// a pool that has ever been used cannot be deleted at all. That is the
	// intended behaviour: the history of who held which address is worth
	// more than the ability to tidy up a list.
	tag, err := s.pool.Exec(ctx, `DELETE FROM ip_pools WHERE id = $1`, id)
	if err != nil {
		if foreignKeyViolation(err, "static_ip_assignments_pool_id_fkey") {
			return fmt.Errorf("%w: addresses from it have been assigned in the past", nas.ErrPoolInUse)
		}
		return fmt.Errorf("db: delete IP pool %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: IP pool %d: %w", id, nas.ErrPoolNotFound)
	}
	return nil
}

// ── Exclusions ──────────────────────────────────────────────────────────────

// ListExclusions returns a pool's excluded ranges, lowest first.
func (s *NASStore) ListExclusions(ctx context.Context, poolID int) ([]nas.PoolExclusion, error) {
	const q = `
		SELECT id, pool_id, host(first_address), host(last_address), reason, created_by, created_at
		  FROM ip_pool_exclusions WHERE pool_id = $1 ORDER BY first_address`

	rows, err := s.pool.Query(ctx, q, poolID)
	if err != nil {
		return nil, fmt.Errorf("db: list exclusions for pool %d: %w", poolID, err)
	}
	defer rows.Close()

	out := make([]nas.PoolExclusion, 0, 4)
	for rows.Next() {
		var (
			e           nas.PoolExclusion
			first, last string
		)
		if err := rows.Scan(&e.ID, &e.PoolID, &first, &last, &e.Reason, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan exclusion: %w", err)
		}
		r, err := nas.ParseAddrRange(first, last)
		if err != nil {
			return nil, fmt.Errorf("db: exclusion %d has an unreadable range: %w", e.ID, err)
		}
		e.Range = r
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate exclusions: %w", err)
	}
	return out, nil
}

// AddExclusion excludes a range of addresses inside a pool.
func (s *NASStore) AddExclusion(ctx context.Context, poolID int, r nas.AddrRange, reason, by string) (*nas.PoolExclusion, error) {
	pool, err := s.GetPool(ctx, poolID)
	if err != nil {
		return nil, err
	}
	e := nas.PoolExclusion{PoolID: poolID, Range: r, Reason: strings.TrimSpace(reason)}
	if err := e.Validate(*pool); err != nil {
		return nil, err
	}

	var out nas.PoolExclusion
	var first, last string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ip_pool_exclusions (pool_id, first_address, last_address, reason, created_by)
		VALUES ($1, $2::inet, $3::inet, $4, $5)
		RETURNING id, pool_id, host(first_address), host(last_address), reason, created_by, created_at`,
		poolID, r.First.String(), r.Last.String(), e.Reason, by,
	).Scan(&out.ID, &out.PoolID, &first, &last, &out.Reason, &out.CreatedBy, &out.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("db: add exclusion to pool %d: %w", poolID, err)
	}
	if out.Range, err = nas.ParseAddrRange(first, last); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveExclusion deletes an exclusion, freeing those addresses again.
func (s *NASStore) RemoveExclusion(ctx context.Context, id int) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ip_pool_exclusions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: remove exclusion %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: exclusion %d: %w", id, nas.ErrPoolNotFound)
	}
	return nil
}

// ── Static assignments ──────────────────────────────────────────────────────

const assignmentColumns = `
	a.id, a.pool_id, p.name, a.subscriber_id, s.username, host(a.address),
	a.status, a.subscriber_product_id, a.assigned_by, a.assigned_at,
	a.released_at, COALESCE(a.released_by, ''), COALESCE(a.release_reason, '')`

const assignmentFrom = `
	FROM static_ip_assignments a
	JOIN ip_pools p    ON p.id = a.pool_id
	JOIN subscribers s ON s.id = a.subscriber_id`

func scanAssignment(row pgx.Row, a *nas.StaticAssignment) error {
	var address string
	if err := row.Scan(&a.ID, &a.PoolID, &a.PoolName, &a.SubscriberID, &a.Username, &address,
		&a.Status, &a.SubscriberProductID, &a.AssignedBy, &a.AssignedAt,
		&a.ReleasedAt, &a.ReleasedBy, &a.ReleaseReason); err != nil {
		return err
	}
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return fmt.Errorf("db: assignment %d holds an unreadable address %q: %w", a.ID, address, err)
	}
	a.Address = addr.Unmap()
	return nil
}

// AllocateStaticIP gives a subscriber an address from a pool, optionally
// attaching a catalogue product to bill it.
//
// One transaction, with the pool row locked: choosing an address means
// reading what is already taken, and without the lock two operators
// allocating at the same moment would both pick the same free address.
// The partial unique index on (address) WHERE active would then reject one
// of them — correct, but as a constraint violation rather than as "here is
// the next free address", which is what the lock turns it into.
//
// The product attachment is what "billed through FR-BIL-022" means: a
// static IP is charged as an ordinary catalogue item on the subscriber's
// invoice, not through a billing path of its own.
func (s *NASStore) AllocateStaticIP(ctx context.Context, req nas.AllocateRequest, by string) (*nas.StaticAssignment, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	var out nas.StaticAssignment
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR UPDATE on the pool, not on the assignments: the pool row is
		// the thing every allocation from it contends on, and locking it
		// serialises them without locking rows that do not exist yet.
		var (
			purpose     string
			first, last string
		)
		err := tx.QueryRow(ctx, `
			SELECT purpose, host(first_address), host(last_address)
			  FROM ip_pools WHERE id = $1 FOR UPDATE`, req.PoolID).Scan(&purpose, &first, &last)
		if isNoRows(err) {
			return fmt.Errorf("db: IP pool %d: %w", req.PoolID, nas.ErrPoolNotFound)
		}
		if err != nil {
			return fmt.Errorf("db: allocate from pool %d: load: %w", req.PoolID, err)
		}
		pool := nas.Pool{ID: req.PoolID, Purpose: purpose}
		if pool.Range, err = nas.ParseAddrRange(first, last); err != nil {
			return err
		}
		if !pool.Static() {
			return fmt.Errorf("%w: %s is a %s pool", nas.ErrPoolNotStatic, first, purpose)
		}
		if pool.Exclusions, err = exclusionsTx(ctx, tx, req.PoolID); err != nil {
			return err
		}

		taken, err := takenAddressesTx(ctx, tx)
		if err != nil {
			return err
		}

		address := req.Address.Unmap()
		if address.IsValid() {
			if err := nas.CheckAddressAllocatable(pool, address, taken); err != nil {
				return err
			}
		} else if address, err = nas.NextFreeAddress(pool, taken); err != nil {
			return err
		}

		// The catalogue charge, attached first so a failure to bill does
		// not leave an address handed out for free. Priced from the
		// catalogue, like any other attachment.
		var productAttachment *int
		if req.ProductID > 0 {
			sp, err := attachProductTx(ctx, tx, billing.AttachRequest{
				SubscriberID: req.SubscriberID, ProductID: req.ProductID, Quantity: 1,
			}, by)
			if err != nil {
				return err
			}
			productAttachment = &sp.ID
		}

		row := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO static_ip_assignments
					(pool_id, subscriber_id, address, subscriber_product_id, assigned_by)
				VALUES ($1, $2, $3::inet, $4, $5)
				RETURNING *
			)
			SELECT `+assignmentColumns+`
			  FROM ins a
			  JOIN ip_pools p    ON p.id = a.pool_id
			  JOIN subscribers s ON s.id = a.subscriber_id`,
			req.PoolID, req.SubscriberID, address.String(), productAttachment, by)
		if err := scanAssignment(row, &out); err != nil {
			switch {
			case uniqueViolation(err, "uq_static_ip_subscriber"):
				return nas.ErrSubscriberHasAddress
			case uniqueViolation(err, "uq_static_ip_address"):
				// Lost a race the pool lock should have prevented, or an
				// address held from a different pool.
				return fmt.Errorf("%w: %s", nas.ErrAddressTaken, address)
			case foreignKeyViolation(err, "static_ip_assignments_subscriber_id_fkey"):
				return fmt.Errorf("db: subscriber %d: %w", req.SubscriberID, ErrNotFound)
			}
			return fmt.Errorf("db: allocate %s to subscriber %d: %w", address, req.SubscriberID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// exclusionsTx reads a pool's exclusions inside a transaction.
func exclusionsTx(ctx context.Context, tx pgx.Tx, poolID int) ([]nas.PoolExclusion, error) {
	rows, err := tx.Query(ctx, `
		SELECT host(first_address), host(last_address)
		  FROM ip_pool_exclusions WHERE pool_id = $1`, poolID)
	if err != nil {
		return nil, fmt.Errorf("db: read exclusions for pool %d: %w", poolID, err)
	}
	defer rows.Close()

	var out []nas.PoolExclusion
	for rows.Next() {
		var first, last string
		if err := rows.Scan(&first, &last); err != nil {
			return nil, fmt.Errorf("db: scan exclusion: %w", err)
		}
		r, err := nas.ParseAddrRange(first, last)
		if err != nil {
			return nil, err
		}
		out = append(out, nas.PoolExclusion{Range: r})
	}
	return out, rows.Err()
}

// takenAddressesTx is every address currently held, across all pools.
//
// Across all pools, not just this one: two pools can overlap by operator
// error, and handing out an address that another pool has already given to
// someone else is the exact collision this feature exists to prevent.
func takenAddressesTx(ctx context.Context, tx pgx.Tx) (map[netip.Addr]bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT host(address) FROM static_ip_assignments WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("db: read held addresses: %w", err)
	}
	defer rows.Close()

	taken := map[netip.Addr]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("db: scan held address: %w", err)
		}
		addr, err := netip.ParseAddr(a)
		if err != nil {
			// A row we cannot parse is still an address somebody holds;
			// skipping it could hand it out twice, so it is an error.
			return nil, fmt.Errorf("db: held address %q is unreadable: %w", a, err)
		}
		taken[addr.Unmap()] = true
	}
	return taken, rows.Err()
}

// ReleaseStaticIP takes an address back, ending its catalogue charge in the
// same transaction when it had one, and returns the released assignment.
//
// Ending the charge together with the address is the whole point of
// recording the link: an address released while its monthly rental went on
// being billed is the kind of error a customer finds before the operator
// does.
//
// The row comes back so the caller knows, without a second lookup that can
// fail, whose session still holds the address on the NAS. That session has
// to end before the address can safely go to anybody else.
func (s *NASStore) ReleaseStaticIP(ctx context.Context, id int, reason, by string) (*nas.StaticAssignment, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: give a reason for releasing the address", nas.ErrPoolInvalid)
	}
	var out nas.StaticAssignment
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var productID *int
		err := tx.QueryRow(ctx, `
			UPDATE static_ip_assignments
			   SET status = 'released', released_at = now(), released_by = $3, release_reason = $2
			 WHERE id = $1 AND status = 'active'
			RETURNING subscriber_product_id`, id, reason, by).Scan(&productID)
		if isNoRows(err) {
			var status string
			if err := tx.QueryRow(ctx,
				`SELECT status FROM static_ip_assignments WHERE id = $1`, id).Scan(&status); err != nil {
				if isNoRows(err) {
					return fmt.Errorf("db: assignment %d: %w", id, nas.ErrAssignmentNotFound)
				}
				return fmt.Errorf("db: release address %d: %w", id, err)
			}
			return fmt.Errorf("%w: it is %s", nas.ErrAssignmentNotActive, status)
		}
		if err != nil {
			return fmt.Errorf("db: release address %d: %w", id, err)
		}

		if productID != nil {
			// Only if it is still active: staff may have ended the charge
			// from the subscriber's Billing tab already, which is not an
			// error.
			if _, err := tx.Exec(ctx, `
				UPDATE subscriber_products
				   SET status = 'ended', ended_at = now(), ended_by = $2,
				       end_reason = 'static IP released: ' || $3
				 WHERE id = $1 AND status = 'active'`, *productID, by, reason); err != nil {
				return fmt.Errorf("db: end the charge for released address %d: %w", id, err)
			}
		}
		if err := scanAssignment(tx.QueryRow(ctx,
			`SELECT `+assignmentColumns+assignmentFrom+` WHERE a.id = $1`, id), &out); err != nil {
			return fmt.Errorf("db: read released address %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSubscriberStaticIP returns the address a subscriber holds, or
// (nil, nil) if they hold none.
func (s *NASStore) GetSubscriberStaticIP(ctx context.Context, subscriberID int) (*nas.StaticAssignment, error) {
	var out nas.StaticAssignment
	err := scanAssignment(s.pool.QueryRow(ctx,
		`SELECT `+assignmentColumns+assignmentFrom+`
		  WHERE a.subscriber_id = $1 AND a.status = 'active'`, subscriberID), &out)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get static address for subscriber %d: %w", subscriberID, err)
	}
	return &out, nil
}

// ListSubscriberStaticIPs returns one subscriber's addresses, current and
// past.
func (s *NASStore) ListSubscriberStaticIPs(ctx context.Context, subscriberID int) ([]nas.StaticAssignment, error) {
	return s.listAssignments(ctx,
		`SELECT `+assignmentColumns+assignmentFrom+`
		  WHERE a.subscriber_id = $1
		  ORDER BY a.status <> 'active', a.assigned_at DESC`, subscriberID)
}

// ListPoolAssignments returns who holds addresses from one pool.
func (s *NASStore) ListPoolAssignments(ctx context.Context, poolID int) ([]nas.StaticAssignment, error) {
	return s.listAssignments(ctx,
		`SELECT `+assignmentColumns+assignmentFrom+`
		  WHERE a.pool_id = $1
		  ORDER BY a.status <> 'active', a.address`, poolID)
}

func (s *NASStore) listAssignments(ctx context.Context, q string, args ...any) ([]nas.StaticAssignment, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list static addresses: %w", err)
	}
	defer rows.Close()

	out := make([]nas.StaticAssignment, 0, 8)
	for rows.Next() {
		var a nas.StaticAssignment
		if err := scanAssignment(rows, &a); err != nil {
			return nil, fmt.Errorf("db: scan static address: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate static addresses: %w", err)
	}
	return out, nil
}
