// Package radius implements the RADIUS AAA worker pool daemon.
//
// FR: FR-AAA-001..004 | NFR: NFR-PERF-001, NFR-SCAL-001 | DDS §5.1
package radius

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"layeh.com/radius"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

const (
	workerCount = 128 // fixed pool — prevents goroutine storms during auth peaks
	// shutdownGrace bounds how long in-flight packets may finish on shutdown.
	shutdownGrace = 10 * time.Second
)

// Prometheus metrics
var (
	radiusAuthDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "radius_auth_duration_seconds",
		Help:    "RADIUS authentication request duration",
		Buckets: []float64{0.001, 0.005, 0.01, 0.015, 0.025, 0.05, 0.1},
	})
	radiusDedupSkipped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_acct_dedup_skipped_total",
		Help: "Accounting packets skipped due to deduplication",
	})
	radiusAuthAccept = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_auth_accept_total",
		Help: "RADIUS Access-Accept responses sent",
	})
	radiusAuthReject = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_auth_reject_total",
		Help: "RADIUS Access-Reject responses sent",
	})
	// Per-NAS outcomes, alongside the unlabelled totals above rather than
	// replacing them: FR-OBS-005 asks for a failure rate "on any NAS", which
	// cannot be computed from a global counter, while existing dashboards and
	// the NFR harness read the totals by name.
	//
	// Cardinality is bounded by the registered inventory, not by traffic. The
	// label is the NAS the resolver identified, and anything it does not
	// recognise collapses into a single "unregistered" bucket — without that,
	// a spoofed source address would mint a new time series per packet.
	radiusAuthOutcome = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "radius_auth_outcome_total",
		Help: "RADIUS authentication outcomes by NAS and result",
	}, []string{"nas", "result"})
)

// unregisteredNAS is the single bucket every unidentified NAS shares, so
// cardinality stays bounded by the inventory rather than by whoever sends
// packets.
const unregisteredNAS = "unregistered"

// authAccepted and authRejected are the only places authentication outcomes
// are counted.
//
// Every accept and reject goes through one of these rather than incrementing
// the counters directly, because there are fourteen such sites across PAP, EAP
// and MAB: updating the global total but not the per-NAS one at any single
// site would leave FR-OBS-005's failure rate quietly wrong for that path, and
// wrong in the direction that under-reports failures.
func (d *RadiusDaemon) authAccepted(r *radius.Request) {
	radiusAuthAccept.Inc()
	d.recordAuthOutcome(r, true)
}

func (d *RadiusDaemon) authRejected(r *radius.Request) {
	radiusAuthReject.Inc()
	d.recordAuthOutcome(r, false)
}

// recordAuthOutcome counts one authentication result against its NAS.
func (d *RadiusDaemon) recordAuthOutcome(r *radius.Request, accepted bool) {
	nasLabel := unregisteredNAS
	if d.nasResolver != nil && r != nil && r.RemoteAddr != nil {
		if device := d.nasResolver.ResolveAddr(r.RemoteAddr); device.IP != "" {
			nasLabel = device.IP
		}
	}
	result := "reject"
	if accepted {
		result = "accept"
	}
	radiusAuthOutcome.WithLabelValues(nasLabel, result).Inc()
}

// DBQuerier is the minimal DB interface required by the RADIUS daemon.
type DBQuerier interface {
	GetSubscriberByUsername(ctx context.Context, username string) (*Subscriber, error)
}

// Subscriber holds the fields needed for RADIUS auth decisions.
type Subscriber struct {
	ID           int
	Username     string
	PasswordHash string
	Status       string // active | grace_period | soft_suspended | hard_suspended | terminated
	RateLimitStr string // MikroTik format: "100M/100M"
	FUPActive    bool
	FUPThrottle  string
	// SpeedOverrideRateLimit is an owner-triggered temporary rate (console
	// "Speed override"), independent of the billed plan and of FUP. Empty
	// means no override is set. Takes precedence over FUPThrottle when set
	// and not yet past SpeedOverrideExpiresAt — see effectiveRateLimit.
	SpeedOverrideRateLimit string
	SpeedOverrideExpiresAt *time.Time
	PlanID                 int // resolves a policy-reference vendor's QoS profile name (FR-NAS-001, MDS §4.11)
	// NTHash is MD4(UTF-16LE(password)), present only for subscribers
	// enrolled for EAP-MSCHAPv2 (FR-AAA-006, migration 029). Nil means PAP
	// against PasswordHash is the only method available to them, which is
	// the default for everybody.
	NTHash []byte
	// VolumeGB is the billed plan's data quota (0 = unlimited), sourced from
	// plans.volume_gb. Carried through purely so acctStart can populate
	// LiveSession.BytesTotal for the portal's live-usage panel.
	VolumeGB int
}

// radiusJob bundles the ResponseWriter and Request so both can pass through the worker queue.
type radiusJob struct {
	w radius.ResponseWriter
	r *radius.Request
}

// RadiusDaemon is the fixed-worker-pool RADIUS server.
type RadiusDaemon struct {
	addr          string
	acctAddr      string
	secret        []byte
	db            DBQuerier
	acctDedup     *localcache.Store[struct{}]
	guard         *BruteForceGuard
	verifierCache *VerifierCache
	packetQueue   chan radiusJob
	nasResolver   *nas.Resolver
	mabDB         MABQuerier
	eapSessions   *EAPSessionStore
	acctDB        AccountingStore
	grantUsageDB  GrantUsageDB
	liveSessions  LiveSessionWriter
}

// DefaultAcctAddr is the RFC 2866 accounting port. Authentication and
// accounting are separate ports by protocol, and every NAS sends them
// separately: binding only :1812 means accounting packets arrive at a closed
// port and the NAS's records are silently discarded on the wire.
const DefaultAcctAddr = ":1813"

// SetAcctAddr overrides the accounting listener address.
func (d *RadiusDaemon) SetAcctAddr(addr string) { d.acctAddr = addr }

func (d *RadiusDaemon) effectiveAcctAddr() string {
	if d.acctAddr == "" {
		return DefaultAcctAddr
	}
	return d.acctAddr
}

// SetEAPSessionStore enables EAP-MSCHAPv2 (FR-AAA-006, MDS §4.18).
//
// Optional: with no store set, an Access-Request carrying an EAP-Message is
// rejected rather than silently falling through to the PAP path, and every
// PAP authentication behaves exactly as before.
func (d *RadiusDaemon) SetEAPSessionStore(s *EAPSessionStore) {
	d.eapSessions = s
}

// SetNASResolver enables per-NAS secret verification and vendor-aware
// Access-Accept attributes (FR-NAS-001..004, MDS §4.11). Optional: with no
// resolver set, the daemon behaves exactly as before — one global secret,
// every Access-Accept gets the MikroTik VSA — so this is safe to leave
// unset on a deployment not ready to register its NAS inventory yet.
func (d *RadiusDaemon) SetNASResolver(r *nas.Resolver) {
	d.nasResolver = r
}

// LiveSessionWriter records the daemon's view of live session state, read by
// the health endpoint and the subscriber portal's live-usage panel.
//
// Satisfied by *cache.SessionStore. A separate interface (rather than
// depending on the cache package directly) keeps this package's dependency
// graph one-directional: cache already imports radius for radius.DBQuerier,
// so radius importing cache back would cycle.
type LiveSessionWriter interface {
	Put(ctx context.Context, sess LiveSession) error
	UpdateOctets(ctx context.Context, sessionID string, inputOctets, outputOctets int64) error
	DeleteBySessionID(ctx context.Context, sessionID string) error
}

// LiveSession is what the daemon knows about a session at Accounting-Start,
// enough for LiveSessionWriter.Put — deliberately a subset of cache.Session,
// which additionally carries fields (byte counters that accumulate over the
// session's life) this package has no reason to compute itself.
type LiveSession struct {
	SessionID    string
	SubscriberID int
	NasIP        string
	AssignedIP   string
	BytesTotal   int64 // plan quota in bytes; 0 = unlimited
	SpeedProfile string
	FUPThrottled bool
}

// SetLiveSessionStore enables live session tracking for the health endpoint
// and the subscriber portal's live-usage panel (DDS §5.9, IDD §8.4).
// Optional: without it, accounting still persists to subscriber_session_history
// exactly as before, and those two read surfaces simply report "offline".
func (d *RadiusDaemon) SetLiveSessionStore(s LiveSessionWriter) {
	d.liveSessions = s
}

// NewRadiusDaemon constructs a RadiusDaemon. verifierSecret keys the
// fast-verifier cache (see VerifierCache) that lets repeat authentications
// skip bcrypt cost-12 to meet NFR-PERF-001's 15ms p99 budget; it must be a
// separate secret from secret (the RADIUS shared secret used for NAS
// protocol obfuscation), not the same value reused for a different purpose.
//
// The brute-force guard, verifier cache and accounting dedup store are all
// in-process caches this daemon owns for its own lifetime — see
// internal/localcache's package doc — so they are constructed here rather
// than threaded in from cmd/radiusd.
func NewRadiusDaemon(addr string, secret []byte, db DBQuerier, verifierSecret []byte) *RadiusDaemon {
	return &RadiusDaemon{
		addr:          addr,
		secret:        secret,
		db:            db,
		acctDedup:     localcache.New[struct{}](0),
		guard:         NewBruteForceGuard(localcache.NewCounter(0)),
		verifierCache: NewVerifierCache(localcache.New[[]byte](0), verifierSecret),
		packetQueue:   make(chan radiusJob, workerCount*4),
	}
}

// Start binds the UDP port, launches the worker pool, and begins serving.
// It blocks until the server returns an error.
//
// Deprecated: prefer StartContext, which can be shut down.
//
// DDS §5.1
func (d *RadiusDaemon) Start() error {
	return d.StartContext(context.Background())
}

// StartContext binds the UDP port, launches the worker pool, and serves until
// ctx is cancelled or the listener fails.
//
// On cancellation the listener stops accepting first, then queued packets are
// drained: a subscriber whose Access-Request was already accepted should get an
// answer rather than a timeout during a rolling restart.
//
// DDS §5.1
func (d *RadiusDaemon) StartContext(ctx context.Context) error {
	udpAddr, err := net.ResolveUDPAddr("udp", d.addr)
	if err != nil {
		return fmt.Errorf("radius: resolve addr %s: %w", d.addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("radius: listen UDP %s: %w", d.addr, err)
	}

	// Accounting is bound before the worker pool starts, so a port already in
	// use fails startup rather than leaving the daemon authenticating happily
	// while silently recording nothing — the exact failure this listener was
	// added to end.
	acctAddr, err := net.ResolveUDPAddr("udp", d.effectiveAcctAddr())
	if err != nil {
		conn.Close() //nolint:errcheck,gosec
		return fmt.Errorf("radius: resolve accounting addr %s: %w", d.effectiveAcctAddr(), err)
	}
	acctConn, err := net.ListenUDP("udp", acctAddr)
	if err != nil {
		conn.Close() //nolint:errcheck,gosec
		return fmt.Errorf("radius: listen UDP %s (accounting): %w", d.effectiveAcctAddr(), err)
	}

	// Said out loud because the alternative is the failure this listener was
	// added to end: a daemon that authenticates perfectly, acknowledges every
	// accounting record, and writes none of them — leaving FUP enforcement, CoA
	// targeting, LEA lookups and portal usage all reading an empty table with
	// nothing anywhere reporting a problem.
	if d.acctDB == nil {
		log.Warn().Str("acct_addr", d.effectiveAcctAddr()).
			Msg("radius: no accounting store configured — sessions will not be recorded, " +
				"and quota enforcement, LEA lookups and portal usage will have no data")
	}

	var workers sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			d.workerPoolConsumer(ctx)
		}()
	}

	packetSecretSource := radius.StaticSecretSource(d.secret)
	if d.nasResolver != nil {
		packetSecretSource = d.nasResolver
	}

	// Both listeners feed the one worker pool. Accounting is far lighter than
	// authentication and shares the same backends, so a second pool would only
	// add tuning surface; what matters is that neither port can starve the
	// other, which the shared bounded queue already ensures.
	enqueue := radius.HandlerFunc(func(w radius.ResponseWriter, r *radius.Request) {
		select {
		case d.packetQueue <- radiusJob{w: w, r: r}:
		case <-ctx.Done():
		}
	})

	server := &radius.PacketServer{
		Addr:         d.addr,
		SecretSource: packetSecretSource,
		Handler:      enqueue,
	}
	acctServer := &radius.PacketServer{
		Addr:         d.effectiveAcctAddr(),
		SecretSource: packetSecretSource,
		Handler:      enqueue,
	}

	// Buffered for both, so whichever listener does not fail first can still
	// deliver its result without leaking a blocked goroutine.
	serveErr := make(chan error, 2)
	go func() { serveErr <- server.Serve(conn) }()
	go func() { serveErr <- acctServer.Serve(acctConn) }()

	// Deliberately not derived from ctx: on cancellation we are here *because*
	// ctx was cancelled, and a Shutdown given a dead context returns
	// immediately without draining, defeating the graceful stop.
	shutdown := func() { //nolint:contextcheck // a fresh deadline is required to drain
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)     //nolint:errcheck
		_ = acctServer.Shutdown(shutdownCtx) //nolint:errcheck
		close(d.packetQueue)
		workers.Wait()
	}

	select {
	case err := <-serveErr:
		// One listener died. Stop the other too rather than running on in a
		// half-serving state where authentication works and accounting does
		// not, or the reverse.
		shutdown()
		return err
	case <-ctx.Done():
		shutdown()
		return nil
	}
}

// workerPoolConsumer drains the packet queue until it is closed.
//
// ctx is the daemon lifetime; each request still gets its own deadline so one
// slow backend cannot pin a worker indefinitely.
func (d *RadiusDaemon) workerPoolConsumer(ctx context.Context) {
	for job := range d.packetQueue {
		d.handleRequest(ctx, job.w, job.r)
	}
}
