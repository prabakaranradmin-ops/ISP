package staffui

import (
	"context"
	"errors"
	"net/http"
	neturl "net/url"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// The shared vendor master — FR-INV-004.
//
// VendorManager is satisfied by *api.Handler, so the console validates and
// audits exactly as the API does rather than reaching the store directly —
// the same arrangement BandwidthManager and GroupManager use, and for the
// same reason here with more force: a GSTIN changed from this screen has to
// leave the same audit record as one changed over the API, because the
// question asked afterwards ("who entered this registration number") does not
// care which screen it came from.
type VendorManager interface {
	AddVendor(ctx context.Context, v vendors.Vendor) (*vendors.Vendor, error)
	EditVendor(ctx context.Context, id int, v vendors.Vendor) (*vendors.Vendor, error)
	SetVendorRetired(ctx context.Context, id int, retired bool) error
	LookupVendor(ctx context.Context, id int) (*vendors.Vendor, error)
	VendorsWithUsage(ctx context.Context) ([]vendors.VendorWithUsage, error)
	// PickableVendors backs the supplier dropdown on the Inventory and
	// Procurement screens, which is the half of this requirement an operator
	// actually notices: those forms had a text box before.
	PickableVendors(ctx context.Context) ([]vendors.Vendor, error)
}

// vendorMessages words each outcome carried back in the URL.
var vendorMessages = map[string]string{
	"created":  "Vendor added. It can now be chosen on a purchase or a purchase order.",
	"saved":    "Vendor saved. The new details show against every purchase already recorded against it.",
	"retired":  "Vendor retired. Existing purchases and orders are unchanged and still show it; it is no longer offered for new ones.",
	"restored": "Vendor restored and offered again.",

	"duplicate":  "Not saved: another active vendor already has that name.",
	"gstin_dupe": "Not saved: another active vendor is already registered under that GSTIN. A GSTIN is one legal entity, so one of the two is a duplicate — check which before adding this one.",
	"missing":    "No vendor with that id.",
	"failed":     "That did not go through. Nothing was changed; try again.",
}

var vendorSuccess = map[string]bool{
	"created": true, "saved": true, "retired": true, "restored": true,
}

type vendorData struct {
	Vendors []vendors.VendorWithUsage
	// Editing is the vendor whose details are loaded into the form, when the
	// operator asked to edit one. Nil for the add form.
	Editing *vendors.Vendor
}

// Vendors handles GET /staff/vendors.
func (h *Handler) Vendors(w http.ResponseWriter, r *http.Request) {
	s, ok := h.vendorGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Vendors", "vendors")
	q := r.URL.Query()

	list, err := h.vendors.VendorsWithUsage(r.Context())
	if err != nil {
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not read the vendor master.")
		return
	}

	data := vendorData{Vendors: list}
	// ?edit=<id> loads one vendor into the form. A bad or unknown id shows
	// the add form rather than an error page: the list is still useful, and
	// an operator who hand-edited the URL does not need a dead end.
	if raw := q.Get("edit"); raw != "" {
		if id := atoiOrZero(raw); id > 0 {
			if v, err := h.vendors.LookupVendor(r.Context(), id); err == nil {
				data.Editing = v
			}
		}
	}

	if code := q.Get("m"); code != "" {
		if m, ok := vendorMessages[code]; ok {
			if vendorSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	// A validation sentence names the field and the rule, so it is carried
	// verbatim rather than mapped to a code — "that GSTIN's check digit does
	// not match the rest of it" is the whole value of the message.
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}

	d.Data = data
	h.render(w, "vendors", d)
}

// CreateVendorForm handles POST /staff/vendors.
func (h *Handler) CreateVendorForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.vendorGate(w, r)
	if !ok {
		return
	}
	in, err := vendorFromForm(r)
	if err != nil {
		h.redirectVendors(w, r, "", err.Error())
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.vendors.AddVendor(ctx, in); err != nil {
		h.vendorFailure(w, r, err)
		return
	}
	h.redirectVendors(w, r, "created", "")
}

// UpdateVendorForm handles POST /staff/vendors/{id}.
func (h *Handler) UpdateVendorForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.vendorGate(w, r)
	if !ok {
		return
	}
	id, ok := h.vendorID(w, r, s)
	if !ok {
		return
	}
	in, err := vendorFromForm(r)
	if err != nil {
		h.redirectVendors(w, r, "", err.Error())
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.vendors.EditVendor(ctx, id, in); err != nil {
		h.vendorFailure(w, r, err)
		return
	}
	h.redirectVendors(w, r, "saved", "")
}

// SetVendorRetiredForm handles POST /staff/vendors/{id}/retired.
//
// There is no delete form, and no delete anywhere: a vendor's name and GSTIN
// are what its purchase invoices were issued under, so the row outlives the
// relationship.
func (h *Handler) SetVendorRetiredForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.vendorGate(w, r)
	if !ok {
		return
	}
	id, ok := h.vendorID(w, r, s)
	if !ok {
		return
	}
	retired := r.PostFormValue("retired") == "true"
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.vendors.SetVendorRetired(ctx, id, retired); err != nil {
		h.vendorFailure(w, r, err)
		return
	}
	if retired {
		h.redirectVendors(w, r, "retired", "")
		return
	}
	h.redirectVendors(w, r, "restored", "")
}

// vendorGate holds the screen to the section's own roles and refuses readably
// when no master is wired.
func (h *Handler) vendorGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "vendors")
	if !ok {
		return s, false
	}
	if h.vendors == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"The vendor master is not configured on this deployment.")
		return s, false
	}
	return s, true
}

func (h *Handler) vendorID(w http.ResponseWriter, r *http.Request, s Session) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid vendor id.")
		return 0, false
	}
	return id, true
}

// vendorFromForm reads a vendor out of the posted form, normalised and
// validated here as well as in the store so the operator gets the sentence
// that names the field rather than a generic failure.
func vendorFromForm(r *http.Request) (vendors.Vendor, error) {
	in := vendors.Vendor{
		Name:          r.PostFormValue("name"),
		ContactPerson: r.PostFormValue("contact_person"),
		Phone:         r.PostFormValue("phone"),
		Email:         r.PostFormValue("email"),
		GSTIN:         r.PostFormValue("gstin"),
		Address:       r.PostFormValue("address"),
		Notes:         r.PostFormValue("notes"),
	}
	in.Normalise()
	if err := in.Validate(); err != nil {
		return in, err
	}
	return in, nil
}

func (h *Handler) redirectVendors(w http.ResponseWriter, r *http.Request, code, problem string) {
	url := "/staff/vendors"
	switch {
	case problem != "":
		url += "?err=" + neturl.QueryEscape(problem)
	case code != "":
		url += "?m=" + code
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// vendorFailure maps a store error to one of the worded outcomes.
func (h *Handler) vendorFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, vendors.ErrNameTaken):
		h.redirectVendors(w, r, "duplicate", "")
	case errors.Is(err, vendors.ErrGSTINTaken):
		h.redirectVendors(w, r, "gstin_dupe", "")
	case errors.Is(err, vendors.ErrNotFound):
		h.redirectVendors(w, r, "missing", "")
	default:
		// Anything else is unexpected: vendorFromForm already ran the same
		// Normalise/Validate the store runs, so a validation message cannot
		// reach here. Showing the raw error would leak storage detail to an
		// operator who can do nothing with it.
		h.redirectVendors(w, r, "failed", "")
	}
}

// pickableVendors reads the supplier dropdown's options for the Inventory and
// Procurement screens.
//
// Returns an empty slice rather than an error when no master is wired, so
// those screens still render: they each say, where the dropdown would be,
// that a vendor has to exist before a purchase can name one. A screen that
// 500ed because the master was missing would take the device roster and the
// issue/return forms down with it, which have nothing to do with vendors.
func (h *Handler) pickableVendors(ctx context.Context) []vendors.Vendor {
	if h.vendors == nil {
		return nil
	}
	list, err := h.vendors.PickableVendors(ctx)
	if err != nil {
		return nil
	}
	return list
}

// vendorProblem turns a vendor-related write failure into a sentence the
// operator can act on, and reports whether it was one at all.
//
// The two cases that reach a form are a vendor retired between the page
// loading and the form posting, and an id that names nothing — both of which
// an operator fixes themselves. Anything else is a storage failure they
// cannot, and is logged rather than shown.
func vendorProblem(err error) (string, bool) {
	switch {
	case errors.Is(err, vendors.ErrInactive):
		return "That vendor was retired since this page loaded. Pick another, or restore it on the Vendors screen.", true
	case errors.Is(err, vendors.ErrNotFound), errors.Is(err, vendors.ErrRequired):
		return "Choose a vendor. If the supplier is not in the list, add it on the Vendors screen first.", true
	}
	return "", false
}
