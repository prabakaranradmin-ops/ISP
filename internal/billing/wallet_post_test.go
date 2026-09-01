package billing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// fakeWalletQuerier is a package-level test double for billing.WalletQuerier
// — a configurable starting balance, plus a record of exactly what
// RecordRecharge was asked to write, so a test can assert both directions of
// Post without touching a real database.
type fakeWalletQuerier struct {
	balance      decimal.Decimal
	byToken      map[string]*billing.Transaction
	lastPosting  billing.RechargePosting
	postingCalls int
	nextID       int
	recordErr    error
}

func newFakeWalletQuerier(balance string) *fakeWalletQuerier {
	return &fakeWalletQuerier{balance: mustDecimalW(balance), byToken: map[string]*billing.Transaction{}, nextID: 1}
}

func mustDecimalW(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (f *fakeWalletQuerier) GetTransactionByToken(_ context.Context, token string) (*billing.Transaction, error) {
	return f.byToken[token], nil
}

func (f *fakeWalletQuerier) GetSubscriberBalance(context.Context, int) (decimal.Decimal, error) {
	return f.balance, nil
}

func (f *fakeWalletQuerier) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	f.postingCalls++
	f.lastPosting = p
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	f.balance = p.NewBalance
	tx := &billing.Transaction{
		ID: f.nextID, SubscriberID: p.Credit.SubscriberID, EntryType: p.Credit.EntryType,
		Amount: p.Credit.Amount, BalanceAfter: p.NewBalance, Description: p.Credit.Description,
	}
	if p.Credit.TransactionToken != nil {
		tx.TransactionToken = *p.Credit.TransactionToken
		f.byToken[tx.TransactionToken] = tx
	}
	f.nextID++
	return tx, nil
}

// TestWalletService_Post_CreditIncreasesBalanceAndReportsWalletLeg verifies a
// credit posting (e.g. a staff goodwill adjustment) increases the balance and
// that the returned Transaction describes the wallet leg — not the counter
// leg RecordRecharge's Debit/Credit struct fields might suggest at a glance.
func TestWalletService_Post_CreditIncreasesBalanceAndReportsWalletLeg(t *testing.T) {
	fake := newFakeWalletQuerier("100.00")
	svc := billing.NewWalletService(fake)

	tx, err := svc.Post(context.Background(), billing.PostRequest{
		SubscriberID: 1, Amount: mustDecimalW("50.00"), Direction: "credit",
		CounterAccount: billing.AccountAdjustmentClearing, Description: "goodwill credit",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !tx.BalanceAfter.Equal(mustDecimalW("150.00")) {
		t.Errorf("balance after credit: want 150.00, got %s", tx.BalanceAfter)
	}
	if tx.EntryType != "credit" {
		t.Errorf("returned transaction entry_type: want credit (the wallet leg), got %q", tx.EntryType)
	}
	if fake.lastPosting.Credit.Account != billing.AccountSubscriberWallet {
		t.Errorf("wallet leg must be posted against subscriber_wallet, got %q", fake.lastPosting.Credit.Account)
	}
	if fake.lastPosting.Debit.Account != billing.AccountAdjustmentClearing {
		t.Errorf("counter leg must be posted against the requested CounterAccount, got %q", fake.lastPosting.Debit.Account)
	}
}

// TestWalletService_Post_DebitDecreasesBalanceAndReportsWalletLeg is the
// mirror case: a debit posting (auto-renewal, adjustment, refund) must still
// report the wallet-affecting row — this is the case the naive "Credit field
// always means credit" reading of RecordRecharge would get backwards.
func TestWalletService_Post_DebitDecreasesBalanceAndReportsWalletLeg(t *testing.T) {
	fake := newFakeWalletQuerier("100.00")
	svc := billing.NewWalletService(fake)

	tx, err := svc.Post(context.Background(), billing.PostRequest{
		SubscriberID: 1, Amount: mustDecimalW("30.00"), Direction: "debit",
		CounterAccount: billing.AccountRevenueClearing, Description: "auto-renewal: Basic",
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !tx.BalanceAfter.Equal(mustDecimalW("70.00")) {
		t.Errorf("balance after debit: want 70.00, got %s", tx.BalanceAfter)
	}
	if tx.EntryType != "debit" {
		t.Errorf("returned transaction entry_type: want debit (the wallet leg), got %q", tx.EntryType)
	}
	if fake.lastPosting.Credit.Account != billing.AccountSubscriberWallet {
		t.Errorf("wallet leg (in the Credit struct field, regardless of its debit EntryType) must be subscriber_wallet, got %q", fake.lastPosting.Credit.Account)
	}
	if fake.lastPosting.Credit.EntryType != "debit" {
		t.Errorf("wallet leg's own entry_type must be debit, got %q", fake.lastPosting.Credit.EntryType)
	}
}

// TestWalletService_Post_DebitExceedingBalanceIsRejected is the overdraft
// guard: a debit larger than the current balance must never reach
// RecordRecharge at all.
func TestWalletService_Post_DebitExceedingBalanceIsRejected(t *testing.T) {
	fake := newFakeWalletQuerier("20.00")
	svc := billing.NewWalletService(fake)

	_, err := svc.Post(context.Background(), billing.PostRequest{
		SubscriberID: 1, Amount: mustDecimalW("50.00"), Direction: "debit",
		CounterAccount: billing.AccountAdjustmentClearing, Description: "refund",
	})
	if !errors.Is(err, billing.ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	if fake.postingCalls != 0 {
		t.Errorf("a rejected debit must never reach RecordRecharge, got %d calls", fake.postingCalls)
	}
}

// TestWalletService_Post_DebitToExactlyZeroIsAllowed verifies the boundary:
// spending down to exactly zero is legal, only going negative is not.
func TestWalletService_Post_DebitToExactlyZeroIsAllowed(t *testing.T) {
	fake := newFakeWalletQuerier("50.00")
	svc := billing.NewWalletService(fake)

	tx, err := svc.Post(context.Background(), billing.PostRequest{
		SubscriberID: 1, Amount: mustDecimalW("50.00"), Direction: "debit",
		CounterAccount: billing.AccountRevenueClearing, Description: "auto-renewal",
	})
	if err != nil {
		t.Fatalf("a debit landing exactly on zero must be allowed: %v", err)
	}
	if !tx.BalanceAfter.IsZero() {
		t.Errorf("balance after: want 0, got %s", tx.BalanceAfter)
	}
}

func TestWalletService_Post_RejectsNonPositiveAmount(t *testing.T) {
	svc := billing.NewWalletService(newFakeWalletQuerier("100.00"))
	for _, amt := range []string{"0", "-10.00"} {
		_, err := svc.Post(context.Background(), billing.PostRequest{
			SubscriberID: 1, Amount: mustDecimalW(amt), Direction: "credit",
			CounterAccount: billing.AccountAdjustmentClearing,
		})
		if err == nil {
			t.Errorf("amount %s: want an error, got nil", amt)
		}
	}
}

func TestWalletService_Post_RejectsInvalidDirection(t *testing.T) {
	svc := billing.NewWalletService(newFakeWalletQuerier("100.00"))
	_, err := svc.Post(context.Background(), billing.PostRequest{
		SubscriberID: 1, Amount: mustDecimalW("10.00"), Direction: "sideways",
		CounterAccount: billing.AccountAdjustmentClearing,
	})
	if err == nil {
		t.Error("want an error for an invalid direction")
	}
}

// TestWalletService_Post_IdempotentOnToken verifies a replayed transaction
// token returns the original posting rather than moving money twice —
// the same idempotency guarantee Recharge already carries, extended to Post.
func TestWalletService_Post_IdempotentOnToken(t *testing.T) {
	fake := newFakeWalletQuerier("100.00")
	svc := billing.NewWalletService(fake)

	req := billing.PostRequest{
		SubscriberID: 1, Amount: mustDecimalW("25.00"), Direction: "debit",
		CounterAccount: billing.AccountAdjustmentClearing, TransactionToken: "refund-abc",
	}
	first, err := svc.Post(context.Background(), req)
	if err != nil {
		t.Fatalf("first Post: %v", err)
	}
	second, err := svc.Post(context.Background(), req)
	if err != nil {
		t.Fatalf("second Post: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("a replayed token must return the original transaction, got a different id")
	}
	if fake.postingCalls != 1 {
		t.Errorf("a replayed token must not move money a second time, got %d RecordRecharge calls", fake.postingCalls)
	}
}

// TestGLAccountCode_EveryWalletAccountIsMapped guards the failure mode that
// makes a missing mapping expensive rather than merely wrong.
//
// db.postWalletGLEntry refuses to write a wallet posting at all when either
// leg's account has no GL code — fail-closed, because a wallet subledger and
// a general ledger that disagree is worse than a rejected transaction. The
// cost of that choice is that an account constant added here and forgotten
// in GLAccountCode does not degrade the ledger quietly; it breaks recharges
// outright, in production, for whichever flow uses the new account.
//
// Enumerated by hand rather than by reflection: Go has no way to iterate a
// package's untyped string constants, so the real guard is that adding one
// to wallet.go and not to this list is a visible omission in review.
func TestGLAccountCode_EveryWalletAccountIsMapped(t *testing.T) {
	for _, account := range []string{
		billing.AccountSubscriberWallet,
		billing.AccountGatewayClearing,
		billing.AccountRevenueClearing,
		billing.AccountAdjustmentClearing,
	} {
		if code := billing.GLAccountCode(account); code == "" {
			t.Errorf("wallet account %q has no chart-of-accounts code: a posting against it "+
				"would fail the whole transaction, not just the ledger entry", account)
		}
	}
}

// TestGLAccountCode_UnknownAccountIsRejected pins the fail-closed half: an
// account this mapping does not know must return empty so the caller aborts,
// never a plausible-looking default that would post real money to the wrong
// account.
func TestGLAccountCode_UnknownAccountIsRejected(t *testing.T) {
	if code := billing.GLAccountCode("some_account_added_later"); code != "" {
		t.Errorf("unknown account mapped to %q; it must return empty so the posting is refused", code)
	}
}
