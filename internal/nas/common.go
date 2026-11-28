package nas

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"layeh.com/radius"
)

// buildVendorSubAttr wraps a single TLV sub-attribute (type + length +
// value) inside a RADIUS Vendor-Specific attribute (type 26). This is the
// standard RFC 2865 §5.26 VSA encoding shared by every vendor here except
// Juniper's Filter-Id, which is a top-level standard attribute, not a VSA.
func buildVendorSubAttr(vendorID uint32, subType byte, value []byte) (Attr, error) {
	if 2+len(value) > 255 {
		return Attr{}, fmt.Errorf("nas: vendor %d sub-attr %d: value too long (%d bytes)", vendorID, subType, len(value))
	}
	data := make([]byte, 2+len(value))
	data[0] = subType
	data[1] = byte(2 + len(value)) //nolint:gosec // bounds-checked above
	copy(data[2:], value)

	vsAttr, err := radius.NewVendorSpecific(vendorID, radius.Attribute(data))
	if err != nil {
		return Attr{}, fmt.Errorf("nas: vendor %d sub-attr %d: build VSA: %w", vendorID, subType, err)
	}
	return Attr{Type: 26, Value: vsAttr}, nil
}

// uint32BE encodes v as 4-byte big-endian, the width every vendor VSA here
// that carries a raw rate uses. The truncating shifts are the intended
// byte-extraction, not an overflow bug — the same kind of intentional
// truncation already suppressed elsewhere in this codebase (handlers.go,
// coa_task.go) for the identical reason.
func uint32BE(v uint32) []byte {
	return []byte{
		byte(v >> 24), //nolint:gosec // intentional truncation: extracting one byte
		byte(v >> 16), //nolint:gosec
		byte(v >> 8),  //nolint:gosec
		byte(v),       //nolint:gosec
	}
}

// ValidatePlainRateLimit reports whether s is a plain "down/up" rate every
// vendor builder can render: two numbers, each with an optional K, M or G
// suffix, separated by one slash ("100M/100M", "512k/256k").
//
// This is the form a speed override and a FUP throttle take — deliberate
// temporary states with no burst. It exists because a rate a builder cannot
// render does not fail the authentication: radiusd sends the Access-Accept
// with no bandwidth attribute at all, so the subscriber is unshaped. An
// override meant to slow somebody down, typed as "fast" or "100Mbps", gave
// them the full line instead, and kept doing so on every login until it was
// cleared.
func ValidatePlainRateLimit(s string) error {
	_, _, err := parseRateLimitString(s)
	return err
}

// parseRateLimitString parses MikroTik-format "first/second" (e.g.
// "50M/50M", "10M/5M") into bits-per-second. Suffixes K/M/G (kilo/mega/giga
// bit per second) are case-insensitive; no suffix means bits/sec.
//
// Named first/second rather than rx/tx or download/upload deliberately:
// MikroTik's own documentation is inconsistent about which side is which
// depending on context (queue simple vs RADIUS reply attribute), and this
// package has no independent way to resolve that ambiguity — callers
// assign the two parsed values to whichever attribute their vendor's
// dictionary calls "input" and "output".
func parseRateLimitString(s string) (first, second uint32, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("rate limit %q: expected \"first/second\" format", s)
	}
	first, err = parseRateToken(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("rate limit %q: %w", s, err)
	}
	second, err = parseRateToken(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("rate limit %q: %w", s, err)
	}
	return first, second, nil
}

func parseRateToken(tok string) (uint32, error) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return 0, fmt.Errorf("empty rate token")
	}

	mult := uint64(1)
	numPart := tok
	switch tok[len(tok)-1] {
	case 'k', 'K':
		mult = 1_000
		numPart = tok[:len(tok)-1]
	case 'm', 'M':
		mult = 1_000_000
		numPart = tok[:len(tok)-1]
	case 'g', 'G':
		mult = 1_000_000_000
		numPart = tok[:len(tok)-1]
	}

	n, err := strconv.ParseUint(numPart, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse rate token %q: %w", tok, err)
	}
	val := n * mult
	if val > math.MaxUint32 {
		return 0, fmt.Errorf("rate token %q overflows a 32-bit bps value", tok)
	}
	return uint32(val), nil //nolint:gosec // bounds-checked above
}
