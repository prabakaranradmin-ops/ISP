package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// CPE inventory endpoints — FR-INV-001..003 | MDS §4.16.

// InventoryQuerier is the persistence surface CPE tracking needs.
// Satisfied by *db.InventoryStore.
type InventoryQuerier interface {
	CreateDeviceType(ctx context.Context, t inventory.DeviceType) (*inventory.DeviceType, error)
	ListDeviceTypes(ctx context.Context) ([]inventory.DeviceType, error)

	CreateDevice(ctx context.Context, d inventory.Device) (*inventory.Device, error)
	GetDeviceBySerial(ctx context.Context, serial string) (*inventory.Device, error)
	ListDevices(ctx context.Context, status *string, deviceTypeID, subscriberID *int) ([]inventory.Device, error)
	// IssueDevice must be an atomic claim over an in-stock row, returning
	// (nil, nil) when the device was not available — that is the guarantee
	// one physical device is never issued to two subscribers.
	IssueDevice(ctx context.Context, serial string, subscriberID int) (*inventory.Device, error)
	ReturnDevice(ctx context.Context, serial, newStatus string) (*inventory.Device, error)

	GetStockLevels(ctx context.Context, lowOnly bool) ([]inventory.StockLevel, error)
	RecordPurchase(ctx context.Context, p inventory.Purchase) (*inventory.Purchase, error)
	ListPurchases(ctx context.Context, deviceTypeID *int) ([]inventory.Purchase, error)
}

// ── Device types ─────────────────────────────────────────────────────────────

type createDeviceTypeRequest struct {
	Name string `json:"name"`
	// VendorID names a supplier in the shared master (FR-INV-004). Vendor is
	// the pre-master field, still accepted as a lookup key only — see
	// Handler.resolveVendorRef.
	VendorID         int    `json:"vendor_id"`
	Vendor           string `json:"vendor"`
	ReorderThreshold *int   `json:"reorder_threshold"`
}

// CreateDeviceType handles POST /api/v1/cpe/types.
func (h *Handler) CreateDeviceType(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}

	var req createDeviceTypeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "name is required")
		return
	}
	vendorID := h.resolveVendorRef(w, r, req.VendorID, req.Vendor)
	if vendorID == 0 {
		return
	}
	threshold := 5 // matches the column default
	if req.ReorderThreshold != nil {
		if *req.ReorderThreshold < 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "reorder_threshold must not be negative")
			return
		}
		threshold = *req.ReorderThreshold
	}

	created, err := h.inventory.CreateDeviceType(r.Context(), inventory.DeviceType{
		Name: req.Name, VendorID: &vendorID, ReorderThreshold: threshold,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "ERR_CONFLICT", "a device type with that name already exists")
			return
		}
		// A vendor retired between the lookup above and the insert, or one
		// that never existed if vendor_id came straight from the client.
		// writeVendorError covers anything else with the fallback message, so
		// there is no case left to fall through to.
		writeVendorError(w, err, "create device type failed")
		return
	}

	middleware.Audit(r.Context(), "cpe.type_create", strconv.Itoa(created.ID), map[string]any{"name": created.Name})
	writeJSON(w, http.StatusCreated, created)
}

// ListDeviceTypes handles GET /api/v1/cpe/types.
func (h *Handler) ListDeviceTypes(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}
	list, err := h.inventory.ListDeviceTypes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list device types failed")
		return
	}
	if list == nil {
		list = []inventory.DeviceType{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── Devices ──────────────────────────────────────────────────────────────────

type createDeviceRequest struct {
	DeviceTypeID int    `json:"device_type_id"`
	SerialNumber string `json:"serial_number"`
	MACAddress   string `json:"mac_address"`
	Location     string `json:"location"`
	Notes        string `json:"notes"`
}

// CreateDevice handles POST /api/v1/cpe/devices — adding stock.
func (h *Handler) CreateDevice(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}

	var req createDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.SerialNumber == "" || req.DeviceTypeID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "serial_number and device_type_id are required")
		return
	}

	created, err := h.inventory.CreateDevice(r.Context(), inventory.Device{
		DeviceTypeID: req.DeviceTypeID, SerialNumber: req.SerialNumber,
		MACAddress: req.MACAddress, Location: req.Location, Notes: req.Notes,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "ERR_CONFLICT", "a device with that serial number or MAC already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create device failed")
		return
	}

	middleware.Audit(r.Context(), "cpe.device_create", created.SerialNumber, nil)
	writeJSON(w, http.StatusCreated, created)
}

// ListDevices handles GET /api/v1/cpe/devices?status=&device_type_id=&subscriber_id=.
func (h *Handler) ListDevices(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}

	var status *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}
	deviceTypeID, err := optionalQueryInt(r, "device_type_id")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	subscriberID, err := optionalQueryInt(r, "subscriber_id")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	list, err := h.inventory.ListDevices(r.Context(), status, deviceTypeID, subscriberID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list devices failed")
		return
	}
	if list == nil {
		list = []inventory.Device{}
	}
	writeJSON(w, http.StatusOK, list)
}

type issueDeviceRequest struct {
	SubscriberID int `json:"subscriber_id"`
}

// IssueDevice handles POST /api/v1/cpe/devices/{serial}/issue.
//
// FR: FR-INV-002 | MDS §4.16
func (h *Handler) IssueDevice(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if serial == "" {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "serial is required")
		return
	}
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}

	var req issueDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.SubscriberID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id is required")
		return
	}

	existing, err := h.inventory.GetDeviceBySerial(r.Context(), serial)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "device lookup failed")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no device with that serial number")
		return
	}

	issued, err := h.inventory.IssueDevice(r.Context(), serial, req.SubscriberID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "issue device failed")
		return
	}
	if issued == nil {
		// The atomic claim did not land: the device was not in stock, or
		// another issue took it first. 409 rather than 422 — nothing about
		// the request was malformed, the world simply moved.
		writeError(w, http.StatusConflict, "ERR_NOT_AVAILABLE", inventory.ErrNotAvailable.Error())
		return
	}

	// inventory_cpe_issued_total and the low-stock gauge are kept by
	// inventory.Metered, which wraps the store for every caller.
	middleware.Audit(r.Context(), "cpe.issue", serial, map[string]any{"subscriber_id": req.SubscriberID})
	writeJSON(w, http.StatusOK, issued)
}

type returnDeviceRequest struct {
	Status string `json:"status"` // returned | faulty
}

// ReturnDevice handles POST /api/v1/cpe/devices/{serial}/return.
func (h *Handler) ReturnDevice(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if serial == "" {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "serial is required")
		return
	}
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}

	var req returnDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	newStatus := req.Status
	if newStatus == "" {
		newStatus = inventory.StatusReturned
	}
	// in_stock is excluded on purpose: hardware coming back from a
	// subscriber goes to 'returned' pending inspection, and only an
	// explicit status update puts it back on the shelf. Skipping that step
	// would put un-inspected devices into the count FR-INV-003's reorder
	// alerts trust.
	if newStatus != inventory.StatusReturned && newStatus != inventory.StatusFaulty {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "status must be \"returned\" or \"faulty\"")
		return
	}

	returned, err := h.inventory.ReturnDevice(r.Context(), serial, newStatus)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "return device failed")
		return
	}
	if returned == nil {
		writeError(w, http.StatusConflict, "ERR_NOT_ISSUED", inventory.ErrNotIssued.Error())
		return
	}

	middleware.Audit(r.Context(), "cpe.return", serial, map[string]any{"status": newStatus})
	writeJSON(w, http.StatusOK, returned)
}

// ── Stock & purchases (FR-INV-003) ───────────────────────────────────────────

// GetStockLevels handles GET /api/v1/cpe/stock and /api/v1/cpe/low-stock.
func (h *Handler) GetStockLevels(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}
	lowOnly := r.URL.Query().Get("low_only") == "true"

	levels, err := h.inventory.GetStockLevels(r.Context(), lowOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "stock levels failed")
		return
	}
	if levels == nil {
		levels = []inventory.StockLevel{}
	}
	writeJSON(w, http.StatusOK, levels)
}

type recordPurchaseRequest struct {
	DeviceTypeID int `json:"device_type_id"`
	// See createDeviceTypeRequest on vendor_id against vendor.
	VendorID   int    `json:"vendor_id"`
	Vendor     string `json:"vendor"`
	Quantity   int    `json:"quantity"`
	UnitCost   string `json:"unit_cost"`
	InvoiceRef string `json:"invoice_ref"`
}

// RecordPurchase handles POST /api/v1/cpe/purchases.
func (h *Handler) RecordPurchase(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}

	var req recordPurchaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.DeviceTypeID <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "device_type_id is required")
		return
	}
	vendorID := h.resolveVendorRef(w, r, req.VendorID, req.Vendor)
	if vendorID == 0 {
		return
	}
	if req.Quantity <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "quantity must be positive")
		return
	}
	unitCost, err := decimal.NewFromString(req.UnitCost)
	if err != nil || unitCost.IsNegative() {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "unit_cost must be a non-negative decimal")
		return
	}

	created, err := h.inventory.RecordPurchase(r.Context(), inventory.Purchase{
		DeviceTypeID: req.DeviceTypeID, VendorID: &vendorID, Quantity: req.Quantity,
		UnitCost: unitCost, InvoiceRef: req.InvoiceRef,
		PurchasedBy: middleware.SubjectFromContext(r.Context()),
	})
	if writeVendorError(w, err, "record purchase failed") {
		return
	}

	middleware.Audit(r.Context(), "cpe.purchase", strconv.Itoa(created.ID), map[string]any{
		"device_type_id": created.DeviceTypeID, "quantity": created.Quantity,
	})
	writeJSON(w, http.StatusCreated, created)
}

// ListPurchases handles GET /api/v1/cpe/purchases?device_type_id=.
func (h *Handler) ListPurchases(w http.ResponseWriter, r *http.Request) {
	if h.inventory == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "inventory store not configured")
		return
	}
	deviceTypeID, err := optionalQueryInt(r, "device_type_id")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	list, err := h.inventory.ListPurchases(r.Context(), deviceTypeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list purchases failed")
		return
	}
	if list == nil {
		list = []inventory.Purchase{}
	}
	writeJSON(w, http.StatusOK, list)
}

// optionalQueryInt reads a nil-able integer query parameter, rejecting a
// malformed value rather than silently ignoring the filter — a dropped
// filter answers a different question while looking correct.
func optionalQueryInt(r *http.Request, key string) (*int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer", key)
	}
	return &n, nil
}
