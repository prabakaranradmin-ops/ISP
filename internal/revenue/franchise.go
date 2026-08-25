package revenue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// FranchiseQuerier is the DB interface for franchise isolation and commission.
type FranchiseQuerier interface {
	GetFranchiseByID(ctx context.Context, franchiseID int) (*Franchise, error)
	CalculateAndStoreLCOCommission(ctx context.Context, entry LCOCommissionEntry) error
	// GetSubscriberFranchiseID reports which franchise (if any) a subscriber
	// belongs to, for SettleCommissionForRecharge to decide whether a
	// recharge owes a partner commission at all.
	GetSubscriberFranchiseID(ctx context.Context, subscriberID int) (*int, error)
}

// Franchise represents a franchise record.
type Franchise struct {
	ID                int
	Name              string
	CommissionRatePct decimal.Decimal
	Status            string
}

// FranchiseRecord is the API representation of a franchise partner. Distinct
// from Franchise, which carries only what the commission calculation needs —
// this adds the contact and audit fields an operator listing partners wants
// to see, and renders as JSON.
type FranchiseRecord struct {
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	OwnerName         string    `json:"owner_name"`
	MobileNumber      string    `json:"mobile_number"`
	CommissionRatePct string    `json:"commission_rate_pct"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

// FranchisePnL is one partner's profit-and-loss summary (FR-FRN-003).
//
// Money is carried as a string for the same reason every other monetary value
// in this codebase is: NUMERIC read through float64 cannot represent 71.91,
// and a P&L that does not reconcile to the paisa is not a P&L.
type FranchisePnL struct {
	FranchiseID       int    `json:"franchise_id"`
	FranchiseName     string `json:"franchise_name"`
	Status            string `json:"status"`
	CommissionRatePct string `json:"commission_rate_pct"`
	SubscriberCount   int    `json:"subscriber_count"`
	RechargeCount     int    `json:"recharge_count"`
	TotalRecharges    string `json:"total_recharges"`
	CommissionEarned  string `json:"commission_earned"`
	// NetToISP is what the ISP keeps after the partner's commission — the
	// number the "consolidated P&L across all LCO partners" in CRD-FRN-001
	// actually asks for, rather than leaving the reader to subtract.
	NetToISP string `json:"net_to_isp"`
}

// ConsolidatedPnL aggregates every partner (FR-FRN-003).
type ConsolidatedPnL struct {
	Partners         []FranchisePnL `json:"partners"`
	TotalRecharges   string         `json:"total_recharges"`
	CommissionEarned string         `json:"commission_earned"`
	NetToISP         string         `json:"net_to_isp"`
}

// CreateFranchiseRequest is the onboarding payload (FR-FRN-006).
type CreateFranchiseRequest struct {
	Name              string `json:"name"`
	OwnerName         string `json:"owner_name"`
	MobileNumber      string `json:"mobile_number"`
	CommissionRatePct string `json:"commission_rate_pct"`
}

// LCOCommissionEntry is one lco_ledger row: the recharge that earned the
// commission plus the computed commission itself.
type LCOCommissionEntry struct {
	FranchiseID      int
	SubscriberID     int
	RechargeAmount   decimal.Decimal
	CommissionRate   decimal.Decimal
	CommissionAmount decimal.Decimal
	TransactionRef   string
}

// CalculateLCOCommission computes and persists the franchise commission for a recharge.
//
// FR: FR-FRN-002 | DBD §6.2 lco_ledger
func CalculateLCOCommission(ctx context.Context, db FranchiseQuerier, entry LCOCommissionEntry) (decimal.Decimal, error) {
	franchise, err := db.GetFranchiseByID(ctx, entry.FranchiseID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("revenue: get franchise %d: %w", entry.FranchiseID, err)
	}
	if franchise.Status != "active" {
		return decimal.Zero, fmt.Errorf("revenue: franchise %d is not active", entry.FranchiseID)
	}

	commission := entry.RechargeAmount.
		Mul(franchise.CommissionRatePct).
		Div(decimal.NewFromInt(100)).
		Round(2)

	ledgerRow := LCOCommissionEntry{
		FranchiseID:      entry.FranchiseID,
		SubscriberID:     entry.SubscriberID,
		RechargeAmount:   entry.RechargeAmount,
		CommissionRate:   franchise.CommissionRatePct,
		CommissionAmount: commission,
		TransactionRef:   entry.TransactionRef,
	}
	if err := db.CalculateAndStoreLCOCommission(ctx, ledgerRow); err != nil {
		return decimal.Zero, fmt.Errorf("revenue: store lco commission: %w", err)
	}
	return commission, nil
}

// SettleCommissionForRecharge is CRD-EXP-006 Phase 2's franchise-commission
// integration: called after a wallet recharge has already committed, it
// looks up whether the recharged subscriber belongs to a franchise and, if
// so, computes and posts that partner's commission (lco_ledger row plus its
// GL entry — see db.RevenueStore.CalculateAndStoreLCOCommission).
//
// Deliberately best-effort and error-swallowing, matching this codebase's
// established pattern for a derived write that follows an already-correct
// primary one (e.g. RadiusDaemon's live-session tracking after
// Accounting-Start persists): the recharge itself already succeeded and
// must never be undone, retried, or reported as failed because a
// downstream commission calculation could not run. A failure here is
// logged loudly enough for ops to reconcile, not silently dropped.
func SettleCommissionForRecharge(ctx context.Context, db FranchiseQuerier, subscriberID int, amount decimal.Decimal, transactionRef string) {
	franchiseID, err := db.GetSubscriberFranchiseID(ctx, subscriberID)
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).
			Msg("revenue: resolve subscriber franchise for commission settlement failed")
		return
	}
	if franchiseID == nil {
		return
	}

	if _, err := CalculateLCOCommission(ctx, db, LCOCommissionEntry{
		FranchiseID:    *franchiseID,
		SubscriberID:   subscriberID,
		RechargeAmount: amount,
		TransactionRef: transactionRef,
	}); err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).Int("franchise_id", *franchiseID).
			Msg("revenue: franchise commission settlement failed")
	}
}

// franchiseScopedRoles are the roles whose visibility is confined to a single
// franchise. Every other role is either ISP-wide staff or has no data access.
var franchiseScopedRoles = map[string]bool{
	"lco":             true,
	"franchise_admin": true,
	"franchise_staff": true,
}

// Scope carries the franchise restriction resolved for a request.
// A nil FranchiseID means unrestricted (ISP-wide) visibility.
type Scope struct {
	FranchiseID *int
}

type scopeCtxKey struct{}

// FranchiseMiddleware resolves the franchise restriction for the caller and
// injects it into the request context, so downstream queries cannot accidentally
// read across franchise boundaries.
//
// A franchise-scoped role presenting a token without a franchise_id is rejected
// rather than silently granted ISP-wide visibility.
//
// FR: FR-FRN-001 | DDS §5.7
func FranchiseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := middleware.RoleFromContext(r.Context())
		franchiseID := middleware.FranchiseIDFromContext(r.Context())

		scope := Scope{}
		if franchiseScopedRoles[role] {
			if franchiseID == 0 {
				http.Error(w, "forbidden: token has no franchise binding", http.StatusForbidden)
				return
			}
			id := franchiseID
			scope.FranchiseID = &id
		}

		ctx := context.WithValue(r.Context(), scopeCtxKey{}, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ScopeFromContext returns the franchise scope injected by FranchiseMiddleware.
func ScopeFromContext(ctx context.Context) Scope {
	scope, _ := ctx.Value(scopeCtxKey{}).(Scope)
	return scope
}

// SubscriberRow is the franchise-scoped subscriber projection.
type SubscriberRow struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	FranchiseID *int   `json:"franchise_id,omitempty"`
}

// SubscriberLister returns subscribers visible within a franchise scope.
// A nil franchiseID must return every subscriber.
type SubscriberLister interface {
	ListSubscribers(ctx context.Context, franchiseID *int) ([]SubscriberRow, error)
}

// ListSubscribersHandler serves GET /api/v1/subscribers scoped to the caller's
// franchise. It must be mounted behind FranchiseMiddleware.
//
// FR: FR-FRN-001
func ListSubscribersHandler(db SubscriberLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope := ScopeFromContext(r.Context())
		rows, err := db.ListSubscribers(r.Context(), scope.FranchiseID)
		if err != nil {
			http.Error(w, "failed to list subscribers", http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []SubscriberRow{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows) //nolint:errcheck
	}
}
