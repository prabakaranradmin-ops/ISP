package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Product catalogue — FR-BIL-022 | migration 058. See
// internal/billing/product.go for the domain rules.

// ProductStore manages the catalogue and subscriber attachments.
// Satisfied by *db.BillingStore.
type ProductStore interface {
	CreateProduct(ctx context.Context, in billing.Product, by string) (*billing.Product, error)
	UpdateProduct(ctx context.Context, in billing.Product, by string) (*billing.Product, error)
	SetProductActive(ctx context.Context, id int, active bool, by string) error
	DeleteProduct(ctx context.Context, id int) error
	GetProduct(ctx context.Context, id int) (*billing.Product, error)
	ListProducts(ctx context.Context, f billing.ProductFilter) ([]billing.Product, error)
	AttachProduct(ctx context.Context, req billing.AttachRequest, by string) (*billing.SubscriberProduct, error)
	EndAttachment(ctx context.Context, id int, reason, by string) error
	ListSubscriberProducts(ctx context.Context, subscriberID int, history bool) ([]billing.SubscriberProduct, error)
	ListAttachmentsAcrossSubscribers(ctx context.Context, productID, limit int) ([]billing.SubscriberProduct, error)
}

// ErrProductsUnwired is returned when no product store is configured.
var ErrProductsUnwired = errors.New("api: product catalogue not configured")

// ── Service layer ───────────────────────────────────────────────────────────

// CreateProduct adds a catalogue entry.
func (h *Handler) CreateProduct(ctx context.Context, in billing.Product) (*billing.Product, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	p, err := h.products.CreateProduct(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "product.created", p.Code, map[string]any{
		"name": p.Name, "kind": p.Kind, "price": p.Price.StringFixed(2), "hsn_sac": p.HSNSAC,
		"adds_connections": p.AddsConnections,
	})
	return p, nil
}

// UpdateProduct changes a catalogue entry's name, description, price or
// HSN/SAC code. Existing subscribers keep the price they were attached at.
func (h *Handler) UpdateProduct(ctx context.Context, in billing.Product) (*billing.Product, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	p, err := h.products.UpdateProduct(ctx, in, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "product.updated", p.Code, map[string]any{
		"name": p.Name, "price": p.Price.StringFixed(2), "hsn_sac": p.HSNSAC,
	})
	return p, nil
}

// SetProductActive retires or reinstates a catalogue entry. Retiring it
// hides it from new attachments and leaves existing ones billing.
func (h *Handler) SetProductActive(ctx context.Context, id int, active bool) error {
	if h.products == nil {
		return ErrProductsUnwired
	}
	if err := h.products.SetProductActive(ctx, id, active, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	action := "product.deactivated"
	if active {
		action = "product.activated"
	}
	middleware.Audit(ctx, action, productRef(id), nil)
	return nil
}

// DeleteProduct removes a catalogue entry nothing has ever used.
func (h *Handler) DeleteProduct(ctx context.Context, id int) error {
	if h.products == nil {
		return ErrProductsUnwired
	}
	if err := h.products.DeleteProduct(ctx, id); err != nil {
		return err
	}
	middleware.Audit(ctx, "product.deleted", productRef(id), nil)
	return nil
}

// GetProduct loads one catalogue entry.
func (h *Handler) GetProduct(ctx context.Context, id int) (*billing.Product, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	return h.products.GetProduct(ctx, id)
}

// ListProducts returns the catalogue.
func (h *Handler) ListProducts(ctx context.Context, f billing.ProductFilter) ([]billing.Product, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	return h.products.ListProducts(ctx, f)
}

// AttachProduct sells a product to a subscriber. It is billed on their next
// renewal — recurring every cycle, one-time just the once.
func (h *Handler) AttachProduct(ctx context.Context, req billing.AttachRequest) (*billing.SubscriberProduct, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	sp, err := h.products.AttachProduct(ctx, req, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "product.attached", sp.ProductCode, map[string]any{
		"subscriber_id": sp.SubscriberID, "quantity": sp.Quantity,
		"unit_price": sp.UnitPrice.StringFixed(2), "kind": sp.Kind,
	})
	billing.LifecycleActionsTotal.WithLabelValues("product_attached").Inc()
	return sp, nil
}

// EndAttachment stops a recurring charge, with a reason.
func (h *Handler) EndAttachment(ctx context.Context, id int, reason string) error {
	if h.products == nil {
		return ErrProductsUnwired
	}
	if err := h.products.EndAttachment(ctx, id, reason, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	middleware.Audit(ctx, "product.ended", attachmentRef(id), map[string]any{
		"reason": strings.TrimSpace(reason),
	})
	billing.LifecycleActionsTotal.WithLabelValues("product_ended").Inc()
	return nil
}

// ListSubscriberProducts returns one subscriber's charges.
func (h *Handler) ListSubscriberProducts(ctx context.Context, subscriberID int, history bool) ([]billing.SubscriberProduct, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	return h.products.ListSubscriberProducts(ctx, subscriberID, history)
}

// ListProductSubscribers returns who holds a given product.
func (h *Handler) ListProductSubscribers(ctx context.Context, productID, limit int) ([]billing.SubscriberProduct, error) {
	if h.products == nil {
		return nil, ErrProductsUnwired
	}
	return h.products.ListAttachmentsAcrossSubscribers(ctx, productID, limit)
}

func productRef(id int) string    { return "product:" + strconv.Itoa(id) }
func attachmentRef(id int) string { return "subscriber-product:" + strconv.Itoa(id) }

// ── HTTP ─────────────────────────────────────────────────────────────────────

type productJSON struct {
	ID          int    `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind"`
	Price       string `json:"price"`
	HSNSAC      string `json:"hsn_sac"`
	Active      bool   `json:"active"`
	Attached    int    `json:"attached"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	UpdatedBy   string `json:"updated_by,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	// AddsConnections: each unit attached lets the subscriber hold one more
	// simultaneous login.
	AddsConnections bool `json:"adds_connections"`
}

func toProductJSON(p billing.Product) productJSON {
	out := productJSON{
		ID: p.ID, Code: p.Code, Name: p.Name, Description: p.Description, Kind: p.Kind,
		Price: p.Price.StringFixed(2), HSNSAC: p.HSNSAC, Active: p.Active,
		Attached: p.Attached, CreatedBy: p.CreatedBy, AddsConnections: p.AddsConnections,
		CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedBy: p.UpdatedBy,
	}
	if p.UpdatedAt != nil {
		out.UpdatedAt = p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

type subscriberProductJSON struct {
	ID            int    `json:"id"`
	SubscriberID  int    `json:"subscriber_id"`
	Username      string `json:"username,omitempty"`
	ProductID     int    `json:"product_id"`
	ProductCode   string `json:"product_code"`
	ProductName   string `json:"product_name"`
	Kind          string `json:"kind"`
	HSNSAC        string `json:"hsn_sac"`
	Quantity      int    `json:"quantity"`
	UnitPrice     string `json:"unit_price"`
	Amount        string `json:"amount"`
	Status        string `json:"status"`
	AttachedBy    string `json:"attached_by"`
	AttachedAt    string `json:"attached_at"`
	EndReason     string `json:"end_reason,omitempty"`
	InvoiceID     *int   `json:"invoice_id,omitempty"`
	InvoiceNumber string `json:"invoice_number,omitempty"`
}

func toSubscriberProductJSON(sp billing.SubscriberProduct) subscriberProductJSON {
	out := subscriberProductJSON{
		ID: sp.ID, SubscriberID: sp.SubscriberID, Username: sp.Username,
		ProductID: sp.ProductID, ProductCode: sp.ProductCode, ProductName: sp.ProductName,
		Kind: sp.Kind, HSNSAC: sp.HSNSAC, Quantity: sp.Quantity,
		UnitPrice: sp.UnitPrice.StringFixed(2), Amount: sp.Base().StringFixed(2),
		Status: sp.Status, AttachedBy: sp.AttachedBy,
		AttachedAt: sp.AttachedAt.Format("2006-01-02T15:04:05Z07:00"),
		EndReason:  sp.EndReason, InvoiceID: sp.InvoiceID,
	}
	out.InvoiceNumber = sp.InvoiceNumber()
	return out
}

// writeProductError maps a catalogue failure to a status and code.
func writeProductError(w http.ResponseWriter, err error) {
	switch {
	case billing.ProductInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", billing.ProductInvalidReason(err))
	case errors.Is(err, billing.ErrProductCodeTaken):
		writeError(w, http.StatusConflict, "ERR_DUPLICATE", "a product with that code already exists")
	case errors.Is(err, billing.ErrProductInactive):
		writeError(w, http.StatusConflict, "ERR_PRODUCT_INACTIVE",
			"that product is no longer offered; reinstate it or choose another")
	case errors.Is(err, billing.ErrProductInUse):
		writeError(w, http.StatusConflict, "ERR_PRODUCT_IN_USE",
			"subscribers hold this product; deactivate it instead of deleting it")
	case errors.Is(err, billing.ErrAttachmentNotActive):
		writeError(w, http.StatusConflict, "ERR_CHARGE_CLOSED",
			"that charge has already been billed or ended")
	case errors.Is(err, billing.ErrProductNotFound), errors.Is(err, billing.ErrAttachmentNotFound), isNotFound(err):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "not found")
	case errors.Is(err, ErrProductsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "product catalogue not configured")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "product operation failed")
	}
}

type productBody struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Price       string `json:"price"`
	HSNSAC      string `json:"hsn_sac"`
	// AddsConnections is honoured at creation only; see UpdateProductHTTP.
	AddsConnections bool `json:"adds_connections"`
}

// toProduct parses a request body into a Product. Validation proper is
// billing.Product.Normalise's job; this only has to turn text into types.
func (b productBody) toProduct() (billing.Product, error) {
	price, err := decimal.NewFromString(strings.TrimSpace(b.Price))
	if err != nil {
		return billing.Product{}, errBadPrice
	}
	return billing.Product{
		Code: b.Code, Name: b.Name, Description: b.Description,
		Kind: b.Kind, Price: price, HSNSAC: b.HSNSAC, AddsConnections: b.AddsConnections,
	}, nil
}

var errBadPrice = errors.New("price must be a number, for example \"1500.00\"")

// CreateProductHTTP handles POST /api/v1/products.
func (h *Handler) CreateProductHTTP(w http.ResponseWriter, r *http.Request) {
	var b productBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	in, err := b.toProduct()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	p, err := h.CreateProduct(r.Context(), in)
	if err != nil {
		writeProductError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProductJSON(*p))
}

// UpdateProductHTTP handles PUT /api/v1/products/{id}.
func (h *Handler) UpdateProductHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b productBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	in, err := b.toProduct()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	in.ID = id
	// The code, kind and whether it adds connections are fixed after
	// creation (see db.UpdateProduct), so whatever the body says about them
	// is ignored rather than silently half-applied. Normalise still needs a
	// valid-looking set.
	existing, err := h.GetProduct(r.Context(), id)
	if err != nil {
		writeProductError(w, err)
		return
	}
	in.Code, in.Kind, in.AddsConnections = existing.Code, existing.Kind, existing.AddsConnections

	p, err := h.UpdateProduct(r.Context(), in)
	if err != nil {
		writeProductError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProductJSON(*p))
}

// GetProductHTTP handles GET /api/v1/products/{id}.
func (h *Handler) GetProductHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	p, err := h.GetProduct(r.Context(), id)
	if err != nil {
		writeProductError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProductJSON(*p))
}

// ListProductsHTTP handles GET /api/v1/products?kind=&active_only=.
func (h *Handler) ListProductsHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := billing.ProductFilter{
		Kind:       q.Get("kind"),
		ActiveOnly: q.Get("active_only") == "true",
		Limit:      parseLimit(q, 200, 500),
	}
	switch f.Kind {
	case "", billing.ProductOneTime, billing.ProductRecurring:
	default:
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "kind must be one_time or recurring")
		return
	}
	list, err := h.ListProducts(r.Context(), f)
	if err != nil {
		writeProductError(w, err)
		return
	}
	out := make([]productJSON, 0, len(list))
	for _, p := range list {
		out = append(out, toProductJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

type productActiveBody struct {
	Active bool `json:"active"`
}

// SetProductActiveHTTP handles POST /api/v1/products/{id}/active.
func (h *Handler) SetProductActiveHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b productActiveBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := h.SetProductActive(r.Context(), id, b.Active); err != nil {
		writeProductError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteProductHTTP handles DELETE /api/v1/products/{id}.
func (h *Handler) DeleteProductHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if err := h.DeleteProduct(r.Context(), id); err != nil {
		writeProductError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListProductSubscribersHTTP handles GET /api/v1/products/{id}/subscribers.
func (h *Handler) ListProductSubscribersHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	list, err := h.ListProductSubscribers(r.Context(), id, parseLimit(r.URL.Query(), 200, 500))
	if err != nil {
		writeProductError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscribers": toSubscriberProductJSONs(list)})
}

func toSubscriberProductJSONs(list []billing.SubscriberProduct) []subscriberProductJSON {
	out := make([]subscriberProductJSON, 0, len(list))
	for _, sp := range list {
		out = append(out, toSubscriberProductJSON(sp))
	}
	return out
}

type attachBody struct {
	ProductID int    `json:"product_id"`
	Quantity  int    `json:"quantity"`
	UnitPrice string `json:"unit_price"`
}

// AttachProductHTTP handles POST /api/v1/subscribers/{id}/products.
func (h *Handler) AttachProductHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b attachBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	req := billing.AttachRequest{SubscriberID: id, ProductID: b.ProductID, Quantity: b.Quantity}
	// An omitted unit_price means "the catalogue price", which the store
	// fills in. Only a supplied one is parsed.
	if s := strings.TrimSpace(b.UnitPrice); s != "" {
		v, err := decimal.NewFromString(s)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", errBadPrice.Error())
			return
		}
		req.UnitPrice = v
	}
	sp, err := h.AttachProduct(r.Context(), req)
	if err != nil {
		writeProductError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSubscriberProductJSON(*sp))
}

// ListSubscriberProductsHTTP handles
// GET /api/v1/subscribers/{id}/products?history=true.
func (h *Handler) ListSubscriberProductsHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	list, err := h.ListSubscriberProducts(r.Context(), id, r.URL.Query().Get("history") == "true")
	if err != nil {
		writeProductError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": toSubscriberProductJSONs(list)})
}

type endAttachmentBody struct {
	Reason string `json:"reason"`
}

// EndAttachmentHTTP handles POST /api/v1/subscriber-products/{id}/end.
func (h *Handler) EndAttachmentHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b endAttachmentBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := h.EndAttachment(r.Context(), id, b.Reason); err != nil {
		writeProductError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
