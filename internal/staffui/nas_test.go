package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// The Routers screen — FR-NAS-001..004.
//
// nas.go was at 0%: 113 statements. This screen registers the devices that
// authenticate every subscriber, and the field that matters most never
// reaches the page again once saved — the RADIUS shared secret. It is
// encrypted before storage with a versioned key, and the two things worth
// pinning down are that it never travels or renders in plaintext, and that a
// blank secret on an edit leaves the existing one alone rather than
// overwriting it with nothing. A device whose secret was silently blanked
// stops authenticating every customer behind it.

var errNASStore = errors.New("store: nas_devices unreachable")

type fakeNASStore struct {
	devices   []nas.DeviceSummary
	created   *nas.NewNASDevice
	updated   *nas.NASDeviceUpdate
	updateID  int
	listErr   error
	createErr error
	updateErr error
}

func (f *fakeNASStore) ListNASDeviceSummaries(context.Context) ([]nas.DeviceSummary, error) {
	return f.devices, f.listErr
}

func (f *fakeNASStore) CreateNASDevice(_ context.Context, d nas.NewNASDevice) (*nas.DeviceSummary, error) {
	f.created = &d
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &nas.DeviceSummary{ID: 5, IP: d.IP, Vendor: d.Vendor, CoAPort: d.CoAPort, PoDPort: d.PoDPort}, nil
}

func (f *fakeNASStore) UpdateNASDevice(_ context.Context, id int, u nas.NASDeviceUpdate) (*nas.DeviceSummary, error) {
	f.updateID, f.updated = id, &u
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &nas.DeviceSummary{ID: id, IP: "192.0.2.10", Vendor: "mikrotik"}, nil
}

// recordingEncryptor stands in for the AES key store. It records what it was
// asked to encrypt, so a test can prove the plaintext reached the encryptor
// and not the store.
//
// Deliberately not traffic_test.go's fakeEncryptor, which returns "enc:"+p —
// a ciphertext containing its own plaintext would make the assertion below
// ("the store was not given the secret") pass for the wrong reason.
type recordingEncryptor struct {
	got string
	err error
}

func (f *recordingEncryptor) Encrypt(plaintext string) (string, error) {
	f.got = plaintext
	if f.err != nil {
		return "", f.err
	}
	return "v3:" + strings.Repeat("x", 24), nil
}

func (f *recordingEncryptor) ActiveVersion() string { return "v3" }

func serveNAS(h *Handler, s Session, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 {
		r.SetPathValue("id", parts[2])
	}

	w := httptest.NewRecorder()
	switch {
	case method == http.MethodPost && len(parts) >= 3:
		h.UpdateNASDeviceForm(w, r)
	case method == http.MethodPost:
		h.CreateNASDeviceForm(w, r)
	default:
		h.NAS(w, r)
	}
	return w
}

func noc() Session { return Session{StaffID: 3, Username: "noc_user", Role: "noc_engineer"} }

func nasHandler(store *fakeNASStore, enc *recordingEncryptor) *Handler {
	h := &Handler{}
	if store != nil {
		h.nas = store
	}
	if enc != nil {
		h.secretEncryptor = enc
	}
	return h
}

// The secret is a 16-character minimum, encrypted with the active key
// version, and never echoed back. Three assertions in one place because they
// are one requirement: a shared secret that leaks is every subscriber behind
// that router.
func TestFR_NAS_001_TheSharedSecretIsEncryptedAndNeverEchoed(t *testing.T) {
	store, enc := &fakeNASStore{}, &recordingEncryptor{}
	h := nasHandler(store, enc)
	const secret = "a-very-long-radius-secret"

	rec := serveNAS(h, noc(), http.MethodPost, "/staff/nas", url.Values{
		"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
		"description": {"Anna Nagar BNG"}, "radius_secret": {secret},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	if enc.got != secret {
		t.Errorf("the encryptor was given %q; it must receive the plaintext secret", enc.got)
	}
	if store.created == nil {
		t.Fatal("nothing reached the store")
	}
	if store.created.SecretEncrypted == secret || store.created.SecretEncrypted == "" {
		t.Errorf("the store was given %q as the secret; it must be the ciphertext",
			store.created.SecretEncrypted)
	}
	if store.created.KeyVersion != "v3" {
		t.Errorf("key version = %q, want the encryptor's active version — without it the "+
			"secret cannot be decrypted after a rotation", store.created.KeyVersion)
	}
	if body := pageText(rec.Body.String()); strings.Contains(body, secret) {
		t.Errorf("the plaintext secret was rendered back to the browser: %s", firstLines(body))
	}
}

// A blank secret on an edit leaves the stored one alone. Overwriting it with
// nothing would stop every subscriber behind that router authenticating, and
// the screen gives no hint that it happened.
func TestFR_NAS_002_AnOmittedSecretOnAnEditLeavesTheExistingOne(t *testing.T) {
	store, enc := &fakeNASStore{}, &recordingEncryptor{}
	h := nasHandler(store, enc)

	rec := serveNAS(h, noc(), http.MethodPost, "/staff/nas/5", url.Values{
		"vendor": {"mikrotik"}, "description": {"renamed"}, "radius_secret": {""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if store.updated == nil {
		t.Fatal("nothing reached the store")
	}
	if store.updated.SecretEncrypted != nil {
		t.Errorf("a blank secret was sent to the store as %v; it must be left untouched",
			*store.updated.SecretEncrypted)
	}
	if enc.got != "" {
		t.Errorf("the encryptor was called with %q for an edit that rotates nothing", enc.got)
	}
	if store.updateID != 5 {
		t.Errorf("the store was asked to update device %d", store.updateID)
	}
}

// A supplied secret on an edit is rotated, through the encryptor.
func TestFR_NAS_002_ASuppliedSecretOnAnEditIsRotated(t *testing.T) {
	store, enc := &fakeNASStore{}, &recordingEncryptor{}
	h := nasHandler(store, enc)
	const rotated = "the-new-radius-secret-value"

	serveNAS(h, noc(), http.MethodPost, "/staff/nas/5", url.Values{
		"vendor": {"mikrotik"}, "radius_secret": {rotated},
	})
	if enc.got != rotated {
		t.Errorf("the encryptor was given %q, want the new secret", enc.got)
	}
	if store.updated == nil || store.updated.SecretEncrypted == nil {
		t.Fatal("the rotated secret did not reach the store")
	}
	if *store.updated.SecretEncrypted == rotated {
		t.Error("the store was given the plaintext secret")
	}
}

// A short secret is refused on both paths, and never reaches the store. Short
// is the one that matters: a two-character shared secret makes the RADIUS
// authenticator trivially forgeable.
func TestFR_NAS_001_AShortSecretIsRefusedOnBothPaths(t *testing.T) {
	for name, tc := range map[string]struct {
		target string
		form   url.Values
	}{
		"create": {"/staff/nas", url.Values{
			"ip": {"192.0.2.10"}, "vendor": {"mikrotik"}, "radius_secret": {"short"},
		}},
		"rotate": {"/staff/nas/5", url.Values{
			"vendor": {"mikrotik"}, "radius_secret": {"short"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			store, enc := &fakeNASStore{}, &recordingEncryptor{}
			rec := serveNAS(nasHandler(store, enc), noc(), http.MethodPost, tc.target, tc.form)
			if !strings.Contains(pageText(rec.Body.String()), "at least 16 characters") {
				t.Errorf("want a length message, got: %s", firstLines(rec.Body.String()))
			}
			if store.created != nil || store.updated != nil {
				t.Error("a short secret reached the store")
			}
			if enc.got != "" {
				t.Errorf("a short secret was passed to the encryptor: %q", enc.got)
			}
		})
	}
}

// Rotation with no key configured is refused rather than storing a plaintext
// secret or a blank one.
func TestFR_NAS_002_RotationWithNoEncryptionKeyIsRefused(t *testing.T) {
	store := &fakeNASStore{}
	h := nasHandler(store, nil) // no encryptor

	rec := serveNAS(h, noc(), http.MethodPost, "/staff/nas/5", url.Values{
		"vendor": {"mikrotik"}, "radius_secret": {"a-very-long-radius-secret"},
	})
	if !strings.Contains(pageText(rec.Body.String()), "No encryption key is configured") {
		t.Errorf("want a refusal naming the missing key, got: %s", firstLines(rec.Body.String()))
	}
	if store.updated != nil {
		t.Errorf("the update went ahead without an encryptor: %+v", store.updated)
	}
}

// An encryption failure does not store the device with an empty secret.
func TestFR_NAS_001_AnEncryptionFailureDoesNotRegisterTheDevice(t *testing.T) {
	store := &fakeNASStore{}
	h := nasHandler(store, &recordingEncryptor{err: errors.New("crypto: key store unreachable")})

	rec := serveNAS(h, noc(), http.MethodPost, "/staff/nas", url.Values{
		"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
		"radius_secret": {"a-very-long-radius-secret"},
	})
	if !strings.Contains(pageText(rec.Body.String()), "Could not encrypt") {
		t.Errorf("want an encryption message, got: %s", firstLines(rec.Body.String()))
	}
	if store.created != nil {
		t.Errorf("a device was registered with no usable secret: %+v", store.created)
	}
}

// The form's validation, with the reason named. Each of these came back as a
// different message, and an operator who is told the wrong field looks in the
// wrong place.
func TestFR_NAS_001_FormValidationNamesTheFieldThatIsWrong(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"not an ip": {url.Values{
			"ip": {"router-1"}, "vendor": {"mikrotik"}, "radius_secret": {"a-very-long-radius-secret"},
		}, "IP must be a valid address"},
		"unknown vendor": {url.Values{
			"ip": {"192.0.2.10"}, "vendor": {"tp-link"}, "radius_secret": {"a-very-long-radius-secret"},
		}, "Vendor must be one of"},
		"bad coa port": {url.Values{
			"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
			"radius_secret": {"a-very-long-radius-secret"}, "coa_port": {"70000"},
		}, "CoA port must be between 1 and 65535"},
		"bad pod port": {url.Values{
			"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
			"radius_secret": {"a-very-long-radius-secret"}, "pod_port": {"-1"},
		}, "PoD port must be between 1 and 65535"},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeNASStore{}
			rec := serveNAS(nasHandler(store, &recordingEncryptor{}), noc(),
				http.MethodPost, "/staff/nas", tc.form)
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if store.created != nil {
				t.Error("invalid input reached the store")
			}
		})
	}
}

// Omitted control ports fall back to the value most deployments here need,
// rather than to zero — a device registered with CoA port 0 cannot be sent a
// disconnect, and nothing about the screen would say why.
func TestFR_NAS_003_OmittedControlPortsGetTheWorkingDefault(t *testing.T) {
	store := &fakeNASStore{}
	serveNAS(nasHandler(store, &recordingEncryptor{}), noc(), http.MethodPost, "/staff/nas", url.Values{
		"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
		"radius_secret": {"a-very-long-radius-secret"},
	})
	if store.created == nil {
		t.Fatal("nothing reached the store")
	}
	if store.created.CoAPort != defaultControlPort || store.created.PoDPort != defaultControlPort {
		t.Errorf("ports defaulted to CoA %d / PoD %d, want %d for both",
			store.created.CoAPort, store.created.PoDPort, defaultControlPort)
	}
}

// MAB is off unless the checkbox was ticked. On by default would let an
// unknown MAC onto the network.
func TestFR_NAS_004_MABIsOffUnlessAskedFor(t *testing.T) {
	base := url.Values{
		"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
		"radius_secret": {"a-very-long-radius-secret"},
	}

	off := &fakeNASStore{}
	serveNAS(nasHandler(off, &recordingEncryptor{}), noc(), http.MethodPost, "/staff/nas", base)
	if off.created == nil || off.created.AllowMAB {
		t.Error("MAB defaulted to on; an unknown MAC would be admitted")
	}

	on := &fakeNASStore{}
	withMAB := url.Values{}
	for k, v := range base {
		withMAB[k] = v
	}
	withMAB.Set("allow_mab", "on")
	serveNAS(nasHandler(on, &recordingEncryptor{}), noc(), http.MethodPost, "/staff/nas", withMAB)
	if on.created == nil || !on.created.AllowMAB {
		t.Error("the ticked checkbox did not reach the store")
	}
}

// Only the owner and the NOC reach this screen. A CSR registering a router is
// not a mistake the console should allow.
func TestFR_NAS_001_OnlyTheOwnerAndNOCReachTheRoutersScreen(t *testing.T) {
	h := nasHandler(&fakeNASStore{}, &recordingEncryptor{})

	for _, role := range []string{"billing_admin", "csr", "technician", "lco",
		"franchise_admin", "franchise_staff"} {
		s := Session{Username: role + "_user", Role: role}
		if code := serveNAS(h, s, http.MethodGet, "/staff/nas", nil).Code; code == http.StatusOK {
			t.Errorf("%s reached the Routers screen", role)
		}
	}
	for _, role := range []string{"isp_owner", "noc_engineer"} {
		s := Session{Username: role + "_user", Role: role}
		if code := serveNAS(h, s, http.MethodGet, "/staff/nas", nil).Code; code != http.StatusOK {
			t.Errorf("%s got %d from the Routers screen", role, code)
		}
	}
}

// Registered devices and the vendor list render; a store failure says so
// without a blank screen.
func TestFR_NAS_001_TheDeviceListRendersAndAFailureIsReadable(t *testing.T) {
	h := nasHandler(&fakeNASStore{devices: []nas.DeviceSummary{
		{ID: 1, IP: "192.0.2.10", Vendor: "mikrotik", Description: "Anna Nagar BNG",
			CoAPort: 1700, PoDPort: 1700},
	}}, &recordingEncryptor{})

	body := pageText(serveNAS(h, noc(), http.MethodGet, "/staff/nas", nil).Body.String())
	for _, want := range []string{"192.0.2.10", "Anna Nagar BNG"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from the device list", want)
		}
	}
	// The vendor list has to reach the form, or the only way to register a
	// device is to already know the accepted strings.
	for _, v := range nas.Vendors() {
		if !strings.Contains(body, v) {
			t.Errorf("vendor %q is not offered on the form", v)
		}
	}

	broken := nasHandler(&fakeNASStore{listErr: errNASStore}, &recordingEncryptor{})
	rec := serveNAS(broken, noc(), http.MethodGet, "/staff/nas", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("a store failure should still render a page, got %d", rec.Code)
	}
	if !strings.Contains(pageText(rec.Body.String()), "Could not load registered devices") {
		t.Errorf("want a message, got: %s", firstLines(rec.Body.String()))
	}
	if strings.Contains(pageText(rec.Body.String()), errNASStore.Error()) {
		t.Error("the store's own error reached the page")
	}
}

// A rejected registration points at the likely cause rather than showing a
// bare failure — in practice a duplicate IP, which is the global unique
// constraint this screen hits most.
func TestFR_NAS_001_ARejectedRegistrationPointsAtTheDuplicateIP(t *testing.T) {
	h := nasHandler(&fakeNASStore{createErr: errNASStore}, &recordingEncryptor{})
	rec := serveNAS(h, noc(), http.MethodPost, "/staff/nas", url.Values{
		"ip": {"192.0.2.10"}, "vendor": {"mikrotik"},
		"radius_secret": {"a-very-long-radius-secret"},
	})
	if !strings.Contains(pageText(rec.Body.String()), "already registered") {
		t.Errorf("want a duplicate-IP hint, got: %s", firstLines(rec.Body.String()))
	}
}

// An unconfigured deployment says so instead of panicking, and the POST paths
// refuse with a status rather than a half-rendered page.
func TestFR_NAS_001_AnUnconfiguredDeploymentSaysSo(t *testing.T) {
	bare := &Handler{}

	rec := serveNAS(bare, noc(), http.MethodGet, "/staff/nas", nil)
	if !strings.Contains(pageText(rec.Body.String()), "not configured") {
		t.Errorf("want a 'not configured' message, got: %s", firstLines(rec.Body.String()))
	}
	for _, target := range []string{"/staff/nas", "/staff/nas/5"} {
		if code := serveNAS(bare, noc(), http.MethodPost, target, url.Values{
			"vendor": {"mikrotik"},
		}).Code; code != http.StatusServiceUnavailable {
			t.Errorf("POST %s: want 503, got %d", target, code)
		}
	}
}

// A non-numeric device id is reported on the page rather than reaching Atoi's
// zero value and updating device 0.
func TestFR_NAS_002_ANonNumericDeviceIdIsRefused(t *testing.T) {
	store := &fakeNASStore{}
	rec := serveNAS(nasHandler(store, &recordingEncryptor{}), noc(),
		http.MethodPost, "/staff/nas/not-a-number", url.Values{"vendor": {"mikrotik"}})
	if !strings.Contains(pageText(rec.Body.String()), "Invalid device id") {
		t.Errorf("want an invalid-id message, got: %s", firstLines(rec.Body.String()))
	}
	if store.updated != nil {
		t.Errorf("a bad id still reached the store as device %d", store.updateID)
	}
}
