// Package procurement implements general purchase orders (CRD-EXP-007) —
// a request/approval/fulfilment lifecycle for spending that is not CPE
// restocking (internal/inventory already owns that narrower case: N units
// of a device type from a vendor, recorded immediately with no approval
// step). A purchase order can name a service or any other non-hardware
// spend, and does not execute — does not even count as approved — until a
// different staff member signs off on it.
//
// Deliberately not wired to any accounting ledger: that depends on
// CRD-EXP-006 (general ledger/accounts management), which does not exist
// yet. This package only tracks the lifecycle itself.
package procurement

import "time"

// Status values a purchase order moves through. Requested is the only entry
// point; every other value is reached through Decide (approved/rejected) or
// an explicit fulfilment update (ordered/received/cancelled).
const (
	StatusRequested = "requested"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusOrdered   = "ordered"
	StatusReceived  = "received"
	StatusCancelled = "cancelled"
)

// ValidCategory reports whether c is a legal purchase-order category.
func ValidCategory(c string) bool {
	switch c {
	case "hardware", "services", "other":
		return true
	default:
		return false
	}
}

// ValidFulfilmentStatus reports whether s is a legal value for
// UpdateFulfilment — the two transitions available only after an order has
// already been approved, plus cancellation.
func ValidFulfilmentStatus(s string) bool {
	switch s {
	case StatusOrdered, StatusReceived, StatusCancelled:
		return true
	default:
		return false
	}
}

// NewPurchaseOrder is a request to persist.
type NewPurchaseOrder struct {
	Description string
	Vendor      string
	Category    string
	Amount      string // decimal string; parsed and validated by the caller
	RequestedBy string
}

// PurchaseOrder is a stored request as an operator sees it.
type PurchaseOrder struct {
	ID             int        `json:"id"`
	Description    string     `json:"description"`
	Vendor         string     `json:"vendor"`
	Category       string     `json:"category"`
	Amount         string     `json:"amount"`
	Status         string     `json:"status"`
	RequestedBy    string     `json:"requested_by"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	ReceivedAt     *time.Time `json:"received_at,omitempty"`
}
