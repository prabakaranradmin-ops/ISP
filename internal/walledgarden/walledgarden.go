// Package walledgarden answers the web traffic a router redirects away from
// soft-suspended subscribers, sending every request to the payment page —
// FR-BIL-004.
//
// RADIUS marks a soft-suspended session (internal/nas/walledgarden.go), and
// the router's firewall (deploy/mikrotik/walled-garden.rsc) sends that
// session's plain-HTTP traffic here instead of to wherever it was going. The
// request that arrives can name any host and any path, because it is
// whatever page the subscriber tried to open. So nothing about it is
// trusted or echoed: every request gets the same redirect to one configured
// address, normally the portal's sign-in page.
//
// Two properties matter more than they look:
//
//   - The redirect is temporary (302) and marked never to be cached. A
//     browser keeps a permanent redirect for the host it was issued under,
//     so a 301 here would go on sending the subscriber from, say,
//     example.com to the payment page long after they had paid.
//   - A phone or laptop that finds its connectivity check redirected shows
//     its own "sign in to network" prompt. That prompt is what most
//     subscribers will actually see, so the redirect is given to every path,
//     including the ones those checks use.
package walledgarden

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"time"
)

// NewHandler returns the handler that sends every request to target, which
// must be an absolute http or https URL.
func NewHandler(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("walledgarden: redirect target %q is not a URL: %w", target, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("walledgarden: redirect target %q must be an absolute http or https URL, "+
			"such as https://portal.example.net/ui/login?suspended=1", target)
	}
	location := u.String()
	body := []byte(`<!doctype html><meta charset="utf-8"><title>Payment due</title>` +
		`<p>Your internet service is paused until your account is paid. ` +
		`<a href="` + html.EscapeString(location) + `">Sign in to pay</a>.</p>`)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Location", location)
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.Set("Pragma", "no-cache")
		h.Set("Expires", "0")
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusFound)
		if r.Method != http.MethodHead {
			w.Write(body) //nolint:errcheck,gosec // nothing useful to do with a failed write to a redirected client
		}
	}), nil
}

// NewServer wraps the handler in a server for addr, with limits suited to a
// listener every suspended subscriber's browser can reach: it holds no
// state, so a slow or idle client is simply dropped.
func NewServer(addr, target string) (*http.Server, error) {
	h, err := NewHandler(target)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}, nil
}
