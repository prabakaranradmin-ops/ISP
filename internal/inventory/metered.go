package inventory

import (
	"context"

	"github.com/rs/zerolog/log"
)

// Store is the persistence surface for CPE tracking. Satisfied by
// *db.InventoryStore, and by *Metered wrapping one.
type Store interface {
	CreateDeviceType(ctx context.Context, t DeviceType) (*DeviceType, error)
	ListDeviceTypes(ctx context.Context) ([]DeviceType, error)
	CreateDevice(ctx context.Context, d Device) (*Device, error)
	GetDeviceBySerial(ctx context.Context, serial string) (*Device, error)
	ListDevices(ctx context.Context, status *string, deviceTypeID, subscriberID *int) ([]Device, error)
	IssueDevice(ctx context.Context, serial string, subscriberID int) (*Device, error)
	ReturnDevice(ctx context.Context, serial, newStatus string) (*Device, error)
	GetStockLevels(ctx context.Context, lowOnly bool) ([]StockLevel, error)
	RecordPurchase(ctx context.Context, p Purchase) (*Purchase, error)
	ListPurchases(ctx context.Context, deviceTypeID *int) ([]Purchase, error)
}

// Metered keeps inventory_cpe_issued_total and inventory_low_stock_types
// true whichever screen made the change.
//
// FR-INV-003 evaluates low stock when stock changes rather than by polling,
// and that is still the rule. What was wrong was where it was applied: the
// API's issue and purchase handlers updated both metrics, so
//
//   - the console, which writes to the store directly and is where operators
//     actually issue devices, moved neither;
//   - lead conversion issued a device and counted it, but left the gauge;
//   - adding a device or a device type, the two writes that change the
//     in-stock count or the set of types, refreshed nothing, while recording
//     a purchase (which changes no device row) did;
//   - and the gauge read 0 after every restart until the next API issuance.
//
// Every write path reaches the store, so the bookkeeping lives here, once.
type Metered struct {
	Store
}

// NewMetered wraps s. Call Refresh once at startup so the gauge is right
// before the first write.
func NewMetered(s Store) *Metered { return &Metered{Store: s} }

// Refresh recomputes how many device types sit at or below their reorder
// point. A failure leaves the last value in place and is logged; it is never
// worth failing the write that triggered it.
func (m *Metered) Refresh(ctx context.Context) {
	low, err := m.GetStockLevels(ctx, true)
	if err != nil {
		log.Warn().Err(err).Msg("inventory: could not refresh the low-stock gauge")
		return
	}
	LowStockTypes.Set(float64(len(low)))
}

// afterWrite refreshes on a context that outlives the request: the write has
// committed, and a client hanging up should not leave the gauge describing
// the stock before it.
func (m *Metered) afterWrite(ctx context.Context) {
	m.Refresh(context.WithoutCancel(ctx))
}

func (m *Metered) CreateDeviceType(ctx context.Context, t DeviceType) (*DeviceType, error) {
	out, err := m.Store.CreateDeviceType(ctx, t)
	if err == nil {
		m.afterWrite(ctx)
	}
	return out, err
}

func (m *Metered) CreateDevice(ctx context.Context, d Device) (*Device, error) {
	out, err := m.Store.CreateDevice(ctx, d)
	if err == nil {
		m.afterWrite(ctx)
	}
	return out, err
}

// IssueDevice counts an issuance only when the atomic claim landed: (nil,
// nil) means the device was not in stock and nothing was issued.
func (m *Metered) IssueDevice(ctx context.Context, serial string, subscriberID int) (*Device, error) {
	out, err := m.Store.IssueDevice(ctx, serial, subscriberID)
	if err == nil && out != nil {
		IssuedTotal.Inc()
		m.afterWrite(ctx)
	}
	return out, err
}

func (m *Metered) ReturnDevice(ctx context.Context, serial, newStatus string) (*Device, error) {
	out, err := m.Store.ReturnDevice(ctx, serial, newStatus)
	if err == nil && out != nil {
		m.afterWrite(ctx)
	}
	return out, err
}

func (m *Metered) RecordPurchase(ctx context.Context, p Purchase) (*Purchase, error) {
	out, err := m.Store.RecordPurchase(ctx, p)
	if err == nil {
		m.afterWrite(ctx)
	}
	return out, err
}
