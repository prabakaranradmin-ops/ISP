package portalui

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/rs/zerolog/log"
)

// sessionCookieName carries the same JWT the JSON API expects on the
// Authorization header, just delivered as a browser cookie instead.
const sessionCookieName = "portal_session"

// cookieBridge translates a portal_session cookie into a Bearer Authorization
// header before handing off to the shared middleware.JWTMiddleware, so token
// parsing/validation stays in exactly one place (internal/middleware) rather
// than being duplicated here.
//
// Deliberately NOT implemented by adding cookie support to JWTMiddleware
// itself: that middleware is the same one internal/portal's JSON routes use
// to gate POST /portal/renew, and /portal/tickets.
// Baking cookie support into it would make a subscriber's browser cookie
// valid credentials for those JSON routes too, silently removing the
// Bearer-only, CSRF-immune guarantee they currently have. Keeping the
// cookie-reading concern confined to this package, combined with scoping the
// cookie itself to Path=/ui (see setSessionCookie below) so the browser never
// even sends it to /portal/* or /api/v1/*, preserves that guarantee at two
// independent layers instead of one.
func cookieBridge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
				r.Header.Set("Authorization", "Bearer "+c.Value)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/ui",
		HttpOnly: true,
		Secure:   true, // safe: Caddy always terminates TLS in front of this process
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(24 * time.Hour), // matches issueSubscriberJWT's own expiry
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/ui", // must match setSessionCookie's Path or the browser won't overwrite it
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// ── CSRF ─────────────────────────────────────────────────────────────────
//
// SameSite=Lax already blocks the classic cross-site POST, but it is a
// browser-behavior guarantee, not an application-level one — nothing here
// would catch a future regression if the cookie's SameSite or Path were ever
// loosened. State-changing routes (POST /ui/renew and, later, POST
// /ui/tickets) add this stateless token on top: it is an HMAC of the
// session's own JWT, so an attacker's cross-origin page can neither read the
// HttpOnly cookie nor guess the JWT string, and therefore cannot compute a
// matching token — that holds even if the SameSite defense is bypassed or
// misconfigured later. No session store is needed since the token is
// entirely derived from data the server already has on every request.

// csrfToken derives a stateless, verifiable token from the session's own JWT.
func csrfToken(jwtSecret, sessionJWT string) string {
	mac := hmac.New(sha256.New, []byte(jwtSecret))
	mac.Write([]byte(sessionJWT)) //nolint:errcheck // hash.Hash.Write never returns an error
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// csrfTokenFromRequest computes the token a form on this request's session
// should embed. Empty if there is no session cookie (the CSRF check itself
// happens after auth middleware, so this is only ever called for rendering).
func (h *Handler) csrfTokenFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	return csrfToken(h.jwtSecret, cookie.Value)
}

// requireCSRF wraps a state-changing POST handler: it re-derives the
// expected token from the request's own session cookie and rejects the
// request unless the submitted csrf_token form field matches exactly.
func (h *Handler) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		want := csrfToken(h.jwtSecret, cookie.Value)
		got := r.FormValue("csrf_token")
		if got == "" || !hmac.Equal([]byte(got), []byte(want)) {
			http.Error(w, "forbidden: invalid csrf token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// statusBuffer buffers a response so redirectToLoginOn401 can inspect the
// status code the wrapped chain produced before any of it reaches the real
// ResponseWriter, and swap in a redirect instead when appropriate.
type statusBuffer struct {
	http.ResponseWriter
	status      int
	buf         bytes.Buffer
	wroteHeader bool
}

func (s *statusBuffer) WriteHeader(code int) {
	s.status = code
	s.wroteHeader = true
}

func (s *statusBuffer) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
	}
	return s.buf.Write(b)
}

// redirectToLoginOn401 wraps a full-page route so an unauthenticated or
// expired visit lands on the login page instead of the bare
// "invalid token" text/plain body middleware.JWTMiddleware writes for API
// callers. That response is correct for the JSON API; it is not a page a
// browser should ever render. Deliberately applied only to full-page GET
// routes, not to HTMX fragment endpoints, where swapping an entire login
// page into a small polling target would be worse than the plain error.
//
// The whole response is buffered in memory before being flushed, since the
// status can only be known after the wrapped chain finishes — acceptable
// here given these are small, bounded HTML pages, not large payloads.
func redirectToLoginOn401(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sb := &statusBuffer{ResponseWriter: w}
		next.ServeHTTP(sb, r)
		if sb.status == http.StatusUnauthorized {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		if sb.wroteHeader {
			w.WriteHeader(sb.status)
		}
		_, _ = w.Write(sb.buf.Bytes())
	})
}

type loginData struct {
	baseData
	// Suspended explains the page to somebody the walled garden sent here
	// (FR-BIL-004): they tried to open some other site and landed on a
	// sign-in form, which needs a sentence of explanation to make sense.
	Suspended bool
	// RenewalMinutes is how long after payment service can take to return:
	// the renewal scanner's interval, read from it so the page cannot
	// promise faster than the scanner runs.
	RenewalMinutes int
}

// LoginPage handles GET /ui/login. ?suspended=1 is what the walled-garden
// redirector is normally pointed at (WALLED_GARDEN_REDIRECT_URL).
func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	renderPage(w, http.StatusOK, "login", loginData{
		Suspended:      r.URL.Query().Get("suspended") == "1",
		RenewalMinutes: int(billing.RenewalScanInterval.Minutes()),
	})
}

// Login handles POST /ui/login — a browser form submission, not the JSON
// POST /portal/login. Both share portal.Authenticate as the single source of
// truth for what makes a valid subscriber login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		renderPage(w, http.StatusBadRequest, "login", loginData{baseData: baseData{Error: "Invalid form submission."}})
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if username == "" || password == "" {
		renderPage(w, http.StatusUnprocessableEntity, "login", loginData{baseData: baseData{Error: "Username and password are required."}})
		return
	}

	token, err := portal.Authenticate(r.Context(), h.subscribers, username, password, h.jwtSecret)
	if errors.Is(err, portal.ErrInvalidCredentials) {
		renderPage(w, http.StatusUnauthorized, "login", loginData{baseData: baseData{Error: "Invalid username or password."}})
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("portalui: login failed")
		renderPage(w, http.StatusInternalServerError, "login", loginData{baseData: baseData{Error: "Something went wrong. Please try again."}})
		return
	}

	setSessionCookie(w, token)
	http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
}

// Logout handles POST /ui/logout.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}
