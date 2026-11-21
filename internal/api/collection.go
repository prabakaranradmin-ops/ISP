package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Field payment collection — FR-COL-001 | migration 068.
// The domain rules live in internal/billing/collection.go.

// CollectionStore manages collector assignments and reads their rounds.
// Satisfied by *db.BillingStore.
type CollectionStore interface {
	AssignCollector(ctx context.Context, subscriberID int, collectorID *int) error
	ListCollectionRound(ctx context.Context, collectorID, windowDays int) ([]billing.CollectionItem, error)
	CollectorFor(ctx context.Context, subscriberID int) (*int, error)
	ListCollectors(ctx context.Context) ([]billing.CollectorLoad, error)
	CollectorTakings(ctx context.Context, from, to time.Time) ([]billing.CollectorTakings, error)
	StaffIDForUsername(ctx context.Context, username string) (int, error)
}

// ErrCollectionUnwired is returned when no collection store is configured.
var ErrCollectionUnwired = errors.New("api: field collection not configured")

// AssignCollector puts a subscriber on a collector's round, or takes them
// off it with a nil collectorID.
func (h *Handler) AssignCollector(ctx context.Context, subscriberID int, collectorID *int) error {
	if h.collection == nil {
		return ErrCollectionUnwired
	}
	if err := h.collection.AssignCollector(ctx, subscriberID, collectorID); err != nil {
		return err
	}
	detail := map[string]any{"subscriber_id": subscriberID}
	if collectorID == nil {
		middleware.Audit(ctx, "subscriber.collector_cleared", strconv.Itoa(subscriberID), detail)
		return nil
	}
	detail["collector_id"] = *collectorID
	middleware.Audit(ctx, "subscriber.collector_assigned", strconv.Itoa(subscriberID), detail)
	return nil
}

// CollectionRoundFor returns the round for one collector.
func (h *Handler) CollectionRoundFor(ctx context.Context, collectorID, windowDays int) ([]billing.CollectionItem, error) {
	if h.collection == nil {
		return nil, ErrCollectionUnwired
	}
	return h.collection.ListCollectionRound(ctx, collectorID, windowDays)
}

// MyCollectionRound returns the signed-in collector's own round.
func (h *Handler) MyCollectionRound(ctx context.Context, windowDays int) ([]billing.CollectionItem, error) {
	if h.collection == nil {
		return nil, ErrCollectionUnwired
	}
	id, err := h.collection.StaffIDForUsername(ctx, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return h.collection.ListCollectionRound(ctx, id, windowDays)
}

// ListCollectors returns the staff who can be given a round, with their
// current load.
func (h *Handler) ListCollectors(ctx context.Context) ([]billing.CollectorLoad, error) {
	if h.collection == nil {
		return nil, ErrCollectionUnwired
	}
	return h.collection.ListCollectors(ctx)
}

// CollectorTakings sums what each collector recorded over a period.
func (h *Handler) CollectorTakings(ctx context.Context, from, to time.Time) ([]billing.CollectorTakings, error) {
	if h.collection == nil {
		return nil, ErrCollectionUnwired
	}
	return h.collection.CollectorTakings(ctx, from, to)
}

// RecordCollection records a payment a collector took at the door —
// FR-COL-001, through FR-BIL-013's wallet credit.
//
// The write is scoped to the collector's own round. Field collectors are not
// billing staff, and widening the general payment gate to them would let any
// of them record a payment for any subscriber from the subscriber page —
// a far larger grant than this requirement asks for. Scoping it here is what
// makes the assignment a permission rather than a convenience, and is why
// this exists instead of a role check alone.
//
// unscoped is true for the owner and billing staff, who may record a payment
// for anybody and already can from the subscriber's page; passing through
// here keeps one path rather than two that could validate differently.
//
// The payment itself is the existing wallet credit, so the method, the cheque
// details, the staleness rule and recorded_by all come from FR-BIL-013
// unchanged. There is deliberately no second money table: a collections
// ledger would be a parallel record of the same rupees, free to disagree with
// the receipt the customer was given.
func (h *Handler) RecordCollection(ctx context.Context, subscriberID int, amount decimal.Decimal, pd billing.PaymentDetail, unscoped bool) (*billing.Transaction, error) {
	if h.collection == nil {
		return nil, ErrCollectionUnwired
	}
	if !unscoped {
		who := middleware.SubjectFromContext(ctx)
		if who == "" {
			return nil, fmt.Errorf("api: a collection must record who took it")
		}
		staffID, err := h.collection.StaffIDForUsername(ctx, who)
		if err != nil {
			return nil, err
		}
		assigned, err := h.collection.CollectorFor(ctx, subscriberID)
		if err != nil {
			return nil, err
		}
		if assigned == nil || *assigned != staffID {
			return nil, billing.ErrNotYourCollection
		}
	}

	tx, err := h.RecordPayment(ctx, subscriberID, amount, pd)
	if err != nil {
		return nil, err
	}
	// Audited as a collection on top of the payment's own audit entry, so
	// "what did this collector take today" is answerable from the trail and
	// not only from the ledger.
	middleware.Audit(ctx, "subscriber.collection_recorded", strconv.Itoa(subscriberID), map[string]any{
		"subscriber_id": subscriberID,
		"amount":        amount.StringFixed(2),
		"method":        pd.Method,
		"at_the_door":   !unscoped,
	})
	return tx, nil
}
