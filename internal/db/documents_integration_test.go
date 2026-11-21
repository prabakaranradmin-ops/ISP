//go:build integration

// Archived documents browsed per subscriber — FR-DOC-003 | migration 034.
//
// document_archives stores a doc_kind and an entity_id and no subscriber, so
// the attribution is resolved per kind by joining the table the entity
// actually lives in. That join is the part only a real database can check:
// get it wrong and a document is either invisible to the subscriber it
// belongs to, or — worse — listed under one it does not.
package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/archive"
)

// seedArchive writes a document_archives row directly. The Archiver's own
// write path is tested in internal/archive; what matters here is the read.
func seedArchive(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	docKind string, entityID int, retainUntil *time.Time) int64 {
	t.Helper()
	var id int64
	// The URL is passed as its own parameter rather than concatenated from
	// $1 and $2: reusing $1 as both a varchar column value and an operand
	// of || leaves Postgres unable to deduce one type for it
	// ("inconsistent types deduced for parameter $1", SQLSTATE 42P08).
	url := fmt.Sprintf("file:///archive/%s-%d", docKind, entityID)
	err := pool.QueryRow(ctx, `
		INSERT INTO document_archives (doc_kind, entity_id, storage_backend, storage_url,
		                               checksum_sha256, size_bytes, archived_at, retain_until)
		VALUES ($1, $2, 'local', $3, $4, 2048, NOW(), $5)
		RETURNING id`, docKind, entityID, url, strings.Repeat("a", 64), retainUntil).Scan(&id)
	if err != nil {
		t.Fatalf("seed archive %s %d: %v", docKind, entityID, err)
	}
	return id
}

func seedInvoiceRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, subscriberID int) int {
	t.Helper()
	var id int
	err := pool.QueryRow(ctx, `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount,
		                      igst_amount, total_amount, gst_rate_id, gb_included, gb_used)
		VALUES ($1, 677.12, 60.94, 60.94, 0, 799.00, 1, 100, 10)
		RETURNING id`, subscriberID).Scan(&id)
	if err != nil {
		t.Fatalf("seed invoice for subscriber %d: %v", subscriberID, err)
	}
	return id
}

func seedKYCRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, subscriberID int) int {
	t.Helper()
	var id int
	err := pool.QueryRow(ctx, `
		INSERT INTO kyc_verifications (subscriber_id, aadhaar_encrypted, pan_encrypted, key_version_id)
		VALUES ($1, '\x00'::bytea, '\x00'::bytea, 'v1')
		RETURNING id`, subscriberID).Scan(&id)
	if err != nil {
		t.Fatalf("seed kyc for subscriber %d: %v", subscriberID, err)
	}
	return id
}

func TestFR_DOC_003_ASubscribersDocumentsAreResolvedPerKind(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Archive()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "theirs@isp"})

	mineInvoice := seedInvoiceRow(ctx, t, pool, 1)
	mineKYC := seedKYCRow(ctx, t, pool, 1)
	theirsInvoice := seedInvoiceRow(ctx, t, pool, 2)

	retain := time.Now().AddDate(8, 0, 0)
	seedArchive(ctx, t, pool, archive.KindInvoice, mineInvoice, &retain)
	seedArchive(ctx, t, pool, archive.KindKYC, mineKYC, &retain)
	seedArchive(ctx, t, pool, archive.KindInvoice, theirsInvoice, &retain)
	// A report belongs to no subscriber and must not appear under one.
	seedArchive(ctx, t, pool, archive.KindReport, 999, &retain)

	docs, err := store.ListSubscriberArchives(ctx, 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("subscriber 1 has %d documents, want 2 (an invoice and a KYC document): %+v",
			len(docs), docs)
	}

	kinds := map[string]bool{}
	for _, d := range docs {
		kinds[d.DocKind] = true
		if d.SubscriberID != 1 {
			t.Errorf("document %d came back attributed to subscriber %d", d.ID, d.SubscriberID)
		}
		// SubjectAt is when the thing the document is about happened, which
		// is what an operator searches by and is not ArchivedAt.
		if d.SubjectAt.IsZero() {
			t.Errorf("document %d has no subject date", d.ID)
		}
		if d.SizeBytes == 0 || d.ChecksumSHA256 == "" {
			t.Errorf("document %d came back without its provenance: %+v", d.ID, d)
		}
	}
	if !kinds[archive.KindInvoice] || !kinds[archive.KindKYC] {
		t.Errorf("both kinds should be listed, got %v", kinds)
	}

	// The other subscriber's invoice must not be in this list, and a report
	// must be in nobody's.
	for _, d := range docs {
		if d.EntityID == theirsInvoice && d.DocKind == archive.KindInvoice {
			t.Error("another subscriber's invoice is listed under this one")
		}
		if d.DocKind == archive.KindReport {
			t.Error("a report export is listed under a subscriber")
		}
	}

	theirs, err := store.ListSubscriberArchives(ctx, 2)
	if err != nil {
		t.Fatalf("list theirs: %v", err)
	}
	if len(theirs) != 1 {
		t.Errorf("subscriber 2 has %d documents, want 1", len(theirs))
	}

	// A subscriber with nothing archived gets an empty list, not an error.
	none, err := store.ListSubscriberArchives(ctx, 999999)
	if err != nil {
		t.Errorf("listing for an unknown subscriber errored: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unknown subscriber has %d documents", len(none))
	}
}

// A purged document keeps its row — that it was held and disposed of on
// schedule is itself the record a retention audit wants — but must not be
// offered for retrieval, because its bytes are gone.
func TestFR_DOC_003_APurgedDocumentIsNotOfferedForRetrieval(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Archive()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mine@isp"})
	invoiceID := seedInvoiceRow(ctx, t, pool, 1)

	// retain_until in the past, so the purge is allowed by
	// chk_archive_not_purged_before_retention.
	past := time.Now().AddDate(0, 0, -1)
	archiveID := seedArchive(ctx, t, pool, archive.KindInvoice, invoiceID, &past)

	// Live first.
	if rec, err := store.GetArchiveByID(ctx, archiveID); err != nil || rec == nil {
		t.Fatalf("precondition: %v, %v", rec, err)
	}

	purged, err := store.MarkPurged(ctx, archiveID)
	if err != nil || !purged {
		t.Fatalf("purge: %v, %v", purged, err)
	}

	rec, err := store.GetArchiveByID(ctx, archiveID)
	if err != nil {
		t.Fatalf("get after purge: %v", err)
	}
	if rec != nil {
		t.Error("a purged document is still offered for retrieval; its bytes are gone, so a " +
			"retrieval would fail the checksum and read as corruption rather than as disposal")
	}

	docs, err := store.ListSubscriberArchives(ctx, 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("a purged document is listed as available: %+v", docs)
	}

	// But the row survives, which is what a retention audit reads.
	var stillThere int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM document_archives WHERE id = $1 AND purged_at IS NOT NULL`,
		archiveID).Scan(&stillThere); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stillThere != 1 {
		t.Error("purging removed the record that the document was ever held")
	}
}

// The ownership resolution the API checks against. Getting it wrong is how
// one customer's KYC scan ends up served under another's name.
func TestFR_DOC_003_SubscriberForArchiveResolvesOwnershipPerKind(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Archive()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "one@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "two@isp"})

	inv1 := seedArchive(ctx, t, pool, archive.KindInvoice, seedInvoiceRow(ctx, t, pool, 1), nil)
	kyc2 := seedArchive(ctx, t, pool, archive.KindKYC, seedKYCRow(ctx, t, pool, 2), nil)
	report := seedArchive(ctx, t, pool, archive.KindReport, 777, nil)

	for _, tc := range []struct {
		name string
		id   int64
		want int
	}{
		{"an invoice resolves to its subscriber", inv1, 1},
		{"a KYC document resolves to its subscriber", kyc2, 2},
		// Zero means "not subscriber-scoped". The API treats it as a
		// refusal rather than a wildcard, which is what stops a report
		// being served under whichever subscriber a caller names.
		{"a report resolves to nobody", report, 0},
		{"an unknown archive resolves to nobody", 999999, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.SubscriberForArchive(ctx, tc.id)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("archive %d resolved to subscriber %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}

// An archive row whose entity has been deleted must not be attributed to a
// subscriber it can no longer be tied to. The join is what guarantees that,
// and it is the reason the list is built from joins rather than from a
// denormalised subscriber_id.
func TestFR_DOC_003_AnOrphanedArchiveIsNotAttributedToAnyone(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Archive()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "one@isp"})
	invoiceID := seedInvoiceRow(ctx, t, pool, 1)
	archiveID := seedArchive(ctx, t, pool, archive.KindInvoice, invoiceID, nil)

	if _, err := pool.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, invoiceID); err != nil {
		t.Skipf("invoices cannot be deleted in this schema, so orphaning is not reachable: %v", err)
	}

	docs, err := store.ListSubscriberArchives(ctx, 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, d := range docs {
		if d.ID == archiveID {
			t.Error("an archive whose invoice is gone is still listed under the subscriber")
		}
	}

	owner, err := store.SubscriberForArchive(ctx, archiveID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if owner != 0 {
		t.Errorf("an orphaned archive resolved to subscriber %d, want 0 so retrieval refuses it", owner)
	}
}
