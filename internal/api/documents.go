package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Archived document retrieval and per-subscriber browsing — FR-DOC-003.
//
// The archive package has had verify-on-read since FR-DOC-001 landed — a
// Store.Get, an Archiver.Retrieve that hashes what it read and refuses to
// return bytes that disagree with the recorded checksum, and metrics for
// each outcome. None of it had a production caller: NFR-DUR-002's gap note
// said there was no code path to read a document back, and closing it in the
// package without reaching an operator left that true in every way that
// matters. This is that path.

// DocumentArchiveStore reads archive rows. Satisfied by *db.ArchiveStore.
type DocumentArchiveStore interface {
	GetArchiveByID(ctx context.Context, id int64) (*archive.Record, error)
	ListSubscriberArchives(ctx context.Context, subscriberID int) ([]archive.SubscriberDocument, error)
	SubscriberForArchive(ctx context.Context, id int64) (int, error)
}

// DocumentRetriever fetches and verifies archived bytes. Satisfied by
// *archive.Archiver.
//
// Narrowed to the one method the read path needs, so the console cannot
// reach Archive or the purge sweep through this seam.
type DocumentRetriever interface {
	Retrieve(ctx context.Context, rec archive.Record) ([]byte, error)
}

var (
	// ErrDocumentsUnwired is returned when archival is not configured.
	ErrDocumentsUnwired = errors.New("api: document archival not configured")
)

// ListSubscriberDocuments returns a subscriber's archived documents.
func (h *Handler) ListSubscriberDocuments(ctx context.Context, subscriberID int) ([]archive.SubscriberDocument, error) {
	if h.docArchives == nil {
		return nil, ErrDocumentsUnwired
	}
	return h.docArchives.ListSubscriberArchives(ctx, subscriberID)
}

// RetrieveSubscriberDocument fetches one archived document, verifying its
// SHA-256 against the checksum recorded when it was written — FR-DOC-003,
// closing NFR-DUR-002.
//
// subscriberID is checked against the row rather than trusted from the
// caller. Without that, a document could be reached by guessing an archive
// id and served under whichever subscriber the caller happened to name,
// which for KYC scans is somebody else's identity documents.
//
// A checksum mismatch returns *archive.ErrChecksumMismatch unchanged, not a
// generic failure. The whole point of verifying on read is to be able to say
// "the stored copy is corrupt" rather than "something went wrong", and an
// operator who cannot tell those apart will assume the second and retry
// forever.
func (h *Handler) RetrieveSubscriberDocument(ctx context.Context, subscriberID int, archiveID int64) ([]byte, *archive.Record, error) {
	if h.docArchives == nil || h.docRetriever == nil {
		return nil, nil, ErrDocumentsUnwired
	}

	rec, err := h.docArchives.GetArchiveByID(ctx, archiveID)
	if err != nil {
		return nil, nil, err
	}
	if rec == nil {
		return nil, nil, archive.ErrNotFound
	}

	owner, err := h.docArchives.SubscriberForArchive(ctx, archiveID)
	if err != nil {
		return nil, nil, err
	}
	if owner == 0 || owner != subscriberID {
		return nil, nil, archive.ErrNotTheirs
	}

	body, err := h.docRetriever.Retrieve(ctx, *rec)
	if err != nil {
		var mismatch *archive.ErrChecksumMismatch
		if errors.As(err, &mismatch) {
			// Audited as a distinct event. A corrupt archive is a durability
			// incident, not a failed download, and it needs to be findable
			// later by somebody asking when the copy went bad rather than
			// buried in an error log that rotates.
			middleware.Audit(ctx, "document.checksum_mismatch", rec.Describe(), map[string]any{
				"archive_id":    rec.ID,
				"doc_kind":      rec.DocKind,
				"entity_id":     rec.EntityID,
				"subscriber_id": subscriberID,
				"expected":      mismatch.Want,
				"found":         mismatch.Got,
			})
		}
		return nil, nil, err
	}

	// Reading somebody's archived documents is worth recording whatever the
	// outcome: these are invoices and KYC scans, and "who looked at this
	// customer's identity documents" is a question the DPDP Act expects an
	// operator to be able to answer.
	middleware.Audit(ctx, "document.retrieved", rec.Describe(), map[string]any{
		"archive_id":    rec.ID,
		"doc_kind":      rec.DocKind,
		"entity_id":     rec.EntityID,
		"subscriber_id": subscriberID,
		"size_bytes":    rec.SizeBytes,
		// Recorded so a later dispute can be answered without re-reading
		// the file: the checksum that verified at this moment.
		"checksum_verified": rec.ChecksumSHA256,
	})
	return body, rec, nil
}

// VerifySubscriberDocument re-reads a document and reports only whether it
// still matches its checksum, without returning the bytes — FR-DOC-003.
//
// Separate from retrieval because they answer different questions. An
// operator checking that an archive is still intact should not have to
// download a KYC scan to find out, and a bulk integrity sweep should not
// move megabytes of identity documents through a console session.
func (h *Handler) VerifySubscriberDocument(ctx context.Context, subscriberID int, archiveID int64) (*archive.Record, error) {
	_, rec, err := h.RetrieveSubscriberDocument(ctx, subscriberID, archiveID)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ArchiveInvoicePDF keeps a durable, checksummed copy of an invoice PDF —
// FR-DOC-001's write side for invoices, wired here because FR-DOC-003's
// per-subscriber browse has nothing to show without it.
//
// Best-effort by design: the caller is serving a download, and a subscriber
// should not be refused their invoice because archival storage is full. The
// failure is returned so the caller can log it, never so it can fail the
// response.
//
// Idempotent: RecordArchive upserts on (doc_kind, entity_id), and the storage
// key is derived from the same pair, so re-rendering an invoice overwrites
// its own copy rather than accumulating one per view.
func (h *Handler) ArchiveInvoicePDF(ctx context.Context, invoiceID int, body []byte) error {
	if h.docArchiver == nil {
		return nil // archival not configured; not an error
	}
	if len(body) == 0 {
		return fmt.Errorf("api: invoice %d produced no PDF to archive", invoiceID)
	}
	_, err := h.docArchiver.ArchiveInvoice(ctx, invoiceID, body)
	return err
}

// InvoiceArchiver is the write seam for invoice PDFs. Satisfied by
// *archive.Archiver.
type InvoiceArchiver interface {
	ArchiveInvoice(ctx context.Context, invoiceID int, body []byte) (*archive.Record, error)
}

// documentAuditTarget is the audit target for a whole subscriber's documents,
// used when the list itself is the thing being read.
func documentAuditTarget(subscriberID int) string { return strconv.Itoa(subscriberID) }
