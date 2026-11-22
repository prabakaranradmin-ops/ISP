package hotspot

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

// Captive-portal HTTP surface — FR-HSP-001 | MDS §4.23.

var (
	portalAttemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hotspot_portal_attempts_total",
		Help: "Captive-portal redemption attempts, by method and outcome",
	}, []string{"method", "outcome"})
	// Separate from the "refused" outcome above: a spike here is somebody
	// searching the voucher space, which is the signal worth paging on.
	portalRateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hotspot_portal_rate_limited_total",
		Help: "Captive-portal attempts refused by the attempt limiter",
	})
)

// DefaultLoginMinutes is how long a grant issued against subscriber
// credentials lasts. Bounded rather than open-ended so a phone left associated
// at a café does not hold an authorisation forever; the subscriber re-logs in,
// which costs them one form and re-checks their status against suspension.
const DefaultLoginMinutes = 12 * 60

// SubscriberCredentialQuerier looks up a subscriber's stored password hash.
//
// Deliberately not portal.PortalSubscriberQuerier, whose second method returns
// a full profile: the captive portal has no business reading a wallet balance,
// and a narrower interface is a narrower blast radius if this public-facing
// handler is ever wrong. Satisfied by *db.PortalStore.
type SubscriberCredentialQuerier interface {
	GetSubscriberByUsername(ctx context.Context, username string) (*portal.SubscriberAuth, error)
}

// Deps are the captive portal's dependencies.
type Deps struct {
	Grants      GrantStore
	Subscribers SubscriberCredentialQuerier
	Limiter     AttemptLimiter
	// LoginMinutes overrides DefaultLoginMinutes for credential-based grants.
	LoginMinutes int
	// Branding supplies the venue's name, text, colours and logo
	// (FR-HSP-004). Nil renders the default page, exactly as before branding
	// existed.
	Branding *BrandingCache
}

// Handler serves the walled garden.
type Handler struct {
	grants       GrantStore
	subscribers  SubscriberCredentialQuerier
	limiter      AttemptLimiter
	loginMinutes int
	branding     *BrandingCache
}

// NewHandler constructs the captive-portal Handler.
func NewHandler(deps Deps) *Handler {
	minutes := deps.LoginMinutes
	if minutes <= 0 {
		minutes = DefaultLoginMinutes
	}
	return &Handler{
		grants:       deps.Grants,
		subscribers:  deps.Subscribers,
		limiter:      deps.Limiter,
		loginMinutes: minutes,
		branding:     deps.Branding,
	}
}

// RegisterRoutes wires the captive-portal routes onto mux.
//
// Every route here is unauthenticated, and has to be: the audience is someone
// with no account, no token and — until this transaction completes — no
// network. That is the one place in this codebase where an open endpoint is
// the requirement rather than a mistake, so the compensating controls (attempt
// limiting, uniform refusals, no plaintext code ever returned) all live inside
// the handlers instead of in middleware above them.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /hotspot/{$}", h.Landing)
	mux.HandleFunc("GET /hotspot/portal", h.Landing)
	mux.HandleFunc("POST /hotspot/voucher", h.RedeemVoucher)
	mux.HandleFunc("POST /hotspot/login", h.Login)
	// The venue's logo (FR-HSP-004). Served from this host rather than
	// inlined or linked elsewhere: it is the one origin a device on the
	// walled garden can already reach, and a separate response lets the
	// phone cache it while the page itself never is.
	mux.HandleFunc("GET /hotspot/logo", h.Logo)
}

// sessionRequest is the NAS-supplied context common to every captive-portal
// request: which device is asking, on which NAS, and where it was headed.
type sessionRequest struct {
	MAC      string
	NASID    *int
	Redirect string
}

// readSessionRequest pulls the NAS redirect parameters from either the query
// string (the landing GET) or the form body (the redemption POSTs).
//
// The MAC arrives as a request parameter and is therefore client-controlled —
// a NAS puts it there when it redirects, and nothing stops a user editing it.
// That is inherent to how captive portals work and is not a privilege
// escalation: the worst a user can do is spend their own voucher, or their own
// credentials, on a MAC other than the one they are sitting behind. What it
// does mean is that the MAC is an identifier here and never an authenticator —
// it grants nothing on its own, and AuthorizeMAC still re-checks the grant's
// NAS binding and expiry at RADIUS time.
func readSessionRequest(r *http.Request) (sessionRequest, bool) {
	get := r.URL.Query().Get
	if r.Method == http.MethodPost {
		get = r.PostFormValue
	}

	mac, ok := radius.NormaliseMAC(get("mac"))
	if !ok {
		return sessionRequest{}, false
	}

	req := sessionRequest{MAC: mac, Redirect: safeRedirect(get("link-orig"))}
	// An absent or unparseable nasid leaves the grant unbound, matching the
	// NULL nas_id the schema already treats as "any NAS this operator runs".
	// Operators who need a voucher bought at one site to be unusable at another
	// put ?nasid=<id> in that site's redirect URL; the binding is then enforced
	// in SQL, not here.
	if id, err := strconv.Atoi(get("nasid")); err == nil && id > 0 {
		req.NASID = &id
	}
	return req, true
}

// safeRedirect keeps the NAS's link-orig from becoming an open redirect.
//
// The walled garden echoes this value back as a link once the user is online,
// so an attacker who can craft the portal URL could otherwise point that link
// at anything. Only absolute http(s) URLs survive; anything else — javascript:,
// data:, a scheme-relative //evil.example — is dropped and the page simply
// omits the "continue" link.
func safeRedirect(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	return raw
}

// clientKey identifies who is making an attempt, for rate-limiting purposes.
//
// Source address and MAC are combined because neither alone is a good bucket: a
// whole café behind one NAT shares a source address and would exhaust a common
// budget between legitimate users, while a MAC alone is meaningless across
// sites.
//
// This does not stop a determined attacker — they control their own MAC and can
// rotate it for a fresh budget. It is not what makes guessing infeasible, and
// treating it as though it were would be the mistake here: the code space is.
// Twelve characters over a 30-symbol alphabet is ~58 bits, so even unmetered
// guessing at a implausible rate does not finish. What this limiter actually
// buys is that a script cannot turn the walled garden into free load, and that
// hotspot_portal_rate_limited_total spikes when someone tries — which is the
// signal an operator can act on.
func clientKey(r *http.Request, mac string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host + "|" + mac
}

// ── Landing ─────────────────────────────────────────────────────────────────

// Landing handles GET /hotspot/ and GET /hotspot/portal — the walled-garden
// page a NAS redirects an unauthenticated device to.
func (h *Handler) Landing(w http.ResponseWriter, r *http.Request) {
	req, ok := readSessionRequest(r)
	if !ok {
		// No usable MAC means the NAS did not send one, which is a
		// misconfiguration of the redirect rather than anything the visitor did.
		// The page still renders so they see something other than a blank
		// browser error, but with no form: submitting one could not work.
		h.renderLanding(w, r, http.StatusOK, landingData{
			Notice: "This hotspot is not passing your device address through. " +
				"Please ask the venue's staff for help.",
		})
		return
	}
	h.renderLanding(w, r, http.StatusOK, landingData{Session: req, Ready: true})
}

// ── Voucher redemption ──────────────────────────────────────────────────────

// RedeemVoucher handles POST /hotspot/voucher.
func (h *Handler) RedeemVoucher(w http.ResponseWriter, r *http.Request) {
	req, ok := h.begin(w, r, "voucher")
	if !ok {
		return
	}

	code := NormaliseCode(r.PostFormValue("code"))
	if code == "" {
		// 422, not the 401 a wrong code gets: the visitor already knows they
		// submitted nothing, so this distinction leaks no information about
		// which codes exist, and it matches how the subscriber portal reports a
		// missing field.
		portalAttemptsTotal.WithLabelValues("voucher", "malformed").Inc()
		h.reject(w, r, req, "Please enter your voucher code.")
		return
	}

	grantID, err := h.grants.RedeemVoucher(r.Context(), HashCode(code), req.MAC, req.NASID)
	if err != nil {
		log.Error().Err(err).Str("mac", req.MAC).Msg("hotspot: voucher redemption failed")
		portalAttemptsTotal.WithLabelValues("voucher", "error").Inc()
		h.fail(w, r, req)
		return
	}
	if grantID == 0 {
		// One message for every refusal — unknown code, already redeemed,
		// expired, voided. The store already collapses these; saying "that code
		// was already used" would confirm to a guesser that they had found a
		// real code, which is the one bit of information a search needs.
		portalAttemptsTotal.WithLabelValues("voucher", "refused").Inc()
		h.refuse(w, r, req, "That code is not valid. Check it and try again, or ask for a new one.")
		return
	}

	portalAttemptsTotal.WithLabelValues("voucher", "granted").Inc()
	log.Info().Str("mac", req.MAC).Int64("grant_id", grantID).Msg("hotspot: voucher redeemed")
	h.renderGranted(w, r, grantedData{Session: req})
}

// ── Subscriber login ────────────────────────────────────────────────────────

// Login handles POST /hotspot/login — an existing subscriber getting their own
// device onto the hotspot with their portal credentials.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	req, ok := h.begin(w, r, "login")
	if !ok {
		return
	}

	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	if username == "" || password == "" {
		portalAttemptsTotal.WithLabelValues("login", "malformed").Inc()
		h.reject(w, r, req, "Please enter your username and password.")
		return
	}

	subscriberID, err := h.authenticate(r.Context(), username, password)
	if errors.Is(err, portal.ErrInvalidCredentials) {
		portalAttemptsTotal.WithLabelValues("login", "refused").Inc()
		h.refuse(w, r, req, "Incorrect username or password.")
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("hotspot: subscriber login failed")
		portalAttemptsTotal.WithLabelValues("login", "error").Inc()
		h.fail(w, r, req)
		return
	}

	grantID, err := h.grants.GrantForSubscriber(r.Context(), req.MAC, subscriberID, req.NASID, h.loginMinutes)
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("hotspot: grant failed")
		portalAttemptsTotal.WithLabelValues("login", "error").Inc()
		h.fail(w, r, req)
		return
	}
	if grantID == 0 {
		// Correct credentials, but the store declined — the subscriber is
		// suspended or terminated. Worth saying plainly: they have already
		// proved who they are, so this is not an oracle, and "your account is
		// not active" is what sends them to support instead of retyping a
		// password that was right the first time.
		portalAttemptsTotal.WithLabelValues("login", "not_active").Inc()
		h.refuse(w, r, req, "Your account is not active. Please contact support.")
		return
	}

	portalAttemptsTotal.WithLabelValues("login", "granted").Inc()
	log.Info().Str("mac", req.MAC).Int("subscriber_id", subscriberID).
		Int64("grant_id", grantID).Msg("hotspot: subscriber signed in")
	h.renderGranted(w, r, grantedData{Session: req})
}

// authenticate verifies subscriber credentials, returning their id.
//
// The dummy-hash comparison on the unknown-username path mirrors
// portal.Authenticate: without it, a missing user returns measurably faster
// than a wrong password and the endpoint becomes a username oracle. This does
// not call portal.Authenticate itself only because that function's product is
// a signed JWT, and minting a portal session for someone who asked for hotspot
// access would hand a walled-garden form the power to open the self-service
// portal — a strictly wider grant than the one requested.
func (h *Handler) authenticate(ctx context.Context, username, password string) (int, error) {
	sub, err := h.subscribers.GetSubscriberByUsername(ctx, username)
	if err != nil {
		return 0, err
	}
	if sub == nil {
		_ = bcrypt.CompareHashAndPassword( //nolint:errcheck // timing equalisation, result unused by design
			[]byte("$2a$12$dummyhashforenumeration/protect"), []byte(password))
		return 0, portal.ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(sub.PasswordHash), []byte(password)); err != nil {
		return 0, portal.ErrInvalidCredentials
	}
	return sub.ID, nil
}

// ── Shared request handling ─────────────────────────────────────────────────

// begin performs the checks every redemption POST shares: a configured
// limiter, a parseable form, a usable MAC, and attempt budget. It writes the
// response itself on refusal, so callers only proceed when ok is true.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, method string) (sessionRequest, bool) {
	if h.grants == nil || h.limiter == nil {
		// 503 rather than silently redeeming without a limiter. An unmetered
		// voucher endpoint is a worse failure than an unavailable one, so a
		// deployment that has not configured Redis does not get a captive portal.
		h.renderLanding(w, r, http.StatusServiceUnavailable, landingData{
			Notice: "The hotspot sign-in service is unavailable. Please try again shortly.",
		})
		return sessionRequest{}, false
	}
	if err := r.ParseForm(); err != nil {
		h.renderLanding(w, r, http.StatusBadRequest, landingData{Notice: "Something went wrong with that form. Please try again."})
		return sessionRequest{}, false
	}

	req, ok := readSessionRequest(r)
	if !ok {
		portalAttemptsTotal.WithLabelValues(method, "malformed").Inc()
		h.renderLanding(w, r, http.StatusBadRequest, landingData{
			Notice: "We could not identify your device. Please reconnect to the Wi-Fi and try again.",
		})
		return sessionRequest{}, false
	}

	allowed, err := h.limiter.Allow(r.Context(), clientKey(r, req.MAC))
	if err != nil {
		log.Error().Err(err).Msg("hotspot: attempt limiter unavailable — refusing redemption")
		portalAttemptsTotal.WithLabelValues(method, "error").Inc()
		h.fail(w, r, req)
		return sessionRequest{}, false
	}
	if !allowed {
		portalRateLimitedTotal.Inc()
		portalAttemptsTotal.WithLabelValues(method, "rate_limited").Inc()
		h.renderLanding(w, r, http.StatusTooManyRequests, landingData{
			Session: req, Ready: true,
			Notice: "Too many attempts. Please wait a few minutes before trying again.",
		})
		return sessionRequest{}, false
	}
	return req, true
}

// refuse re-renders the landing page after a credential was wrong, keeping the
// NAS session parameters so the visitor can correct their entry and resubmit.
//
// Every voucher refusal renders through here with the same fixed notice,
// whatever the underlying reason. That uniformity is the anti-oracle property,
// so a caller must never pass a message derived from the request.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, req sessionRequest, notice string) {
	h.renderLanding(w, r, http.StatusUnauthorized, landingData{Session: req, Ready: true, Notice: notice})
}

// reject re-renders after an incomplete submission — a field the visitor left
// blank, rather than a credential that was wrong.
func (h *Handler) reject(w http.ResponseWriter, r *http.Request, req sessionRequest, notice string) {
	h.renderLanding(w, r, http.StatusUnprocessableEntity, landingData{Session: req, Ready: true, Notice: notice})
}

// fail reports an internal problem without hinting at what it was.
//
// Only reached once a session request has parsed, so the forms are always
// re-offered: the visitor's next move is to retry, not to reconnect.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, req sessionRequest) {
	h.renderLanding(w, r, http.StatusInternalServerError, landingData{
		Session: req, Ready: true,
		Notice: "Something went wrong on our side. Please try again in a moment.",
	})
}

// Logo handles GET /hotspot/logo — the venue's logo (FR-HSP-004).
//
// The page links it as /hotspot/logo?v=<hash prefix>, so a replaced logo is a
// different URL and it is safe to let a phone cache this one for a long time.
// A request for a version that is no longer current is still answered with
// the current logo rather than a 404: a page rendered a moment before the
// change should not show a broken image.
//
// Content-Type is the type the upload was sniffed as, never what the uploader
// claimed, and nosniff stops a browser second-guessing it. Together they are
// what keep this from being a way to serve arbitrary content from the portal's
// origin.
func (h *Handler) Logo(w http.ResponseWriter, r *http.Request) {
	data, contentType, sha := h.branding.Logo(r.Context())
	if len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	etag := `"` + sha + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if _, err := w.Write(data); err != nil {
		log.Debug().Err(err).Msg("hotspot: client went away mid-logo")
	}
}
