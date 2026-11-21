package staffui

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
)

// Archived documents, browsed per subscriber — FR-DOC-003.

type fakeDocuments struct {
	docs        []archive.SubscriberDocument
	body        []byte
	rec         *archive.Record
	retrieveErr error
	gotSub      int
	gotID       int64
	verifies    int
	downloads   int
}

func (f *fakeDocuments) ListSubscriberDocuments(_ context.Context, subscriberID int) ([]archive.SubscriberDocument, error) {
	f.gotSub = subscriberID
	return f.docs, nil
}

func (f *fakeDocuments) RetrieveSubscriberDocument(_ context.Context, subscriberID int, archiveID int64) ([]byte, *archive.Record, error) {
	f.gotSub, f.gotID = subscriberID, archiveID
	f.downloads++
	if f.retrieveErr != nil {
		return nil, nil, f.retrieveErr
	}
	return f.body, f.rec, nil
}

func (f *fakeDocuments) VerifySubscriberDocument(_ context.Context, subscriberID int, archiveID int64) (*archive.Record, error) {
	f.gotSub, f.gotID = subscriberID, archiveID
	f.verifies++
	if f.retrieveErr != nil {
		return nil, f.retrieveErr
	}
	return f.rec, nil
}

func invoiceDoc(id int64, subscriberID, entityID int) archive.SubscriberDocument {
	retain := time.Now().AddDate(8, 0, 0)
	return archive.SubscriberDocument{
		Record: archive.Record{
			ID: id, DocKind: archive.KindInvoice, EntityID: entityID,
			StorageBackend: archive.BackendLocal,
			StorageURL:     "file:///archive/invoice.pdf",
			ChecksumSHA256: strings.Repeat("a", 64),
			SizeBytes:      204800,
			ArchivedAt:     time.Now().Add(-48 * time.Hour),
			RetainUntil:    &retain,
		},
		SubscriberID: subscriberID,
		SubjectAt:    time.Now().Add(-72 * time.Hour),
	}
}

func docScreen(docs ...archive.SubscriberDocument) (*Handler, *fakeDocuments) {
	f := &fakeDocuments{
		docs: docs,
		body: []byte("%PDF-1.4 verified bytes"),
		rec:  &archive.Record{ID: 1, DocKind: archive.KindInvoice, EntityID: 123},
	}
	return &Handler{documents: f}, f
}

func TestFR_DOC_003_TheScreenListsADocumentWithItsProvenance(t *testing.T) {
	h, f := docScreen(invoiceDoc(1, 42, 123))

	w := servePR(h, "billing_admin", "GET", "/staff/documents?subscriber_id=42", nil, h.Documents)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if f.gotSub != 42 {
		t.Errorf("the store was asked for subscriber %d, want 42", f.gotSub)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Named the way an operator would ask for it, matching the invoice
		// numbering the rest of the system shows.
		"Invoice 000123",
		"200.0 kB",
		"local",
		// The claim that makes this archival rather than copying.
		"Every read is verified",
		// And that there is no unverified way out.
		"not a control",
		// Both actions.
		"Check it is intact", "Download",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// Nothing archived is a legitimate state, and the screen has to say what to
// do about it rather than looking broken.
func TestFR_DOC_003_AnEmptyArchiveIsExplained(t *testing.T) {
	h, _ := docScreen()
	body := servePR(h, "isp_owner", "GET", "/staff/documents?subscriber_id=42", nil, h.Documents).Body.String()

	if !strings.Contains(body, "Nothing archived for this subscriber yet") {
		t.Error("an empty archive is not explained")
	}
	// Says how a document gets there, so the emptiness is actionable.
	if !strings.Contains(body, "archived when their PDF is first generated") {
		t.Error("the page does not say how a document comes to be archived")
	}
	// And is honest that KYC archival is not wired.
	if !strings.Contains(body, "KYC document archival is not wired") {
		t.Error("the page implies KYC documents would be archived when they are not")
	}
}

// The property the whole feature exists for: a corrupt copy is refused, and
// the operator is told it is corrupt rather than that something went wrong.
func TestFR_DOC_003_ACorruptDocumentIsRefusedAndNamedAsCorrupt(t *testing.T) {
	h, f := docScreen(invoiceDoc(1, 42, 123))
	f.retrieveErr = &archive.ErrChecksumMismatch{
		DocKind: archive.KindInvoice, EntityID: 123,
		Want: strings.Repeat("a", 64), Got: strings.Repeat("b", 64),
	}

	w := servePR(h, "billing_admin", "GET", "/staff/documents/1/download?subscriber_id=42",
		nil, h.DownloadDocument)
	if w.Code != 303 {
		t.Fatalf("a corrupt document answered %d, want a redirect carrying the refusal", w.Code)
	}
	// Crucially: no bytes.
	if w.Body.Len() > 200 {
		t.Errorf("a corrupt document returned %d bytes of body", w.Body.Len())
	}
	if strings.Contains(w.Body.String(), "verified bytes") {
		t.Error("a corrupt document's bytes were served")
	}
	if !strings.Contains(w.Header().Get("Location"), "m=corrupt") {
		t.Errorf("redirect %q does not carry the corruption outcome", w.Header().Get("Location"))
	}

	msg := documentMessages["corrupt"]
	for _, want := range []string{"does not match", "altered or damaged", "do not treat it as the original"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the corruption message does not say %q: %q", want, msg)
		}
	}
}

// A storage failure is not corruption, and must not be reported as it: the
// checksum was never compared, so telling an operator the document is
// damaged would send them chasing the wrong problem.
func TestFR_DOC_003_AnUnreadableDocumentIsNotReportedAsCorrupt(t *testing.T) {
	h, f := docScreen(invoiceDoc(1, 42, 123))
	f.retrieveErr = errors.New("archive: open file:///gone: no such file")

	w := servePR(h, "billing_admin", "GET", "/staff/documents/1/download?subscriber_id=42",
		nil, h.DownloadDocument)
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "m=unreadable") {
		t.Errorf("a storage failure redirected to %q, want the unreadable outcome", loc)
	}
	if strings.Contains(loc, "m=corrupt") {
		t.Error("a storage failure was reported as a corrupt document")
	}

	msg := documentMessages["unreadable"]
	if !strings.Contains(msg, "rather than a corrupt document") {
		t.Errorf("the message does not distinguish the two: %q", msg)
	}
	if !strings.Contains(msg, "could not even be compared") {
		t.Errorf("the message does not say the checksum was never compared: %q", msg)
	}
}

// A verified download serves the bytes as an attachment, with the kind's own
// content type. Inline would open a KYC scan in a browser tab.
func TestFR_DOC_003_AVerifiedDownloadIsServedAsAnAttachment(t *testing.T) {
	h, _ := docScreen(invoiceDoc(1, 42, 123))

	w := servePR(h, "billing_admin", "GET", "/staff/documents/1/download?subscriber_id=42",
		nil, h.DownloadDocument)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, "verified bytes") {
		t.Errorf("the verified bytes were not served: %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type is %q, want application/pdf", got)
	}
	disp := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disp, "attachment;") {
		t.Errorf("Content-Disposition is %q; a KYC scan opening inline is one screenshot from being "+
			"somewhere it should not be", disp)
	}
	if !strings.Contains(disp, ".pdf") {
		t.Errorf("the filename has no extension: %q", disp)
	}
}

// Verifying must not move the bytes to the operator. An integrity check that
// downloads a customer's identity documents to answer "is it intact" has
// defeated its own purpose.
func TestFR_DOC_003_VerifyingReportsTheOutcomeWithoutServingTheDocument(t *testing.T) {
	h, f := docScreen(invoiceDoc(1, 42, 123))

	w := servePR(h, "billing_admin", "POST", "/staff/documents/1/verify",
		url.Values{"subscriber_id": {"42"}}, h.VerifyDocument)
	if w.Code != 303 {
		t.Fatalf("got %d", w.Code)
	}
	if f.verifies != 1 {
		t.Errorf("verify called the store %d times, want 1", f.verifies)
	}
	if f.downloads != 0 {
		t.Error("verifying went through the download path, which moves the bytes")
	}
	if strings.Contains(w.Body.String(), "verified bytes") {
		t.Error("verifying served the document")
	}
	if !strings.Contains(w.Header().Get("Location"), "m=verified") {
		t.Errorf("redirect %q does not carry the result", w.Header().Get("Location"))
	}
	if !strings.Contains(documentMessages["verified"], "intact") {
		t.Error("the success message does not say the copy is intact")
	}
}

// A document belonging to somebody else is refused with its own message:
// "not found" would imply the id is wrong when the problem is whose it is.
func TestFR_DOC_003_ADocumentBelongingToSomebodyElseIsRefused(t *testing.T) {
	h, f := docScreen(invoiceDoc(1, 42, 123))
	f.retrieveErr = archive.ErrNotTheirs

	w := servePR(h, "billing_admin", "GET", "/staff/documents/1/download?subscriber_id=99",
		nil, h.DownloadDocument)
	if !strings.Contains(w.Header().Get("Location"), "m=nottheirs") {
		t.Errorf("redirect %q does not carry the ownership refusal", w.Header().Get("Location"))
	}
	// And the subscriber the caller named is what was checked, not one
	// taken from the row.
	if f.gotSub != 99 {
		t.Errorf("the API was asked about subscriber %d, want the one named in the request", f.gotSub)
	}

	f.retrieveErr = archive.ErrNotFound
	w = servePR(h, "billing_admin", "GET", "/staff/documents/1/download?subscriber_id=42",
		nil, h.DownloadDocument)
	if !strings.Contains(w.Header().Get("Location"), "m=gone") {
		t.Errorf("a missing document redirected to %q", w.Header().Get("Location"))
	}
	if !strings.Contains(documentMessages["gone"], "purged") {
		t.Error("the missing-document message does not mention purging, which is the usual reason")
	}
}

func TestFR_DOC_003_BadIdsAreRefusedBeforeAnyRead(t *testing.T) {
	for _, tc := range []struct{ name, path, sub string }{
		{"a non-numeric document id", "notanumber", "42"},
		{"a zero document id", "0", "42"},
		{"a negative document id", "-1", "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, f := docScreen()
			w := servePR(h, "billing_admin", "GET",
				"/staff/documents/"+tc.path+"/download?subscriber_id="+tc.sub, nil, h.DownloadDocument)
			if w.Code != 400 {
				t.Errorf("got %d, want 400", w.Code)
			}
			if f.downloads != 0 {
				t.Error("a bad id reached the read path")
			}
		})
	}

	t.Run("a missing subscriber id", func(t *testing.T) {
		h, f := docScreen()
		w := servePR(h, "billing_admin", "GET", "/staff/documents/1/download", nil, h.DownloadDocument)
		if w.Code != 400 {
			t.Errorf("got %d, want 400", w.Code)
		}
		if f.downloads != 0 {
			t.Error("a request with no subscriber reached the read path, where the ownership " +
				"check is what stops one document being served under another customer")
		}
	})
}

// Billing and the owner, and specifically not the front desk or the field:
// the archive holds KYC scans.
func TestFR_DOC_003_DocumentsReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "documents" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "billing_admin"} {
		if !has(role) {
			t.Errorf("%s cannot reach archived documents", role)
		}
	}
	for _, role := range []string{"csr", "technician", "noc_engineer"} {
		if has(role) {
			t.Errorf("%s can reach archived documents, which include KYC scans", role)
		}
	}
}

func TestFR_DOC_003_AnUnconfiguredArchiveSaysSoRatherThanFailing(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/documents", nil, h.Documents)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}

// Describe is what both the list and the download filename are built from,
// so the two cannot drift apart.
func TestFR_DOC_003_DocumentsAreNamedConsistently(t *testing.T) {
	cases := map[string]archive.Record{
		"Invoice 000123":  {DocKind: archive.KindInvoice, EntityID: 123},
		"KYC document":    {DocKind: archive.KindKYC, EntityID: 7},
		"Report export 9": {DocKind: archive.KindReport, EntityID: 9},
	}
	for want, rec := range cases {
		if got := rec.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}

	// A purged row keeps its metadata but cannot be fetched, and the two
	// must be distinguishable.
	purged := time.Now()
	if (archive.Record{PurgedAt: &purged}).Retrievable() {
		t.Error("a purged document reports itself retrievable")
	}
	if !(archive.Record{}).Retrievable() {
		t.Error("a live document reports itself unretrievable")
	}
}
