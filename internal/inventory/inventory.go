// Package inventory implements CPE stock tracking: device types, individual
// devices by serial number, issuance to subscribers, and vendor purchase
// records with low-stock alerting.
//
// FR: FR-INV-001..003 | MDS §4.16 | DBD §6.2 cpe_device_types, cpe_devices,
// cpe_purchases
package inventory

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/shopspring/decimal"
)

// Device lifecycle states.
const (
	StatusInStock  = "in_stock"
	StatusIssued   = "issued"
	StatusReturned = "returned"
	StatusFaulty   = "faulty"
)

var (
	// ErrNotAvailable is returned when a device cannot be issued because it
	// is not in stock — already with another subscriber, returned pending
	// inspection, or marked faulty. This is the guarantee that one physical
	// router is never issued to two people (FR-INV-002).
	ErrNotAvailable = errors.New("inventory: this device is not in stock and cannot be issued")
	// ErrNotIssued is returned when returning a device that nobody holds.
	ErrNotIssued = errors.New("inventory: this device is not currently issued")
)

var (
	IssuedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "inventory_cpe_issued_total",
		Help: "CPE devices issued to subscribers",
	})
	// LowStockTypes tracks how many device types are currently at or below
	// their reorder threshold. A gauge rather than a counter: it can go
	// down again when stock is replenished.
	LowStockTypes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "inventory_low_stock_types",
		Help: "Device types currently at or below their reorder threshold",
	})
)

// DeviceType is a model of CPE, and the unit low-stock alerting works in.
type DeviceType struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// Vendor is the supplier's current name, read from the vendor master
	// (FR-INV-004). Output only: the store takes the name from the master
	// in the same statement as the insert, so setting this on a write has
	// no effect — VendorID is what decides who the vendor is.
	Vendor string `json:"vendor"`
	// VendorID points into the vendor master. Required on every write;
	// nil only on a row migration 071's backfill could not link, which
	// means its free text was blank.
	VendorID         *int      `json:"vendor_id,omitempty"`
	ReorderThreshold int       `json:"reorder_threshold"`
	CreatedAt        time.Time `json:"created_at"`
}

// Device is one physical unit, identified by its serial number.
type Device struct {
	ID           int        `json:"id"`
	DeviceTypeID int        `json:"device_type_id"`
	DeviceType   string     `json:"device_type,omitempty"` // joined name, for list views
	SerialNumber string     `json:"serial_number"`
	MACAddress   string     `json:"mac_address,omitempty"`
	Status       string     `json:"status"`
	Location     string     `json:"location,omitempty"`
	SubscriberID *int       `json:"subscriber_id,omitempty"`
	IssuedAt     *time.Time `json:"issued_at,omitempty"`
	Notes        string     `json:"notes,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Purchase is a vendor purchase record (FR-INV-003).
type Purchase struct {
	ID           int `json:"id"`
	DeviceTypeID int `json:"device_type_id"`
	// Vendor is the supplier's current name from the master; VendorID is the
	// link. Same read/write split as DeviceType above — the name is output
	// only, and the id is what a write must carry (FR-INV-004).
	Vendor   string          `json:"vendor"`
	VendorID *int            `json:"vendor_id,omitempty"`
	Quantity int             `json:"quantity"`
	UnitCost decimal.Decimal `json:"-"`
	// UnitCostStr is the fixed-2dp rendering; money never reaches a JSON
	// consumer as a float in this codebase (FR-BIL-002).
	UnitCostStr  string    `json:"unit_cost"`
	TotalCostStr string    `json:"total_cost"`
	InvoiceRef   string    `json:"invoice_ref,omitempty"`
	PurchasedBy  string    `json:"purchased_by"`
	PurchasedAt  time.Time `json:"purchased_at"`
}

// StockLevel is one device type's current availability, and whether that has
// fallen to its reorder point.
type StockLevel struct {
	DeviceTypeID int    `json:"device_type_id"`
	DeviceType   string `json:"device_type"`
	// Vendor is the master's current name, so a reorder decision reads the
	// supplier's details as they stand now rather than as they were typed
	// when the model was added (FR-INV-004).
	Vendor           string `json:"vendor"`
	VendorID         *int   `json:"vendor_id,omitempty"`
	InStock          int    `json:"in_stock"`
	Issued           int    `json:"issued"`
	Faulty           int    `json:"faulty"`
	ReorderThreshold int    `json:"reorder_threshold"`
	IsLow            bool   `json:"is_low"`
}

// ValidStatus reports whether s is a legal device status for a manual
// update. Excludes "issued": that state is only reachable through the
// issuance path, which must also set subscriber_id — a bare status write
// would violate chk_cpe_issued_has_subscriber.
func ValidStatus(s string) bool {
	switch s {
	case StatusInStock, StatusReturned, StatusFaulty:
		return true
	default:
		return false
	}
}
