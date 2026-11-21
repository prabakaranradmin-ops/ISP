//go:build integration

// Field payment collection — FR-COL-001.
//
// What is tested here is the security property the feature turns on: a
// collector may record a payment only for a subscriber on their own round.
// The console passes that intent as a flag; this is where the flag is acted
// on, and without a test the check could be deleted and every console test
// would still pass — they assert the flag, not the refusal.
package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// stubCollection is a CollectionStore with one assignment and one staff
// account, which is all the scoping check reads.
type stubCollection struct {
	staffIDs  map[string]int
	assigned  map[int]*int
	recorded  []int
	staffErr  error
	assignErr error
}

func (s *stubCollection) AssignCollector(_ context.Context, subscriberID int, collectorID *int) error {
	if s.assigned == nil {
		s.assigned = map[int]*int{}
	}
	s.assigned[subscriberID] = collectorID
	return nil
}

func (s *stubCollection) ListCollectionRound(context.Context, int, int) ([]billing.CollectionItem, error) {
	return nil, nil
}

func (s *stubCollection) CollectorFor(_ context.Context, subscriberID int) (*int, error) {
	if s.assignErr != nil {
		return nil, s.assignErr
	}
	return s.assigned[subscriberID], nil
}

func (s *stubCollection) ListCollectors(context.Context) ([]billing.CollectorLoad, error) {
	return nil, nil
}

func (s *stubCollection) CollectorTakings(context.Context, time.Time, time.Time) ([]billing.CollectorTakings, error) {
	return nil, nil
}

func (s *stubCollection) StaffIDForUsername(_ context.Context, username string) (int, error) {
	if s.staffErr != nil {
		return 0, s.staffErr
	}
	id, ok := s.staffIDs[username]
	if !ok {
		return 0, errors.New("db: no staff account: not found")
	}
	return id, nil
}

func collectionHandler(t *testing.T, store *stubCollection) *api.Handler {
	t.Helper()
	return api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Wallet:     billing.NewWalletService(&stubWallet{}),
		Collection: store,
	})
}

// The refusal. Without it, a field collector could record a payment against
// any subscriber in the system.
func TestFR_COL_001_ACollectorCannotRecordOutsideTheirRound(t *testing.T) {
	ravi, sunil := 10, 11
	store := &stubCollection{
		staffIDs: map[string]int{"ravi": ravi, "sunil": sunil},
		assigned: map[int]*int{
			1: &ravi,  // on ravi's round
			2: &sunil, // on somebody else's
			3: nil,    // on nobody's
		},
	}
	h := collectionHandler(t, store)
	amount := decimal.RequireFromString("799.00")
	pd := billing.PaymentDetail{Method: billing.MethodCash}

	ctx := middleware.WithSubject(context.Background(), "ravi")

	// Somebody else's subscriber.
	_, err := h.RecordCollection(ctx, 2, amount, pd, false)
	if !errors.Is(err, billing.ErrNotYourCollection) {
		t.Errorf("recording for another collector's subscriber gave %v, want ErrNotYourCollection", err)
	}

	// Nobody's subscriber. Unassigned must be refused too: "not on a round"
	// is not the same as "on mine", and treating a nil assignment as a
	// match would make every unassigned subscriber collectable by anyone.
	_, err = h.RecordCollection(ctx, 3, amount, pd, false)
	if !errors.Is(err, billing.ErrNotYourCollection) {
		t.Errorf("recording for an unassigned subscriber gave %v, want ErrNotYourCollection", err)
	}

	// A subscriber who does not exist at all.
	store.assignErr = errors.New("db: subscriber 99 not found")
	if _, err := h.RecordCollection(ctx, 99, amount, pd, false); err == nil {
		t.Error("recording for a non-existent subscriber succeeded")
	}
	store.assignErr = nil
}

// A collector with no session identity must be refused rather than treated
// as matching nothing — an empty subject with a nil assignment would
// otherwise compare as "not mine" by luck rather than by rule.
func TestFR_COL_001_AnUnidentifiedCallerIsRefused(t *testing.T) {
	store := &stubCollection{staffIDs: map[string]int{"ravi": 10}}
	h := collectionHandler(t, store)

	// No subject on the context at all.
	_, err := h.RecordCollection(context.Background(), 1,
		decimal.RequireFromString("799.00"), billing.PaymentDetail{Method: billing.MethodCash}, false)
	if err == nil {
		t.Fatal("an unidentified caller recorded a collection")
	}
	if errors.Is(err, billing.ErrNotYourCollection) {
		t.Error("an unidentified caller was refused as 'not your round' rather than as unidentified; " +
			"the message an operator sees should say which problem it is")
	}

	// A subject that is not a staff account.
	ctx := middleware.WithSubject(context.Background(), "ghost")
	if _, err := h.RecordCollection(ctx, 1,
		decimal.RequireFromString("799.00"), billing.PaymentDetail{Method: billing.MethodCash}, false); err == nil {
		t.Error("a caller with no staff account recorded a collection")
	}
}

// Owner and billing bypass the scoping, because they can already record from
// the subscriber's page. Routing them through here keeps one path rather
// than two that could validate differently.
func TestFR_COL_001_UnscopedCallersSkipTheAssignmentCheck(t *testing.T) {
	store := &stubCollection{
		staffIDs: map[string]int{"owner": 1},
		// Deliberately nobody's subscriber: an unscoped caller must not
		// need an assignment to exist.
		assigned: map[int]*int{5: nil},
	}
	h := collectionHandler(t, store)
	ctx := middleware.WithSubject(context.Background(), "owner")

	_, err := h.RecordCollection(ctx, 5, decimal.RequireFromString("799.00"),
		billing.PaymentDetail{Method: billing.MethodCash}, true)
	// The payment itself runs against the stub wallet, so what matters here
	// is only that it was not refused for scoping.
	if errors.Is(err, billing.ErrNotYourCollection) {
		t.Error("an unscoped caller was refused by the assignment check")
	}
}

// With no store wired the service refuses rather than panicking, and says
// which capability is missing.
func TestFR_COL_001_NoStoreWiredIsRefusedNotFatal(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}),
	})
	ctx := middleware.WithSubject(context.Background(), "ravi")

	if _, err := h.RecordCollection(ctx, 1, decimal.RequireFromString("799.00"),
		billing.PaymentDetail{Method: billing.MethodCash}, false); !errors.Is(err, api.ErrCollectionUnwired) {
		t.Errorf("with no store wired, recording gave %v, want ErrCollectionUnwired", err)
	}
	if _, err := h.CollectionRoundFor(ctx, 1, 7); !errors.Is(err, api.ErrCollectionUnwired) {
		t.Errorf("reading a round gave %v, want ErrCollectionUnwired", err)
	}
	if err := h.AssignCollector(ctx, 1, nil); !errors.Is(err, api.ErrCollectionUnwired) {
		t.Errorf("assigning gave %v, want ErrCollectionUnwired", err)
	}
}
