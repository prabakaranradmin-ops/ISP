package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
)

// Partner API management — FR-API-001..003 | MDS §4.22.
//
// Two audiences on one file. Key issuance and revocation are staff-only
// (isp_owner), because minting a credential that reads subscriber data is an
// administrative act. Webhook registration is partner-facing and authenticated
// by the key itself, so an integrator can manage their own callbacks without a
// support ticket.

// PartnerQuerier is the persistence surface. Satisfied by *db.PartnerStore.
type PartnerQuerier interface {
	CreateAPIKey(ctx context.Context, partnerName, prefix, hash string, scopes []string,
		expiresAt *time.Time, createdBy string) (*partner.APIKey, error)
	ListAPIKeys(ctx context.Context) ([]partner.APIKey, error)
	RevokeAPIKey(ctx context.Context, keyID int) (bool, error)
	CreateWebhookEndpoint(ctx context.Context, apiKeyID int, url, secretEncrypted,
		keyVersion string, events []string, description string) (*partner.WebhookEndpoint, error)
	ListWebhookEndpoints(ctx context.Context, apiKeyID int) ([]partner.WebhookEndpoint, error)
	DeactivateWebhookEndpoint(ctx context.Context, endpointID, apiKeyID int) (bool, error)
	ListDeliveries(ctx context.Context, endpointID, limit int) ([]partner.Delivery, error)
}

// SecretEncryptor encrypts a webhook signing secret for storage.
type SecretEncryptor interface {
	Encrypt(plaintext string) (string, error)
	ActiveVersion() string
}

type createKeyRequest struct {
	PartnerName   string   `json:"partner_name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays *int     `json:"expires_in_days,omitempty"`
	Test          bool     `json:"test,omitempty"`
}

// CreateAPIKey handles POST /api/v1/partner-keys (staff, isp_owner).
func (h *Handler) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "partner API not configured")
		return
	}

	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.PartnerName == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "partner_name is required")
		return
	}
	// Refused here rather than stored: a key carrying a scope no route checks
	// reads as working right up until somebody depends on it.
	if err := partner.ValidateScopes(req.Scopes); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	env := partner.KeyEnvLive
	if req.Test {
		env = partner.KeyEnvTest
	}
	generated, err := partner.GenerateKey(env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not generate a key")
		return
	}

	var expiresAt *time.Time
	if req.ExpiresInDays != nil && *req.ExpiresInDays > 0 {
		t := time.Now().AddDate(0, 0, *req.ExpiresInDays)
		expiresAt = &t
	}

	key, err := h.partners.CreateAPIKey(r.Context(), req.PartnerName, generated.Prefix,
		generated.Hash, req.Scopes, expiresAt, middleware.SubjectFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not create the key")
		return
	}

	middleware.Audit(r.Context(), "partner.key_created", req.PartnerName, map[string]any{
		"key_id": key.ID, "scopes": req.Scopes,
	})

	// The plaintext appears here and nowhere else, ever. A key an operator can
	// re-read from the console is one an attacker with console access can too.
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     key,
		"api_key": generated.Plaintext,
		"warning": "This is the only time the key is shown. Store it now; it cannot be recovered.",
	})
}

// ListAPIKeys handles GET /api/v1/partner-keys (staff).
func (h *Handler) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "partner API not configured")
		return
	}
	keys, err := h.partners.ListAPIKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list keys failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// RevokeAPIKey handles DELETE /api/v1/partner-keys/{id} (staff).
func (h *Handler) RevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "partner API not configured")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "id must be numeric")
		return
	}

	revoked, err := h.partners.RevokeAPIKey(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "revoke failed")
		return
	}
	if !revoked {
		// Honest rather than idempotent-looking: reporting success would
		// overwrite revoked_at and lose when the key actually stopped working.
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no active key with that id")
		return
	}

	middleware.Audit(r.Context(), "partner.key_revoked", strconv.Itoa(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "key_id": id})
}

// ── Partner-facing: webhook management ──────────────────────────────────────

type createEndpointRequest struct {
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Description string   `json:"description,omitempty"`
}

// CreateWebhookEndpoint handles POST /api/v1/partner/webhooks.
//
// Authenticated by the partner's own key; the endpoint is bound to that key,
// so a partner can only ever register callbacks under their own credential.
func (h *Handler) CreateWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil || h.secretEncryptor == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "webhooks not configured")
		return
	}
	apiKeyID := middleware.APIKeyIDFromContext(r.Context())
	if apiKeyID == 0 {
		writeError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "invalid API key")
		return
	}

	var req createEndpointRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := partner.ValidateEvents(req.Events); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	// The friendly half of the SSRF defence — an immediate, readable refusal
	// instead of an endpoint that registers cleanly and fails on every
	// delivery. The dialler re-checks after DNS resolution, which is the half
	// that actually holds against rebinding.
	if err := partner.ValidateWebhookURL(r.Context(), req.URL); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	secret, err := generateWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not generate a signing secret")
		return
	}
	encrypted, err := h.secretEncryptor.Encrypt(secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not store the signing secret")
		return
	}

	endpoint, err := h.partners.CreateWebhookEndpoint(r.Context(), apiKeyID, req.URL,
		encrypted, h.secretEncryptor.ActiveVersion(), req.Events, req.Description)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not register the endpoint")
		return
	}

	middleware.Audit(r.Context(), "partner.webhook_registered", req.URL, map[string]any{
		"endpoint_id": endpoint.ID, "events": req.Events,
	})

	// Same one-shot rule as the API key: the secret is returned once and after
	// that exists only encrypted. A partner who loses it registers a new
	// endpoint rather than reading the old secret back.
	writeJSON(w, http.StatusCreated, map[string]any{
		"endpoint":         endpoint,
		"signing_secret":   secret,
		"warning":          "This is the only time the signing secret is shown.",
		"signature_header": partner.HeaderSignature,
	})
}

// ListWebhookEndpoints handles GET /api/v1/partner/webhooks.
func (h *Handler) ListWebhookEndpoints(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "webhooks not configured")
		return
	}
	apiKeyID := middleware.APIKeyIDFromContext(r.Context())
	endpoints, err := h.partners.ListWebhookEndpoints(r.Context(), apiKeyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list endpoints failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": endpoints})
}

// DeleteWebhookEndpoint handles DELETE /api/v1/partner/webhooks/{id}.
func (h *Handler) DeleteWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "webhooks not configured")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "id must be numeric")
		return
	}

	// Scoped to the calling key: without the api_key_id predicate one partner
	// could disable another's webhooks by guessing an integer.
	apiKeyID := middleware.APIKeyIDFromContext(r.Context())
	ok, err := h.partners.DeactivateWebhookEndpoint(r.Context(), id, apiKeyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "deactivate failed")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no active endpoint with that id")
		return
	}

	middleware.Audit(r.Context(), "partner.webhook_removed", strconv.Itoa(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"deactivated": true, "endpoint_id": id})
}

// ListWebhookDeliveries handles GET /api/v1/partner/webhooks/{id}/deliveries —
// the audit trail FR-API-003 requires, readable by the partner themselves so a
// failed integration is self-diagnosable.
func (h *Handler) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	if h.partners == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "webhooks not configured")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "id must be numeric")
		return
	}

	// Ownership check before reading: the delivery log is scoped to the
	// endpoint's owner, not to whoever knows its id.
	apiKeyID := middleware.APIKeyIDFromContext(r.Context())
	endpoints, err := h.partners.ListWebhookEndpoints(r.Context(), apiKeyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "lookup failed")
		return
	}
	owned := false
	for _, e := range endpoints {
		if e.ID == id {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no endpoint with that id")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	deliveries, err := h.partners.ListDeliveries(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list deliveries failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
}

// generateWebhookSecret mints a signing secret.
//
// 32 bytes of CSPRNG output, base64url encoded. The partner stores this and
// recomputes the HMAC over each delivery; it never travels again after the
// registration response.
func generateWebhookSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// EventEmitter fans a lifecycle event out to subscribed partner endpoints.
// Satisfied by *partner.Emitter; nil in deployments with no integrations.
type EventEmitter interface {
	Emit(ctx context.Context, eventType string, entityID int)
}

// emit is the nil-safe call site used from the lifecycle handlers.
//
// Emission is deliberately fire-and-forget and never affects the response: a
// partner's webhook configuration must not be able to fail subscriber
// creation, and an operator should never see an error about a third party's
// integration while doing their own job.
func (h *Handler) emit(ctx context.Context, eventType string, entityID int) {
	if h.events == nil {
		return
	}
	h.events.Emit(ctx, eventType, entityID)
}
