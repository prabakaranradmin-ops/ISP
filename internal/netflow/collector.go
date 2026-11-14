package netflow

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/rs/zerolog/log"
)

// The collector: a UDP listener that decodes NetFlow v9 packets and hands the
// flows to a sink. Templates are kept per (exporter address, observation source
// id), because that pair is what scopes a template in NetFlow v9 — two exporters
// may use the same template id for different layouts.

// FlowSink receives decoded flows. Satisfied by a database store (the next
// slice wires one) and by an in-memory sink in the tests. Kept an interface so
// the collector has no database dependency of its own.
type FlowSink interface {
	RecordFlows(ctx context.Context, flows []Flow) error
}

// Collector decodes NetFlow v9 traffic into a FlowSink.
type Collector struct {
	sink   FlowSink
	mu     sync.Mutex
	caches map[string]TemplateCache
}

// NewCollector returns a collector that writes to sink.
func NewCollector(sink FlowSink) *Collector {
	return &Collector{sink: sink, caches: make(map[string]TemplateCache)}
}

// cacheFor returns the persistent template cache for one exporter/source,
// creating it on first use. Persistence is the point: a data packet that
// arrives minutes after its template packet still decodes.
func (c *Collector) cacheFor(remote string, sourceID uint32) TemplateCache {
	key := fmt.Sprintf("%s|%d", remote, sourceID)
	c.mu.Lock()
	defer c.mu.Unlock()
	cache, ok := c.caches[key]
	if !ok {
		cache = NewTemplateCache()
		c.caches[key] = cache
	}
	return cache
}

// Handle decodes one datagram from remote and records its flows, returning how
// many were recorded. Exposed so a test can drive the full decode→sink path
// without opening a socket.
func (c *Collector) Handle(ctx context.Context, remote string, data []byte) (int, error) {
	var sourceID uint32
	if len(data) >= headerLen {
		sourceID = binary.BigEndian.Uint32(data[16:20])
	}
	cache := c.cacheFor(remote, sourceID)

	_, flows, err := DecodePacket(data, cache)
	if err != nil {
		return 0, err
	}
	if len(flows) == 0 {
		// A template-only packet, or data whose template has not arrived yet.
		return 0, nil
	}
	if err := c.sink.RecordFlows(ctx, flows); err != nil {
		return 0, fmt.Errorf("netflow: record flows: %w", err)
	}
	return len(flows), nil
}

// ListenAndServe runs the UDP collector on addr until ctx is cancelled. A
// decode or sink error on one packet is logged and dropped — one malformed
// datagram must not take down flow collection for the whole exporter estate.
func (c *Collector) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("netflow: listen %s: %w", addr, err)
	}
	defer pc.Close() //nolint:errcheck

	log.Info().Str("addr", addr).Msg("netflow: collector listening")
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	buf := make([]byte, 65535)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("netflow: read: %w", err)
			}
		}
		// Copy out of the shared read buffer before handing it on.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if _, err := c.Handle(ctx, src.String(), pkt); err != nil {
			log.Warn().Err(err).Str("src", src.String()).Msg("netflow: packet dropped")
		}
	}
}
