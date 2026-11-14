package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"golang.org/x/crypto/bcrypt"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// EAP-MSCHAPv2 enrolment — FR-AAA-006 | MDS §4.18.
//
// Enrolment is a deliberate, per-subscriber action rather than something the
// migration did for everybody. The NT hash is unsalted MD4 and therefore
// credential-equivalent for MSCHAPv2 if the database leaks, so the
// population carrying one is kept to the population that actually needs it —
// hotspot and wireless-controller users, not the whole base.
//
// It also cannot be backfilled: MD4(UTF-16LE(password)) needs the plaintext,
// and all we store is bcrypt. So the password has to be presented again.

// EAPEnrolmentQuerier is the persistence surface enrolment needs.
// Satisfied by *db.APIStore.
type EAPEnrolmentQuerier interface {
	SetNTHash(ctx context.Context, subscriberID int, ntHash []byte) error
	IsEAPEnrolled(ctx context.Context, subscriberID int) (bool, error)
}

type eapEnrolRequest struct {
	// Password is the subscriber's existing password, re-presented so the
	// NT hash can be derived from it.
	Password string `json:"password"`
}

// EnrolEAP handles POST /api/v1/subscribers/{id}/eap.
//
// The supplied password is verified against the stored bcrypt hash before
// anything is written. Without that check this endpoint would be a way to
// *set* a second, divergent credential: an operator could enrol a password
// of their choosing and authenticate as the subscriber over EAP while the
// subscriber's own PAP password kept working, leaving no visible trace.
//
// FR: FR-AAA-006 | MDS §4.18
func (h *Handler) EnrolEAP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.eapEnrolment == nil || h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "EAP enrolment not configured")
		return
	}

	var req eapEnrolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"password is required — the NT hash cannot be derived from the stored bcrypt hash")
		return
	}

	sub, err := h.db.GetSubscriberByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "subscriber lookup failed")
		return
	}
	if sub == nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND", "subscriber not found")
		return
	}

	// The password must be the subscriber's real one, proven against bcrypt.
	if err := h.verifySubscriberPassword(r.Context(), sub.Username, req.Password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"password does not match this subscriber's current password")
		return
	}

	if err := h.eapEnrolment.SetNTHash(r.Context(), id, radius.NTPasswordHash(req.Password)); err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "EAP enrolment failed")
		return
	}

	middleware.Audit(r.Context(), "subscriber.eap_enrol", strconv.Itoa(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"subscriber_id": id, "eap_enrolled": true})
}

// UnenrolEAP handles DELETE /api/v1/subscribers/{id}/eap — clearing the NT
// hash so only PAP remains.
//
// Worth having as a first-class action rather than leaving the row set
// forever: it is the remediation step if a device is lost or the hash is
// suspected exposed, and it must not require a password to perform.
func (h *Handler) UnenrolEAP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.eapEnrolment == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "EAP enrolment not configured")
		return
	}

	if err := h.eapEnrolment.SetNTHash(r.Context(), id, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "EAP un-enrolment failed")
		return
	}

	middleware.Audit(r.Context(), "subscriber.eap_unenrol", strconv.Itoa(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"subscriber_id": id, "eap_enrolled": false})
}

// GetEAPStatus handles GET /api/v1/subscribers/{id}/eap.
//
// Reports only whether an NT hash exists, never the hash itself — an
// endpoint returning credential material would defeat the point of keeping
// the enrolled population small.
func (h *Handler) GetEAPStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.eapEnrolment == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "EAP enrolment not configured")
		return
	}

	enrolled, err := h.eapEnrolment.IsEAPEnrolled(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "ERR_SUBSCRIBER_NOT_FOUND", "subscriber not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriber_id": id, "eap_enrolled": enrolled})
}

// verifySubscriberPassword compares a presented password against the stored
// bcrypt hash.
//
// Uses the API-side subscriber record, which carries no password hash, so it
// goes through the dedicated credential check rather than widening
// SubscriberRecord with a field every other handler would then be able to
// serialise by accident.
func (h *Handler) verifySubscriberPassword(ctx context.Context, username, password string) error {
	if h.credentials == nil {
		return errors.New("api: credential verification not configured")
	}
	hash, err := h.credentials.GetPasswordHash(ctx, username)
	if err != nil {
		return err
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// CredentialQuerier reads and writes a subscriber's stored credentials.
// Deliberately a separate, narrow interface: the password material must not
// become a field on SubscriberRecord, which is serialised to API clients.
//
// GetPasswordHash serves verification (bcrypt compare). The three that follow
// back the console's reveal/reset feature: a recoverable, AES-encrypted copy of
// the password kept beside the hash (migration 051) so an operator can show a
// customer their PPPoE password — the bcrypt hash alone can never be read back.
type CredentialQuerier interface {
	GetPasswordHash(ctx context.Context, username string) (string, error)
	GetSubscriberPasswordEnc(ctx context.Context, id int) (string, error)
	SetSubscriberPasswordEnc(ctx context.Context, id int, passwordEnc string) error
	SetSubscriberCredential(ctx context.Context, id int, passwordHash, passwordEnc string) error
}
