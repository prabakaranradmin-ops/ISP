package nastraffic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var pollsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "nas_traffic_polls_total",
	Help: "SNMP interface polls of NAS devices, by result",
}, []string{"result"})

// Router is a NAS with SNMP polling enabled.
type Router struct {
	NASID              int
	Host               string
	Port               int
	CommunityEncrypted string
}

// Write is one interface's outcome from a poll, for the store to apply.
type Write struct {
	Iface Interface
	// Expect is the baseline this poll measured against — nil when there was
	// none. The store applies the write only if the interface's stored
	// baseline is still this one, so two pollers racing on one router cannot
	// both record the same interval.
	Expect *Previous
	// AdvanceBaseline stores Iface's counters as the new baseline.
	AdvanceBaseline bool
	// Sample is the rate to record, or nil.
	Sample *Sample
}

// Store persists polls. Satisfied by *db.NASStore.
type Store interface {
	ListPollableRouters(ctx context.Context) ([]Router, error)
	InterfaceBaselines(ctx context.Context, nasID int) (map[string]Previous, error)
	// SavePoll applies the writes and marks the router as polled
	// successfully at at, in one transaction.
	SavePoll(ctx context.Context, nasID int, at time.Time, writes []Write) error
	RecordPollFailure(ctx context.Context, nasID int, at time.Time, reason string) error
	PurgeSamplesBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Decrypter turns a stored community back into its plaintext.
type Decrypter func(ciphertext string) (string, error)

// Poller polls every enabled router on a ticker.
type Poller struct {
	store       Store
	client      SNMPClient
	decrypt     Decrypter
	interval    time.Duration
	concurrency int
	// routerTimeout bounds one router's whole poll, so a router that answers
	// the first walk and then stalls cannot hold a worker indefinitely.
	routerTimeout time.Duration
	now           func() time.Time
	lastPurge     time.Time
}

// NewPoller builds a Poller. decrypt must be non-nil: the community is a
// secret stored encrypted, and there is no plaintext fallback.
func NewPoller(store Store, client SNMPClient, decrypt Decrypter) *Poller {
	return &Poller{
		store: store, client: client, decrypt: decrypt,
		interval:      PollInterval,
		concurrency:   8,
		routerTimeout: time.Minute,
		now:           time.Now,
	}
}

// SetClock replaces the poller's clock. For tests and the simulator, which
// need to step through five-minute intervals without waiting for them; the
// service always runs on the wall clock.
func (p *Poller) SetClock(now func() time.Time) { p.now = now }

// Run polls immediately and then every interval until ctx is cancelled.
//
// Immediately rather than after the first tick, so a restart does not leave a
// five-minute hole at the start of every graph — and so a freshly enabled
// router shows its interfaces within one interval of the service starting.
func (p *Poller) Run(ctx context.Context) {
	p.PollOnce(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.PollOnce(ctx)
		}
	}
}

// PollOnce polls every enabled router, then purges expired samples at most
// once an hour.
func (p *Poller) PollOnce(ctx context.Context) {
	routers, err := p.store.ListPollableRouters(ctx)
	if err != nil {
		log.Error().Err(err).Msg("nastraffic: could not list routers to poll")
		return
	}

	sem := make(chan struct{}, p.concurrency)
	var wg sync.WaitGroup
	for _, r := range routers {
		wg.Add(1)
		sem <- struct{}{}
		go func(r Router) {
			defer wg.Done()
			defer func() { <-sem }()
			p.pollRouter(ctx, r)
		}(r)
	}
	wg.Wait()

	if now := p.now(); now.Sub(p.lastPurge) >= time.Hour {
		n, err := p.store.PurgeSamplesBefore(ctx, now.Add(-Retention))
		if err != nil {
			log.Warn().Err(err).Msg("nastraffic: purging expired samples failed")
		} else {
			p.lastPurge = now
			if n > 0 {
				log.Info().Int64("rows", n).Msg("nastraffic: purged samples past retention")
			}
		}
	}
}

func (p *Poller) pollRouter(ctx context.Context, r Router) {
	at := p.now()
	fail := func(reason string) {
		pollsTotal.WithLabelValues("failed").Inc()
		if err := p.store.RecordPollFailure(ctx, r.NASID, at, reason); err != nil {
			log.Warn().Err(err).Int("nas_id", r.NASID).Msg("nastraffic: could not record poll failure")
		}
	}

	community, err := p.decrypt(r.CommunityEncrypted)
	if err != nil {
		log.Error().Err(err).Int("nas_id", r.NASID).Msg("nastraffic: could not decrypt SNMP community")
		fail("The stored SNMP community could not be decrypted — set it again on the Routers screen.")
		return
	}

	pctx, cancel := context.WithTimeout(ctx, p.routerTimeout)
	defer cancel()
	res, err := p.client.Poll(pctx, Target{Host: r.Host, Port: uint16(r.Port), Community: community}) //nolint:gosec // port is CHECKed 1..65535
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not the router's fault
		}
		fail(describePollError(err))
		return
	}
	if res.Truncated > 0 {
		log.Warn().Int("nas_id", r.NASID).Int("truncated", res.Truncated).
			Msg("nastraffic: router reports more interfaces than are kept; the rest are not graphed")
	}

	prev, err := p.store.InterfaceBaselines(ctx, r.NASID)
	if err != nil {
		log.Error().Err(err).Int("nas_id", r.NASID).Msg("nastraffic: could not read baselines")
		return
	}
	writes := make([]Write, 0, len(res.Interfaces))
	for _, iface := range res.Interfaces {
		var expect *Previous
		if b, ok := prev[iface.Name]; ok {
			b := b
			expect = &b
		}
		sample, outcome := Rate(expect, iface, at)
		w := Write{Iface: iface, Expect: expect, AdvanceBaseline: outcome.AdvancesBaseline()}
		if outcome == OutcomeSample {
			s := sample
			w.Sample = &s
		}
		writes = append(writes, w)
	}
	if err := p.store.SavePoll(ctx, r.NASID, at, writes); err != nil {
		log.Error().Err(err).Int("nas_id", r.NASID).Msg("nastraffic: could not save poll")
		pollsTotal.WithLabelValues("store_failed").Inc()
		return
	}
	pollsTotal.WithLabelValues("ok").Inc()
}

// describePollError turns a poll failure into a sentence for the Routers
// screen. The raw error is kept on the end: the operator debugging a router
// that will not answer needs the detail, and this console is staff-only.
func describePollError(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, ErrNoInterfaces):
		return ErrNoInterfaces.Error()
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(msg), "timeout"):
		// SNMPv2c gives no error for a wrong community — the agent simply
		// does not answer — so the community is listed first.
		return "No answer from the router. Check the community string, that the router's SNMP service is enabled and allows this server's address, and that UDP " +
			"is not blocked on the way. (" + msg + ")"
	}
	return fmt.Sprintf("The poll failed: %s", msg)
}
