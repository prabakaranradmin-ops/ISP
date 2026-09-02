package radius

import (
	"net"

	"github.com/rs/zerolog/log"
)

// applyReadBuffer requests readBufferBytes of socket receive buffer on a
// listener and reports what the kernel actually granted.
//
// The distinction matters more than it looks. SetReadBuffer is a request:
// Linux silently clamps it to net.core.rmem_max, so on a host with the
// default ceiling (~212KB) this call succeeds and changes almost nothing.
// A daemon that logged "requested 4MiB" would be reporting an intention,
// not a fact, and the operator would have no way to tell a tuned host from
// an untuned one short of reading /proc. Logging the granted size makes the
// missing sysctl visible at startup instead of during the storm it was
// supposed to survive.
//
// Failure here is logged, never fatal. A smaller buffer degrades throughput
// under burst; it does not stop the daemon serving, and refusing to start
// over it would turn a tuning problem into an outage.
func applyReadBuffer(conn *net.UDPConn, listener, addr string) {
	if err := conn.SetReadBuffer(readBufferBytes); err != nil {
		log.Warn().Err(err).Str("listener", listener).Str("addr", addr).
			Int("requested_bytes", readBufferBytes).
			Msg("radius: could not set UDP read buffer — the socket keeps the OS default, " +
				"which drops packets under burst")
		return
	}

	granted, err := readBufferSize(conn)
	if err != nil {
		// Readback is diagnostics, not a precondition: the buffer was set.
		log.Debug().Err(err).Str("listener", listener).
			Msg("radius: UDP read buffer set, but its effective size could not be read back")
		return
	}

	ev := log.Info()
	// Linux reports double what was requested (the kernel's own accounting
	// overhead is included in SO_RCVBUF), so a grant of at least the request
	// means the request was honoured in full.
	if granted < readBufferBytes {
		ev = log.Warn()
	}
	ev.Str("listener", listener).Str("addr", addr).
		Int("requested_bytes", readBufferBytes).
		Int("granted_bytes", granted).
		Msg("radius: UDP read buffer configured")

	if granted < readBufferBytes {
		log.Warn().Str("listener", listener).
			Msg("radius: the kernel capped the UDP read buffer below the request — raise " +
				"net.core.rmem_max (see deploy/gcp/provision.sh) or this listener will shed " +
				"packets during a reconnect storm")
	}
}
