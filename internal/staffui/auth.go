package staffui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// staffSessionCookie carries the same JWT the JSON API validates, delivered as
// a browser cookie.
//
// Scoped to Path=/staff so the browser never sends it to /api/v1/* or
// /portal/*. Those routes stay Bearer-only and therefore CSRF-immune by
// construction: a cookie the browser will not attach cannot be used to forge a
// request against them. That is the same two-layer arrangement the subscriber
// portal uses, and the reason cookie handling lives in this package rather
// than inside the shared JWT middleware.
const staffSessionCookie = "staff_session"

// sessionTTL bounds a console session. Shorter than the subscriber portal's
// 24h: these accounts can disconnect sessions, credit wallets and resolve a
// law-enforcement lookup, so an unattended browser is a larger problem.
const sessionTTL = 8 * time.Hour

type ctxKey int

const sessionKey ctxKey = 0

// Session is the signed-in operator, as carried through a request.
type Session struct {
	StaffID   int
	Username  string
	FullName  string
	Role      string
	LeaAccess bool
	Token     string
}

// SessionFrom returns the operator for a request, if any.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey).(Session)
	return s, ok
}

func (h *Handler) issueToken(a *StaffAccount) (string, error) {
	claims := middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			// The subject is the operator's username, so an action recorded in
			// lea_audit_log names a person rather than a service.
			Subject:   a.Username,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionTTL)),
		},
		Role:      a.Role,
		LeaAccess: a.LeaAccess,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.jwtSecret))
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     staffSessionCookie,
		Value:    token,
		Path:     "/staff",
		HttpOnly: true,
		Secure:   true, // Caddy always terminates TLS in front of this process
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: staffSessionCookie, Value: "", Path: "/staff",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

// authed resolves the session cookie and rejects anonymous requests.
func (h *Handler) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(staffSessionCookie)
		if err != nil || c.Value == "" {
			http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
			return
		}

		var claims middleware.Claims
		token, err := jwt.ParseWithClaims(c.Value, &claims, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(h.jwtSecret), nil
		})
		if err != nil || !token.Valid {
			// Clear it: leaving an expired cookie in place makes every
			// subsequent page bounce with no way for the operator to recover
			// except knowing to delete it themselves.
			h.clearSessionCookie(w)
			http.Redirect(w, r, "/staff/login?expired=1", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), sessionKey, Session{
			Username:  claims.Subject,
			FullName:  claims.Subject,
			Role:      claims.Role,
			LeaAccess: claims.LeaAccess,
			Token:     c.Value,
		})
		next(w, r.WithContext(ctx))
	})
}

// requireSection enforces the same role rules the navigation uses.
//
// Checked per handler, not only when rendering the menu: hiding a link is a
// convenience for the operator, never a control. Anyone can type the URL.
func (h *Handler) requireSection(w http.ResponseWriter, r *http.Request, key string) (Session, bool) {
	s, ok := SessionFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
		return Session{}, false
	}
	if !canAccess(key, s.Role, s.LeaAccess) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Your role does not have access to this area.")
		return Session{}, false
	}
	return s, true
}

// ── CSRF ─────────────────────────────────────────────────────────────────────

// csrfToken derives a per-session token by HMAC-ing the session JWT with the
// signing secret. Nothing extra has to be stored, and it cannot be produced by
// a site that does not already hold the session token.
func (h *Handler) csrfToken(sessionJWT string) string {
	mac := hmac.New(sha256.New, []byte(h.jwtSecret))
	mac.Write([]byte("staffui-csrf:" + sessionJWT)) //nolint:errcheck // hash.Hash.Write never errors
	return hex.EncodeToString(mac.Sum(nil))
}

// requireCSRF rejects a state-changing request whose token does not match.
func (h *Handler) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := SessionFrom(r.Context())
		if !ok {
			http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "malformed form", http.StatusBadRequest)
			return
		}
		want := h.csrfToken(s.Token)
		// Constant-time: a byte-by-byte comparison leaks how much of a guessed
		// token was correct.
		if !hmac.Equal([]byte(r.PostFormValue("csrf_token")), []byte(want)) {
			http.Error(w, "forbidden: invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// ── Login ────────────────────────────────────────────────────────────────────

// LoginPage renders the console sign-in form.
func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	msg := ""
	if r.URL.Query().Get("expired") != "" {
		msg = "Your session expired. Please sign in again."
	}
	h.renderLogin(w, r, msg)
}

// Login authenticates an operator against staff_users.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if h.staff == nil {
		h.renderLogin(w, r, "Staff accounts are not configured on this deployment.")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderLogin(w, r, "Could not read the form.")
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	account, err := h.staff.GetStaffByUsername(r.Context(), username)
	if err != nil {
		log.Error().Err(err).Msg("staffui: staff lookup failed")
		h.renderLogin(w, r, "Sign-in is temporarily unavailable.")
		return
	}

	// One message for every failure, and the bcrypt comparison runs even when
	// there is no such account, so a missing username and a wrong password
	// take the same path and roughly the same time. Saying which was wrong
	// turns this form into a way to enumerate staff accounts.
	const failed = "Incorrect username or password."
	if account == nil {
		// Compare against a known hash purely to spend the same time.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$12$C6UzMDM.H6dfI/f/IKcEe.7cRLJ0BeQ0OJ6NcSJ1WQPRhF7wOaK1y"), []byte(password))
		h.renderLogin(w, r, failed)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(password)); err != nil {
		h.renderLogin(w, r, failed)
		return
	}

	token, err := h.issueToken(account)
	if err != nil {
		log.Error().Err(err).Msg("staffui: token signing failed")
		h.renderLogin(w, r, "Sign-in is temporarily unavailable.")
		return
	}

	// Best-effort: an audit convenience must not cost a working sign-in.
	if err := h.staff.TouchStaffLogin(r.Context(), account.ID); err != nil {
		log.Warn().Err(err).Int("staff_id", account.ID).Msg("staffui: last_login_at not recorded")
	}

	h.setSessionCookie(w, token)
	http.Redirect(w, r, "/staff/subscribers", http.StatusSeeOther)
}

// Logout ends the session.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	h.clearSessionCookie(w)
	http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
}

// ── Self-service password change ────────────────────────────────────────────
//
// Available to every signed-in role, not gated by requireSection: everyone
// should be able to change their own password, independent of what console
// sections their role can otherwise reach. Owner-only resetting of someone
// else's password lives on the Staff Accounts screen instead (accounts.go)
// — a different action with a different trust boundary (no current
// password needed there, since the owner isn't proving they are that
// person).

// ChangePasswordPage renders the form.
func (h *Handler) ChangePasswordPage(w http.ResponseWriter, r *http.Request) {
	s, ok := SessionFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
		return
	}
	h.renderChangePassword(w, s, "", "")
}

func (h *Handler) renderChangePassword(w http.ResponseWriter, s Session, message, errMsg string) {
	d := h.page(s, "Change password", "")
	d.Message, d.Error = message, errMsg
	h.render(w, "change_password", d)
}

// ChangePassword verifies the caller's current password before setting a
// new one — the one place this file trusts a password the caller typed
// rather than one it already validated via the session cookie, so it has
// to re-check it here.
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	s, ok := SessionFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
		return
	}
	if h.staff == nil {
		h.renderChangePassword(w, s, "", "Staff account management is not configured.")
		return
	}

	current := r.PostFormValue("current_password")
	newPassword := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	account, err := h.staff.GetStaffByUsername(r.Context(), s.Username)
	if err != nil || account == nil {
		log.Error().Err(err).Str("username", s.Username).Msg("staffui: credential-change lookup failed")
		h.renderChangePassword(w, s, "", "Could not verify your account. Try signing in again.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(current)); err != nil {
		h.renderChangePassword(w, s, "", "Current password is incorrect.")
		return
	}
	if len(newPassword) < minStaffPasswordLength {
		h.renderChangePassword(w, s, "", fmt.Sprintf("New password must be at least %d characters.", minStaffPasswordLength))
		return
	}
	if newPassword != confirm {
		h.renderChangePassword(w, s, "", "New password and confirmation do not match.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
	if err != nil {
		log.Error().Err(err).Msg("staffui: hash new credential failed")
		h.renderChangePassword(w, s, "", "Could not change your password.")
		return
	}
	if err := h.staff.SetStaffPassword(r.Context(), account.ID, string(hash)); err != nil {
		log.Error().Err(err).Int("staff_id", account.ID).Msg("staffui: set new credential failed")
		h.renderChangePassword(w, s, "", "Could not change your password.")
		return
	}
	h.renderChangePassword(w, s, "Password changed.", "")
}
