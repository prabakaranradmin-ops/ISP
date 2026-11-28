package inventory

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// FR-INV-003: the inventory metrics follow the stock, whichever screen moved
// it. See Metered for the four ways they did not.

// stockFake holds per-type in-stock counts and thresholds, and answers
// GetStockLevels from them the way db.InventoryStore does.
type stockFake struct {
	types     map[int]int // type id → reorder threshold
	inStock   map[int]int // type id → devices in stock
	bySerial  map[string]int
	issueFail bool
	stockErr  error
	stockCtx  context.Context
}

func newStockFake() *stockFake {
	return &stockFake{types: map[int]int{}, inStock: map[int]int{}, bySerial: map[string]int{}}
}

func (f *stockFake) CreateDeviceType(_ context.Context, t DeviceType) (*DeviceType, error) {
	t.ID = len(f.types) + 1
	f.types[t.ID] = t.ReorderThreshold
	return &t, nil
}
func (f *stockFake) ListDeviceTypes(context.Context) ([]DeviceType, error) { return nil, nil }
func (f *stockFake) CreateDevice(_ context.Context, d Device) (*Device, error) {
	f.inStock[d.DeviceTypeID]++
	f.bySerial[d.SerialNumber] = d.DeviceTypeID
	return &d, nil
}
func (f *stockFake) GetDeviceBySerial(context.Context, string) (*Device, error) { return nil, nil }
func (f *stockFake) ListDevices(context.Context, *string, *int, *int) ([]Device, error) {
	return nil, nil
}
func (f *stockFake) IssueDevice(_ context.Context, serial string, subscriberID int) (*Device, error) {
	typ, ok := f.bySerial[serial]
	if f.issueFail {
		return nil, errors.New("store: issue failed")
	}
	if !ok || f.inStock[typ] == 0 {
		return nil, nil // not in stock: the claim did not land
	}
	f.inStock[typ]--
	delete(f.bySerial, serial)
	return &Device{SerialNumber: serial, DeviceTypeID: typ, SubscriberID: &subscriberID}, nil
}
func (f *stockFake) ReturnDevice(_ context.Context, serial, status string) (*Device, error) {
	return &Device{SerialNumber: serial, Status: status}, nil
}
func (f *stockFake) GetStockLevels(ctx context.Context, lowOnly bool) ([]StockLevel, error) {
	f.stockCtx = ctx
	if f.stockErr != nil {
		return nil, f.stockErr
	}
	var out []StockLevel
	for id, threshold := range f.types {
		l := StockLevel{DeviceTypeID: id, InStock: f.inStock[id], ReorderThreshold: threshold}
		l.IsLow = l.InStock <= l.ReorderThreshold
		if lowOnly && !l.IsLow {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}
func (f *stockFake) RecordPurchase(_ context.Context, p Purchase) (*Purchase, error) { return &p, nil }
func (f *stockFake) ListPurchases(context.Context, *int) ([]Purchase, error)         { return nil, nil }

func lowGauge() float64 { return testutil.ToFloat64(LowStockTypes) }

// After a restart the gauge says what the database says, not 0.
func TestFR_INV_003_TheGaugeIsRightFromStartup(t *testing.T) {
	f := newStockFake()
	// Types 1 and 2 have nothing in stock against a threshold of 5: low.
	// Type 3 has 4 in stock against a threshold of 0: not low.
	f.types[1], f.types[2], f.types[3] = 5, 5, 0
	f.inStock[3] = 4
	LowStockTypes.Set(0)

	NewMetered(f).Refresh(context.Background())
	if got := lowGauge(); got != 2 {
		t.Errorf("gauge after startup = %v, want 2", got)
	}
}

// Every write that can move the number moves the gauge — including the ones
// the API handlers never refreshed on.
func TestFR_INV_003_EveryStockChangeMovesTheGauge(t *testing.T) {
	f := newStockFake()
	m := NewMetered(f)
	ctx := context.Background()
	LowStockTypes.Set(0)

	// A new type has no stock, so it is low the moment it exists.
	typ, _ := m.CreateDeviceType(ctx, DeviceType{Name: "ONT", ReorderThreshold: 1})
	if got := lowGauge(); got != 1 {
		t.Fatalf("after creating a type with no stock: gauge = %v, want 1", got)
	}

	// Adding stock past the threshold takes it off the list. This is the
	// write the handlers did not refresh on.
	for _, sn := range []string{"SN1", "SN2"} {
		if _, err := m.CreateDevice(ctx, Device{DeviceTypeID: typ.ID, SerialNumber: sn}); err != nil {
			t.Fatal(err)
		}
	}
	if got := lowGauge(); got != 0 {
		t.Errorf("after stocking two devices over a threshold of one: gauge = %v, want 0", got)
	}

	// Issuing one puts it back at the threshold, which counts as low.
	before := testutil.ToFloat64(IssuedTotal)
	if d, err := m.IssueDevice(ctx, "SN1", 7); err != nil || d == nil {
		t.Fatalf("issue: %v %v", d, err)
	}
	if got := testutil.ToFloat64(IssuedTotal) - before; got != 1 {
		t.Errorf("issued_total moved by %v, want 1", got)
	}
	if got := lowGauge(); got != 1 {
		t.Errorf("after issuing down to the threshold: gauge = %v, want 1", got)
	}
}

// Only an issuance that landed is counted.
func TestFR_INV_003_AnIssueThatDidNotLandIsNotCounted(t *testing.T) {
	f := newStockFake()
	m := NewMetered(f)
	ctx := context.Background()
	before := testutil.ToFloat64(IssuedTotal)

	if d, err := m.IssueDevice(ctx, "NOT-IN-STOCK", 7); d != nil || err != nil {
		t.Fatalf("got %v %v", d, err)
	}
	f.issueFail = true
	if _, err := m.IssueDevice(ctx, "ANY", 7); err == nil {
		t.Fatal("the store's error was swallowed")
	}
	if got := testutil.ToFloat64(IssuedTotal) - before; got != 0 {
		t.Errorf("issued_total moved by %v for issuances that did not happen", got)
	}
}

// The refresh runs even if the request's context is gone by then: the write
// committed, and the gauge has to describe the stock after it.
func TestFR_INV_003_TheRefreshOutlivesTheRequest(t *testing.T) {
	f := newStockFake()
	m := NewMetered(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.CreateDeviceType(ctx, DeviceType{Name: "ONT"}); err != nil {
		t.Fatal(err)
	}
	if f.stockCtx == nil || f.stockCtx.Err() != nil {
		t.Errorf("the refresh ran on a cancelled context (err=%v); against the real store it would fail",
			f.stockCtx.Err())
	}
}

// A failed refresh keeps the last value rather than zeroing it, and does not
// fail the write.
func TestFR_INV_003_AFailedRefreshKeepsTheLastValue(t *testing.T) {
	f := newStockFake()
	m := NewMetered(f)
	LowStockTypes.Set(3)
	f.stockErr = errors.New("store: unreachable")

	if _, err := m.RecordPurchase(context.Background(), Purchase{Quantity: 1}); err != nil {
		t.Errorf("a refresh failure failed the write: %v", err)
	}
	if got := lowGauge(); got != 3 {
		t.Errorf("gauge = %v after a failed refresh, want the last value 3", got)
	}
	if _, err := m.ReturnDevice(context.Background(), "SN", StatusFaulty); err != nil {
		t.Error(err)
	}
}
