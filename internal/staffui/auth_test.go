package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"golang.org/x/crypto/bcrypt"
)

// The console's authentication — FR-SEC-001, FR-SEC-005.
//
// auth.go was at 17.4% with every one of login, logout, the session cookie,
// the authed middleware and both CSRF guards at 0%. This is the security
// boundary for all thirty-nine console screens, and the operator behind it
// can disconnect sessions, credit wallets and run a law-enforcement lookup.
//
// Four properties here are the boundary itself:
//
//   - The cookie is HttpOnly, Secure, SameSite and scoped to /staff. The path
//     scope is what keeps /api/v1 and /portal Bearer-only and therefore
//     CSRF-immune: a cookie the browser will not attach cannot forge a
//     request against them.
//   - A wrong password and an unknown username are the same answer. Anything
//     else turns the sign-in form into a staff directory.
//   - The role and the franchise binding come from the signed token, never
//     from the request.
//   - A state-changing POST needs a CSRF token derived from that session, and
//     the comparison is constant-time.

var errStaffStore = errors.New("store: staff_users unreachable")

type fakeStaff struct {
	account  *StaffAccount
	getErr   error
	touchErr error

	askedFor string
	touched  int
}

// GetStaffByUsername matches by username, the way the real store does.
//
// An earlier version returned its account for any username at all, which made
// "no username but the right password" look like a successful sign-in — a
// defect in the stub reported as a defect in the product. A fake that answers
// a question the real store would not is worse than no fake.
func (f *fakeStaff) GetStaffByUsername(_ context.Context, username string) (*StaffAccount, error) {
	f.askedFor = username
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.account == nil || f.account.Username != username {
		return nil, nil
	}
	return f.account, nil
}

func (f *fakeStaff) TouchStaffLogin(_ context.Context, id int) error {
	f.touched = id
	return f.touchErr
}

func (f *fakeStaff) ListStaff(context.Context) ([]StaffAccount, error) { return nil, f.getErr }

func (f *fakeStaff) CreateStaff(context.Context, string, string, string, string, bool,
	*int) (*StaffAccount, error) {
	return f.account, f.getErr
}

func (f *fakeStaff) UpdateStaff(context.Context, int, *string, *bool, *bool,
	*int) (*StaffAccount, error) {
	return f.account, f.getErr
}

func (f *fakeStaff) SetStaffPassword(context.Context, int, string) error { return f.getErr }

const testSecret = "a-console-signing-secret-long-enough-for-hs256"

func authHandler(staff StaffQuerier) *Handler {
	h := &Handler{jwtSecret: testSecret}
	if staff != nil {
		h.staff = staff
	}
	return h
}

func staffAccount(t *testing.T, password, role string, franchiseID *int) *StaffAccount {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return &StaffAccount{
		ID: 7, Username: "meena", PasswordHash: string(hash), FullName: "Meena R",
		Role: role, Active: true, FranchiseID: franchiseID,
	}
}

func authPost(target string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// sessionCookie finds the cookie a response set, or reports that none was.
func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == staffSessionCookie {
			return c
		}
	}
	return nil
}

// A wrong password and an unknown username give the same answer, and the
// bcrypt comparison runs either way so the two take roughly the same time.
// Anything else makes this form a way to enumerate staff accounts.
func TestFR_SEC_001_AWrongPasswordAndAnUnknownUserAreIndistinguishable(t *testing.T) {
	const password = "the-right-password"

	wrongPassword := authHandler(&fakeStaff{account: staffAccount(t, password, "csr", nil)})
	w1 := httptest.NewRecorder()
	wrongPassword.Login(w1, authPost("/staff/login", url.Values{
		"username": {"meena"}, "password": {"not-the-right-password"},
	}))

	noSuchUser := authHandler(&fakeStaff{account: nil})
	w2 := httptest.NewRecorder()
	noSuchUser.Login(w2, authPost("/staff/login", url.Values{
		"username": {"nobody"}, "password": {password},
	}))

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"wrong password": w1, "unknown user": w2,
	} {
		if !strings.Contains(pageText(rec.Body.String()), "Incorrect username or password") {
			t.Errorf("%s: want the single failure message, got: %s",
				name, firstLines(rec.Body.String()))
		}
		if sessionCookie(t, rec) != nil {
			t.Errorf("%s: a session cookie was set on a failed sign-in", name)
		}
		if rec.Code == http.StatusSeeOther {
			t.Errorf("%s: a failed sign-in redirected as though it had worked", name)
		}
	}

	if b1, b2 := pageText(w1.Body.String()), pageText(w2.Body.String()); b1 != b2 {
		t.Error("the two failures render differently, which is enough to tell an attacker " +
			"whether a username exists")
	}
}

// A good sign-in sets a cookie with every flag that matters, scoped to
// /staff. The path scope is what keeps the JSON API and the subscriber portal
// Bearer-only, and therefore CSRF-immune by construction.
func TestFR_SEC_001_TheSessionCookieIsHttpOnlySecureAndScopedToStaff(t *testing.T) {
	const password = "the-right-password"
	staff := &fakeStaff{account: staffAccount(t, password, "csr", nil)}
	h := authHandler(staff)

	rec := httptest.NewRecorder()
	h.Login(rec, authPost("/staff/login", url.Values{
		"username": {"meena"}, "password": {password},
	}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect after sign-in, got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	c := sessionCookie(t, rec)
	if c == nil {
		t.Fatal("no session cookie was set")
	}
	if !c.HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}
	if !c.Secure {
		t.Error("the session cookie would be sent over plain HTTP")
	}
	if c.SameSite == http.SameSiteNoneMode || c.SameSite == 0 {
		t.Errorf("SameSite = %v; a console session cookie must not be sent on "+
			"cross-site requests", c.SameSite)
	}
	if c.Path != "/staff" {
		t.Errorf("Path = %q, want /staff — a wider path would attach this cookie to "+
			"/api/v1 and /portal and make both forgeable", c.Path)
	}
	if c.Value == "" {
		t.Fatal("the cookie carries no token")
	}
	// The token is a signed JWT this process will accept back.
	var claims middleware.Claims
	tok, err := jwt.ParseWithClaims(c.Value, &claims, func(*jwt.Token) (any, error) {
		return []byte(testSecret), nil
	})
	if err != nil || !tok.Valid {
		t.Fatalf("the issued token does not verify: %v", err)
	}
	if claims.Subject != "meena" {
		t.Errorf("subject = %q; an audit entry has to name a person", claims.Subject)
	}
	if claims.Role != "csr" {
		t.Errorf("role = %q", claims.Role)
	}
	if claims.ExpiresAt == nil {
		t.Fatal("the token never expires")
	}
	if d := time.Until(claims.ExpiresAt.Time); d > sessionTTL+time.Minute || d < sessionTTL-time.Hour {
		t.Errorf("the token lives %v, want about %v", d, sessionTTL)
	}
	if staff.touched != 7 {
		t.Errorf("last_login_at was recorded for staff %d", staff.touched)
	}
}

// The role and the franchise binding come from the signed token. A
// franchise-scoped operator's own id is in the claim, which is what every
// self-scoped screen reads instead of trusting the request.
func TestFR_SEC_005_TheRoleAndFranchiseBindingComeFromTheToken(t *testing.T) {
	franchise := 2
	h := authHandler(&fakeStaff{account: staffAccount(t, "pw-long-enough", "lco", &franchise)})

	rec := httptest.NewRecorder()
	h.Login(rec, authPost("/staff/login", url.Values{
		"username": {"meena"}, "password": {"pw-long-enough"},
	}))
	c := sessionCookie(t, rec)
	if c == nil {
		t.Fatal("no session cookie")
	}

	// Round-trip it through the middleware the way a real request does.
	var got Session
	next := func(_ http.ResponseWriter, r *http.Request) {
		got, _ = SessionFrom(r.Context())
	}
	req := httptest.NewRequest(http.MethodGet, "/staff/my-pnl", nil)
	req.AddCookie(c)
	h.authed(next).ServeHTTP(httptest.NewRecorder(), req)

	if got.Role != "lco" {
		t.Errorf("role = %q", got.Role)
	}
	if got.FranchiseID != 2 {
		t.Errorf("franchise id = %d, want the claim's 2 — a zero here makes every "+
			"self-scoped screen fall back to unscoped", got.FranchiseID)
	}
	if got.Username != "meena" {
		t.Errorf("username = %q", got.Username)
	}
}

// No cookie, a blank cookie, a cookie signed with another key, an expired one
// and one using an unexpected algorithm are all refused.
func TestFR_SEC_001_OnlyATokenThisProcessSignedIsAccepted(t *testing.T) {
	h := authHandler(&fakeStaff{})
	reached := false
	next := func(http.ResponseWriter, *http.Request) { reached = true }

	claims := func(exp time.Time) middleware.Claims {
		return middleware.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "meena", ExpiresAt: jwt.NewNumericDate(exp),
			},
			Role: "isp_owner",
		}
	}
	signedWith := func(secret string, exp time.Time) string {
		s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims(exp)).SignedString([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims(time.Now().Add(time.Hour))).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]string{
		"no cookie":   "",
		"empty value": "",
		"another key": signedWith("a-different-secret-entirely-long", time.Now().Add(time.Hour)),
		"expired":     signedWith(testSecret, time.Now().Add(-time.Minute)),
		"alg none":    unsigned,
		"not a token": "not-a-jwt-at-all",
	} {
		t.Run(name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodGet, "/staff/subscribers", nil)
			if value != "" {
				req.AddCookie(&http.Cookie{Name: staffSessionCookie, Value: value})
			}
			rec := httptest.NewRecorder()
			h.authed(next).ServeHTTP(rec, req)

			if reached {
				t.Error("the handler ran for an unauthenticated request")
			}
			if rec.Code != http.StatusSeeOther {
				t.Errorf("want a redirect to the login page, got %d", rec.Code)
			}
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/staff/login") {
				t.Errorf("redirected to %q", loc)
			}
		})
	}
}

// An unusable cookie is cleared on the way out. Leaving it in place makes
// every subsequent page bounce with no way for the operator to recover except
// knowing to delete it by hand.
func TestFR_SEC_001_AnUnusableCookieIsClearedAndTheReasonIsShown(t *testing.T) {
	h := authHandler(&fakeStaff{})
	expired, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "meena", ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/staff/subscribers", nil)
	req.AddCookie(&http.Cookie{Name: staffSessionCookie, Value: expired})
	rec := httptest.NewRecorder()
	h.authed(func(http.ResponseWriter, *http.Request) {}).ServeHTTP(rec, req)

	c := sessionCookie(t, rec)
	if c == nil {
		t.Fatal("the expired cookie was not cleared")
	}
	if c.MaxAge >= 0 && c.Value != "" {
		t.Errorf("the cookie was not expired: MaxAge=%d value=%q", c.MaxAge, c.Value)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "expired=1") {
		t.Errorf("redirected to %q; the login page should be able to say why", loc)
	}

	// And it does say why.
	page := httptest.NewRecorder()
	h.LoginPage(page, httptest.NewRequest(http.MethodGet, "/staff/login?expired=1", nil))
	if !strings.Contains(pageText(page.Body.String()), "session expired") {
		t.Errorf("the login page does not explain the bounce: %s", firstLines(page.Body.String()))
	}
}

// Signing out clears the cookie and sends the operator to the login page.
func TestFR_SEC_001_SigningOutClearsTheCookie(t *testing.T) {
	h := authHandler(&fakeStaff{})
	rec := httptest.NewRecorder()
	h.Logout(rec, httptest.NewRequest(http.MethodPost, "/staff/logout", nil))

	c := sessionCookie(t, rec)
	if c == nil {
		t.Fatal("logout set no cookie, so the browser keeps the session")
	}
	if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("the cookie was not cleared: value=%q MaxAge=%d", c.Value, c.MaxAge)
	}
	if !c.HttpOnly || !c.Secure {
		t.Error("the clearing cookie drops the flags, which some browsers treat as a " +
			"different cookie and leave the original in place")
	}
	if loc := rec.Header().Get("Location"); loc != "/staff/login" {
		t.Errorf("redirected to %q", loc)
	}
}

// A state-changing POST needs the session's own CSRF token.
func TestFR_SEC_005_AStateChangingPostNeedsTheSessionsCSRFToken(t *testing.T) {
	h := authHandler(&fakeStaff{})
	const token = "a-session-token"
	s := Session{Username: "meena", Role: "isp_owner", Token: token}
	good := h.csrfToken(token)

	reached := false
	guarded := h.requireCSRF(func(http.ResponseWriter, *http.Request) { reached = true })

	for name, supplied := range map[string]string{
		"absent":            "",
		"empty":             "",
		"wrong":             "0000000000000000000000000000000000000000000000000000000000000000",
		"another session's": h.csrfToken("a-different-session-token"),
		"truncated":         good[:len(good)-2],
	} {
		t.Run(name, func(t *testing.T) {
			reached = false
			form := url.Values{}
			if supplied != "" {
				form.Set("csrf_token", supplied)
			}
			r := authPost("/staff/subscribers/bulk", form)
			r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
			rec := httptest.NewRecorder()
			guarded(rec, r)

			if reached {
				t.Error("the handler ran without a valid CSRF token")
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("want 403, got %d", rec.Code)
			}
		})
	}

	reached = false
	r := authPost("/staff/subscribers/bulk", url.Values{"csrf_token": {good}})
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	rec := httptest.NewRecorder()
	guarded(rec, r)
	if !reached {
		t.Errorf("a correct token was refused: %d — %s", rec.Code, rec.Body.String())
	}
}

// The token is bound to the session, so one operator's token cannot be used
// in another's request — and it cannot be produced without the session token.
func TestFR_SEC_005_TheCSRFTokenIsBoundToTheSession(t *testing.T) {
	h := authHandler(&fakeStaff{})
	a := h.csrfToken("session-a")
	b := h.csrfToken("session-b")
	if a == b {
		t.Fatal("two sessions share a CSRF token")
	}
	if a == "" {
		t.Fatal("the token is empty")
	}
	if strings.Contains(a, "session-a") {
		t.Error("the token contains the session token it was derived from")
	}
	if h.csrfToken("session-a") != a {
		t.Error("the token is not stable for one session, so the form would never match")
	}

	// A different signing secret gives a different token, so a token minted
	// elsewhere does not verify here.
	other := &Handler{jwtSecret: "an-entirely-different-console-secret"}
	if other.csrfToken("session-a") == a {
		t.Error("the token does not depend on the signing secret")
	}
}

// An anonymous state-changing request is sent to the login page rather than
// refused as a CSRF failure, which would be a confusing answer to "you are
// not signed in".
func TestFR_SEC_005_AnAnonymousPostGoesToTheLoginPage(t *testing.T) {
	h := authHandler(&fakeStaff{})
	reached := false

	rec := httptest.NewRecorder()
	h.requireCSRF(func(http.ResponseWriter, *http.Request) { reached = true })(
		rec, authPost("/staff/subscribers/bulk", url.Values{}))
	if reached {
		t.Error("the handler ran for an anonymous request")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("want a redirect, got %d", rec.Code)
	}

	reached = false
	rec = httptest.NewRecorder()
	h.requireCSRFUpload(1<<20, "/staff/documents",
		func(http.ResponseWriter, *http.Request) { reached = true })(
		rec, authPost("/staff/documents", url.Values{}))
	if reached {
		t.Error("the upload handler ran for an anonymous request")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("upload: want a redirect, got %d", rec.Code)
	}
}

// An oversized upload is turned away before the handler sees it, with a
// sentence rather than a bare 413 — the person chose a file that was too
// large and an error page would not tell them so.
func TestFR_SEC_005_AnOversizedUploadIsRefusedWithAnExplanation(t *testing.T) {
	h := authHandler(&fakeStaff{})
	s := Session{Username: "meena", Role: "isp_owner", Token: "a-session-token"}

	var body strings.Builder
	body.WriteString("--b\r\nContent-Disposition: form-data; name=\"csrf_token\"\r\n\r\n")
	body.WriteString(h.csrfToken(s.Token))
	body.WriteString("\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"big.pdf\"\r\n\r\n")
	body.WriteString(strings.Repeat("x", 4096))
	body.WriteString("\r\n--b--\r\n")

	r := httptest.NewRequest(http.MethodPost, "/staff/documents", strings.NewReader(body.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	reached := false
	rec := httptest.NewRecorder()
	// A cap well below the body size.
	h.requireCSRFUpload(512, "/staff/documents",
		func(http.ResponseWriter, *http.Request) { reached = true })(rec, r)

	if reached {
		t.Error("an oversized upload reached the handler")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect carrying an explanation, got %d — %s",
			rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "too+large") && !strings.Contains(loc, "too%20large") {
		t.Errorf("the redirect does not explain the refusal: %q", loc)
	}
}

// A valid upload with a valid token gets through, and the CSRF check happens
// after the body is capped rather than after a 32 MB parse.
func TestFR_SEC_005_AValidUploadPassesTheGuard(t *testing.T) {
	h := authHandler(&fakeStaff{})
	s := Session{Username: "meena", Role: "isp_owner", Token: "a-session-token"}

	var body strings.Builder
	body.WriteString("--b\r\nContent-Disposition: form-data; name=\"csrf_token\"\r\n\r\n")
	body.WriteString(h.csrfToken(s.Token))
	body.WriteString("\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"small.pdf\"\r\n\r\n")
	body.WriteString("a small file")
	body.WriteString("\r\n--b--\r\n")

	r := httptest.NewRequest(http.MethodPost, "/staff/documents", strings.NewReader(body.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	reached := false
	rec := httptest.NewRecorder()
	h.requireCSRFUpload(1<<20, "/staff/documents",
		func(http.ResponseWriter, *http.Request) { reached = true })(rec, r)
	if !reached {
		t.Errorf("a valid upload was refused: %d — %s", rec.Code, rec.Body.String())
	}

	// A forged token on an upload is refused too.
	forged := strings.Replace(body.String(), h.csrfToken(s.Token),
		strings.Repeat("0", 64), 1)
	r2 := httptest.NewRequest(http.MethodPost, "/staff/documents", strings.NewReader(forged))
	r2.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	r2 = r2.WithContext(context.WithValue(r2.Context(), sessionKey, s))
	reached = false
	rec2 := httptest.NewRecorder()
	h.requireCSRFUpload(1<<20, "/staff/documents",
		func(http.ResponseWriter, *http.Request) { reached = true })(rec2, r2)
	if reached {
		t.Error("a forged token on an upload reached the handler")
	}
	if rec2.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", rec2.Code)
	}
}

// A store failure at sign-in says "temporarily unavailable" rather than
// "incorrect password", which would send an operator to reset a password that
// was never wrong.
func TestFR_SEC_001_AStoreFailureIsNotReportedAsABadPassword(t *testing.T) {
	h := authHandler(&fakeStaff{getErr: errStaffStore})
	rec := httptest.NewRecorder()
	h.Login(rec, authPost("/staff/login", url.Values{
		"username": {"meena"}, "password": {"whatever"},
	}))

	body := pageText(rec.Body.String())
	if !strings.Contains(body, "temporarily unavailable") {
		t.Errorf("want an availability message, got: %s", firstLines(body))
	}
	if strings.Contains(body, "Incorrect username or password") {
		t.Error("a store outage was reported as a wrong password")
	}
	if strings.Contains(body, errStaffStore.Error()) {
		t.Error("the store's own error reached the page")
	}
	if sessionCookie(t, rec) != nil {
		t.Error("a cookie was set despite the failure")
	}
}

// Recording the login time is best effort: it must not cost a working
// sign-in.
func TestFR_SEC_001_AFailureToRecordTheLoginTimeStillSignsIn(t *testing.T) {
	const password = "the-right-password"
	h := authHandler(&fakeStaff{
		account:  staffAccount(t, password, "csr", nil),
		touchErr: errStaffStore,
	})
	rec := httptest.NewRecorder()
	h.Login(rec, authPost("/staff/login", url.Values{
		"username": {"meena"}, "password": {password},
	}))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("want a redirect, got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	if sessionCookie(t, rec) == nil {
		t.Error("no session cookie; an audit convenience cost a working sign-in")
	}
}

// With no staff store the form says so rather than refusing everybody as
// though they had the wrong password.
func TestFR_SEC_001_AnUnconfiguredDeploymentSaysSo(t *testing.T) {
	rec := httptest.NewRecorder()
	authHandler(nil).Login(rec, authPost("/staff/login", url.Values{
		"username": {"meena"}, "password": {"x"},
	}))
	if !strings.Contains(pageText(rec.Body.String()), "not configured") {
		t.Errorf("want a configuration message, got: %s", firstLines(rec.Body.String()))
	}
}

// Empty credentials are refused without reaching the store for an empty
// username, and never sign anybody in.
func TestFR_SEC_001_EmptyCredentialsNeverSignIn(t *testing.T) {
	for name, form := range map[string]url.Values{
		"nothing":     {},
		"no password": {"username": {"meena"}},
		"no username": {"password": {"the-right-password"}},
		"both blank":  {"username": {""}, "password": {""}},
	} {
		t.Run(name, func(t *testing.T) {
			h := authHandler(&fakeStaff{account: staffAccount(t, "the-right-password", "csr", nil)})
			rec := httptest.NewRecorder()
			h.Login(rec, authPost("/staff/login", form))
			if sessionCookie(t, rec) != nil {
				t.Error("a session was issued for empty credentials")
			}
			if rec.Code == http.StatusSeeOther {
				t.Errorf("an empty sign-in redirected as though it worked")
			}
		})
	}
}

// requireSection refuses an unauthenticated request before it consults the
// role list, so a missing session is a redirect rather than a 403 that
// suggests the operator merely lacks permission.
func TestFR_SEC_005_RequireSectionSendsAnAnonymousRequestToLogin(t *testing.T) {
	h := authHandler(&fakeStaff{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/staff/subscribers", nil)

	if _, ok := h.requireSection(rec, r, "subscribers"); ok {
		t.Error("requireSection allowed a request with no session")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("want a redirect, got %d", rec.Code)
	}
}

// Changing your own password is the one place this file trusts a password the
// caller typed rather than one the session cookie already proved, so it
// re-checks the current password first. Without that, a borrowed unlocked
// browser becomes a permanent account takeover rather than a session that
// expires in eight hours.
func TestFR_SEC_001_ChangingYourOwnPasswordRequiresTheCurrentOne(t *testing.T) {
	const current = "the-current-password"
	staff := &fakeStaff{account: staffAccount(t, current, "csr", nil)}
	h := authHandler(staff)
	s := Session{StaffID: 7, Username: "meena", Role: "csr", Token: "a-session-token"}

	change := func(form url.Values) *httptest.ResponseRecorder {
		r := authPost("/staff/password", form)
		r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
		rec := httptest.NewRecorder()
		h.ChangePassword(rec, r)
		return rec
	}

	wrong := change(url.Values{
		"current_password": {"not-the-current-password"},
		"new_password":     {"a-brand-new-password"},
		"confirm_password": {"a-brand-new-password"},
	})
	if !strings.Contains(pageText(wrong.Body.String()), "Current password is incorrect") {
		t.Errorf("want a current-password message, got: %s", firstLines(wrong.Body.String()))
	}

	ok := change(url.Values{
		"current_password": {current},
		"new_password":     {"a-brand-new-password"},
		"confirm_password": {"a-brand-new-password"},
	})
	if !strings.Contains(pageText(ok.Body.String()), "Password changed") {
		t.Errorf("a correct current password was refused: %s", firstLines(ok.Body.String()))
	}
}

// The new password has to meet the length rule and match its confirmation.
func TestFR_SEC_001_ChangePasswordValidation(t *testing.T) {
	const current = "the-current-password"
	s := Session{StaffID: 7, Username: "meena", Role: "csr", Token: "a-session-token"}

	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"too short": {url.Values{
			"current_password": {current}, "new_password": {"abc"}, "confirm_password": {"abc"},
		}, "at least"},
		"mismatch": {url.Values{
			"current_password": {current}, "new_password": {"a-brand-new-password"},
			"confirm_password": {"a-different-password"},
		}, "do not match"},
		"empty new": {url.Values{
			"current_password": {current}, "new_password": {""}, "confirm_password": {""},
		}, "at least"},
	} {
		t.Run(name, func(t *testing.T) {
			h := authHandler(&fakeStaff{account: staffAccount(t, current, "csr", nil)})
			r := authPost("/staff/password", tc.form)
			r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
			rec := httptest.NewRecorder()
			h.ChangePassword(rec, r)
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if strings.Contains(pageText(rec.Body.String()), "Password changed") {
				t.Error("the password was changed anyway")
			}
		})
	}
}

// An anonymous request to either the form or the action goes to the login
// page, and an account that cannot be read is reported rather than treated as
// verified.
func TestFR_SEC_001_ChangePasswordNeedsASessionAndAnAccount(t *testing.T) {
	h := authHandler(&fakeStaff{account: staffAccount(t, "x", "csr", nil)})

	for name, fn := range map[string]func(http.ResponseWriter, *http.Request){
		"form":   h.ChangePasswordPage,
		"action": h.ChangePassword,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			fn(rec, authPost("/staff/password", url.Values{}))
			if rec.Code != http.StatusSeeOther {
				t.Errorf("want a redirect for an anonymous request, got %d", rec.Code)
			}
		})
	}

	s := Session{StaffID: 7, Username: "meena", Role: "csr", Token: "a-session-token"}

	// The session names an account the store does not have — a deleted
	// operator with a still-valid cookie.
	gone := authHandler(&fakeStaff{account: nil})
	r := authPost("/staff/password", url.Values{
		"current_password": {"anything"}, "new_password": {"a-brand-new-password"},
		"confirm_password": {"a-brand-new-password"},
	})
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	rec := httptest.NewRecorder()
	gone.ChangePassword(rec, r)
	if !strings.Contains(pageText(rec.Body.String()), "Could not verify your account") {
		t.Errorf("want a verification message, got: %s", firstLines(rec.Body.String()))
	}

	// And with no staff store at all.
	none := authHandler(nil)
	r2 := authPost("/staff/password", url.Values{})
	r2 = r2.WithContext(context.WithValue(r2.Context(), sessionKey, s))
	rec2 := httptest.NewRecorder()
	none.ChangePassword(rec2, r2)
	if !strings.Contains(pageText(rec2.Body.String()), "not configured") {
		t.Errorf("want a configuration message, got: %s", firstLines(rec2.Body.String()))
	}

	// The form itself renders for a signed-in operator.
	page := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/staff/password", nil)
	req = req.WithContext(context.WithValue(req.Context(), sessionKey, s))
	h.ChangePasswordPage(page, req)
	if page.Code != http.StatusOK {
		t.Errorf("the form did not render: %d", page.Code)
	}
}

// A store failure writing the new password says so rather than reporting
// success — an operator who believes their password changed and finds it did
// not is locked out of their own guess.
func TestFR_SEC_001_AFailedPasswordWriteIsNotReportedAsChanged(t *testing.T) {
	const current = "the-current-password"
	acct := staffAccount(t, current, "csr", nil)
	h := authHandler(&failingSetPassword{fakeStaff{account: acct}})
	s := Session{StaffID: 7, Username: "meena", Role: "csr", Token: "a-session-token"}

	r := authPost("/staff/password", url.Values{
		"current_password": {current}, "new_password": {"a-brand-new-password"},
		"confirm_password": {"a-brand-new-password"},
	})
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	rec := httptest.NewRecorder()
	h.ChangePassword(rec, r)

	body := pageText(rec.Body.String())
	if strings.Contains(body, "Password changed") {
		t.Error("a failed write was reported as a changed password")
	}
	if !strings.Contains(body, "Could not change your password") {
		t.Errorf("want a failure message, got: %s", firstLines(body))
	}
}

// failingSetPassword is fakeStaff with only the write broken, so the current
// password still verifies and the failure is isolated to the write.
type failingSetPassword struct{ fakeStaff }

func (f *failingSetPassword) SetStaffPassword(context.Context, int, string) error {
	return errStaffStore
}
