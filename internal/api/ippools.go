package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// IP pools and static addresses — FR-NET-005 | migration 060. See
// internal/nas/ippool.go for the domain rules.

// IPPoolStore manages pools, their exclusions and static assignments.
// Satisfied by *db.NASStore.
type IPPoolStore interface {
	CreatePool(ctx context.Context, in nas.Pool, by string) (*nas.Pool, error)
	GetPool(ctx context.Context, id int) (*nas.Pool, error)
	ListPools(ctx context.Context, f nas.PoolFilter) ([]nas.Pool, error)
	DeletePool(ctx context.Context, id int) error
	AddExclusion(ctx context.Context, poolID int, r nas.AddrRange, reason, by string) (*nas.PoolExclusion, error)
	RemoveExclusion(ctx context.Context, id int) error
	AllocateStaticIP(ctx context.Context, req nas.AllocateRequest, by string) (*nas.StaticAssignment, error)
	// ReleaseStaticIP returns the assignment it released, so the caller
	// knows whose session still holds the address.
	ReleaseStaticIP(ctx context.Context, id int, reason, by string) (*nas.StaticAssignment, error)
	GetSubscriberStaticIP(ctx context.Context, subscriberID int) (*nas.StaticAssignment, error)
	ListSubscriberStaticIPs(ctx context.Context, subscriberID int) ([]nas.StaticAssignment, error)
	ListPoolAssignments(ctx context.Context, poolID int) ([]nas.StaticAssignment, error)
}

// ErrIPPoolsUnwired is returned when no pool store is configured.
var ErrIPPoolsUnwired = errors.New("api: IP pools not configured")

// ── Service layer ───────────────────────────────────────────────────────────

// CreatePool adds an IP pool.
func (h *Handler) CreatePool(ctx context.Context, in nas.Pool) (*nas.Pool, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	p, err := h.ipPools.CreatePool(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "ip_pool.created", p.Name, map[string]any{
		"pool_id": p.ID, "purpose": p.Purpose, "range": p.Range.String(), "nas_id": p.NASID,
	})
	return p, nil
}

// DeletePool removes a pool no address has ever been assigned from.
func (h *Handler) DeletePool(ctx context.Context, id int) error {
	if h.ipPools == nil {
		return ErrIPPoolsUnwired
	}
	if err := h.ipPools.DeletePool(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "ip_pool.deleted", strconv.Itoa(id), nil)
	return nil
}

// GetPool loads one pool with its exclusions.
func (h *Handler) GetPool(ctx context.Context, id int) (*nas.Pool, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	return h.ipPools.GetPool(ctx, id)
}

// ListPools returns the pools.
func (h *Handler) ListPools(ctx context.Context, f nas.PoolFilter) ([]nas.Pool, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	return h.ipPools.ListPools(ctx, f)
}

// AddExclusion excludes a range inside a pool.
func (h *Handler) AddExclusion(ctx context.Context, poolID int, r nas.AddrRange, reason string) (*nas.PoolExclusion, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	e, err := h.ipPools.AddExclusion(ctx, poolID, r, reason, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "ip_pool.exclusion_added", strconv.Itoa(poolID), map[string]any{
		"range": e.Range.String(), "reason": e.Reason,
	})
	return e, nil
}

// RemoveExclusion frees an excluded range again.
func (h *Handler) RemoveExclusion(ctx context.Context, id int) error {
	if h.ipPools == nil {
		return ErrIPPoolsUnwired
	}
	if err := h.ipPools.RemoveExclusion(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "ip_pool.exclusion_removed", strconv.Itoa(id), nil)
	return nil
}

// AllocateStaticIP gives a subscriber a static address, optionally billed
// through a catalogue product (FR-BIL-022).
func (h *Handler) AllocateStaticIP(ctx context.Context, req nas.AllocateRequest) (*nas.StaticAssignment, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	a, err := h.ipPools.AllocateStaticIP(ctx, req, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "static_ip.allocated", a.Address.String(), map[string]any{
		"subscriber_id": a.SubscriberID, "pool_id": a.PoolID, "billed": a.Billed(),
	})
	h.reconnectForNewAddress(ctx, a)
	return a, nil
}

// ReleaseStaticIP takes an address back and ends its charge.
func (h *Handler) ReleaseStaticIP(ctx context.Context, id int, reason string) error {
	if h.ipPools == nil {
		return ErrIPPoolsUnwired
	}
	released, err := h.ipPools.ReleaseStaticIP(ctx, id, reason, middleware.SubjectFromContext(ctx))
	if err != nil {
		return err
	}
	address := ""
	if released != nil {
		address = released.Address.String()
	}
	middleware.Audit(ctx, "static_ip.released", address, map[string]any{
		"assignment_id": id, "reason": strings.TrimSpace(reason),
	})
	h.reconnectForNewAddress(ctx, released)
	return nil
}

// reconnectForNewAddress ends the live session of a subscriber whose static
// address has just been given or taken away (FR-NET-005), so the redial
// picks up the change.
//
// A session's address is fixed when it starts: the NAS applies the Accept's
// Framed-IP-Address then and does not ask again. Until 2026-11-28 nothing
// ended the session, and the console said the subscriber would get the
// address "on their next authentication", which on always-on PPPoE can be
// weeks away:
//
//   - an allocation billed the subscriber for an address they did not have;
//   - a release worse: the address went straight back into the pool as free
//     while its former holder was still using it on the NAS, so the next
//     allocation could hand it to somebody else, and two sessions would
//     claim one address.
//
// The username rides on the disconnect, so radiusd clears its own auth
// cache before the redial; the invalidation here reaches only this process.
func (h *Handler) reconnectForNewAddress(ctx context.Context, a *nas.StaticAssignment) {
	if a == nil {
		return
	}
	h.invalidateSubscriberCache(ctx, a.Username)
	enqueuePoDIfSessionActive(ctx, h, a.SubscriberID, a.Username)
}

// invalidateSubscriberCache drops a cached auth record, logging a failure
// rather than failing the action that prompted it.
func (h *Handler) invalidateSubscriberCache(ctx context.Context, username string) {
	if h.subCache == nil || username == "" {
		return
	}
	if err := h.subCache.InvalidateSubscriber(ctx, username); err != nil {
		middleware.Audit(ctx, "subscriber.cache_invalidation_failed", username, nil)
	}
}

// GetSubscriberStaticIP returns the address a subscriber holds, or nil.
func (h *Handler) GetSubscriberStaticIP(ctx context.Context, subscriberID int) (*nas.StaticAssignment, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	return h.ipPools.GetSubscriberStaticIP(ctx, subscriberID)
}

// ListSubscriberStaticIPs returns a subscriber's addresses, past included.
func (h *Handler) ListSubscriberStaticIPs(ctx context.Context, subscriberID int) ([]nas.StaticAssignment, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	return h.ipPools.ListSubscriberStaticIPs(ctx, subscriberID)
}

// ListPoolAssignments returns who holds addresses from a pool.
func (h *Handler) ListPoolAssignments(ctx context.Context, poolID int) ([]nas.StaticAssignment, error) {
	if h.ipPools == nil {
		return nil, ErrIPPoolsUnwired
	}
	return h.ipPools.ListPoolAssignments(ctx, poolID)
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type poolJSON struct {
	ID             int             `json:"id"`
	NASID          *int            `json:"nas_id,omitempty"`
	NASIP          string          `json:"nas_ip,omitempty"`
	NASDescription string          `json:"nas_description,omitempty"`
	Name           string          `json:"name"`
	Purpose        string          `json:"purpose"`
	FirstAddress   string          `json:"first_address"`
	LastAddress    string          `json:"last_address"`
	Size           string          `json:"size"`
	Assigned       int             `json:"assigned"`
	Description    string          `json:"description,omitempty"`
	Exclusions     []exclusionJSON `json:"exclusions,omitempty"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      string          `json:"created_at"`
}

type exclusionJSON struct {
	ID           int    `json:"id"`
	FirstAddress string `json:"first_address"`
	LastAddress  string `json:"last_address"`
	Reason       string `json:"reason"`
}

func toPoolJSON(p nas.Pool) poolJSON {
	out := poolJSON{
		ID: p.ID, NASID: p.NASID, NASIP: p.NASIP, NASDescription: p.NASDescription,
		Name: p.Name, Purpose: p.Purpose,
		FirstAddress: p.Range.First.String(), LastAddress: p.Range.Last.String(),
		Size: p.SizeLabel(), Assigned: p.Assigned, Description: p.Description,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	for _, e := range p.Exclusions {
		out.Exclusions = append(out.Exclusions, exclusionJSON{
			ID: e.ID, FirstAddress: e.Range.First.String(),
			LastAddress: e.Range.Last.String(), Reason: e.Reason,
		})
	}
	return out
}

type assignmentJSON struct {
	ID                  int    `json:"id"`
	PoolID              int    `json:"pool_id"`
	PoolName            string `json:"pool_name"`
	SubscriberID        int    `json:"subscriber_id"`
	Username            string `json:"username"`
	Address             string `json:"address"`
	Status              string `json:"status"`
	SubscriberProductID *int   `json:"subscriber_product_id,omitempty"`
	AssignedBy          string `json:"assigned_by"`
	AssignedAt          string `json:"assigned_at"`
	ReleaseReason       string `json:"release_reason,omitempty"`
}

func toAssignmentJSON(a nas.StaticAssignment) assignmentJSON {
	return assignmentJSON{
		ID: a.ID, PoolID: a.PoolID, PoolName: a.PoolName,
		SubscriberID: a.SubscriberID, Username: a.Username,
		Address: a.Address.String(), Status: a.Status,
		SubscriberProductID: a.SubscriberProductID,
		AssignedBy:          a.AssignedBy,
		AssignedAt:          a.AssignedAt.Format("2006-01-02T15:04:05Z07:00"),
		ReleaseReason:       a.ReleaseReason,
	}
}

// writeIPPoolError maps a pool or assignment failure to a status and code.
func writeIPPoolError(w http.ResponseWriter, err error) {
	switch {
	case nas.PoolInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", nas.PoolInvalidReason(err))
	case errors.Is(err, nas.ErrPoolNameTaken):
		writeError(w, http.StatusConflict, "ERR_DUPLICATE", "a pool with that name already exists on this router")
	case errors.Is(err, nas.ErrPoolInUse):
		writeError(w, http.StatusConflict, "ERR_POOL_IN_USE",
			"addresses from this pool have been assigned; release them and keep the pool")
	case errors.Is(err, nas.ErrPoolNotStatic):
		writeError(w, http.StatusConflict, "ERR_POOL_NOT_STATIC",
			"addresses are only handed out from a static pool")
	case errors.Is(err, nas.ErrPoolExhausted):
		writeError(w, http.StatusConflict, "ERR_POOL_EXHAUSTED", "no free address left in this pool")
	case errors.Is(err, nas.ErrAddressTaken):
		writeError(w, http.StatusConflict, "ERR_ADDRESS_TAKEN", "that address is already assigned")
	case errors.Is(err, nas.ErrAddressExcluded):
		writeError(w, http.StatusConflict, "ERR_ADDRESS_EXCLUDED", "that address is excluded from the pool")
	case errors.Is(err, nas.ErrAddressOutsidePool):
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "that address is not in the pool")
	case errors.Is(err, nas.ErrSubscriberHasAddress):
		writeError(w, http.StatusConflict, "ERR_ALREADY_ASSIGNED",
			"this subscriber already holds a static address; release it first")
	case errors.Is(err, nas.ErrAssignmentNotActive):
		writeError(w, http.StatusConflict, "ERR_ALREADY_RELEASED", "that address has already been released")
	case errors.Is(err, nas.ErrPoolNotFound), errors.Is(err, nas.ErrAssignmentNotFound), isNotFound(err):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "not found")
	case errors.Is(err, ErrIPPoolsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "IP pools not configured")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "IP pool operation failed")
	}
}

type createPoolBody struct {
	NASID        *int   `json:"nas_id"`
	Name         string `json:"name"`
	Purpose      string `json:"purpose"`
	FirstAddress string `json:"first_address"`
	LastAddress  string `json:"last_address"`
	Description  string `json:"description"`
}

// CreatePoolHTTP handles POST /api/v1/ip-pools.
func (h *Handler) CreatePoolHTTP(w http.ResponseWriter, r *http.Request) {
	var b createPoolBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	rng, err := nas.ParseAddrRange(b.FirstAddress, b.LastAddress)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	p, err := h.CreatePool(r.Context(), nas.Pool{
		NASID: b.NASID, Name: b.Name, Purpose: b.Purpose,
		Range: rng, Description: b.Description,
	})
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPoolJSON(*p))
}

// ListPoolsHTTP handles GET /api/v1/ip-pools?nas_id=&purpose=.
func (h *Handler) ListPoolsHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := nas.PoolFilter{Purpose: q.Get("purpose"), Limit: parseLimit(q, 200, 500)}
	switch f.Purpose {
	case "", nas.PoolStatic, nas.PoolDHCP, nas.PoolCGNAT:
	default:
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "purpose must be static, dhcp or cgnat")
		return
	}
	if v := q.Get("nas_id"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "nas_id must be a positive number")
			return
		}
		f.NASID = n
	}
	list, err := h.ListPools(r.Context(), f)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	out := make([]poolJSON, 0, len(list))
	for _, p := range list {
		out = append(out, toPoolJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

// GetPoolHTTP handles GET /api/v1/ip-pools/{id}.
func (h *Handler) GetPoolHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	p, err := h.GetPool(r.Context(), id)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPoolJSON(*p))
}

// DeletePoolHTTP handles DELETE /api/v1/ip-pools/{id}.
func (h *Handler) DeletePoolHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if err := h.DeletePool(r.Context(), id); err != nil {
		writeIPPoolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListPoolAssignmentsHTTP handles GET /api/v1/ip-pools/{id}/assignments.
func (h *Handler) ListPoolAssignmentsHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	list, err := h.ListPoolAssignments(r.Context(), id)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": toAssignmentJSONs(list)})
}

func toAssignmentJSONs(list []nas.StaticAssignment) []assignmentJSON {
	out := make([]assignmentJSON, 0, len(list))
	for _, a := range list {
		out = append(out, toAssignmentJSON(a))
	}
	return out
}

type exclusionBody struct {
	FirstAddress string `json:"first_address"`
	LastAddress  string `json:"last_address"`
	Reason       string `json:"reason"`
}

// AddExclusionHTTP handles POST /api/v1/ip-pools/{id}/exclusions.
func (h *Handler) AddExclusionHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b exclusionBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	rng, err := nas.ParseAddrRange(b.FirstAddress, b.LastAddress)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	e, err := h.AddExclusion(r.Context(), id, rng, b.Reason)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, exclusionJSON{
		ID: e.ID, FirstAddress: e.Range.First.String(),
		LastAddress: e.Range.Last.String(), Reason: e.Reason,
	})
}

// RemoveExclusionHTTP handles DELETE /api/v1/ip-pool-exclusions/{id}.
func (h *Handler) RemoveExclusionHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if err := h.RemoveExclusion(r.Context(), id); err != nil {
		writeIPPoolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type allocateBody struct {
	PoolID    int    `json:"pool_id"`
	Address   string `json:"address"`
	ProductID int    `json:"product_id"`
}

// AllocateStaticIPHTTP handles POST /api/v1/subscribers/{id}/static-ip.
func (h *Handler) AllocateStaticIPHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b allocateBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	req := nas.AllocateRequest{PoolID: b.PoolID, SubscriberID: id, ProductID: b.ProductID}
	// An omitted address means "the next free one", which is the ordinary
	// case; only a supplied one is parsed.
	if s := strings.TrimSpace(b.Address); s != "" {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				"address must be an IP address, for example \"203.0.113.4\"")
			return
		}
		req.Address = addr.Unmap()
	}
	a, err := h.AllocateStaticIP(r.Context(), req)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAssignmentJSON(*a))
}

// ListSubscriberStaticIPsHTTP handles
// GET /api/v1/subscribers/{id}/static-ip.
func (h *Handler) ListSubscriberStaticIPsHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	list, err := h.ListSubscriberStaticIPs(r.Context(), id)
	if err != nil {
		writeIPPoolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": toAssignmentJSONs(list)})
}

type releaseBody struct {
	Reason string `json:"reason"`
}

// ReleaseStaticIPHTTP handles POST /api/v1/static-ips/{id}/release.
func (h *Handler) ReleaseStaticIPHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b releaseBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := h.ReleaseStaticIP(r.Context(), id, b.Reason); err != nil {
		writeIPPoolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
