//go:build integration

// Archived document retrieval — FR-DOC-003.
//
// internal/archive already tests that Retrieve verifies and refuses a
// corrupt document; those tests predate this requirement. What is tested
// here is the layer that was missing: that an archive row cannot be served
// under a subscriber it does not belong to, and that retrieval cannot be
// reached without the verification, since a path that skipped it would leave
// NFR-DUR-002's gap open while looking closed.
package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/archive"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

type stubDocArchives struct {
	rows   map[int64]*archive.Record
	owners map[int64]int
	list   []archive.SubscriberDocument
	err    error
}

func (s *stubDocArchives) GetArchiveByID(_ context.Context, id int64) (*archive.Record, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows[id], nil
}

func (s *stubDocArchives) ListSubscriberArchives(_ context.Context, subscriberID int) ([]archive.SubscriberDocument, error) {
	return s.list, nil
}

func (s *stubDocArchives) SubscriberForArchive(_ context.Context, id int64) (int, error) {
	return s.owners[id], nil
}

// countingRetriever records whether verification was reached at all.
type countingRetriever struct {
	calls int
	body  []byte
	err   error
}

func (r *countingRetriever) Retrieve(_ context.Context, rec archive.Record) ([]byte, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.body, nil
}

func docHandler(t *testing.T, store *stubDocArchives, ret *countingRetriever) *api.Handler {
	t.Helper()
	return api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Wallet:       billing.NewWalletService(&stubWallet{}),
		DocArchives:  store,
		DocRetriever: ret,
	})
}

func liveRecord(id int64, kind string, entityID int) *archive.Record {
	return &archive.Record{
		ID: id, DocKind: kind, EntityID: entityID,
		StorageBackend: archive.BackendLocal,
		StorageURL:     "file:///archive/x",
		ChecksumSHA256: strings.Repeat("a", 64),
		SizeBytes:      1024,
		ArchivedAt:     time.Now().Add(-time.Hour),
	}
}

// The check that stops an archive id being used to reach somebody else's
// documents. For a KYC scan that is another customer's identity papers.
func TestFR_DOC_003_ADocumentIsNotServedUnderTheWrongSubscriber(t *testing.T) {
	store := &stubDocArchives{
		rows: map[int64]*archive.Record{
			1: liveRecord(1, archive.KindInvoice, 100),
			2: liveRecord(2, archive.KindKYC, 200),
			// A report belongs to no subscriber at all.
			3: liveRecord(3, archive.KindReport, 300),
		},
		owners: map[int64]int{1: 42, 2: 42, 3: 0},
	}
	ret := &countingRetriever{body: []byte("bytes")}
	h := docHandler(t, store, ret)
	ctx := context.Background()

	// Theirs: served.
	if _, _, err := h.RetrieveSubscriberDocument(ctx, 42, 1); err != nil {
		t.Fatalf("the subscriber's own document was refused: %v", err)
	}

	// Somebody else's: refused, and the verification must not even be
	// reached — the ownership check comes first so a mismatch cannot be
	// inferred from timing or from a corruption error about a document the
	// caller should not know exists.
	before := ret.calls
	if _, _, err := h.RetrieveSubscriberDocument(ctx, 99, 1); !errors.Is(err, archive.ErrNotTheirs) {
		t.Errorf("another subscriber's document gave %v, want ErrNotTheirs", err)
	}
	if ret.calls != before {
		t.Error("the document was fetched before the ownership check refused it")
	}

	// A report, which belongs to nobody, must not be reachable by naming
	// any subscriber: owner 0 is "not subscriber-scoped", not a wildcard.
	if _, _, err := h.RetrieveSubscriberDocument(ctx, 42, 3); !errors.Is(err, archive.ErrNotTheirs) {
		t.Errorf("a report was served under a subscriber: %v", err)
	}
}

func TestFR_DOC_003_AMissingOrPurgedDocumentIsNotFound(t *testing.T) {
	store := &stubDocArchives{
		rows:   map[int64]*archive.Record{},
		owners: map[int64]int{},
	}
	ret := &countingRetriever{body: []byte("bytes")}
	h := docHandler(t, store, ret)

	if _, _, err := h.RetrieveSubscriberDocument(context.Background(), 42, 7); !errors.Is(err, archive.ErrNotFound) {
		t.Errorf("an absent row gave %v, want ErrNotFound", err)
	}
	if ret.calls != 0 {
		t.Error("an absent row still reached storage")
	}
}

// Verification cannot be bypassed: every retrieval goes through the
// retriever, which is the thing that compares the hash. A path that returned
// bytes without calling it would leave NFR-DUR-002 open while appearing
// closed.
func TestFR_DOC_003_EveryRetrievalGoesThroughVerification(t *testing.T) {
	store := &stubDocArchives{
		rows:   map[int64]*archive.Record{1: liveRecord(1, archive.KindInvoice, 100)},
		owners: map[int64]int{1: 42},
	}
	ret := &countingRetriever{body: []byte("bytes")}
	h := docHandler(t, store, ret)
	ctx := context.Background()

	if _, _, err := h.RetrieveSubscriberDocument(ctx, 42, 1); err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if ret.calls != 1 {
		t.Fatalf("retrieval called the verifier %d times, want 1", ret.calls)
	}

	// And the verify-only path uses the same route rather than a second one
	// that could skip the comparison.
	if _, err := h.VerifySubscriberDocument(ctx, 42, 1); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ret.calls != 2 {
		t.Errorf("verifying did not go through the verifier (calls=%d)", ret.calls)
	}
}

// A corruption error must come back unchanged rather than flattened, because
// the console tells an operator "this copy is damaged" only if it can still
// recognise it.
func TestFR_DOC_003_ACorruptionErrorSurvivesTheServiceLayer(t *testing.T) {
	store := &stubDocArchives{
		rows:   map[int64]*archive.Record{1: liveRecord(1, archive.KindInvoice, 100)},
		owners: map[int64]int{1: 42},
	}
	ret := &countingRetriever{err: &archive.ErrChecksumMismatch{
		DocKind: archive.KindInvoice, EntityID: 100,
		Want: strings.Repeat("a", 64), Got: strings.Repeat("b", 64),
	}}
	h := docHandler(t, store, ret)

	body, rec, err := h.RetrieveSubscriberDocument(context.Background(), 42, 1)
	var mismatch *archive.ErrChecksumMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("a corrupt document gave %v, which the console cannot recognise as corruption", err)
	}
	if mismatch.Want == mismatch.Got {
		t.Error("the error carries no useful hash pair")
	}
	// No bytes, and no record either: handing back the row alongside a
	// corruption error invites a caller to treat the read as partially
	// successful.
	if body != nil {
		t.Errorf("a corrupt document returned %d bytes", len(body))
	}
	if rec != nil {
		t.Error("a corrupt document returned its record alongside the error")
	}
}

// With nothing wired the service refuses and says which capability is
// missing, rather than panicking on a nil retriever.
func TestFR_DOC_003_NoArchiveWiredIsRefusedNotFatal(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}),
	})
	ctx := context.Background()

	if _, _, err := h.RetrieveSubscriberDocument(ctx, 42, 1); !errors.Is(err, api.ErrDocumentsUnwired) {
		t.Errorf("retrieving with no archive wired gave %v, want ErrDocumentsUnwired", err)
	}
	if _, err := h.ListSubscriberDocuments(ctx, 42); !errors.Is(err, api.ErrDocumentsUnwired) {
		t.Errorf("listing gave %v, want ErrDocumentsUnwired", err)
	}
	// Archiving an invoice with no archiver is deliberately NOT an error:
	// the caller is serving a download and archival being off must not turn
	// that into a failure.
	if err := h.ArchiveInvoicePDF(ctx, 1, []byte("pdf")); err != nil {
		t.Errorf("archiving with no archiver configured gave %v, want nil", err)
	}
}
