// Package middleware implements JWT authentication and RBAC role enforcement.
//
// FR: FR-SEC-005 | DDS §5.7
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog/log"
)

type contextKey string

const (
	ctxKeyRole        contextKey = "role"
	ctxKeySubject     contextKey = "sub"
	ctxKeySubID       contextKey = "subscriber_id"
	ctxKeyFranchiseID contextKey = "franchise_id"
	ctxKeyLeaAccess   contextKey = "lea_access"
)

// Claims extends the standard JWT claims with ISP-specific fields.
type Claims struct {
	jwt.RegisteredClaims
	Role         string `json:"role"`
	SubscriberID int    `json:"subscriber_id,omitempty"`
	FranchiseID  int    `json:"franchise_id,omitempty"`
	// LeaAccess gates /api/v1/lea/*. SecD §9.3 requires this as a claim
	// independent of role: a noc_engineer token does not carry LEA access
	// unless this flag was explicitly set when the token was issued, so
	// granting it can never be a side effect of a role assignment elsewhere.
	LeaAccess bool `json:"lea_access,omitempty"`
}

// writeAuthError answers with the same {code, message} envelope every handler
// in this codebase uses.
//
// Previously http.Error, which sends text/plain. That made the most common
// error a client ever sees — an expired token — the one response its JSON
// parser could not read, while every handler-level error was well-formed JSON
// (FR-MOB-002). A mobile app cannot write one error path against that.
//
// The envelope is duplicated here rather than shared with internal/api's
// writeError because api imports this package; the shape is pinned by the
// portal contract tests on both sides, which is what keeps the two in step.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // client hung up; nothing to recover
		"code": code, "message": message,
	})
}

// JWTMiddleware validates the Bearer token and injects the parsed claims into ctx.
func JWTMiddleware(jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				writeAuthError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "missing or malformed Authorization header")
				return
			}
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")

			claims := &Claims{}
			token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
				}
				return []byte(jwtSecret), nil
			})
			if err != nil || !token.Valid {
				writeAuthError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "invalid or expired token")
				return
			}

			ctx := context.WithValue(r.Context(), ctxKeyRole, claims.Role)
			ctx = context.WithValue(ctx, ctxKeySubject, claims.Subject)
			ctx = context.WithValue(ctx, ctxKeySubID, claims.SubscriberID)
			ctx = context.WithValue(ctx, ctxKeyFranchiseID, claims.FranchiseID)
			ctx = context.WithValue(ctx, ctxKeyLeaAccess, claims.LeaAccess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole returns a middleware that allows only the listed roles.
//
// DDS §5.7
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, _ := r.Context().Value(ctxKeyRole).(string)
			if !allowed[role] {
				writeAuthError(w, http.StatusForbidden, "ERR_FORBIDDEN", "your role does not permit this action")
				return
			}
			auditLog(r, role)
			next.ServeHTTP(w, r)
		})
	}
}

// RoleFromContext extracts the role claim from the request context.
func RoleFromContext(ctx context.Context) string {
	role, _ := ctx.Value(ctxKeyRole).(string)
	return role
}

// SubjectFromContext extracts the subject claim.
func SubjectFromContext(ctx context.Context) string {
	sub, _ := ctx.Value(ctxKeySubject).(string)
	return sub
}

// WithSubject attaches an acting principal to a context that did not come
// from an HTTP request.
//
// Background workers — the dunning scanner, the renewal scanner — change
// subscriber status with no JWT anywhere in sight, and the status-capture
// triggers (migration 031) would otherwise attribute their work to "unknown".
// Naming them explicitly, as "system:dunning-scanner", is what lets an
// operator reading the history tell an automatic suspension apart from one a
// person decided on.
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, ctxKeySubject, subject)
}

// SubscriberIDFromContext extracts the subscriber_id claim.
func SubscriberIDFromContext(ctx context.Context) int {
	id, _ := ctx.Value(ctxKeySubID).(int)
	return id
}

// FranchiseIDFromContext extracts the franchise_id claim. A zero result means
// the token carries no franchise binding.
func FranchiseIDFromContext(ctx context.Context) int {
	id, _ := ctx.Value(ctxKeyFranchiseID).(int)
	return id
}

// LeaAccessFromContext reports whether the caller's token carries the
// lea_access claim.
func LeaAccessFromContext(ctx context.Context) bool {
	ok, _ := ctx.Value(ctxKeyLeaAccess).(bool)
	return ok
}

// RequireLeaAccess returns middleware that additionally demands the
// lea_access claim, on top of whatever role gate already ran. It must sit
// behind RequireRole, not replace it: the SecD table lists LEA authorization
// as "noc + lea_flag" — both conditions, not either.
func RequireLeaAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LeaAccessFromContext(r.Context()) {
			writeAuthError(w, http.StatusForbidden, "ERR_FORBIDDEN", "lea_access claim required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auditLog emits a structured audit log entry for RBAC-protected routes.
func auditLog(r *http.Request, role string) {
	log.Info().
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("role", role).
		Str("remote", r.RemoteAddr).
		Msg("rbac_access")
}

// Audit emits the structured admin-action audit entry SecD §9.3 requires for
// every state-mutating request: actor identity, actor role, the action taken,
// and its target. Handlers for POST/PATCH/DELETE routes should call this once
// the action has succeeded.
func Audit(ctx context.Context, action, target string, detail map[string]any) {
	correlationID := newCorrelationID()

	// Persisted as well as logged (FR-OBS-006), when a sink is wired.
	//
	// After the log line, never instead of it: the line is the authority,
	// and a sink that fails must leave the record intact. See
	// SetAuditSink for why a failure here cannot fail the caller.
	defer persistAudit(ctx, AuditEntry{
		CorrelationID: correlationID,
		ActorID:       SubjectFromContext(ctx),
		ActorRole:     RoleFromContext(ctx),
		Action:        action,
		Target:        target,
		Detail:        detail,
	})

	event := log.Info().
		Bool("audit", true).
		Str("correlation_id", correlationID).
		Str("actor_id", SubjectFromContext(ctx)).
		Str("actor_role", RoleFromContext(ctx)).
		Str("action", action).
		Str("target", target)
	if len(detail) > 0 {
		event = event.Interface("detail", detail)
	}
	event.Msg("audit_log")
}

// newCorrelationID returns a short random identifier for one audit entry.
// Not tied to request tracing infrastructure, which this project does not
// have yet — it exists so a single audit line can be located and cross-checked
// against other logs for the same action.
func newCorrelationID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(buf)
}
