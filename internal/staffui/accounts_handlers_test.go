package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"golang.org/x/crypto/bcrypt"
)

// The Staff Accounts screen — FR-SEC-003, FR-SEC-005.
//
// accounts.go's four handlers were at 0% (117 statements); the pure helpers
// beside them were already covered in accounts_test.go. This is the screen
// that mints console operators, so what it must not allow matters more than
// what it does:
//
//   - It must not leave the console with no active owner. This screen is
//     owner-only, so the last owner deactivating themselves — or demoting
//     themselves — locks everybody out with no way back in short of SQL.
//   - It must not create a franchise-scoped account with no franchise, or an
//     ISP-wide account bound to one. The schema refuses both; doing it here
//     turns a 500 into a sentence.
//   - A password it accepts must clear the console floor, which is higher
//     than a subscriber's because these accounts can credit wallets.

var errAccountsStore = errors.New("store: staff_users unreachable")

// recordingStaff records what the handlers asked the store to do.
type recordingStaff struct {
	roster []StaffAccount

	listErr   error
	createErr error
	updateErr error
	setPwErr  error
	updateNil bool

	createdUsername  string
	createdFullName  string
	createdHash      string
	createdRole      string
	createdLEA       bool
	createdFranchise *int

	updatedID        int
	updatedRole      *string
	updatedLEA       *bool
	updatedActive    *bool
	updatedFranchise *int

	passwordSetFor int
	passwordHash   string
}

func (r *recordingStaff) GetStaffByUsername(context.Context, string) (*StaffAccount, error) {
	return nil, nil
}

func (r *recordingStaff) TouchStaffLogin(context.Context, int) error { return nil }

func (r *recordingStaff) ListStaff(context.Context) ([]StaffAccount, error) {
	return r.roster, r.listErr
}

func (r *recordingStaff) CreateStaff(_ context.Context, username, fullName, hash, role string,
	lea bool, franchiseID *int) (*StaffAccount, error) {
	r.createdUsername, r.createdFullName, r.createdHash = username, fullName, hash
	r.createdRole, r.createdLEA, r.createdFranchise = role, lea, franchiseID
	if r.createErr != nil {
		return nil, r.createErr
	}
	return &StaffAccount{ID: 9, Username: username, FullName: fullName, Role: role,
		LeaAccess: lea, Active: true, FranchiseID: franchiseID}, nil
}

func (r *recordingStaff) UpdateStaff(_ context.Context, id int, role *string, lea, active *bool,
	franchiseID *int) (*StaffAccount, error) {
	r.updatedID, r.updatedRole, r.updatedLEA = id, role, lea
	r.updatedActive, r.updatedFranchise = active, franchiseID
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	if r.updateNil {
		return nil, nil
	}
	out := &StaffAccount{ID: id, Username: "edited"}
	if role != nil {
		out.Role = *role
	}
	return out, nil
}

func (r *recordingStaff) SetStaffPassword(_ context.Context, id int, hash string) error {
	r.passwordSetFor, r.passwordHash = id, hash
	return r.setPwErr
}

func serveAccounts(h *Handler, s Session, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) >= 3 {
		r.SetPathValue("id", parts[2])
	}

	w := httptest.NewRecorder()
	switch {
	case method == http.MethodPost && strings.Count(strings.Trim(r.URL.Path, "/"), "/") >= 2:
		h.UpdateStaffAccount(w, r)
	case method == http.MethodPost:
		h.CreateStaffAccount(w, r)
	default:
		h.StaffAccounts(w, r)
	}
	return w
}

func accountsHandler(staff StaffQuerier, fr FranchiseStore) *Handler {
	h := &Handler{}
	if staff != nil {
		h.staff = staff
	}
	if fr != nil {
		h.franchises = fr
	}
	return h
}

func twoOwners() []StaffAccount {
	return []StaffAccount{
		{ID: 1, Username: "owner1", Role: "isp_owner", Active: true},
		{ID: 2, Username: "owner2", Role: "isp_owner", Active: true},
	}
}

func oneOwnerAndACSR() []StaffAccount {
	return []StaffAccount{
		{ID: 1, Username: "owner1", Role: "isp_owner", Active: true},
		{ID: 3, Username: "csr1", Role: "csr", Active: true},
	}
}

func validAccountForm() url.Values {
	return url.Values{
		"username":  {"  ravi  "},
		"full_name": {"  Ravi Kumar  "},
		"password":  {"a-long-enough-password"},
		"role":      {"csr"},
	}
}

// The last active owner cannot deactivate themselves or change their own
// role. This screen is owner-only, so either would lock everybody out of it
// with no way back in that does not involve SQL.
func TestFR_SEC_003_TheLastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	for name, form := range map[string]url.Values{
		"deactivated": {"role": {"isp_owner"}},             // active unticked
		"demoted":     {"role": {"csr"}, "active": {"on"}}, // still active, no longer owner
		"both":        {"role": {"billing_admin"}},         // demoted and deactivated
	} {
		t.Run(name, func(t *testing.T) {
			staff := &recordingStaff{roster: oneOwnerAndACSR()}
			rec := serveAccounts(accountsHandler(staff, nil), owner(),
				http.MethodPost, "/staff/accounts/1", form)

			if !strings.Contains(pageText(rec.Body.String()), "no active owner account") {
				t.Errorf("want a lockout refusal, got: %s", firstLines(rec.Body.String()))
			}
			if staff.updatedID != 0 {
				t.Errorf("the change reached the store for account %d", staff.updatedID)
			}
		})
	}

	// With a second owner, the same edits are allowed.
	for name, form := range map[string]url.Values{
		"deactivated": {"role": {"isp_owner"}},
		"demoted":     {"role": {"csr"}, "active": {"on"}},
	} {
		t.Run("with a spare owner/"+name, func(t *testing.T) {
			staff := &recordingStaff{roster: twoOwners()}
			rec := serveAccounts(accountsHandler(staff, nil), owner(),
				http.MethodPost, "/staff/accounts/1", form)
			if strings.Contains(pageText(rec.Body.String()), "no active owner") {
				t.Errorf("refused although another owner exists: %s", firstLines(rec.Body.String()))
			}
			if staff.updatedID != 1 {
				t.Error("the change did not reach the store")
			}
		})
	}
}

// Deactivating a non-owner is never a lockout risk, and must not be refused
// as one — the check re-tallies the roster rather than asking "is this edit
// moving a role away from owner".
func TestFR_SEC_003_DeactivatingANonOwnerIsNotALockout(t *testing.T) {
	staff := &recordingStaff{roster: oneOwnerAndACSR()}
	rec := serveAccounts(accountsHandler(staff, nil), owner(),
		http.MethodPost, "/staff/accounts/3", url.Values{"role": {"csr"}})

	if strings.Contains(pageText(rec.Body.String()), "no active owner") {
		t.Errorf("deactivating a CSR was refused as a lockout: %s", firstLines(rec.Body.String()))
	}
	if staff.updatedID != 3 {
		t.Error("the change did not reach the store")
	}
}

// If the roster cannot be read, the edit is refused rather than risked. The
// check fails closed because the cost of being wrong is total lockout.
func TestFR_SEC_003_TheLockoutCheckFailsClosed(t *testing.T) {
	staff := &recordingStaff{listErr: errAccountsStore}
	rec := serveAccounts(accountsHandler(staff, nil), owner(),
		http.MethodPost, "/staff/accounts/1", url.Values{"role": {"csr"}, "active": {"on"}})

	if !strings.Contains(pageText(rec.Body.String()), "Could not verify") {
		t.Errorf("want a refusal naming the uncertainty, got: %s", firstLines(rec.Body.String()))
	}
	if staff.updatedID != 0 {
		t.Error("the change went ahead without the roster being readable")
	}
}

// A franchise-scoped role must name a franchise at creation; an ISP-wide role
// must not be bound to one whatever the form said.
func TestFR_SEC_003_TheFranchiseBindingMatchesTheRole(t *testing.T) {
	t.Run("franchise role with no franchise", func(t *testing.T) {
		staff := &recordingStaff{roster: twoOwners()}
		form := validAccountForm()
		form.Set("role", "lco")
		rec := serveAccounts(accountsHandler(staff, nil), owner(),
			http.MethodPost, "/staff/accounts", form)
		if !strings.Contains(pageText(rec.Body.String()), "franchise must be selected") {
			t.Errorf("want a franchise message, got: %s", firstLines(rec.Body.String()))
		}
		if staff.createdUsername != "" {
			t.Error("an unbound franchise account reached the store")
		}
	})

	t.Run("franchise role with a franchise", func(t *testing.T) {
		staff := &recordingStaff{roster: twoOwners()}
		form := validAccountForm()
		form.Set("role", "lco")
		form.Set("franchise_id", "2")
		rec := serveAccounts(accountsHandler(staff, nil), owner(),
			http.MethodPost, "/staff/accounts", form)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d — %s", rec.Code, firstLines(rec.Body.String()))
		}
		if staff.createdFranchise == nil || *staff.createdFranchise != 2 {
			t.Errorf("the binding reached the store as %v", staff.createdFranchise)
		}
	})

	t.Run("ISP-wide role ignores a submitted franchise", func(t *testing.T) {
		staff := &recordingStaff{roster: twoOwners()}
		form := validAccountForm() // role csr
		form.Set("franchise_id", "2")
		serveAccounts(accountsHandler(staff, nil), owner(),
			http.MethodPost, "/staff/accounts", form)
		if staff.createdFranchise != nil {
			t.Errorf("a csr was bound to franchise %v; the schema forbids it and the "+
				"form should not be able to ask", *staff.createdFranchise)
		}
	})

	t.Run("a malformed franchise id is refused", func(t *testing.T) {
		for _, raw := range []string{"two", "0", "-1"} {
			staff := &recordingStaff{roster: twoOwners()}
			form := validAccountForm()
			form.Set("role", "franchise_admin")
			form.Set("franchise_id", raw)
			rec := serveAccounts(accountsHandler(staff, nil), owner(),
				http.MethodPost, "/staff/accounts", form)
			if !strings.Contains(pageText(rec.Body.String()), "invalid") {
				t.Errorf("franchise_id %q: want a refusal, got: %s",
					raw, firstLines(rec.Body.String()))
			}
			if staff.createdUsername != "" {
				t.Errorf("franchise_id %q reached the store", raw)
			}
		}
	})
}

// A created account is stored hashed, never with the password, and the
// username and name are trimmed.
func TestFR_SEC_003_ACreatedAccountIsStoredHashedAndTrimmed(t *testing.T) {
	staff := &recordingStaff{roster: twoOwners()}
	rec := serveAccounts(accountsHandler(staff, nil), owner(),
		http.MethodPost, "/staff/accounts", validAccountForm())

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	if staff.createdUsername != "ravi" {
		t.Errorf("username reached the store as %q; it should be trimmed", staff.createdUsername)
	}
	if staff.createdFullName != "Ravi Kumar" {
		t.Errorf("full name reached the store as %q", staff.createdFullName)
	}
	if staff.createdHash == "a-long-enough-password" {
		t.Fatal("the password was stored verbatim")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(staff.createdHash),
		[]byte("a-long-enough-password")); err != nil {
		t.Errorf("what reached the store is not a bcrypt hash of the password: %v", err)
	}
	if body := pageText(rec.Body.String()); strings.Contains(body, "a-long-enough-password") {
		t.Errorf("the password was rendered back: %s", firstLines(body))
	}
	if !strings.Contains(pageText(rec.Body.String()), "created") {
		t.Errorf("no confirmation: %s", firstLines(rec.Body.String()))
	}
}

// The password floor is enforced, and it is higher than a subscriber's
// because these accounts can credit wallets and run an LEA lookup.
func TestFR_SEC_003_ThePasswordFloorIsEnforcedOnCreate(t *testing.T) {
	for _, pw := range []string{"", "short", strings.Repeat("a", minStaffPasswordLength-1)} {
		staff := &recordingStaff{roster: twoOwners()}
		form := validAccountForm()
		form.Set("password", pw)
		rec := serveAccounts(accountsHandler(staff, nil), owner(),
			http.MethodPost, "/staff/accounts", form)

		want := fmt.Sprintf("at least %d characters", minStaffPasswordLength)
		if !strings.Contains(pageText(rec.Body.String()), want) {
			t.Errorf("password %q: want %q, got: %s", pw, want, firstLines(rec.Body.String()))
		}
		if staff.createdUsername != "" {
			t.Errorf("password %q reached the store", pw)
		}
	}
}

// Only assignable roles can be granted, so the form cannot mint a role no
// section list knows about.
func TestFR_SEC_003_OnlyAssignableRolesCanBeGranted(t *testing.T) {
	for _, role := range []string{"", "superuser", "admin", "ISP_OWNER", "subscriber"} {
		staff := &recordingStaff{roster: twoOwners()}
		form := validAccountForm()
		form.Set("role", role)
		rec := serveAccounts(accountsHandler(staff, nil), owner(),
			http.MethodPost, "/staff/accounts", form)

		if !strings.Contains(pageText(rec.Body.String()), "Role must be one of") {
			t.Errorf("role %q: want a refusal, got: %s", role, firstLines(rec.Body.String()))
		}
		if staff.createdUsername != "" {
			t.Errorf("role %q reached the store", role)
		}
	}

	for _, role := range allAssignableRoles() {
		staff := &recordingStaff{roster: twoOwners()}
		form := validAccountForm()
		form.Set("role", role)
		if isFranchiseRole(role) {
			form.Set("franchise_id", "2")
		}
		serveAccounts(accountsHandler(staff, nil), owner(), http.MethodPost, "/staff/accounts", form)
		if staff.createdRole != role {
			t.Errorf("assignable role %q was refused", role)
		}
	}
}

// Username and full name are both required.
func TestFR_SEC_003_UsernameAndFullNameAreRequired(t *testing.T) {
	for name, override := range map[string]string{
		"username": "username", "full name": "full_name",
	} {
		t.Run(name, func(t *testing.T) {
			staff := &recordingStaff{roster: twoOwners()}
			form := validAccountForm()
			form.Set(override, "   ")
			rec := serveAccounts(accountsHandler(staff, nil), owner(),
				http.MethodPost, "/staff/accounts", form)
			if !strings.Contains(pageText(rec.Body.String()), "are required") {
				t.Errorf("want a required-field message, got: %s", firstLines(rec.Body.String()))
			}
			if staff.createdUsername != "" {
				t.Error("an incomplete account reached the store")
			}
		})
	}
}

// A duplicate username is reported as such rather than as a generic failure,
// because it is the one the operator can fix themselves.
func TestFR_SEC_003_ADuplicateUsernameSaysSo(t *testing.T) {
	staff := &recordingStaff{
		roster: twoOwners(),
		createErr: errors.New(
			`ERROR: duplicate key value violates unique constraint "staff_users_username_key"`),
	}
	rec := serveAccounts(accountsHandler(staff, nil), owner(),
		http.MethodPost, "/staff/accounts", validAccountForm())

	body := pageText(rec.Body.String())
	if !strings.Contains(body, "already in use") {
		t.Errorf("want a duplicate message, got: %s", firstLines(body))
	}
	if strings.Contains(body, "staff_users_username_key") {
		t.Error("the constraint name reached the page")
	}

	// Any other store failure is generic and does not leak.
	other := &recordingStaff{roster: twoOwners(), createErr: errAccountsStore}
	rec = serveAccounts(accountsHandler(other, nil), owner(),
		http.MethodPost, "/staff/accounts", validAccountForm())
	body = pageText(rec.Body.String())
	if !strings.Contains(body, "Could not create") {
		t.Errorf("want a generic failure, got: %s", firstLines(body))
	}
	if strings.Contains(body, errAccountsStore.Error()) {
		t.Error("the store's own error reached the page")
	}
}

// An optional password reset on the edit form is applied only when supplied,
// and a reset that fails says the account still saved — two outcomes, not one.
func TestFR_SEC_003_AnOptionalPasswordResetIsAppliedOnlyWhenSupplied(t *testing.T) {
	blank := &recordingStaff{roster: twoOwners()}
	serveAccounts(accountsHandler(blank, nil), owner(), http.MethodPost, "/staff/accounts/2",
		url.Values{"role": {"isp_owner"}, "active": {"on"}, "password": {""}})
	if blank.passwordSetFor != 0 {
		t.Errorf("a blank password reset the credential for account %d", blank.passwordSetFor)
	}

	supplied := &recordingStaff{roster: twoOwners()}
	serveAccounts(accountsHandler(supplied, nil), owner(), http.MethodPost, "/staff/accounts/2",
		url.Values{"role": {"isp_owner"}, "active": {"on"},
			"password": {"a-long-enough-password"}})
	if supplied.passwordSetFor != 2 {
		t.Fatalf("the reset was applied to account %d", supplied.passwordSetFor)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(supplied.passwordHash),
		[]byte("a-long-enough-password")); err != nil {
		t.Errorf("the stored value is not a bcrypt hash of the new password: %v", err)
	}

	// Too short: the account still saved, and the page says both things.
	short := &recordingStaff{roster: twoOwners()}
	rec := serveAccounts(accountsHandler(short, nil), owner(), http.MethodPost,
		"/staff/accounts/2", url.Values{"role": {"isp_owner"}, "active": {"on"},
			"password": {"abc"}})
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "Account saved") || !strings.Contains(body, "not changed") {
		t.Errorf("the page should say the account saved and the password did not change, "+
			"got: %s", firstLines(body))
	}
	if short.passwordSetFor != 0 {
		t.Error("a too-short password was stored")
	}

	// A failing write says the same.
	failing := &recordingStaff{roster: twoOwners(), setPwErr: errAccountsStore}
	rec = serveAccounts(accountsHandler(failing, nil), owner(), http.MethodPost,
		"/staff/accounts/2", url.Values{"role": {"isp_owner"}, "active": {"on"},
			"password": {"a-long-enough-password"}})
	if !strings.Contains(pageText(rec.Body.String()), "password reset failed") {
		t.Errorf("want a reset-failed message, got: %s", firstLines(rec.Body.String()))
	}
}

// An unknown or malformed account id is reported rather than updating
// account 0.
func TestFR_SEC_003_AnUnknownOrMalformedIdIsReported(t *testing.T) {
	malformed := &recordingStaff{roster: twoOwners()}
	rec := serveAccounts(accountsHandler(malformed, nil), owner(), http.MethodPost,
		"/staff/accounts/not-a-number", url.Values{"role": {"csr"}, "active": {"on"}})
	if !strings.Contains(pageText(rec.Body.String()), "Invalid account id") {
		t.Errorf("want an invalid-id message, got: %s", firstLines(rec.Body.String()))
	}
	if malformed.updatedID != 0 {
		t.Errorf("a malformed id reached the store as %d", malformed.updatedID)
	}

	missing := &recordingStaff{roster: twoOwners(), updateNil: true}
	rec = serveAccounts(accountsHandler(missing, nil), owner(), http.MethodPost,
		"/staff/accounts/404", url.Values{"role": {"csr"}, "active": {"on"}})
	if !strings.Contains(pageText(rec.Body.String()), "No account with that id") {
		t.Errorf("want a not-found message, got: %s", firstLines(rec.Body.String()))
	}

	failing := &recordingStaff{roster: twoOwners(), updateErr: errAccountsStore}
	rec = serveAccounts(accountsHandler(failing, nil), owner(), http.MethodPost,
		"/staff/accounts/2", url.Values{"role": {"csr"}, "active": {"on"}})
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "Could not save") {
		t.Errorf("want a save-failed message, got: %s", firstLines(body))
	}
	if strings.Contains(body, errAccountsStore.Error()) {
		t.Error("the store's own error reached the page")
	}
}

// Only the owner reaches this screen at all. Every other role, including the
// ones that can manage subscribers and money, is refused.
func TestFR_SEC_005_OnlyTheOwnerReachesStaffAccounts(t *testing.T) {
	staff := &recordingStaff{roster: twoOwners()}
	h := accountsHandler(staff, nil)

	for _, role := range []string{"billing_admin", "csr", "technician", "noc_engineer",
		"lco", "franchise_admin", "franchise_staff"} {
		s := Session{Username: role + "_user", Role: role, FranchiseID: 2}
		for _, tc := range []struct {
			method, target string
		}{
			{http.MethodGet, "/staff/accounts"},
			{http.MethodPost, "/staff/accounts"},
			{http.MethodPost, "/staff/accounts/2"},
		} {
			rec := serveAccounts(h, s, tc.method, tc.target, validAccountForm())
			if rec.Code == http.StatusOK {
				t.Errorf("%s reached %s %s", role, tc.method, tc.target)
			}
		}
	}
	if staff.createdUsername != "" || staff.updatedID != 0 {
		t.Error("a non-owner reached the store")
	}

	if code := serveAccounts(h, owner(), http.MethodGet, "/staff/accounts", nil).Code; code != http.StatusOK {
		t.Errorf("the owner got %d", code)
	}
}

// The roster renders with the assignable roles and, when configured, the
// franchises to bind to. A franchise store that fails does not take the page
// with it: the ISP-wide half of the screen still works.
func TestFR_SEC_003_TheScreenRendersRolesAndFranchisesBestEffort(t *testing.T) {
	staff := &recordingStaff{roster: oneOwnerAndACSR()}
	fr := &fakeFranchises{list: []revenue.FranchiseRecord{
		{ID: 2, Name: "Anna Nagar Networks", Status: "active"},
	}}

	body := pageText(serveAccounts(accountsHandler(staff, fr), owner(),
		http.MethodGet, "/staff/accounts", nil).Body.String())
	for _, want := range []string{"owner1", "csr1", "Anna Nagar Networks"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from the screen", want)
		}
	}
	for _, role := range allAssignableRoles() {
		if !strings.Contains(body, role) {
			t.Errorf("role %q is not offered on the form", role)
		}
	}

	// A failing franchise store leaves the rest of the screen intact.
	broken := accountsHandler(staff, &fakeFranchises{listErr: errFranchiseStore})
	rec := serveAccounts(broken, owner(), http.MethodGet, "/staff/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("a franchise failure took the page down: %d", rec.Code)
	}
	if !strings.Contains(pageText(rec.Body.String()), "owner1") {
		t.Error("the staff roster is missing when the franchise store fails")
	}

	// No franchise store at all is fine too.
	none := accountsHandler(staff, nil)
	if code := serveAccounts(none, owner(), http.MethodGet, "/staff/accounts", nil).Code; code != http.StatusOK {
		t.Errorf("with no franchise store the screen returned %d", code)
	}
}

// A failing staff store renders a page that says so, and an unconfigured
// deployment says that instead.
func TestFR_SEC_003_StoreFailuresAreReadable(t *testing.T) {
	rec := serveAccounts(accountsHandler(&recordingStaff{listErr: errAccountsStore}, nil),
		owner(), http.MethodGet, "/staff/accounts", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("want a rendered page, got %d", rec.Code)
	}
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "Could not load staff accounts") {
		t.Errorf("want a load message, got: %s", firstLines(body))
	}
	if strings.Contains(body, errAccountsStore.Error()) {
		t.Error("the store's own error reached the page")
	}

	bare := accountsHandler(nil, nil)
	if !strings.Contains(pageText(serveAccounts(bare, owner(), http.MethodGet,
		"/staff/accounts", nil).Body.String()), "not configured") {
		t.Error("an unconfigured deployment did not say so")
	}
	for _, target := range []string{"/staff/accounts", "/staff/accounts/2"} {
		if code := serveAccounts(bare, owner(), http.MethodPost, target,
			validAccountForm()).Code; code != http.StatusServiceUnavailable {
			t.Errorf("POST %s: want 503, got %d", target, code)
		}
	}
}

// LEA access is granted only when asked for. It is the single most sensitive
// flag on an account — it opens the lawful-interception lookup — so defaulting
// it on would be the worst available default.
func TestFR_SEC_003_LEAAccessIsOffUnlessAskedFor(t *testing.T) {
	off := &recordingStaff{roster: twoOwners()}
	serveAccounts(accountsHandler(off, nil), owner(), http.MethodPost,
		"/staff/accounts", validAccountForm())
	if off.createdLEA {
		t.Error("LEA access defaulted to on")
	}

	on := &recordingStaff{roster: twoOwners()}
	form := validAccountForm()
	form.Set("lea_access", "on")
	serveAccounts(accountsHandler(on, nil), owner(), http.MethodPost, "/staff/accounts", form)
	if !on.createdLEA {
		t.Error("the ticked LEA box did not reach the store")
	}

	// And on the edit form.
	edit := &recordingStaff{roster: twoOwners()}
	serveAccounts(accountsHandler(edit, nil), owner(), http.MethodPost, "/staff/accounts/2",
		url.Values{"role": {"isp_owner"}, "active": {"on"}})
	if edit.updatedLEA == nil || *edit.updatedLEA {
		t.Errorf("an edit without the box ticked set LEA access to %v", edit.updatedLEA)
	}
}
