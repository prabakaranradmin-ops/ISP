package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// Subscriber password reveal and reset — the operations console's
// "view / reset password" feature.
//
// The authoritative credential is the one-way bcrypt hash RADIUS checks; it
// can never be read back. To let an operator show a customer their PPPoE
// password, a second, recoverable copy is encrypted with the AES key store
// (migration 051, same scheme as NAS secrets) and kept beside the hash. These
// methods are the only path that reads it back, and every read is audited.
//
// Exposed as methods on api.Handler, not new HTTP routes: the console calls
// them directly, the same way it calls ProvisionSubscriber and the bulk
// actions, so the credential logic lives in one place.

// minSubscriberPasswordLength is the floor for a reset password. Modest on
// purpose: PPPoE/hotspot passwords are often short and set by the operator, not
// the subscriber, so this guards against an empty or one-character mistake
// rather than imposing a portal-style policy.
const minSubscriberPasswordLength = 6

// ErrPasswordNotCaptured reports that a subscriber has no recoverable password
// stored — created before migration 051, or not re-saved since. The caller
// offers a reset rather than treating it as an error.
var ErrPasswordNotCaptured = errors.New("api: no recoverable password stored for this subscriber")

// StoreSubscriberPasswordEnc captures the recoverable copy of a just-set
// password. Called right after ProvisionSubscriber, whose INSERT already wrote
// the bcrypt hash.
//
// Best-effort by contract: the caller logs a failure rather than failing the
// create, because the subscriber can still authenticate (the hash is set) and
// the copy can be captured later by a reset. It still returns the error so the
// caller can log it.
func (h *Handler) StoreSubscriberPasswordEnc(ctx context.Context, id int, plaintext string) error {
	if h.secretEncryptor == nil || h.credentials == nil {
		return errors.New("api: password capture not configured (no key store)")
	}
	enc, err := h.secretEncryptor.Encrypt(plaintext)
	if err != nil {
		return fmt.Errorf("api: encrypt subscriber password: %w", err)
	}
	return h.credentials.SetSubscriberPasswordEnc(ctx, id, enc)
}

// RevealSubscriberPassword returns a subscriber's plaintext PPPoE password,
// decrypted from the recoverable copy. The bool is false (with a nil error)
// when none was captured, which the console shows as "not captured — reset to
// set one".
//
// Every successful reveal is written to the audit log: showing a live
// credential is exactly the kind of access an operator must be answerable for.
func (h *Handler) RevealSubscriberPassword(ctx context.Context, id int) (string, bool, error) {
	if h.credentials == nil || h.keyStore == nil {
		return "", false, errors.New("api: password reveal not configured (no key store)")
	}
	enc, err := h.credentials.GetSubscriberPasswordEnc(ctx, id)
	if err != nil {
		return "", false, err
	}
	if enc == "" {
		return "", false, nil
	}
	plaintext, err := crypto.Decrypt(enc, h.keyStore)
	if err != nil {
		return "", false, fmt.Errorf("api: decrypt subscriber password: %w", err)
	}
	middleware.Audit(ctx, "subscriber.password_revealed", strconv.Itoa(id), nil)
	return plaintext, true, nil
}

// ResetSubscriberPassword sets a new password: it replaces the bcrypt hash RADIUS
// checks and the recoverable copy the console reveals, in one write so the two
// cannot drift. If the subscriber is enrolled for EAP-MSCHAPv2, its NT hash is
// re-derived too, so a password change does not silently break their EAP login.
func (h *Handler) ResetSubscriberPassword(ctx context.Context, id int, newPassword string) error {
	if h.credentials == nil || h.secretEncryptor == nil {
		return errors.New("api: password reset not configured (no key store)")
	}
	if len(newPassword) < minSubscriberPasswordLength {
		return fmt.Errorf("%w: password must be at least %d characters",
			ErrSubscriberInvalid, minSubscriberPasswordLength)
	}

	hash, err := hashSubscriberPassword(newPassword)
	if err != nil {
		return fmt.Errorf("api: hash new password: %w", err)
	}
	enc, err := h.secretEncryptor.Encrypt(newPassword)
	if err != nil {
		return fmt.Errorf("api: encrypt new password: %w", err)
	}
	if err := h.credentials.SetSubscriberCredential(ctx, id, hash, enc); err != nil {
		return err
	}

	// Keep EAP working across the change: the NT hash is derived from the
	// plaintext, which only exists here, so a stale one left behind would reject
	// the subscriber's EAP login with no obvious cause. Only touched when they
	// are already enrolled — this never enrols someone who was not.
	if h.eapEnrolment != nil {
		if enrolled, err := h.eapEnrolment.IsEAPEnrolled(ctx, id); err == nil && enrolled {
			if err := h.eapEnrolment.SetNTHash(ctx, id, radius.NTPasswordHash(newPassword)); err != nil {
				return fmt.Errorf("api: refresh NT hash after password reset: %w", err)
			}
		}
	}

	middleware.Audit(ctx, "subscriber.password_reset", strconv.Itoa(id), nil)
	return nil
}
