//go:build integration

// The partner API's own surface — FR-API-001..003.
//
// internal/api/partner.go was at 2.1%: 141 statements, the largest untested
// file in the package, and the one that hands out credentials. Three things
// here are security properties rather than features:
//
//   - A key and a webhook signing secret are shown once and never again. An
//     operator who can re-read a key from the console is an attacker with
//     console access who can too.
//   - A partner's webhook endpoints are scoped to their own key. Without the
//     api_key_id predicate, one integrator could disable another's callbacks,
//     or read another's delivery log, by guessing an integer.
//   - A callback URL is checked before it is stored, so an endpoint pointed at
//     the metadata service or a private address is refused at registration
//     rather than at every delivery.
//
// The partner-facing handlers are driven through the real
// middleware.APIKeyMiddleware rather than by injecting a context value, so the
// scoping is exercised the way a request actually arrives.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
)

type fakePartners struct {
	keys       []partner.APIKey
	endpoints  map[int][]partner.WebhookEndpoint // by api key id
	deliveries []partner.Delivery

	gotName    string
	gotPrefix  string
	gotHash    string
	gotScopes  []string
	gotExpires *time.Time
	gotCreator string

	gotEndpointKeyID int
	gotURL           string
	gotEncrypted     string
	gotKeyVersion    string
	gotEvents        []string

	deactivatedID    int
	deactivatedKeyID int
	deliveriesFor    int
	gotLimit         int

	revoked     bool
	deactivated bool

	createKeyErr  error
	listKeysErr   error
	revokeErr     error
	createEPErr   error
	listEPErr     error
	deactivateErr error
	deliveriesErr error
}

func (f *fakePartners) CreateAPIKey(_ context.Context, name, prefix, hash string, scopes []string,
	expiresAt *time.Time, createdBy string) (*partner.APIKey, error) {
	f.gotName, f.gotPrefix, f.gotHash = name, prefix, hash
	f.gotScopes, f.gotExpires, f.gotCreator = scopes, expiresAt, createdBy
	if f.createKeyErr != nil {
		return nil, f.createKeyErr
	}
	return &partner.APIKey{ID: 3, PartnerName: name, KeyPrefix: prefix, Scopes: scopes, ExpiresAt: expiresAt}, nil
}

func (f *fakePartners) ListAPIKeys(context.Context) ([]partner.APIKey, error) {
	return f.keys, f.listKeysErr
}

func (f *fakePartners) RevokeAPIKey(_ context.Context, _ int) (bool, error) {
	return f.revoked, f.revokeErr
}

func (f *fakePartners) CreateWebhookEndpoint(_ context.Context, apiKeyID int, url, secretEncrypted,
	keyVersion string, events []string, _ string) (*partner.WebhookEndpoint, error) {
	f.gotEndpointKeyID, f.gotURL = apiKeyID, url
	f.gotEncrypted, f.gotKeyVersion, f.gotEvents = secretEncrypted, keyVersion, events
	if f.createEPErr != nil {
		return nil, f.createEPErr
	}
	return &partner.WebhookEndpoint{ID: 8, APIKeyID: apiKeyID, URL: url, Events: events}, nil
}

func (f *fakePartners) ListWebhookEndpoints(_ context.Context, apiKeyID int) ([]partner.WebhookEndpoint, error) {
	if f.listEPErr != nil {
		return nil, f.listEPErr
	}
	return f.endpoints[apiKeyID], nil
}

func (f *fakePartners) DeactivateWebhookEndpoint(_ context.Context, endpointID, apiKeyID int) (bool, error) {
	f.deactivatedID, f.deactivatedKeyID = endpointID, apiKeyID
	return f.deactivated, f.deactivateErr
}

func (f *fakePartners) ListDeliveries(_ context.Context, endpointID, limit int) ([]partner.Delivery, error) {
	f.deliveriesFor, f.gotLimit = endpointID, limit
	return f.deliveries, f.deliveriesErr
}

// keyAuth authenticates whatever bearer token a test presents, as the key it
// was built with. Nil means "no such key", which is the unauthorised path.
type keyAuth struct {
	key     *partner.APIKey
	err     error
	touched int
}

func (a *keyAuth) AuthenticateKey(context.Context, string) (*partner.APIKey, error) {
	return a.key, a.err
}

func (a *keyAuth) TouchKeyUsage(_ context.Context, id int) { a.touched = id }

// staffCall is for the operator-facing key management, which is behind the
// staff JWT and needs only a subject.
func staffCall(h *api.Handler, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r = r.WithContext(middleware.WithSubject(r.Context(), "meena"))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) >= 4 {
		r.SetPathValue("id", parts[3])
	}

	w := httptest.NewRecorder()
	switch method {
	case http.MethodPost:
		h.CreateAPIKey(w, r)
	case http.MethodDelete:
		h.RevokeAPIKey(w, r)
	default:
		h.ListAPIKeys(w, r)
	}
	return w
}

// partnerCall drives a partner-facing handler through the real API-key
// middleware, so the api_key_id on the context is the one authentication put
// there.
func partnerCall(h *api.Handler, auth *keyAuth, method, target, body string) *httptest.ResponseRecorder {
	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(path, "/")
		for i, p := range parts {
			if p == "webhooks" && len(parts) > i+1 {
				r.SetPathValue("id", parts[i+1])
			}
		}
		switch {
		case strings.HasSuffix(path, "/deliveries"):
			h.ListWebhookDeliveries(w, r)
		case method == http.MethodPost:
			h.CreateWebhookEndpoint(w, r)
		case method == http.MethodDelete:
			h.DeleteWebhookEndpoint(w, r)
		default:
			h.ListWebhookEndpoints(w, r)
		}
	})

	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer isp_live_whatever")
	w := httptest.NewRecorder()
	middleware.APIKeyMiddleware(auth)(inner).ServeHTTP(w, r)
	return w
}

func partnerHandler(p api.PartnerQuerier, enc api.SecretEncryptor) *api.Handler {
	return api.NewHandler(api.HandlerDeps{Partners: p, SecretEncryptor: enc})
}

// A recording encryptor whose output does not contain its input, so "the
// plaintext did not reach the store" cannot pass by accident.
type partnerEncryptor struct {
	got string
	err error
}

func (e *partnerEncryptor) Encrypt(plaintext string) (string, error) {
	e.got = plaintext
	if e.err != nil {
		return "", e.err
	}
	return "v7:" + strings.Repeat("z", 32), nil
}

func (e *partnerEncryptor) ActiveVersion() string { return "v7" }

// The key is shown once, and what is stored is a hash — not the key.
func TestFR_API_001_TheKeyIsShownOnceAndStoredHashed(t *testing.T) {
	f := &fakePartners{}
	rec := staffCall(partnerHandler(f, nil), http.MethodPost, "/api/v1/partner-keys",
		`{"partner_name":"AcmeCRM","scopes":["read:subscribers"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	var body struct {
		APIKey  string `json:"api_key"`
		Warning string `json:"warning"`
		Key     struct {
			KeyPrefix string `json:"key_prefix"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if body.APIKey == "" {
		t.Fatal("no key was returned; the caller has nothing to store")
	}
	if body.Warning == "" {
		t.Error("the response should say this is the only time the key is shown")
	}

	// The store must never see the key itself.
	if f.gotHash == "" {
		t.Fatal("no hash reached the store")
	}
	if f.gotHash == body.APIKey {
		t.Error("the key was stored verbatim instead of hashed")
	}
	if strings.Contains(f.gotHash, body.APIKey) {
		t.Error("the stored hash contains the key")
	}
	// The prefix is the searchable half and is expected to appear in both.
	if f.gotPrefix == "" || !strings.Contains(body.APIKey, f.gotPrefix) {
		t.Errorf("prefix %q is not the start of the issued key", f.gotPrefix)
	}
	if f.gotCreator != "meena" {
		t.Errorf("created_by = %q, want the staff subject", f.gotCreator)
	}
}

// A test key and a live key are distinguishable, so an integrator cannot
// mistake one for the other in a log.
func TestFR_API_001_TestAndLiveKeysAreDistinguishable(t *testing.T) {
	issue := func(body string) string {
		t.Helper()
		rec := staffCall(partnerHandler(&fakePartners{}, nil), http.MethodPost,
			"/api/v1/partner-keys", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
		}
		var out struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.APIKey
	}

	live := issue(`{"partner_name":"AcmeCRM","scopes":["read:subscribers"]}`)
	test := issue(`{"partner_name":"AcmeCRM","scopes":["read:subscribers"],"test":true}`)
	if live == test {
		t.Fatal("two generated keys are identical")
	}
	if strings.HasPrefix(live, partner.KeyEnvTest) || !strings.Contains(test, partner.KeyEnvTest) {
		t.Errorf("a test key (%q) and a live key (%q) are not distinguishable by sight",
			test[:min(12, len(test))], live[:min(12, len(live))])
	}
}

// A scope no route checks is refused at issue time. A key carrying one reads
// as working right up until somebody depends on it.
func TestFR_API_001_AnUnknownScopeIsRefusedAtIssueTime(t *testing.T) {
	f := &fakePartners{}
	rec := staffCall(partnerHandler(f, nil), http.MethodPost, "/api/v1/partner-keys",
		`{"partner_name":"AcmeCRM","scopes":["read:subscribers","read:everything"]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.gotName != "" {
		t.Error("a key with an unknown scope reached the store")
	}
}

// An expiry in days becomes a timestamp; omitting it leaves the key open.
func TestFR_API_001_AnExpiryInDaysBecomesATimestamp(t *testing.T) {
	f := &fakePartners{}
	staffCall(partnerHandler(f, nil), http.MethodPost, "/api/v1/partner-keys",
		`{"partner_name":"AcmeCRM","scopes":["read:subscribers"],"expires_in_days":30}`)
	if f.gotExpires == nil {
		t.Fatal("expires_in_days did not become an expiry")
	}
	if days := time.Until(*f.gotExpires).Hours() / 24; days < 29 || days > 31 {
		t.Errorf("the key expires in %.1f days, want about 30", days)
	}

	f2 := &fakePartners{}
	staffCall(partnerHandler(f2, nil), http.MethodPost, "/api/v1/partner-keys",
		`{"partner_name":"AcmeCRM","scopes":["read:subscribers"]}`)
	if f2.gotExpires != nil {
		t.Errorf("a key with no expiry was given one: %v", f2.gotExpires)
	}

	// Zero and negative are the same as omitted rather than an expiry in the
	// past, which would issue a key that never worked.
	for _, body := range []string{
		`{"partner_name":"A","scopes":[],"expires_in_days":0}`,
		`{"partner_name":"A","scopes":[],"expires_in_days":-5}`,
	} {
		f3 := &fakePartners{}
		staffCall(partnerHandler(f3, nil), http.MethodPost, "/api/v1/partner-keys", body)
		if f3.gotExpires != nil {
			t.Errorf("%s issued a key expiring at %v", body, f3.gotExpires)
		}
	}
}

// Revoking a key that is not active is 404, not a cheerful success. Reporting
// success would overwrite revoked_at and lose when the key actually stopped
// working — which is the one fact an incident review needs.
func TestFR_API_001_RevokingAnInactiveKeyIsNotFound(t *testing.T) {
	notRevoked := staffCall(partnerHandler(&fakePartners{revoked: false}, nil),
		http.MethodDelete, "/api/v1/partner-keys/3", "")
	if notRevoked.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", notRevoked.Code, notRevoked.Body.String())
	}

	ok := staffCall(partnerHandler(&fakePartners{revoked: true}, nil),
		http.MethodDelete, "/api/v1/partner-keys/3", "")
	if ok.Code != http.StatusOK {
		t.Errorf("want 200 for a real revocation, got %d", ok.Code)
	}

	bad := staffCall(partnerHandler(&fakePartners{}, nil),
		http.MethodDelete, "/api/v1/partner-keys/not-a-number", "")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric id, got %d", bad.Code)
	}
}

// The signing secret is shown once, stored encrypted with the active key
// version, and the endpoint is bound to the calling key — not to anything the
// request body could name.
func TestFR_API_002_TheWebhookSecretIsOneShotAndTheEndpointIsBoundToTheCaller(t *testing.T) {
	f := &fakePartners{endpoints: map[int][]partner.WebhookEndpoint{}}
	enc := &partnerEncryptor{}
	auth := &keyAuth{key: &partner.APIKey{ID: 42, PartnerName: "AcmeCRM",
		Scopes: []string{"manage:webhooks"}}}

	rec := partnerCall(partnerHandler(f, enc), auth, http.MethodPost, "/api/v1/partner/webhooks",
		`{"url":"https://acme.example/hooks/isp","events":["subscriber.created"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	var body struct {
		SigningSecret   string `json:"signing_secret"`
		Warning         string `json:"warning"`
		SignatureHeader string `json:"signature_header"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if body.SigningSecret == "" {
		t.Fatal("no signing secret was returned; the partner cannot verify a delivery")
	}
	if body.Warning == "" || body.SignatureHeader == "" {
		t.Error("the response should name the one-shot rule and the signature header")
	}
	if enc.got != body.SigningSecret {
		t.Errorf("the encryptor was given %q but %q was returned", enc.got, body.SigningSecret)
	}
	if strings.Contains(f.gotEncrypted, body.SigningSecret) || f.gotEncrypted == body.SigningSecret {
		t.Error("the store was given the plaintext signing secret")
	}
	if f.gotKeyVersion != "v7" {
		t.Errorf("key version = %q; without it the secret cannot be read after a rotation",
			f.gotKeyVersion)
	}
	if f.gotEndpointKeyID != 42 {
		t.Errorf("the endpoint was bound to key %d, want the authenticated 42",
			f.gotEndpointKeyID)
	}
	if auth.touched != 42 {
		t.Errorf("key usage was recorded against %d", auth.touched)
	}
}

// A callback URL is validated before registration. Checked at this layer so a
// private or link-local target is a readable refusal rather than an endpoint
// that registers cleanly and fails on every delivery.
func TestFR_API_002_APrivateOrMalformedCallbackURLIsRefused(t *testing.T) {
	for name, url := range map[string]string{
		"loopback":      "http://127.0.0.1/hook",
		"link local":    "http://169.254.169.254/latest/meta-data/",
		"private range": "http://10.0.0.5/hook",
		"not a url":     "not-a-url",
		"no scheme":     "acme.example/hook",
		"file":          "file:///etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakePartners{endpoints: map[int][]partner.WebhookEndpoint{}}
			auth := &keyAuth{key: &partner.APIKey{ID: 42, PartnerName: "AcmeCRM"}}
			rec := partnerCall(partnerHandler(f, &partnerEncryptor{}), auth,
				http.MethodPost, "/api/v1/partner/webhooks",
				`{"url":"`+url+`","events":["subscriber.created"]}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422 for %s, got %d — %s", url, rec.Code, rec.Body.String())
			}
			if f.gotURL != "" {
				t.Errorf("%s was registered anyway", url)
			}
		})
	}
}

// An unknown event type is refused: an endpoint subscribed to an event that
// is never emitted looks configured and delivers nothing.
func TestFR_API_002_AnUnknownEventTypeIsRefused(t *testing.T) {
	f := &fakePartners{endpoints: map[int][]partner.WebhookEndpoint{}}
	auth := &keyAuth{key: &partner.APIKey{ID: 42, PartnerName: "AcmeCRM"}}
	rec := partnerCall(partnerHandler(f, &partnerEncryptor{}), auth,
		http.MethodPost, "/api/v1/partner/webhooks",
		`{"url":"https://acme.example/hook","events":["subscriber.abducted"]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.gotURL != "" {
		t.Error("an endpoint with an unknown event was registered")
	}
}

// One partner cannot touch another's endpoints. The deactivation is passed to
// the store with the caller's key id, so the predicate is in the query rather
// than in a check somebody can forget.
func TestFR_API_002_OnePartnerCannotDisableAnothersWebhook(t *testing.T) {
	f := &fakePartners{
		endpoints:   map[int][]partner.WebhookEndpoint{42: {{ID: 8, APIKeyID: 42}}},
		deactivated: false, // the store finds nothing for this (endpoint, key) pair
	}
	auth := &keyAuth{key: &partner.APIKey{ID: 99, PartnerName: "Intruder"}}

	rec := partnerCall(partnerHandler(f, &partnerEncryptor{}), auth,
		http.MethodDelete, "/api/v1/partner/webhooks/8", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.deactivatedKeyID != 99 {
		t.Errorf("the store was given key id %d; without the caller's own id in the "+
			"predicate any partner could disable any endpoint", f.deactivatedKeyID)
	}
}

// Nor read another's delivery log. Ownership is checked before the log is
// read, not by trusting the id in the path.
func TestFR_API_003_OnePartnerCannotReadAnothersDeliveryLog(t *testing.T) {
	f := &fakePartners{
		endpoints:  map[int][]partner.WebhookEndpoint{42: {{ID: 8, APIKeyID: 42}}},
		deliveries: []partner.Delivery{{ID: 1, EndpointID: 8}},
	}
	intruder := &keyAuth{key: &partner.APIKey{ID: 99, PartnerName: "Intruder"}}

	rec := partnerCall(partnerHandler(f, &partnerEncryptor{}), intruder,
		http.MethodGet, "/api/v1/partner/webhooks/8/deliveries", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.deliveriesFor != 0 {
		t.Errorf("the delivery log for endpoint %d was read for a partner that does not "+
			"own it", f.deliveriesFor)
	}

	// The owner reads their own.
	owner := &keyAuth{key: &partner.APIKey{ID: 42, PartnerName: "AcmeCRM"}}
	ok := partnerCall(partnerHandler(f, &partnerEncryptor{}), owner,
		http.MethodGet, "/api/v1/partner/webhooks/8/deliveries", "")
	if ok.Code != http.StatusOK {
		t.Fatalf("the owner got %d — %s", ok.Code, ok.Body.String())
	}
	if f.deliveriesFor != 8 {
		t.Errorf("the owner's read went to endpoint %d", f.deliveriesFor)
	}
}

// A listing is scoped to the calling key, so a partner with no endpoints sees
// none rather than everybody's.
func TestFR_API_002_AListingIsScopedToTheCallingKey(t *testing.T) {
	f := &fakePartners{endpoints: map[int][]partner.WebhookEndpoint{
		42: {{ID: 8, APIKeyID: 42, URL: "https://acme.example/hook"}},
	}}

	mine := partnerCall(partnerHandler(f, &partnerEncryptor{}),
		&keyAuth{key: &partner.APIKey{ID: 42}}, http.MethodGet, "/api/v1/partner/webhooks", "")
	if !strings.Contains(mine.Body.String(), "acme.example") {
		t.Errorf("the owner's endpoint is missing: %s", mine.Body.String())
	}

	theirs := partnerCall(partnerHandler(f, &partnerEncryptor{}),
		&keyAuth{key: &partner.APIKey{ID: 99}}, http.MethodGet, "/api/v1/partner/webhooks", "")
	if strings.Contains(theirs.Body.String(), "acme.example") {
		t.Errorf("another partner's endpoint was listed: %s", theirs.Body.String())
	}
}

// An unauthenticated or unknown key gets one message, the same for every
// failure mode — telling an attacker which half of a credential was wrong
// tells them what to fix.
func TestFR_API_001_AnInvalidKeyIsRefusedWithoutSayingWhy(t *testing.T) {
	f := &fakePartners{endpoints: map[int][]partner.WebhookEndpoint{}}

	unknown := partnerCall(partnerHandler(f, &partnerEncryptor{}), &keyAuth{key: nil},
		http.MethodGet, "/api/v1/partner/webhooks", "")
	if unknown.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for an unknown key, got %d", unknown.Code)
	}

	broken := partnerCall(partnerHandler(f, &partnerEncryptor{}),
		&keyAuth{err: errors.New("store down")}, http.MethodGet, "/api/v1/partner/webhooks", "")
	if broken.Code != http.StatusInternalServerError {
		t.Errorf("want 500 when the lookup fails, got %d", broken.Code)
	}
	if strings.Contains(broken.Body.String(), "store down") {
		t.Errorf("the store's own error reached the caller: %s", broken.Body.String())
	}
}

// An encryption failure does not register the endpoint — an endpoint whose
// secret could not be stored can never have a verifiable delivery.
func TestFR_API_002_AnEncryptionFailureDoesNotRegisterTheEndpoint(t *testing.T) {
	f := &fakePartners{endpoints: map[int][]partner.WebhookEndpoint{}}
	h := partnerHandler(f, &partnerEncryptor{err: errors.New("crypto: key store unreachable")})

	rec := partnerCall(h, &keyAuth{key: &partner.APIKey{ID: 42}}, http.MethodPost,
		"/api/v1/partner/webhooks",
		`{"url":"https://acme.example/hook","events":["subscriber.created"]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d — %s", rec.Code, rec.Body.String())
	}
	if f.gotURL != "" {
		t.Error("the endpoint was registered with no usable signing secret")
	}
}

// Every endpoint reports an unconfigured deployment rather than panicking.
func TestFR_API_001_AnUnconfiguredPartnerAPIIsReported(t *testing.T) {
	bare := api.NewHandler(api.HandlerDeps{})

	for name, tc := range map[string]struct{ method, target, body string }{
		"create key": {http.MethodPost, "/api/v1/partner-keys", `{"partner_name":"A"}`},
		"list keys":  {http.MethodGet, "/api/v1/partner-keys", ""},
		"revoke":     {http.MethodDelete, "/api/v1/partner-keys/3", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := staffCall(bare, tc.method, tc.target, tc.body); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d", rec.Code)
			}
		})
	}

	auth := &keyAuth{key: &partner.APIKey{ID: 42}}
	for name, tc := range map[string]struct{ method, target, body string }{
		"register":   {http.MethodPost, "/api/v1/partner/webhooks", `{"url":"https://a.example/h"}`},
		"list":       {http.MethodGet, "/api/v1/partner/webhooks", ""},
		"delete":     {http.MethodDelete, "/api/v1/partner/webhooks/8", ""},
		"deliveries": {http.MethodGet, "/api/v1/partner/webhooks/8/deliveries", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := partnerCall(bare, auth, tc.method, tc.target, tc.body); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d", rec.Code)
			}
		})
	}
}

// Issuing and revoking a key, and registering and removing an endpoint, are
// each audited. A credential that appears with no trail of who issued it is
// the thing an incident review cannot work with.
func TestFR_API_001_CredentialLifecycleIsAudited(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	f := &fakePartners{
		revoked:     true,
		deactivated: true,
		endpoints:   map[int][]partner.WebhookEndpoint{42: {{ID: 8, APIKeyID: 42}}},
	}
	h := partnerHandler(f, &partnerEncryptor{})
	auth := &keyAuth{key: &partner.APIKey{ID: 42, PartnerName: "AcmeCRM"}}

	staffCall(h, http.MethodPost, "/api/v1/partner-keys",
		`{"partner_name":"AcmeCRM","scopes":["read:subscribers"]}`)
	staffCall(h, http.MethodDelete, "/api/v1/partner-keys/3", "")
	partnerCall(h, auth, http.MethodPost, "/api/v1/partner/webhooks",
		`{"url":"https://acme.example/hook","events":["subscriber.created"]}`)
	partnerCall(h, auth, http.MethodDelete, "/api/v1/partner/webhooks/8", "")

	seen := map[string]bool{}
	for _, e := range sink.entries() {
		seen[e.Action] = true
		// The key itself must never appear in the trail.
		for _, v := range e.Detail {
			if s, ok := v.(string); ok && (strings.HasPrefix(s, partner.KeyEnvLive) || strings.HasPrefix(s, partner.KeyEnvTest)) {
				t.Errorf("a credential reached the audit trail: %q", s)
			}
		}
	}
	for _, want := range []string{
		"partner.key_created", "partner.key_revoked",
		"partner.webhook_registered", "partner.webhook_removed",
	} {
		if !seen[want] {
			t.Errorf("%q was not audited; recorded: %v", want, seen)
		}
	}
}

// The delivery listing passes the caller's limit through, and the keys
// listing answers a shape a client can read.
func TestFR_API_003_ReadsPassThroughAndFailuresAreReported(t *testing.T) {
	f := &fakePartners{
		keys:       []partner.APIKey{{ID: 3, PartnerName: "AcmeCRM", KeyPrefix: "pk_live_abcd"}},
		endpoints:  map[int][]partner.WebhookEndpoint{42: {{ID: 8, APIKeyID: 42}}},
		deliveries: []partner.Delivery{{ID: 1, EndpointID: 8}},
	}
	h := partnerHandler(f, &partnerEncryptor{})
	auth := &keyAuth{key: &partner.APIKey{ID: 42}}

	if rec := staffCall(h, http.MethodGet, "/api/v1/partner-keys", ""); rec.Code != http.StatusOK {
		t.Errorf("list keys: %d — %s", rec.Code, rec.Body.String())
	}
	if rec := partnerCall(h, auth, http.MethodGet,
		"/api/v1/partner/webhooks/8/deliveries?limit=5", ""); rec.Code != http.StatusOK {
		t.Errorf("deliveries: %d — %s", rec.Code, rec.Body.String())
	}
	if f.gotLimit != 5 {
		t.Errorf("the limit reached the store as %d, want 5", f.gotLimit)
	}

	broken := partnerHandler(&fakePartners{
		listKeysErr:   errors.New("store down"),
		listEPErr:     errors.New("store down"),
		deactivateErr: errors.New("store down"),
		revokeErr:     errors.New("store down"),
	}, &partnerEncryptor{})
	if rec := staffCall(broken, http.MethodGet, "/api/v1/partner-keys", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	if rec := staffCall(broken, http.MethodDelete, "/api/v1/partner-keys/3", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	if rec := partnerCall(broken, auth, http.MethodGet, "/api/v1/partner/webhooks", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	if rec := partnerCall(broken, auth, http.MethodDelete, "/api/v1/partner/webhooks/8", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", rec.Code)
	}
	if rec := partnerCall(broken, auth, http.MethodGet, "/api/v1/partner/webhooks/not-a-number/deliveries", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric endpoint id, got %d", rec.Code)
	}
}
