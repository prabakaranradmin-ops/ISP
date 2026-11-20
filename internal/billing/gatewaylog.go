package billing

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Payment-gateway order and callback log — FR-PAY-001 | migration 064.
//
// Money arriving from the gateway left almost no trace of its own: a
// successful capture produced a wallet ledger entry, which is the money
// but not the conversation. Nothing recorded the order that was created,
// the callbacks that were ignored, the ones rejected for a bad signature,
// or a captured payment that arrived with no subscriber_id and credited
// nobody — which are exactly the cases a reconciliation asks about.
//
// The requirement is explicit that this is recorded **whether or not it
// credits a wallet**, so every exit path writes a row, including the ones
// that deliberately do nothing.
//
// Nothing here carries the webhook signature, the gateway secret or the
// raw body. Authenticity is decided before anything is written, so
// keeping the signature would preserve a credential-shaped value for no
// operational gain.

// Gateway log kinds.
const (
	// GatewayKindOrder is a payment link this system asked for.
	GatewayKindOrder = "order"
	// GatewayKindCallback is anything the gateway sent us.
	GatewayKindCallback = "callback"
)

// Gateway log statuses — what this system did about the event. A fixed
// vocabulary, matching migration 064's CHECK, so the column can be
// grouped and counted.
//
// There is deliberately no "duplicate": WalletService.Recharge is
// idempotent on transaction_token (FR-BIL-005) and returns the original
// transaction for a repeated callback, with no error and no signal that
// it was one. A retried callback is therefore recorded as credited —
// which is true — and shows up as a second credited row sharing one
// payment id. A status nothing can produce would be a lie about what this
// system records.
const (
	// GatewayCreated: an order/payment link was created.
	GatewayCreated = "created"
	// GatewayCredited: a capture that moved money into a wallet.
	GatewayCredited = "credited"
	// GatewayIgnored: an authentic event this system does not act on —
	// order.paid, refund.*, payment.failed.
	GatewayIgnored = "ignored"
	// GatewayRejected: failed the signature check, or was unreadable.
	GatewayRejected = "rejected"
	// GatewayUnmatched: authentic and captured, but names no subscriber
	// we could find. Money received and credited to nobody — the row
	// most worth having.
	GatewayUnmatched = "unmatched"
	// GatewayFailed: this system could not complete it.
	GatewayFailed = "failed"
	// GatewayUnconfigured: a callback arrived with no webhook secret set,
	// so it could not be authenticated at all.
	GatewayUnconfigured = "unconfigured"
)

// DefaultGateway is the gateway name used when none is given.
const DefaultGateway = "razorpay"

// GatewayEvent is one order or callback as it is recorded.
type GatewayEvent struct {
	Gateway string
	Kind    string
	// Event is the gateway's own event name for a callback, or
	// "order.create" for a request this system made.
	Event            string
	GatewayOrderID   string
	GatewayPaymentID string
	// SubscriberID is 0 when the event names nobody we could match.
	SubscriberID int
	// Amount is zero when there was none, or none readable.
	Amount decimal.Decimal
	Status string
	Detail string
}

// GatewayLogger records gateway events. Satisfied by
// *db.GatewayLogStore.
//
// Recording never fails the operation it describes: a payment that
// credited a wallet has credited it whether or not its log row was
// written, and refusing the credit over a logging failure would turn a
// bookkeeping problem into a lost payment. Callers log and carry on.
type GatewayLogger interface {
	RecordGatewayEvent(ctx context.Context, e GatewayEvent) error
}

// GatewayLogEntry is one stored event, as the console reads it back.
type GatewayLogEntry struct {
	ID               int64
	Gateway          string
	Kind             string
	Event            string
	GatewayOrderID   string
	GatewayPaymentID string
	SubscriberID     *int
	Username         string
	Amount           *decimal.Decimal
	Status           string
	Detail           string
	CreatedAt        time.Time
}

// Credited reports whether this event moved money into a wallet.
func (e GatewayLogEntry) Credited() bool { return e.Status == GatewayCredited }

// NeedsAttention reports whether this event is one a reconciliation
// should look at: authentic money that did not reach a wallet.
//
// 'ignored' is excluded deliberately: an event the gateway sends that
// this system does not act on is working as intended, and those rows
// would drown the ones that are not.
func (e GatewayLogEntry) NeedsAttention() bool {
	switch e.Status {
	case GatewayUnmatched, GatewayFailed, GatewayRejected, GatewayUnconfigured:
		return true
	}
	return false
}

// StatusLabel is the operator-facing status.
func (e GatewayLogEntry) StatusLabel() string {
	if l, ok := gatewayStatusLabels[e.Status]; ok {
		return l
	}
	return e.Status
}

var gatewayStatusLabels = map[string]string{
	GatewayCreated:      "Payment link created",
	GatewayCredited:     "Credited to the wallet",
	GatewayIgnored:      "Acknowledged, no action needed",
	GatewayRejected:     "Rejected — failed authentication",
	GatewayUnmatched:    "Paid, but matched no subscriber",
	GatewayFailed:       "Could not be completed",
	GatewayUnconfigured: "Arrived with no webhook secret configured",
}

// GatewayStatusAdvice says what each outcome means for an operator,
// because the status says what happened and not what to do.
var GatewayStatusAdvice = map[string]string{
	GatewayUnmatched: "The gateway took the customer's money and this system could not tell whose it was. " +
		"Find the payment in the gateway dashboard, identify the subscriber, and record the payment against " +
		"them by hand. Order creation should always set subscriber_id, so this also means a bug or a payment " +
		"link made outside this system.",
	GatewayFailed: "Authentic and unprocessed — usually the database being briefly unavailable. The gateway " +
		"retries, so this often resolves itself; check for a later 'credited' row with the same payment id.",
	GatewayRejected: "Failed the signature check. A few are normal internet noise; a run of them means either " +
		"the webhook secret here and at the gateway disagree, or somebody is probing the endpoint.",
	GatewayUnconfigured: "A callback arrived and could not be authenticated because no webhook secret is set. " +
		"Any payment it described has not been credited. Set RAZORPAY_WEBHOOK_SECRET and reconcile by hand.",
}

// GatewayLogFilter narrows a listing of gateway events.
type GatewayLogFilter struct {
	Status    string // "" for any
	Kind      string // "" for any
	PaymentID string // "" for any
	// ProblemsOnly shows only what NeedsAttention reports.
	ProblemsOnly bool
	SubscriberID int
	From         *time.Time
	To           *time.Time
	Limit        int
}

// GatewayStatusCount is one status and how often it occurred.
type GatewayStatusCount struct {
	Status string
	Count  int
}
