package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Document kinds match the doc_kind CHECK constraint in migration 034.
const (
	KindInvoice = "invoice"
	KindKYC     = "kyc_document"
	KindReport  = "report"
)

var (
	archivedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "document_archived_total",
		Help: "Documents archived, by kind and outcome",
	}, []string{"kind", "outcome"})
	archivedBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "document_archived_bytes_total",
		Help: "Bytes written to archival storage, by kind",
	}, []string{"kind"})
	retrievedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "document_retrieved_total",
		Help: "Archived documents retrieved and checksum-verified, by kind and outcome",
	}, []string{"kind", "outcome"})
)

// ErrChecksumMismatch means the bytes a Store returned do not hash to the
// checksum recorded when the document was archived — the one outcome this
// package exists to catch, and the reason Retrieve exists at all rather than
// callers reading Store.Get directly.
type ErrChecksumMismatch struct {
	DocKind, Want, Got string
	EntityID           int
}

func (e *ErrChecksumMismatch) Error() string {
	return fmt.Sprintf("archive: %s %d failed checksum verification: recorded %s, got %s",
		e.DocKind, e.EntityID, e.Want, e.Got)
}

// Record is a stored archive row.
type Record struct {
	ID             int64      `json:"id"`
	DocKind        string     `json:"doc_kind"`
	EntityID       int        `json:"entity_id"`
	StorageBackend string     `json:"storage_backend"`
	StorageURL     string     `json:"storage_url"`
	ChecksumSHA256 string     `json:"checksum_sha256"`
	SizeBytes      int64      `json:"size_bytes"`
	ArchivedAt     time.Time  `json:"archived_at"`
	RetainUntil    *time.Time `json:"retain_until,omitempty"`
	PurgedAt       *time.Time `json:"purged_at,omitempty"`
}

// Recorder persists and retrieves archive rows. Satisfied by *db.ArchiveStore.
type Recorder interface {
	RecordArchive(ctx context.Context, r Record) (int64, error)
	ListDueForPurge(ctx context.Context, limit int) ([]Record, error)
	MarkPurged(ctx context.Context, id int64) (bool, error)
	// GetArchive returns the live archive row for a document, or nil, nil when
	// none exists (purged, or never archived) — not an error, since "is this
	// archived yet" is a legitimate question with "no" as a legitimate answer.
	GetArchive(ctx context.Context, docKind string, entityID int) (*Record, error)
}

// Document is one thing to archive.
type Document struct {
	Kind     string
	EntityID int
	// Filename is used only to build a readable storage key; it never reaches
	// the filesystem unsanitised (see storageKey).
	Filename string
	Body     io.Reader
}

// Retention is a calendar-relative keep-for period.
//
// Calendar units rather than a time.Duration, because retention obligations are
// written in years and a fixed 365-day year drifts: eight of them fall two days
// short of eight calendar years, so a fixed-duration policy would delete GST
// records two days before the statute allows. Nobody would notice, and that is
// precisely the problem with getting it wrong.
type Retention struct {
	Years  int
	Months int
	Days   int
}

// IsZero reports whether the period is empty — which the policy treats as
// "keep indefinitely" rather than "purge immediately".
func (r Retention) IsZero() bool { return r.Years == 0 && r.Months == 0 && r.Days == 0 }

// RetentionPolicy decides how long each kind of document is kept.
//
// Per-kind rather than one global interval because the statutory minimums
// genuinely differ, and a single sweep would either delete invoices before the
// tax authority is done with them or hoard KYC scans long past the point the
// DPDP Act's storage-limitation principle allows.
//
// These defaults are a starting point an operator must confirm against their
// own licence conditions and counsel — they are encoded here so retention is
// explicit and reviewable, not because this package can know the answer for a
// given deployment.
type RetentionPolicy map[string]Retention

// DefaultRetention is the shipped policy.
//
//   - invoice: 8 years. India's GST rules require books and records to be kept
//     72 months from the due date of the annual return, and the extra margin
//     covers the gap between an invoice's date and that due date.
//   - kyc_document: 5 years. DoT licence conditions require subscriber
//     verification records well past disconnection; the counterpoint is the
//     DPDP Act, which is why this is finite rather than forever.
//   - report: 1 year. Regenerable from the underlying data, so keeping it is a
//     convenience rather than an obligation.
var DefaultRetention = RetentionPolicy{
	KindInvoice: {Years: 8},
	KindKYC:     {Years: 5},
	KindReport:  {Years: 1},
}

// RetainUntil returns the purge date for a kind archived at now, or nil when
// the policy names no period.
//
// A nil result means "keep indefinitely", and the purge scanner skips those
// rows. That is the safe direction for an unrecognised kind: keeping a document
// too long is a compliance conversation, deleting one too early can be
// unrecoverable.
func (p RetentionPolicy) RetainUntil(kind string, now time.Time) *time.Time {
	r, ok := p[kind]
	if !ok || r.IsZero() {
		return nil
	}
	t := now.AddDate(r.Years, r.Months, r.Days)
	return &t
}

// Archiver streams documents into a Store and records where they went.
type Archiver struct {
	store     Store
	db        Recorder
	retention RetentionPolicy
	now       func() time.Time
}

// NewArchiver constructs an Archiver with the default retention policy.
func NewArchiver(store Store, recorder Recorder) *Archiver {
	return &Archiver{store: store, db: recorder, retention: DefaultRetention, now: time.Now}
}

// SetRetention overrides the retention policy.
func (a *Archiver) SetRetention(p RetentionPolicy) { a.retention = p }

// Archive writes doc to storage and records it.
//
// Order matters and is deliberate: the bytes land first, the row second. The
// reverse would produce a row promising a document that does not exist, which
// is worse than an orphaned object — a restore would report success and hand
// back nothing. An orphan, by contrast, is wasted space that a later archival
// of the same document simply overwrites.
func (a *Archiver) Archive(ctx context.Context, doc Document) (*Record, error) {
	if !ValidKind(doc.Kind) {
		return nil, fmt.Errorf("archive: unknown document kind %q", doc.Kind)
	}
	if doc.EntityID <= 0 {
		return nil, fmt.Errorf("archive: %s has no entity id", doc.Kind)
	}
	if doc.Body == nil {
		return nil, fmt.Errorf("archive: %s %d has no content", doc.Kind, doc.EntityID)
	}

	now := a.now()
	put, err := a.store.Put(ctx, storageKey(doc, now), doc.Body)
	if err != nil {
		archivedTotal.WithLabelValues(doc.Kind, "store_error").Inc()
		return nil, err
	}

	rec := Record{
		DocKind:        doc.Kind,
		EntityID:       doc.EntityID,
		StorageBackend: a.store.Backend(),
		StorageURL:     put.URL,
		ChecksumSHA256: put.ChecksumSHA256,
		SizeBytes:      put.SizeBytes,
		ArchivedAt:     now,
		RetainUntil:    a.retention.RetainUntil(doc.Kind, now),
	}
	id, err := a.db.RecordArchive(ctx, rec)
	if err != nil {
		archivedTotal.WithLabelValues(doc.Kind, "record_error").Inc()
		return nil, err
	}
	rec.ID = id

	archivedTotal.WithLabelValues(doc.Kind, "archived").Inc()
	archivedBytes.WithLabelValues(doc.Kind).Add(float64(put.SizeBytes))
	return &rec, nil
}

// Retrieve fetches an archived document and verifies it against the checksum
// recorded when it was written, returning ErrChecksumMismatch rather than the
// bytes if they disagree.
//
// Buffered rather than streamed back to the caller on purpose. The documents
// this package holds — invoice PDFs, KYC scans — are bounded in size, and a
// streaming verify would need a wrapper that only reports a mismatch after
// the caller has already consumed a corrupt prefix. Reading the whole thing
// first means a caller gets either fully verified bytes or an error, never
// partial trust.
func (a *Archiver) Retrieve(ctx context.Context, rec Record) ([]byte, error) {
	rc, err := a.store.Get(ctx, rec.StorageURL)
	if err != nil {
		retrievedTotal.WithLabelValues(rec.DocKind, "store_error").Inc()
		return nil, err
	}
	defer rc.Close() //nolint:errcheck // read-only handle; nothing to recover from a close failure

	hasher := sha256.New()
	body, err := io.ReadAll(io.TeeReader(rc, hasher))
	if err != nil {
		retrievedTotal.WithLabelValues(rec.DocKind, "read_error").Inc()
		return nil, fmt.Errorf("archive: read %s %d: %w", rec.DocKind, rec.EntityID, err)
	}

	if got := hex.EncodeToString(hasher.Sum(nil)); got != rec.ChecksumSHA256 {
		retrievedTotal.WithLabelValues(rec.DocKind, "checksum_mismatch").Inc()
		return nil, &ErrChecksumMismatch{
			DocKind: rec.DocKind, EntityID: rec.EntityID, Want: rec.ChecksumSHA256, Got: got,
		}
	}

	retrievedTotal.WithLabelValues(rec.DocKind, "verified").Inc()
	return body, nil
}

// RetrieveLatest looks up a document's live archive row and retrieves it in
// one call — the read-side counterpart to Archive, for the common case where
// a caller has a doc kind and an entity id and nothing more. Returns nil, nil
// when the document was never archived or has since been purged, matching
// Recorder.GetArchive.
func (a *Archiver) RetrieveLatest(ctx context.Context, docKind string, entityID int) ([]byte, *Record, error) {
	rec, err := a.db.GetArchive(ctx, docKind, entityID)
	if err != nil {
		return nil, nil, err
	}
	if rec == nil {
		return nil, nil, nil
	}
	body, err := a.Retrieve(ctx, *rec)
	if err != nil {
		return nil, rec, err
	}
	return body, rec, nil
}

// ArchiveReport stores a generated report and returns where it landed.
//
// A named convenience over Archive because report delivery (FR-RPT-002) is the
// one caller that already holds the whole document in memory, and threading a
// bytes.Reader through the general path at every call site reads worse than
// saying what is happening.
func (a *Archiver) ArchiveReport(ctx context.Context, entityID int, filename string, body []byte) (string, error) {
	rec, err := a.Archive(ctx, Document{
		Kind:     KindReport,
		EntityID: entityID,
		Filename: filename,
		Body:     bytes.NewReader(body),
	})
	if err != nil {
		return "", err
	}
	return rec.StorageURL, nil
}

// ValidKind reports whether kind is one the schema accepts.
func ValidKind(kind string) bool {
	switch kind {
	case KindInvoice, KindKYC, KindReport:
		return true
	default:
		return false
	}
}

// storageKey lays documents out as kind/YYYY/MM/entity-filename.
//
// Dated directories keep any single directory to a month's worth of documents,
// which matters for the local backend where a flat directory of hundreds of
// thousands of invoices becomes slow to even list. The entity id leads the
// filename so the object is identifiable without consulting the database.
func storageKey(doc Document, now time.Time) string {
	name := sanitiseFilename(doc.Filename)
	if name == "" {
		name = doc.Kind
	}
	return path.Join(
		doc.Kind,
		now.UTC().Format("2006"),
		now.UTC().Format("01"),
		strconv.Itoa(doc.EntityID)+"-"+name,
	)
}

// sanitiseFilename reduces a caller-supplied name to something safe to place
// in a path: no separators, no traversal, no leading dots, bounded length.
//
// The Store re-checks containment, so this is the first of two independent
// defences rather than the only one — but it is the one that keeps the
// resulting key readable instead of merely safe.
func sanitiseFilename(name string) string {
	name = strings.TrimSpace(name)
	name = name[strings.LastIndexAny(name, `/\`)+1:]

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	cleaned := strings.TrimLeft(b.String(), ".")
	if len(cleaned) > 100 {
		cleaned = cleaned[:100]
	}
	return cleaned
}

// ErrNotFound covers both "no such archive row" and "purged", which are the
// same thing to a caller asking for bytes — FR-DOC-003.
//
// This and ErrNotTheirs live here rather than in internal/api because the
// console has to tell them apart from a corrupt document and from a storage
// failure, and it cannot import internal/api: *api.Handler is what satisfies
// the console's interfaces, so the dependency runs the other way.
var ErrNotFound = errors.New("archive: archived document not found")

// ErrNotTheirs is returned when an archive row does not belong to the
// subscriber it was requested under. Guessing an archive id must not serve
// somebody else's KYC scan.
var ErrNotTheirs = errors.New("archive: that document does not belong to this subscriber")

// SubscriberDocument is one archived document attributed to a subscriber —
// FR-DOC-003.
//
// document_archives stores only a doc_kind and an entity_id, so the
// attribution is resolved per kind by the store. SubjectAt is when the thing
// the document is about happened — the invoice's date, the KYC check's date —
// which is what an operator looking for "the invoice from March" is
// searching by, and is not the same as ArchivedAt.
type SubscriberDocument struct {
	Record
	SubscriberID int
	SubjectAt    time.Time
}

// Describe names a document the way an operator would ask for it.
//
// Lives here rather than in the console because the mapping from a doc_kind
// to a human name is a property of the kinds this package defines, and two
// screens naming the same kind differently is how an operator ends up
// believing there are two sorts of document.
func (r Record) Describe() string {
	switch r.DocKind {
	case KindInvoice:
		// Matches the invoice numbering the rest of the system shows, so
		// the archive can be matched against a customer's copy.
		return fmt.Sprintf("Invoice %06d", r.EntityID)
	case KindKYC:
		return "KYC document"
	case KindReport:
		return fmt.Sprintf("Report export %d", r.EntityID)
	default:
		return fmt.Sprintf("%s %d", r.DocKind, r.EntityID)
	}
}

// Retrievable reports whether this row's bytes can still be fetched. A purged
// row keeps its metadata — what was held, and that it was disposed of on
// schedule, is itself the record a retention audit asks for — but the bytes
// are gone.
func (r Record) Retrievable() bool { return r.PurgedAt == nil }

// ArchiveInvoice stores an invoice PDF and returns the row it recorded.
//
// A named convenience over Archive for the same reason ArchiveReport is one:
// the caller is serving a download and already holds the whole document, and
// threading a bytes.Reader through the general path at that call site reads
// worse than saying what is happening.
//
// Idempotent on the invoice: Archive's storage key is derived from the kind
// and entity id, and RecordArchive upserts on the same pair, so re-rendering
// an invoice overwrites its own copy instead of accumulating one per view.
func (a *Archiver) ArchiveInvoice(ctx context.Context, invoiceID int, body []byte) (*Record, error) {
	return a.Archive(ctx, Document{
		Kind:     KindInvoice,
		EntityID: invoiceID,
		Filename: fmt.Sprintf("invoice-%06d.pdf", invoiceID),
		Body:     bytes.NewReader(body),
	})
}
