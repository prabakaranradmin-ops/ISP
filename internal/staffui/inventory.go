package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// InventoryStore backs the Inventory screen — CPE device types, individual
// devices by serial, issuance/return, stock levels and vendor purchases.
// The console counterpart to internal/api/inventory.go, which (like NAS and
// Franchises before it) had a complete, tested backend and no UI.
//
// Redefined per package rather than sharing api.InventoryQuerier, matching
// every other store interface in this file. Satisfied by *db.InventoryStore,
// the same instance already wired for internal/api.
type InventoryStore interface {
	ListDeviceTypes(ctx context.Context) ([]inventory.DeviceType, error)
	CreateDeviceType(ctx context.Context, t inventory.DeviceType) (*inventory.DeviceType, error)
	ListDevices(ctx context.Context, status *string, deviceTypeID, subscriberID *int) ([]inventory.Device, error)
	CreateDevice(ctx context.Context, d inventory.Device) (*inventory.Device, error)
	GetDeviceBySerial(ctx context.Context, serial string) (*inventory.Device, error)
	IssueDevice(ctx context.Context, serial string, subscriberID int) (*inventory.Device, error)
	ReturnDevice(ctx context.Context, serial, newStatus string) (*inventory.Device, error)
	GetStockLevels(ctx context.Context, lowOnly bool) ([]inventory.StockLevel, error)
	RecordPurchase(ctx context.Context, p inventory.Purchase) (*inventory.Purchase, error)
	ListPurchases(ctx context.Context, deviceTypeID *int) ([]inventory.Purchase, error)
}

type inventoryData struct {
	StockLevels  []inventory.StockLevel
	Devices      []inventory.Device
	DeviceTypes  []inventory.DeviceType
	Purchases    []inventory.Purchase
	StatusFilter string
	// ShowProcurement gates the "Add device type" and "Record purchase"
	// forms: per the original gap analysis, purchases and catalogue changes
	// are procurement decisions that stay with billing_admin/isp_owner,
	// while day-to-day issue/return is exactly what a technician needs.
	ShowProcurement bool
}

// Inventory shows stock levels, the device roster, and (for owner/billing)
// the purchasing and device-type forms.
func (h *Handler) Inventory(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "inventory")
	if !ok {
		return
	}
	h.renderInventory(w, r, s, r.URL.Query().Get("status"), "", "")
}

func (h *Handler) renderInventory(w http.ResponseWriter, r *http.Request, s Session, statusFilter, message, errMsg string) {
	d := h.page(s, "Inventory", "inventory")
	d.Message, d.Error = message, errMsg

	if h.inventory == nil {
		d.Error = "Inventory management is not configured on this deployment."
		h.render(w, "inventory", d)
		return
	}

	stock, err := h.inventory.GetStockLevels(r.Context(), false)
	if err != nil {
		log.Error().Err(err).Msg("staffui: get stock levels failed")
		d.Error = "Could not load stock levels."
		h.render(w, "inventory", d)
		return
	}

	var statusPtr *string
	if statusFilter != "" {
		statusPtr = &statusFilter
	}
	devices, err := h.inventory.ListDevices(r.Context(), statusPtr, nil, nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list devices failed")
		d.Error = "Could not load the device roster."
		h.render(w, "inventory", d)
		return
	}

	types, err := h.inventory.ListDeviceTypes(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list device types failed")
		d.Error = "Could not load device types."
		h.render(w, "inventory", d)
		return
	}

	purchases, err := h.inventory.ListPurchases(r.Context(), nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list purchases failed")
		d.Error = "Could not load purchase history."
		h.render(w, "inventory", d)
		return
	}

	d.Data = inventoryData{
		StockLevels:     stock,
		Devices:         devices,
		DeviceTypes:     types,
		Purchases:       purchases,
		StatusFilter:    statusFilter,
		ShowProcurement: s.Role == "isp_owner" || s.Role == "billing_admin",
	}
	h.render(w, "inventory", d)
}

// CreateDeviceTypeForm adds a new model of CPE. Owner/billing only — see
// ShowProcurement's reasoning above, enforced here too since a hidden form
// is not the control.
func (h *Handler) CreateDeviceTypeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "inventory")
	if !ok {
		return
	}
	if s.Role != "isp_owner" && s.Role != "billing_admin" {
		h.renderError(w, r, s, http.StatusForbidden, "Only the owner or billing admin can add device types.")
		return
	}
	if h.inventory == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Inventory management is not configured.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	vendor := strings.TrimSpace(r.PostFormValue("vendor"))
	if name == "" || vendor == "" {
		h.renderInventory(w, r, s, "", "", "Name and vendor are required.")
		return
	}
	threshold, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("reorder_threshold")))
	if err != nil || threshold < 0 {
		h.renderInventory(w, r, s, "", "", "Reorder threshold must be a non-negative number.")
		return
	}

	created, err := h.inventory.CreateDeviceType(r.Context(), inventory.DeviceType{
		Name: name, Vendor: vendor, ReorderThreshold: threshold,
	})
	if err != nil {
		log.Error().Err(err).Str("name", name).Msg("staffui: create device type failed")
		h.renderInventory(w, r, s, "", "", "Could not add that device type.")
		return
	}
	h.renderInventory(w, r, s, "", fmt.Sprintf("Device type %q added.", created.Name), "")
}

// CreateDeviceForm registers a new physical unit by serial number.
func (h *Handler) CreateDeviceForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "inventory")
	if !ok {
		return
	}
	if h.inventory == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Inventory management is not configured.")
		return
	}

	serial := strings.TrimSpace(r.PostFormValue("serial_number"))
	mac := strings.TrimSpace(r.PostFormValue("mac_address"))
	location := strings.TrimSpace(r.PostFormValue("location"))
	deviceTypeID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("device_type_id")))
	if serial == "" || err != nil || deviceTypeID <= 0 {
		h.renderInventory(w, r, s, "", "", "Serial number and a device type are required.")
		return
	}

	created, err := h.inventory.CreateDevice(r.Context(), inventory.Device{
		DeviceTypeID: deviceTypeID, SerialNumber: serial, MACAddress: mac, Location: location,
	})
	if err != nil {
		log.Error().Err(err).Str("serial", serial).Msg("staffui: create device failed")
		h.renderInventory(w, r, s, "", "", "Could not register that device — check the serial number and MAC address are not already in use.")
		return
	}
	h.renderInventory(w, r, s, "", fmt.Sprintf("Device %s registered.", created.SerialNumber), "")
}

// IssueDeviceForm hands a device to a subscriber. Any role that can see this
// screen can do this — it's the day-to-day technician action the CRD gap
// analysis called out as distinct from procurement.
func (h *Handler) IssueDeviceForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "inventory")
	if !ok {
		return
	}
	if h.inventory == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Inventory management is not configured.")
		return
	}

	serial := r.PathValue("serial")
	subscriberID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("subscriber_id")))
	if err != nil || subscriberID <= 0 {
		h.renderInventory(w, r, s, "", "", "A valid subscriber id is required to issue a device.")
		return
	}

	issued, err := h.inventory.IssueDevice(r.Context(), serial, subscriberID)
	if err != nil {
		log.Error().Err(err).Str("serial", serial).Msg("staffui: issue device failed")
		h.renderInventory(w, r, s, "", "", "Could not issue that device.")
		return
	}
	if issued == nil {
		h.renderInventory(w, r, s, "", "", "That device is not in stock — it may already be issued, returned pending inspection, or marked faulty.")
		return
	}
	h.renderInventory(w, r, s, "", fmt.Sprintf("%s issued to subscriber #%d.", issued.SerialNumber, subscriberID), "")
}

// ReturnDeviceForm takes a device back from a subscriber, pending inspection
// (status "returned") or writing it off (status "faulty") — never straight
// back to "in_stock", matching internal/api/inventory.go's own rule.
func (h *Handler) ReturnDeviceForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "inventory")
	if !ok {
		return
	}
	if h.inventory == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Inventory management is not configured.")
		return
	}

	serial := r.PathValue("serial")
	status := strings.TrimSpace(r.PostFormValue("status"))
	if status != inventory.StatusReturned && status != inventory.StatusFaulty {
		h.renderInventory(w, r, s, "", "", "Status must be \"returned\" or \"faulty\".")
		return
	}

	returned, err := h.inventory.ReturnDevice(r.Context(), serial, status)
	if err != nil {
		log.Error().Err(err).Str("serial", serial).Msg("staffui: return device failed")
		h.renderInventory(w, r, s, "", "", "Could not update that device.")
		return
	}
	if returned == nil {
		h.renderInventory(w, r, s, "", "", "That device is not currently issued to anyone.")
		return
	}
	h.renderInventory(w, r, s, "", fmt.Sprintf("%s marked %s.", returned.SerialNumber, status), "")
}

// RecordPurchaseForm logs a vendor purchase. Owner/billing only, same
// reasoning as CreateDeviceTypeForm.
func (h *Handler) RecordPurchaseForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "inventory")
	if !ok {
		return
	}
	if s.Role != "isp_owner" && s.Role != "billing_admin" {
		h.renderError(w, r, s, http.StatusForbidden, "Only the owner or billing admin can record purchases.")
		return
	}
	if h.inventory == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Inventory management is not configured.")
		return
	}

	deviceTypeID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("device_type_id")))
	vendor := strings.TrimSpace(r.PostFormValue("vendor"))
	quantity, qErr := strconv.Atoi(strings.TrimSpace(r.PostFormValue("quantity")))
	if err != nil || deviceTypeID <= 0 || vendor == "" || qErr != nil || quantity <= 0 {
		h.renderInventory(w, r, s, "", "", "Device type, vendor and a positive quantity are required.")
		return
	}
	unitCost, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("unit_cost")))
	if err != nil || unitCost.IsNegative() {
		h.renderInventory(w, r, s, "", "", "Unit cost must be a non-negative number.")
		return
	}
	invoiceRef := strings.TrimSpace(r.PostFormValue("invoice_ref"))

	created, err := h.inventory.RecordPurchase(r.Context(), inventory.Purchase{
		DeviceTypeID: deviceTypeID, Vendor: vendor, Quantity: quantity,
		UnitCost: unitCost, InvoiceRef: invoiceRef, PurchasedBy: s.Username,
	})
	if err != nil {
		log.Error().Err(err).Msg("staffui: record purchase failed")
		h.renderInventory(w, r, s, "", "", "Could not record that purchase.")
		return
	}
	h.renderInventory(w, r, s, "", fmt.Sprintf("Purchase of %d unit(s) from %s recorded.", created.Quantity, created.Vendor), "")
}
