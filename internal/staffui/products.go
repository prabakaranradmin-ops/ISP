package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Product catalogue — FR-BIL-022.
//
// ProductManager is satisfied by *api.Handler, so the console applies the
// same validation, writes the same audit entries and increments the same
// metrics as the API. The screen is the operator's way in; the rules live
// in one place.
type ProductManager interface {
	CreateProduct(ctx context.Context, in billing.Product) (*billing.Product, error)
	UpdateProduct(ctx context.Context, in billing.Product) (*billing.Product, error)
	SetProductActive(ctx context.Context, id int, active bool) error
	DeleteProduct(ctx context.Context, id int) error
	GetProduct(ctx context.Context, id int) (*billing.Product, error)
	ListProducts(ctx context.Context, f billing.ProductFilter) ([]billing.Product, error)
	AttachProduct(ctx context.Context, req billing.AttachRequest) (*billing.SubscriberProduct, error)
	EndAttachment(ctx context.Context, id int, reason string) error
	ListSubscriberProducts(ctx context.Context, subscriberID int, history bool) ([]billing.SubscriberProduct, error)
	ListProductSubscribers(ctx context.Context, productID, limit int) ([]billing.SubscriberProduct, error)
}

// productMessages words each outcome carried back in the URL as a code.
var productMessages = map[string]string{
	"created":     "Product added. Attach it to a subscriber from their Billing tab.",
	"updated":     "Product updated. Subscribers already attached keep the price they were attached at.",
	"deactivated": "Product retired. Nobody new can be put on it; subscribers who already hold it keep being billed until you end their charge.",
	"activated":   "Product reinstated and available to attach again.",
	"deleted":     "Product deleted.",
	"attached":    "Charge added. It goes on the subscriber's next invoice.",
	"ended":       "Charge ended. It will not appear on any further invoice.",

	"duplicate": "Not added: a product with that code already exists.",
	"inactive":  "Not attached: that product has been retired. Reinstate it, or choose another.",
	"inuse":     "Not deleted: subscribers hold this product. Retire it instead — its invoice lines have to stay explicable.",
	"closed":    "That charge has already been billed or ended.",
	"reason":    "Not ended: give a reason.",
	"missing":   "No product with that id.",
	"failed":    "That did not go through. Nothing was changed; try again.",
}

var productSuccess = map[string]bool{
	"created": true, "updated": true, "deactivated": true, "activated": true,
	"deleted": true, "attached": true, "ended": true,
}

type productRow struct {
	billing.Product
	// Total is price × 1 with GST at the headline rate — not shown as a
	// billed figure, since the real tax depends on the subscriber's state
	// and the rate on the invoice date. Left out rather than estimated
	// wrongly; the list shows the taxable price only.
	Subscribers []billing.SubscriberProduct
}

type productsData struct {
	Kind      string // "", "one_time", "recurring"
	Rows      []productRow
	CanManage bool
	Chips     []productChip
	// Expanded is the product whose subscriber list is open, 0 for none.
	Expanded int
	Kinds    []productKindOption
}

type productChip struct{ Key, Label string }

var productChips = []productChip{
	{"", "All"}, {billing.ProductOneTime, "One-time"}, {billing.ProductRecurring, "Recurring"},
}

type productKindOption struct{ Key, Label string }

var productKinds = []productKindOption{
	{billing.ProductOneTime, "One-time (billed once)"},
	{billing.ProductRecurring, "Recurring (billed every renewal)"},
}

// canManageProducts: the roles that may change what things cost. The same
// set as the Catalogue screen's own gate — a price change re-prices
// everyone attached to it from their next renewal.
func canManageProducts(role string) bool {
	return role == "isp_owner" || role == "billing_admin"
}

func (h *Handler) productGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "products")
	if !ok {
		return s, false
	}
	if h.products == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"The product catalogue is not configured on this deployment.")
		return s, false
	}
	return s, true
}

// requireProductManager gates the write actions, which a CSR may not take
// even though they may read the screen.
func (h *Handler) requireProductManager(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.productGate(w, r)
	if !ok {
		return s, false
	}
	if !canManageProducts(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only billing staff and the owner can change the product catalogue.")
		return s, false
	}
	return s, true
}

// Products handles GET /staff/products.
func (h *Handler) Products(w http.ResponseWriter, r *http.Request) {
	s, ok := h.productGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Products & Charges", "products")
	q := r.URL.Query()

	pd := productsData{
		Kind:      q.Get("kind"),
		CanManage: canManageProducts(s.Role),
		Chips:     productChips,
		Kinds:     productKinds,
	}
	switch pd.Kind {
	case billing.ProductOneTime, billing.ProductRecurring:
	default:
		pd.Kind = ""
	}
	if code := q.Get("p"); code != "" {
		if m, ok := productMessages[code]; ok {
			if productSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	// A validation failure carries its own sentence rather than a code,
	// since "the code must be 2 to 32 characters" is more use than
	// "invalid". Rendered as text by the template, never as markup.
	if reason := strings.TrimSpace(q.Get("err")); reason != "" {
		if len([]rune(reason)) > 200 {
			reason = string([]rune(reason)[:200])
		}
		d.Error = "Not saved: " + reason + "."
	}
	if n, err := strconv.Atoi(q.Get("show")); err == nil && n > 0 {
		pd.Expanded = n
	}

	list, err := h.products.ListProducts(r.Context(), billing.ProductFilter{Kind: pd.Kind, Limit: 200})
	if err != nil {
		log.Error().Err(err).Msg("staffui: list products failed")
		d.Error = "Could not load the product catalogue."
	}
	for _, p := range list {
		row := productRow{Product: p}
		// Only the expanded product's holders are fetched: the list is
		// otherwise one query, and "who has this" is a question about one
		// product at a time.
		if p.ID == pd.Expanded {
			if subs, err := h.products.ListProductSubscribers(r.Context(), p.ID, 200); err == nil {
				row.Subscribers = subs
			} else {
				log.Error().Err(err).Int("product_id", p.ID).Msg("staffui: list product subscribers failed")
				d.Error = "Could not load who holds that product."
			}
		}
		pd.Rows = append(pd.Rows, row)
	}
	d.Data = pd
	h.render(w, "products", d)
}

// productFormValues reads a product out of a posted form.
func productFormValues(r *http.Request) (billing.Product, error) {
	price, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("price")))
	if err != nil {
		return billing.Product{}, fmt.Errorf("%w: enter the price as a number, for example 1500.00",
			billing.ErrProductInvalid)
	}
	return billing.Product{
		Code:        r.PostFormValue("code"),
		Name:        r.PostFormValue("name"),
		Description: r.PostFormValue("description"),
		Kind:        r.PostFormValue("kind"),
		Price:       price,
		HSNSAC:      r.PostFormValue("hsn_sac"),
	}, nil
}

// CreateProductForm handles POST /staff/products.
func (h *Handler) CreateProductForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireProductManager(w, r)
	if !ok {
		return
	}
	in, err := productFormValues(r)
	if err != nil {
		h.redirectProducts(w, r, productErrCode(err), r.PostFormValue("kind"))
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.products.CreateProduct(ctx, in); err != nil {
		h.productFailure(w, r, err, "create product", in.Code)
		return
	}
	h.redirectProducts(w, r, "created", "")
}

// UpdateProductForm handles POST /staff/products/{id}.
func (h *Handler) UpdateProductForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireProductManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid product id.")
		return
	}
	in, err := productFormValues(r)
	if err != nil {
		h.redirectProducts(w, r, productErrCode(err), "")
		return
	}
	in.ID = id
	ctx := middleware.WithSubject(r.Context(), s.Username)
	// The code and kind are fixed after creation, so they are read back
	// from the stored row rather than taken from the form — see
	// db.UpdateProduct for why changing either would misdescribe every
	// attachment that already exists.
	existing, err := h.products.GetProduct(ctx, id)
	if err != nil {
		h.productFailure(w, r, err, "load product", strconv.Itoa(id))
		return
	}
	in.Code, in.Kind = existing.Code, existing.Kind

	if _, err := h.products.UpdateProduct(ctx, in); err != nil {
		h.productFailure(w, r, err, "update product", existing.Code)
		return
	}
	h.redirectProducts(w, r, "updated", "")
}

// SetProductActiveForm handles POST /staff/products/{id}/active.
func (h *Handler) SetProductActiveForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireProductManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid product id.")
		return
	}
	active := r.PostFormValue("active") == "true"
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.products.SetProductActive(ctx, id, active); err != nil {
		h.productFailure(w, r, err, "set product active", strconv.Itoa(id))
		return
	}
	code := "deactivated"
	if active {
		code = "activated"
	}
	h.redirectProducts(w, r, code, "")
}

// DeleteProductForm handles POST /staff/products/{id}/delete.
func (h *Handler) DeleteProductForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireProductManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid product id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.products.DeleteProduct(ctx, id); err != nil {
		h.productFailure(w, r, err, "delete product", strconv.Itoa(id))
		return
	}
	h.redirectProducts(w, r, "deleted", "")
}

// AttachProductForm handles POST /staff/subscribers/{id}/products.
//
// Posted from the subscriber's Billing tab, and redirects back there:
// attaching a charge is something staff do while looking at one customer.
func (h *Handler) AttachProductForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canManageProducts(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only billing staff and the owner can add charges to a subscriber.")
		return
	}
	if h.products == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"The product catalogue is not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}

	productID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("product_id")))
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Charge not added: choose a product.")
		return
	}
	req := billing.AttachRequest{SubscriberID: id, ProductID: productID}
	if v := strings.TrimSpace(r.PostFormValue("quantity")); v != "" {
		if req.Quantity, err = strconv.Atoi(v); err != nil {
			h.renderSubscriberDetail(w, r, s, id, "Charge not added: the quantity is a whole number.")
			return
		}
	}
	// An empty override means the catalogue price, which the store fills
	// in. Only a supplied one is parsed.
	if v := strings.TrimSpace(r.PostFormValue("unit_price")); v != "" {
		if req.UnitPrice, err = decimal.NewFromString(v); err != nil {
			h.renderSubscriberDetail(w, r, s, id,
				"Charge not added: enter the price as a number, for example 150.00.")
			return
		}
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	_, err = h.products.AttachProduct(ctx, req)
	switch {
	case err == nil:
		http.Redirect(w, r, fmt.Sprintf("/staff/subscribers/%d?msg=%s", id,
			url.QueryEscape(productMessages["attached"])), http.StatusSeeOther)
	case errors.Is(err, billing.ErrProductInactive):
		h.renderSubscriberDetail(w, r, s, id, productMessages["inactive"])
	case billing.ProductInvalidReason(err) != "":
		h.renderSubscriberDetail(w, r, s, id, "Charge not added: "+billing.ProductInvalidReason(err)+".")
	case errors.Is(err, billing.ErrProductNotFound):
		h.renderSubscriberDetail(w, r, s, id, "Charge not added: no product with that id.")
	default:
		log.Error().Err(err).Int("subscriber_id", id).Int("product_id", productID).
			Msg("staffui: attach product failed")
		h.renderSubscriberDetail(w, r, s, id, "Charge not added. Nothing was saved; try again.")
	}
}

// EndAttachmentForm handles POST /staff/subscriber-products/{id}/end.
func (h *Handler) EndAttachmentForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canManageProducts(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only billing staff and the owner can end a subscriber's charge.")
		return
	}
	if h.products == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"The product catalogue is not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid charge id.")
		return
	}
	// Which subscriber to go back to: the form carries it, since the
	// attachment id alone does not say whose page we came from.
	subscriberID, _ := strconv.Atoi(r.PostFormValue("subscriber_id"))

	ctx := middleware.WithSubject(r.Context(), s.Username)
	err = h.products.EndAttachment(ctx, id, r.PostFormValue("reason"))

	if subscriberID <= 0 {
		// Posted from somewhere without a subscriber in hand; fall back to
		// the catalogue screen rather than a dead end.
		h.redirectProducts(w, r, endCode(err), "")
		return
	}
	switch {
	case err == nil:
		http.Redirect(w, r, fmt.Sprintf("/staff/subscribers/%d?msg=%s", subscriberID,
			url.QueryEscape(productMessages["ended"])), http.StatusSeeOther)
	case errors.Is(err, billing.ErrAttachmentNotActive):
		h.renderSubscriberDetail(w, r, s, subscriberID, productMessages["closed"])
	case billing.ProductInvalidReason(err) != "":
		h.renderSubscriberDetail(w, r, s, subscriberID,
			"Charge not ended: "+billing.ProductInvalidReason(err)+".")
	case errors.Is(err, billing.ErrAttachmentNotFound):
		h.renderSubscriberDetail(w, r, s, subscriberID, "Charge not ended: no such charge.")
	default:
		log.Error().Err(err).Int("attachment_id", id).Msg("staffui: end attachment failed")
		h.renderSubscriberDetail(w, r, s, subscriberID, "Charge not ended. Nothing was changed; try again.")
	}
}

func endCode(err error) string {
	if err == nil {
		return "ended"
	}
	return productErrCode(err)
}

// productErrCode maps a failure onto one of productMessages' keys.
func productErrCode(err error) string {
	switch {
	case errors.Is(err, billing.ErrProductCodeTaken):
		return "duplicate"
	case errors.Is(err, billing.ErrProductInactive):
		return "inactive"
	case errors.Is(err, billing.ErrProductInUse):
		return "inuse"
	case errors.Is(err, billing.ErrAttachmentNotActive):
		return "closed"
	case errors.Is(err, billing.ErrProductNotFound), errors.Is(err, billing.ErrAttachmentNotFound):
		return "missing"
	case billing.ProductInvalidReason(err) != "":
		// The specific sentence is more useful than a generic code, so it
		// is carried in the URL as text rather than mapped to a key.
		return "invalid:" + billing.ProductInvalidReason(err)
	default:
		return "failed"
	}
}

// productFailure logs and redirects back to the list with a worded code.
func (h *Handler) productFailure(w http.ResponseWriter, r *http.Request, err error, what, ref string) {
	code := productErrCode(err)
	if code == "failed" {
		log.Error().Err(err).Str("ref", ref).Msgf("staffui: %s failed", what)
	}
	h.redirectProducts(w, r, code, "")
}

// redirectProducts goes back to the list, carrying an outcome code. A code
// prefixed "invalid:" carries its own sentence, which the list shows as the
// error instead of looking one up.
func (h *Handler) redirectProducts(w http.ResponseWriter, r *http.Request, code, kind string) {
	q := url.Values{}
	if reason, ok := strings.CutPrefix(code, "invalid:"); ok {
		q.Set("err", reason)
	} else {
		q.Set("p", code)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if from := r.PostFormValue("from_kind"); from != "" && kind == "" {
		q.Set("kind", from)
	}
	http.Redirect(w, r, "/staff/products?"+q.Encode(), http.StatusSeeOther)
}
