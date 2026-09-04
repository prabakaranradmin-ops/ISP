package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// Ledger account labels for the two legs a recharge posts.
const (
	AccountSubscriberWallet = "subscriber_wallet"
	AccountGatewayClearing  = "payment_gateway_clearing"
	// AccountRevenueClearing is the counter-leg for a plan charge consumed
	// from the wallet (auto-renewal) — MDS §4.14.
	AccountRevenueClearing = "revenue_clearing"
	// AccountAdjustmentClearing is the counter-leg for staff-issued wallet
	// credits, debits and refunds — MDS §4.14.
	AccountAdjustmentClearing = "adjustment_clearing"
)

// GLAccountCode maps a wallet ledger leg's Account label to the
// chart-of-accounts code its GL journal line posts against (migration 043,
// plus 045 for AccountAdjustmentClearing's account). Every wallet_ledgers
// leg's own EntryType ("debit"/"credit") already IS its GL journal line's
// debit/credit side — Recharge and Post construct both legs of every
// posting as a matched pair (one increases a GL-debit-normal account
// exactly when the other increases a GL-credit-normal one), so nothing
// here needs to flip a direction, only resolve which account.
func GLAccountCode(account string) string {
	switch account {
	case AccountSubscriberWallet:
		return "1200" // Subscriber Wallet Liability
	case AccountGatewayClearing:
		return "1000" // Cash / Bank
	case AccountRevenueClearing:
		return "4000" // Subscription Revenue
	case AccountAdjustmentClearing:
		return "5200" // Wallet Adjustments & Refunds
	default:
		return ""
	}
}

// GLAccountGSTPayable is the output-tax liability a plan charge creates
// (migration 047). It has no wallet-ledger counterpart and so is absent from
// GLAccountCode above: the wallet subledger records movement of the
// subscriber's money, and the tax split is a general-ledger concern — see
// splitTaxLeg in internal/db/billing.go for why the split lives there and
// not in wallet_ledgers.
const GLAccountGSTPayable = "2200"

// ErrInsufficientBalance is returned by Post when a debit would take the
// wallet below zero. The application-level check that returns this is the
// normal path (a clean 4xx for the caller); a DB-level CHECK constraint on
// subscribers.wallet_balance is the backstop for the race this check cannot
// close on its own (two concurrent debits both reading the same starting
// balance) — see MDS §4.14.
var ErrInsufficientBalance = errors.New("billing: insufficient wallet balance")

// WalletQuerier is the DB interface required by WalletService.
type WalletQuerier interface {
	GetTransactionByToken(ctx context.Context, token string) (*Transaction, error)
	// RecordRecharge must persist both ledger legs and the new wallet balance in
	// a single DB transaction. Splitting them would let a crash between the two
	// leave the ledger and subscribers.wallet_balance permanently disagreeing,
	// which the nightly reconciliation (FR-REV-002) would then report as variance.
	RecordRecharge(ctx context.Context, p RechargePosting) (*Transaction, error)
	GetSubscriberBalance(ctx context.Context, subscriberID int) (decimal.Decimal, error)
}

// Transaction represents a completed wallet ledger entry.
type Transaction struct {
	ID               int
	SubscriberID     int
	EntryType        string
	Amount           decimal.Decimal
	BalanceAfter     decimal.Decimal
	TransactionToken string
	Description      string
}

// WalletEntry is one leg of a ledger posting.
type WalletEntry struct {
	SubscriberID     int
	FranchiseID      *int
	Account          string // subscriber_wallet | payment_gateway_clearing | revenue_clearing | adjustment_clearing
	EntryType        string // credit | debit
	Amount           decimal.Decimal
	BalanceAfter     decimal.Decimal
	TransactionToken *string // nil = no idempotency key (cash, or counter-leg)
	Description      string
	AdjustedBy       string // staff username; empty for non-staff-initiated legs
}

// RechargePosting is the atomic unit a recharge writes: both ledger legs plus
// the resulting wallet balance.
type RechargePosting struct {
	SubscriberID int
	Debit        WalletEntry
	Credit       WalletEntry
	NewBalance   decimal.Decimal
	// TaxAmount is the output GST contained within the counter leg's amount,
	// zero for a posting that carries no tax. It splits the counter leg's GL
	// line in two (revenue and 2200 GST Payable) without changing either
	// wallet_ledgers leg — see postWalletGLEntry.
	TaxAmount decimal.Decimal
}

// RechargeRequest carries the inputs for a subscriber wallet top-up.
type RechargeRequest struct {
	SubscriberID     int
	Amount           decimal.Decimal
	TransactionToken string // Razorpay payment_id or equivalent
	FranchiseID      *int
	Description      string
}

// WalletService performs double-entry wallet operations with idempotency.
type WalletService struct {
	db WalletQuerier
}

// NewWalletService constructs a WalletService.
func NewWalletService(db WalletQuerier) *WalletService {
	return &WalletService{db: db}
}

// Recharge credits a subscriber's wallet, posting both legs of the double entry.
// Idempotent: a second call with the same TransactionToken returns the original
// transaction without moving money again.
//
// FR: FR-BIL-003, FR-BIL-005 | DDS §5.6
func (s *WalletService) Recharge(ctx context.Context, req RechargeRequest) (*Transaction, error) {
	if req.Amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("billing: recharge amount must be positive, got %s", req.Amount)
	}

	// Idempotency check — if the token already exists, return the original transaction
	if req.TransactionToken != "" {
		existing, err := s.db.GetTransactionByToken(ctx, req.TransactionToken)
		if err == nil && existing != nil {
			return existing, nil
		}
	}

	currentBalance, err := s.db.GetSubscriberBalance(ctx, req.SubscriberID)
	if err != nil {
		return nil, fmt.Errorf("billing: get balance: %w", err)
	}
	newBalance := currentBalance.Add(req.Amount)

	// Only the credit leg carries the idempotency token: wallet_ledgers has a
	// unique index on transaction_token, so both legs holding it would collide.
	var tokenPtr *string
	if req.TransactionToken != "" {
		t := req.TransactionToken
		tokenPtr = &t
	}

	posting := RechargePosting{
		SubscriberID: req.SubscriberID,
		Debit: WalletEntry{
			SubscriberID: req.SubscriberID,
			FranchiseID:  req.FranchiseID,
			Account:      AccountGatewayClearing,
			EntryType:    "debit",
			Amount:       req.Amount,
			BalanceAfter: newBalance,
			Description:  "counter-entry: " + req.Description,
		},
		Credit: WalletEntry{
			SubscriberID:     req.SubscriberID,
			FranchiseID:      req.FranchiseID,
			Account:          AccountSubscriberWallet,
			EntryType:        "credit",
			Amount:           req.Amount,
			BalanceAfter:     newBalance,
			TransactionToken: tokenPtr,
			Description:      req.Description,
		},
		NewBalance: newBalance,
	}

	tx, err := s.db.RecordRecharge(ctx, posting)
	if err != nil {
		return nil, fmt.Errorf("billing: record recharge: %w", err)
	}
	tx.BalanceAfter = newBalance
	return tx, nil
}

// PostRequest carries the inputs for an arbitrary-direction wallet posting:
// auto-renewal charges, staff adjustments, and refunds.
type PostRequest struct {
	SubscriberID     int
	FranchiseID      *int
	Amount           decimal.Decimal // always positive; Direction says which way it moves
	Direction        string          // "credit" (wallet balance increases) or "debit" (decreases)
	CounterAccount   string          // AccountRevenueClearing or AccountAdjustmentClearing
	TransactionToken string          // optional idempotency key
	AdjustedBy       string          // staff username; empty for non-staff-initiated postings
	Description      string
	// TaxAmount is how much of Amount is output GST, for a posting that
	// charges a taxed service. Zero (the default) means the whole amount is
	// revenue, which is correct for adjustments, refunds and top-ups.
	//
	// Passing it here rather than deriving it inside Post is deliberate: the
	// caller has already computed the invoice, and re-deriving the tax from
	// a rate would let the ledger and the invoice round differently — the
	// GL must agree with the document the operator files, to the paisa.
	TaxAmount decimal.Decimal
}

// Post performs an arbitrary-direction double-entry wallet posting: a
// subscriber_wallet leg (credit or debit, per Direction) and a matching
// counter leg against CounterAccount. Unlike Recharge, which always credits
// the wallet, Post is the shared primitive auto-renewal debits, staff
// adjustments and refunds all need — MDS §4.14. It reuses the same
// WalletQuerier.RecordRecharge DB primitive Recharge uses: that method
// already writes an arbitrary debit/credit leg pair atomically with the new
// balance, so nothing about it is actually recharge-specific except the name.
func (s *WalletService) Post(ctx context.Context, req PostRequest) (*Transaction, error) {
	if req.Amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("billing: post amount must be positive, got %s", req.Amount)
	}
	if req.Direction != "credit" && req.Direction != "debit" {
		return nil, fmt.Errorf("billing: post direction must be \"credit\" or \"debit\", got %q", req.Direction)
	}
	// Rejected rather than clamped: a tax component that does not fit inside
	// the amount means the caller's invoice and its charge have diverged, and
	// silently posting either interpretation would put a wrong number in the
	// ledger. Failing the renewal leaves the subscriber's money untouched and
	// dunning to pick them up, which is recoverable; a mis-split GL entry is
	// not noticed until a reconciliation months later.
	if req.TaxAmount.IsNegative() || req.TaxAmount.GreaterThanOrEqual(req.Amount) {
		return nil, fmt.Errorf("billing: post tax amount %s must be within [0, %s)", req.TaxAmount, req.Amount)
	}

	if req.TransactionToken != "" {
		existing, err := s.db.GetTransactionByToken(ctx, req.TransactionToken)
		if err == nil && existing != nil {
			return existing, nil
		}
	}

	currentBalance, err := s.db.GetSubscriberBalance(ctx, req.SubscriberID)
	if err != nil {
		return nil, fmt.Errorf("billing: get balance: %w", err)
	}

	var newBalance decimal.Decimal
	if req.Direction == "credit" {
		newBalance = currentBalance.Add(req.Amount)
	} else {
		newBalance = currentBalance.Sub(req.Amount)
		if newBalance.IsNegative() {
			return nil, ErrInsufficientBalance
		}
	}

	var tokenPtr *string
	if req.TransactionToken != "" {
		t := req.TransactionToken
		tokenPtr = &t
	}

	// RecordRecharge inserts whatever is in the Debit/Credit struct fields
	// using each WalletEntry's own EntryType — the field names are really
	// "tokenless leg" (Debit) and "token-bearing leg that becomes the
	// returned Transaction" (Credit), not a claim that Credit.EntryType is
	// always "credit". The wallet leg always goes in Credit (so a caller
	// gets back the wallet-affecting row's own id/description, not the
	// counter leg's) with its real direction as EntryType; only which
	// physical row ends up counted as a ledger "credit" vs "debit" comes
	// from that field, same as every other caller of this DB primitive.
	counterEntryType := "debit"
	if req.Direction == "debit" {
		counterEntryType = "credit"
	}
	posting := RechargePosting{
		SubscriberID: req.SubscriberID,
		NewBalance:   newBalance,
		TaxAmount:    req.TaxAmount,
		Credit: WalletEntry{
			SubscriberID:     req.SubscriberID,
			FranchiseID:      req.FranchiseID,
			Account:          AccountSubscriberWallet,
			EntryType:        req.Direction,
			Amount:           req.Amount,
			BalanceAfter:     newBalance,
			TransactionToken: tokenPtr,
			Description:      req.Description,
			AdjustedBy:       req.AdjustedBy,
		},
		Debit: WalletEntry{
			SubscriberID: req.SubscriberID,
			FranchiseID:  req.FranchiseID,
			Account:      req.CounterAccount,
			EntryType:    counterEntryType,
			Amount:       req.Amount,
			BalanceAfter: newBalance,
			Description:  "counter-entry: " + req.Description,
		},
	}

	tx, err := s.db.RecordRecharge(ctx, posting)
	if err != nil {
		return nil, fmt.Errorf("billing: record posting: %w", err)
	}
	tx.BalanceAfter = newBalance
	return tx, nil
}
