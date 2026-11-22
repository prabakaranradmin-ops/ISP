package middleware

import "net/http"

// DenyCrossOriginFraming stops other sites putting this server's pages in a
// frame.
//
// Without it, any site could load a console page — or a signed-in subscriber's
// portal page — inside an invisible frame and line a button of its own up over
// "Approve", "Disconnect" or "Pay", so that a click the operator or customer
// meant for the attacker's page lands on ours with their session (clickjacking).
// Found while adding the captive-portal preview frame, which was the first
// iframe in the codebase: nothing anywhere set a framing policy.
//
// 'self', not 'none', because the console frames one page of its own — the
// captive-portal preview — and same-origin framing grants nothing an attacker
// does not already have by being on the page.
//
// Two headers for one rule. frame-ancestors is the standard and wins in every
// current browser; X-Frame-Options is what older ones read, and costs nothing
// to send alongside. The CSP carries only frame-ancestors, so it restricts
// framing and nothing else — no script, style or image source is affected.
//
// Set before the handler runs, so every response carries it — error pages and
// 404s included, which are framed as readily as anything else. A handler that
// needs a different policy can still replace either header.
func DenyCrossOriginFraming(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}
