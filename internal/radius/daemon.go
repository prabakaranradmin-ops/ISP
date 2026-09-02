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
	// readBufferBytes is the UDP socket receive buffer requested per listener.
	//
	// Without this the socket runs on net.core.rmem_default (~212KB on
	// Linux), a few hundred RADIUS packets. A mass reconnect — a BNG
	// rebooting and every subscriber behind it re-authenticating at once —
	// overruns that in well under a second, and the kernel discards the
	// excess silently: no error reaches this process, nothing appears in any
	// metric here, and the only evidence is the host's own counters
	// (`nstat -az '*Udp*'`, `netstat -su`).
	//
	// Requested, not guaranteed. The kernel caps this at net.core.rmem_max,
	// so the sysctl in deploy/gcp/provision.sh is what makes the larger
	// value actually take effect; on a host with the default ceiling this
	// silently yields a smaller buffer, which is why StartContext logs the
	// size it actually got rather than the size it asked for.
	readBufferBytes = 4 << 20 // 4 MiB
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
	// radiusPacketsDropped counts packets shed because the worker queue was
	// full. Non-zero means the daemon is over capacity right now — the
	// single most important number during a reconnect storm, and until this
	// existed there was nothing anywhere that reported overload.
	//
	// Labelled by port so a flood of accounting traffic is distinguishable
	// from an authentication storm; they have very different causes and very
	// different responses.
	radiusPacketsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "radius_packets_dropped_total",
		Help: "RADIUS packets shed without processing because the worker queue was saturated",
	}, []string{"listener"})
	// radiusQueueDepth is the live occupancy of the worker queue.
	//
	// OPS §12.3.4 has cited a metric by this name for some time; it did not
	// exist. Rising depth is the leading indicator that precedes drops, so
	// alerting on this buys warning that the counter above cannot.
	radiusQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "radius_worker_queue_depth",
		Help: "Packets currently queued for the RADIUS worker pool",
	})
	// radiusQueueCapacity is exported alongside depth so a dashboard can plot
	// utilisation without hard-coding workerCount*4, which would then be
	// wrong the moment the pool is resized.
	radiusQueueCapacity = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "radius_worker_queue_capacity",
		Help: "Maximum packets the RADIUS worker queue can hold",
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

// SetVerifierPersistence enables the fast-verifier cache's L2 tier
// (migration 046), so verifiers survive a restart and are visible to
// api_service for immediate invalidation.
//
// Optional. Without it the cache is in-process only and behaves exactly as
// it did before — correct, but empty after every restart, which is when a
// reconnect storm is most likely and bcrypt is least affordable.
func (d *RadiusDaemon) SetVerifierPersistence(s VerifierStore) {
	d.verifierCache.SetPersistence(s)
}

// WarmVerifierCache repopulates the fast-verifier cache from its persistent
// tier. Call once before serving; returns the number of entries restored.
func (d *RadiusDaemon) WarmVerifierCache(ctx context.Context) (int, error) {
	return d.verifierCache.Warm(ctx)
}

// InvalidateVerifier drops one subscriber's cached verifier from both tiers.
func (d *RadiusDaemon) InvalidateVerifier(ctx context.Context, username string) error {
	return d.verifierCache.Invalidate(ctx, username)
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
	d := &RadiusDaemon{
		addr:          addr,
		secret:        secret,
		db:            db,
		acctDedup:     localcache.New[struct{}](0),
		guard:         NewBruteForceGuard(localcache.NewCounter(0)),
		verifierCache: NewVerifierCache(localcache.New[[]byte](0), verifierSecret),
		packetQueue:   make(chan radiusJob, workerCount*4),
	}
	// Published at construction so utilisation (depth/capacity) is plottable
	// from the moment the process starts, including for a daemon that never
	// receives a packet — a flat zero is a meaningful reading, an absent
	// series is not.
	radiusQueueCapacity.Set(float64(cap(d.packetQueue)))
	return d
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
	applyReadBuffer(conn, "auth", d.addr)

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
	applyReadBuffer(acctConn, "acct", d.effectiveAcctAddr())

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
	// Shed rather than block when the queue is full.
	//
	// This looks like it throws work away, and that is the point. The
	// alternative — the blocking send this replaced — does not preserve the
	// packet, it preserves the goroutine holding it, and layeh's PacketServer
	// spawns one goroutine per packet with no bound (server-packet.go's read
	// loop calls `go func(...)` for every datagram and never waits). So a
	// full queue did not apply backpressure to the socket; it converted
	// offered load into resident memory, without limit, until the OOM killer
	// chose this process. Restarting then emptied the verifier cache, making
	// the next minute of authentication ~19x more expensive (bcrypt cost 12
	// on every request instead of a cache hit) and refilling the queue faster
	// than before — a crash loop that got worse each cycle rather than
	// recovering.
	//
	// Dropping is also what the protocol expects. RADIUS is request/response
	// over UDP with client-side retransmission (RFC 2865 §2.5): a NAS that
	// gets no answer retries, typically after 3-5s. A shed packet costs one
	// retransmit. A blocked one costs the daemon.
	//
	// ctx.Done() stays in the select so shutdown is not gated on queue space.
	enqueueTo := func(listener string) radius.HandlerFunc {
		return func(w radius.ResponseWriter, r *radius.Request) {
			d.tryEnqueue(ctx, listener, w, r)
		}
	}

	server := &radius.PacketServer{
		Addr:         d.addr,
		SecretSource: packetSecretSource,
		Handler:      enqueueTo("auth"),
	}
	acctServer := &radius.PacketServer{
		Addr:         d.effectiveAcctAddr(),
		SecretSource: packetSecretSource,
		Handler:      enqueueTo("acct"),
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
// tryEnqueue offers one packet to the worker pool, shedding it if the queue
// is already full. Reports whether it was accepted.
//
// The `default` arm is the whole point, and it replaced a blocking send.
// This looks like it throws work away; what it actually prevents is worse.
// layeh's PacketServer spawns one goroutine per datagram with no bound
// (server-packet.go's read loop calls `go func(...)` for every packet and
// never waits on it), so a blocking send here did not apply backpressure to
// the socket — it parked an unbounded number of goroutines, each holding a
// copied packet, until the OOM killer chose this process. Restarting then
// emptied the verifier cache, making the next minute of authentication far
// more expensive (bcrypt cost 12 on every request rather than a cache hit)
// and refilling the queue faster than before: a crash loop that deepened
// each cycle instead of recovering.
//
// Shedding is also what the protocol expects. RADIUS is request/response
// over UDP with client-side retransmission (RFC 2865 §2.5) — a NAS that
// gets no answer retries, typically after 3-5s. A dropped packet costs one
// retransmit; a blocked one costs the daemon.
//
// ctx.Done() remains a case so a shutdown in progress does not keep
// accepting work. Note that `default` makes this select non-blocking
// regardless, so ctx is consulted only when it is already cancelled.
func (d *RadiusDaemon) tryEnqueue(ctx context.Context, listener string, w radius.ResponseWriter, r *radius.Request) bool {
	select {
	case d.packetQueue <- radiusJob{w: w, r: r}:
		radiusQueueDepth.Set(float64(len(d.packetQueue)))
		return true
	case <-ctx.Done():
		return false
	default:
		// Deliberately not logged. One line per shed packet during a storm is
		// itself a load source on the very process that is already
		// overloaded; the counter is the signal, and alerting belongs at the
		// Prometheus layer.
		radiusPacketsDropped.WithLabelValues(listener).Inc()
		radiusQueueDepth.Set(float64(len(d.packetQueue)))
		return false
	}
}

func (d *RadiusDaemon) workerPoolConsumer(ctx context.Context) {
	for job := range d.packetQueue {
		// Sampled on both sides of the queue rather than by a ticker: depth
		// during a storm changes far faster than any sane scrape interval,
		// and the value that matters for alerting is the peak, which a
		// periodic sample would mostly miss.
		radiusQueueDepth.Set(float64(len(d.packetQueue)))
		d.handleRequest(ctx, job.w, job.r)
	}
}
