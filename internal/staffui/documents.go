package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/archive"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// Archived documents, browsed per subscriber — FR-DOC-003.
//
// DocumentManager is satisfied by *api.Handler, so the console verifies and
// audits exactly as the API does. Retrieval deliberately has no unverified
// variant: every read of an archived document goes through the checksum
// comparison, which is what closes NFR-DUR-002 rather than merely making it
// possible to close.
type DocumentManager interface {
	ListSubscriberDocuments(ctx context.Context, subscriberID int) ([]archive.SubscriberDocument, error)
	RetrieveSubscriberDocument(ctx context.Context, subscriberID int, archiveID int64) ([]byte, *archive.Record, error)
	VerifySubscriberDocument(ctx context.Context, subscriberID int, archiveID int64) (*archive.Record, error)
}

var documentMessages = map[string]string{
	"verified": "Document re-read and its checksum matches what was recorded when it was archived. The stored copy is intact.",

	"corrupt":    "The stored copy does not match its recorded checksum. It has been altered or damaged since it was archived — do not treat it as the original, and raise this with whoever runs the storage.",
	"gone":       "No archived document with that id. It may have been purged once its retention period ended.",
	"nottheirs":  "That document does not belong to this subscriber.",
	"unreadable": "The archived copy could not be read at all. That is a storage failure rather than a corrupt document; the checksum could not even be compared.",
	"failed":     "That did not go through. Nothing was changed; try again.",
}

var documentSuccess = map[string]bool{"verified": true}

// documentRow is one archived document as the screen shows it.
type documentRow struct {
	archive.SubscriberDocument
	Title       string
	ArchivedOn  string
	SubjectOn   string
	SizeLabel   string
	RetainUntil string
}

type documentsData struct {
	Rows         []documentRow
	Plans        []Plan
	SubscriberID int
	Subscriber   string
	Configured   bool
}

// Who may read documents is the "documents" entry in staffui.sections
// (isp_owner and billing_admin), enforced by requireSection below. The
// archive holds invoice PDFs and KYC scans, so that is the reach of the two
// screens which already show those — billing for invoices, and the owner,
// who can see KYC anyway. Deliberately not CSR or technician: a KYC scan is a
// customer's identity documents, and the DPDP Act's purpose-limitation
// principle is not satisfied by "the front desk might want to look".
//
// A canReadDocuments(role) helper used to sit here repeating that list. It
// was never called — the section gate is the enforcement — so it was two
// copies of one policy, free to drift apart silently.

func (h *Handler) documentGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "documents")
	if !ok {
		return s, false
	}
	if h.documents == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Document archival is not configured on this deployment.")
		return s, false
	}
	return s, true
}

// Documents handles GET /staff/documents.
func (h *Handler) Documents(w http.ResponseWriter, r *http.Request) {
	s, ok := h.documentGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Archived documents", "documents")
	q := r.URL.Query()

	pd := documentsData{Configured: true}
	if raw := strings.TrimSpace(q.Get("subscriber_id")); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			d.Error = "That is not a valid subscriber id."
		} else {
			pd.SubscriberID = id
		}
	}

	if pd.SubscriberID != 0 {
		ctx := middleware.WithSubject(r.Context(), s.Username)
		docs, err := h.documents.ListSubscriberDocuments(ctx, pd.SubscriberID)
		if err != nil {
			log.Error().Err(err).Int("subscriber_id", pd.SubscriberID).
				Msg("staffui: list archived documents failed")
			h.renderError(w, r, s, http.StatusInternalServerError,
				"Could not read this subscriber's archived documents.")
			return
		}
		for _, doc := range docs {
			row := documentRow{
				SubscriberDocument: doc,
				Title:              doc.Describe(),
				ArchivedOn:         doc.ArchivedAt.Local().Format("2 Jan 2006 15:04"),
				SubjectOn:          doc.SubjectAt.Local().Format("2 Jan 2006"),
				SizeLabel:          sizeLabel(doc.SizeBytes),
			}
			if doc.RetainUntil != nil {
				row.RetainUntil = doc.RetainUntil.Local().Format("2 Jan 2006")
			}
			pd.Rows = append(pd.Rows, row)
		}
	}

	if code := q.Get("m"); code != "" {
		if msg, ok := documentMessages[code]; ok {
			if documentSuccess[code] {
				d.Message = msg
			} else {
				d.Error = msg
			}
		}
	}

	d.Data = pd
	h.render(w, "documents", d)
}

// sizeLabel renders a byte count the way somebody reads it off a screen.
func sizeLabel(n int64) string {
	switch {
	case n <= 0:
		return "empty"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f kB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// DownloadDocument handles GET /staff/documents/{id}/download.
//
// Serves the bytes only after the checksum has matched. A corrupt copy is
// refused rather than handed over with a warning: an operator who has
// downloaded a file will use it, and "this might be wrong" next to a
// successful download is not a control.
func (h *Handler) DownloadDocument(w http.ResponseWriter, r *http.Request) {
	s, ok := h.documentGate(w, r)
	if !ok {
		return
	}
	archiveID, subscriberID, ok := h.documentTarget(w, r, s)
	if !ok {
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	body, rec, err := h.documents.RetrieveSubscriberDocument(ctx, subscriberID, archiveID)
	if err != nil {
		h.documentFailure(w, r, subscriberID, err, "download")
		return
	}

	filename := strings.ReplaceAll(rec.Describe(), " ", "-") + extensionFor(rec.DocKind)
	w.Header().Set("Content-Type", contentTypeFor(rec.DocKind))
	// The other four download paths (invoices, proformas, receipts, the
	// hotspot logo) all set this and this one did not. An archived document
	// is bytes a customer supplied, so a browser that sniffs its way to
	// text/html would be running their content on our origin; the attachment
	// disposition below already prevents that, and this closes the gap the
	// rest of the codebase had already decided to close.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// attachment rather than inline: a KYC scan opening in a browser tab is
	// one stray screenshot away from being somewhere it should not be.
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// G705 reads the archived bytes as untrusted output, which they are: a
	// customer supplied them. What makes it safe is above — Content-Disposition
	// attachment so no browser renders it inline, nosniff so none second-guesses
	// the type, and a filename from Record.Describe(), which only ever formats
	// constants and integers, so nothing can break out of the header.
	//nolint:gosec // G705: see above
	if _, err := w.Write(body); err != nil {
		log.Warn().Err(err).Int64("archive_id", archiveID).
			Msg("staffui: archived document write to client failed after verification")
	}
}

// VerifyDocument handles POST /staff/documents/{id}/verify.
//
// Re-reads and compares, reporting only the outcome. Separate from the
// download so an operator can confirm an archive is still intact without
// pulling a customer's identity documents onto their laptop to find out.
func (h *Handler) VerifyDocument(w http.ResponseWriter, r *http.Request) {
	s, ok := h.documentGate(w, r)
	if !ok {
		return
	}
	archiveID, subscriberID, ok := h.documentTarget(w, r, s)
	if !ok {
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.documents.VerifySubscriberDocument(ctx, subscriberID, archiveID); err != nil {
		h.documentFailure(w, r, subscriberID, err, "verify")
		return
	}
	h.redirectDocuments(w, r, subscriberID, "verified")
}

// documentTarget reads the archive id from the path and the subscriber from
// the form or query.
//
// The subscriber is required rather than inferred from the archive row: it is
// what the API checks the row against, and supplying it means a mismatch is
// caught instead of the caller being told whatever the row happened to say.
func (h *Handler) documentTarget(w http.ResponseWriter, r *http.Request, s Session) (int64, int, bool) {
	archiveID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || archiveID <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid document id.")
		return 0, 0, false
	}
	raw := strings.TrimSpace(r.PostFormValue("subscriber_id"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("subscriber_id"))
	}
	subscriberID, err := strconv.Atoi(raw)
	if err != nil || subscriberID <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return 0, 0, false
	}
	return archiveID, subscriberID, true
}

func (h *Handler) documentFailure(w http.ResponseWriter, r *http.Request, subscriberID int, err error, action string) {
	var mismatch *archive.ErrChecksumMismatch
	switch {
	case errors.As(err, &mismatch):
		// The outcome this feature exists to surface. Logged at error level
		// with both hashes, because it is a durability incident and the
		// pair is what an investigation starts from.
		log.Error().Err(err).Int("subscriber_id", subscriberID).
			Str("expected", mismatch.Want).Str("found", mismatch.Got).
			Msg("staffui: archived document failed checksum verification")
		h.redirectDocuments(w, r, subscriberID, "corrupt")
	case errors.Is(err, archive.ErrNotFound):
		h.redirectDocuments(w, r, subscriberID, "gone")
	case errors.Is(err, archive.ErrNotTheirs):
		h.redirectDocuments(w, r, subscriberID, "nottheirs")
	default:
		// Could not read the bytes at all, which is a different problem
		// from a corrupt document and must not be reported as one: the
		// checksum was never compared.
		log.Error().Err(err).Int("subscriber_id", subscriberID).Str("action", action).
			Msg("staffui: archived document could not be read")
		h.redirectDocuments(w, r, subscriberID, "unreadable")
	}
}

func (h *Handler) redirectDocuments(w http.ResponseWriter, r *http.Request, subscriberID int, code string) {
	v := neturl.Values{}
	if subscriberID != 0 {
		v.Set("subscriber_id", strconv.Itoa(subscriberID))
	}
	if code != "" {
		v.Set("m", code)
	}
	url := "/staff/documents"
	if len(v) > 0 {
		url += "?" + v.Encode()
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func extensionFor(kind string) string {
	switch kind {
	case archive.KindInvoice:
		return ".pdf"
	default:
		// KYC scans and report exports are whatever was uploaded or
		// generated, and the archive does not record a media type. No
		// extension is more honest than guessing one.
		return ""
	}
}

func contentTypeFor(kind string) string {
	switch kind {
	case archive.KindInvoice:
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}
