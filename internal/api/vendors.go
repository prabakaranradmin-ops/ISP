package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// The shared vendor master — FR-INV-004 | migration 071. The domain rules
// live in internal/vendors; storage in internal/db/vendors.go.
//
// Owner/billing_admin only, reads included — not the staffRead gate the CPE
// device list uses. A vendor row carries a contact person, a phone number and
// a GSTIN: that is commercial-relationship data, and a technician issuing a
// router has no reason to hold it.
//
// This file is split the way internal/api/bandwidth.go and groups.go are: the
// service methods below take a context and audit, the HTTP handlers under
// them only parse and write JSON, and internal/staffui calls the service
// methods rather than the store. One validation path and one audit trail for
// both surfaces — a GSTIN changed from the console has to leave the same
// record as one changed over the API, because the question asked afterwards
// ("who entered this registration number") does not care which screen it came
// from.

// VendorStore manages the master. Satisfied by *db.VendorStore.
//
// There is no DeleteVendor, by design: see internal/db/vendors.go.
type VendorStore interface {
	CreateVendor(ctx context.Context, v vendors.Vendor, by string) (*vendors.Vendor, error)
	UpdateVendor(ctx context.Context, id int, v vendors.Vendor, by string) (*vendors.Vendor, error)
	SetVendorActive(ctx context.Context, id int, active bool, by string) error
	GetVendor(ctx context.Context, id int) (*vendors.Vendor, error)
	ListVendors(ctx context.Context) ([]vendors.VendorWithUsage, error)
	ListActiveVendors(ctx context.Context) ([]vendors.Vendor, error)
	ResolveVendorByName(ctx context.Context, name string) (*vendors.Vendor, error)
}

// ErrVendorsUnwired is returned when no vendor master is configured.
var ErrVendorsUnwired = errors.New("api: the vendor master is not configured")

// ── Service methods ──────────────────────────────────────────────────────────

// AddVendor puts a supplier in the master.
//
// The GSTIN is audited as a flag rather than a value: what the trail needs is
// that a registration number was entered and by whom, and the number itself
// is on the row the id names. Writing it into the audit detail as well would
// copy a tax identifier into a second table that nothing cleans up.
func (h *Handler) AddVendor(ctx context.Context, in vendors.Vendor) (*vendors.Vendor, error) {
	if h.vendors == nil {
		return nil, ErrVendorsUnwired
	}
	v, err := h.vendors.CreateVendor(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "vendor.created", v.Name,
		map[string]any{"vendor_id": v.ID, "has_gstin": v.GSTIN != ""})
	return v, nil
}

// EditVendor replaces a vendor's details.
//
// Audited with the GSTIN's presence *and* whether it changed, because that is
// the edit somebody will come back asking about: a purchase invoice claimed
// against the wrong registration is a denied input tax credit, and "when did
// this GSTIN change, and who changed it" has to be answerable from the trail
// alone.
func (h *Handler) EditVendor(ctx context.Context, id int, in vendors.Vendor) (*vendors.Vendor, error) {
	if h.vendors == nil {
		return nil, ErrVendorsUnwired
	}
	// Read first, so the audit entry can say whether the registration number
	// moved. A failure here is reported rather than ignored: auditing an edit
	// as "GSTIN unchanged" when it is not known to be unchanged would make
	// the trail say something false.
	before, err := h.vendors.GetVendor(ctx, id)
	if err != nil {
		return nil, err
	}

	v, err := h.vendors.UpdateVendor(ctx, id, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "vendor.updated", v.Name, map[string]any{
		"vendor_id":     v.ID,
		"has_gstin":     v.GSTIN != "",
		"gstin_changed": before.GSTIN != v.GSTIN,
		"renamed_from":  renamedFrom(before.Name, v.Name),
	})
	return v, nil
}

// renamedFrom returns the old name only when it actually changed, so the
// audit detail carries it on a rename and stays quiet otherwise rather than
// repeating the new name back in every entry.
func renamedFrom(before, after string) string {
	if before == after {
		return ""
	}
	return before
}

// SetVendorRetired retires a supplier or brings one back.
//
// Retiring, never deleting: everything already bought from this vendor keeps
// pointing at it and stays readable; only new purchases are refused. See
// internal/db/vendors.go for why there is no delete at all.
func (h *Handler) SetVendorRetired(ctx context.Context, id int, retired bool) error {
	if h.vendors == nil {
		return ErrVendorsUnwired
	}
	if err := h.vendors.SetVendorActive(ctx, id, !retired, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	action := "vendor.restored"
	if retired {
		action = "vendor.retired"
	}
	middleware.Audit(ctx, action, strconv.Itoa(id), nil)
	return nil
}

// LookupVendor returns one vendor.
func (h *Handler) LookupVendor(ctx context.Context, id int) (*vendors.Vendor, error) {
	if h.vendors == nil {
		return nil, ErrVendorsUnwired
	}
	return h.vendors.GetVendor(ctx, id)
}

// VendorsWithUsage returns the master list with what points at each row.
func (h *Handler) VendorsWithUsage(ctx context.Context) ([]vendors.VendorWithUsage, error) {
	if h.vendors == nil {
		return nil, ErrVendorsUnwired
	}
	return h.vendors.ListVendors(ctx)
}

// PickableVendors returns the vendors a form may offer — the active ones.
func (h *Handler) PickableVendors(ctx context.Context) ([]vendors.Vendor, error) {
	if h.vendors == nil {
		return nil, ErrVendorsUnwired
	}
	return h.vendors.ListActiveVendors(ctx)
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

// writeVendorError maps the domain's errors onto status codes, and reports
// whether it wrote anything — so a nil error falls through to the happy path.
//
// Every validation failure carries the domain's own message rather than a
// generic one, because "a GSTIN is 15 characters in this shape" and "that
// GSTIN's check digit does not match" send the caller to different places.
//
// fallback is the 500 message for an error that is not the master's. A
// parameter rather than a constant because the purchase and purchase-order
// handlers call this too, where a database failure is not a "vendor
// operation" and saying so would send the reader to the wrong screen.
func writeVendorError(w http.ResponseWriter, err error, fallback string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrVendorsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", err.Error())
	case errors.Is(err, vendors.ErrNotFound):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no such vendor")
	case errors.Is(err, vendors.ErrNameTaken), errors.Is(err, vendors.ErrGSTINTaken):
		writeError(w, http.StatusConflict, "ERR_CONFLICT", err.Error())
	case errors.Is(err, vendors.ErrInactive),
		errors.Is(err, vendors.ErrRequired),
		errors.Is(err, vendors.ErrNameRequired),
		errors.Is(err, vendors.ErrNameTooLong),
		errors.Is(err, vendors.ErrContactTooLong),
		errors.Is(err, vendors.ErrPhoneTooLong),
		errors.Is(err, vendors.ErrEmailTooLong),
		errors.Is(err, vendors.ErrGSTINFormat),
		errors.Is(err, vendors.ErrGSTINChecksum):
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", fallback)
	}
	return true
}

type vendorRequest struct {
	Name          string `json:"name"`
	ContactPerson string `json:"contact_person"`
	Phone         string `json:"phone"`
	Email         string `json:"email"`
	GSTIN         string `json:"gstin"`
	Address       string `json:"address"`
	Notes         string `json:"notes"`
}

func (r vendorRequest) vendor() vendors.Vendor {
	return vendors.Vendor{
		Name: r.Name, ContactPerson: r.ContactPerson, Phone: r.Phone,
		Email: r.Email, GSTIN: r.GSTIN, Address: r.Address, Notes: r.Notes,
	}
}

// CreateVendor handles POST /api/v1/vendors.
func (h *Handler) CreateVendor(w http.ResponseWriter, r *http.Request) {
	var req vendorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	created, err := h.AddVendor(r.Context(), req.vendor())
	if writeVendorError(w, err, "create vendor failed") {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// UpdateVendor handles PATCH /api/v1/vendors/{id}.
//
// Every field is replaced, including the omitted ones — this is the edit
// form's shape, and a partial update would make it impossible to clear a
// contact who has left the supplier.
func (h *Handler) UpdateVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := vendorPathID(w, r)
	if !ok {
		return
	}
	var req vendorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	updated, err := h.EditVendor(r.Context(), id, req.vendor())
	if writeVendorError(w, err, "update vendor failed") {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type vendorActiveRequest struct {
	Active *bool `json:"active"`
}

// SetVendorActive handles POST /api/v1/vendors/{id}/active — retiring a
// vendor or bringing one back. There is no DELETE endpoint.
func (h *Handler) SetVendorActive(w http.ResponseWriter, r *http.Request) {
	id, ok := vendorPathID(w, r)
	if !ok {
		return
	}
	var req vendorActiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// A pointer rather than a bool, so an omitted field is told apart from
	// false: defaulting a missing "active" to false would retire a vendor on
	// an empty body.
	if req.Active == nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "active must be true or false")
		return
	}
	if writeVendorError(w, h.SetVendorRetired(r.Context(), id, !*req.Active), "retire vendor failed") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListVendors handles GET /api/v1/vendors?active=true.
//
// The unfiltered list carries usage counts and spend, which is what makes it
// useful for finding the duplicates migration 071's backfill could not merge.
// active=true returns the bare pickable list instead — the shape a form's
// dropdown wants, without three correlated counts per row it will not show.
func (h *Handler) ListVendors(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("active") == "true" {
		list, err := h.PickableVendors(r.Context())
		if writeVendorError(w, err, "list vendors failed") {
			return
		}
		if list == nil {
			list = []vendors.Vendor{}
		}
		writeJSON(w, http.StatusOK, list)
		return
	}

	list, err := h.VendorsWithUsage(r.Context())
	if writeVendorError(w, err, "list vendors failed") {
		return
	}
	if list == nil {
		list = []vendors.VendorWithUsage{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetVendor handles GET /api/v1/vendors/{id}.
func (h *Handler) GetVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := vendorPathID(w, r)
	if !ok {
		return
	}
	v, err := h.LookupVendor(r.Context(), id)
	if writeVendorError(w, err, "get vendor failed") {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func vendorPathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid vendor id")
		return 0, false
	}
	return id, true
}

// resolveVendorRef turns the vendor a purchase request names into a master id.
//
// vendor_id is the real field. A vendor *name* is still accepted, for clients
// written against the pre-FR-INV-004 API — but only as a lookup key: it is
// resolved against the master exactly as the unique index keys it
// (case-insensitively, with runs of whitespace collapsed), and a name that
// matches nothing is refused rather than created. That distinction is the
// whole requirement. Silently creating a vendor from an unrecognised name
// would reintroduce the duplication the master exists to end, through the one
// path nobody is watching.
//
// Returns 0 and writes the response when the reference cannot be resolved.
func (h *Handler) resolveVendorRef(w http.ResponseWriter, r *http.Request, vendorID int, name string) int {
	if vendorID > 0 {
		return vendorID
	}
	if name == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"vendor_id is required — choose one from GET /api/v1/vendors?active=true")
		return 0
	}
	if h.vendors == nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"vendor_id is required: a vendor name cannot be resolved because the vendor master is not configured")
		return 0
	}
	resolved, err := h.vendors.ResolveVendorByName(r.Context(), name)
	if errors.Is(err, vendors.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"no active vendor is named "+strconv.Quote(name)+
				" — add it to the vendor master first, or send vendor_id")
		return 0
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "resolve vendor failed")
		return 0
	}
	return resolved.ID
}
