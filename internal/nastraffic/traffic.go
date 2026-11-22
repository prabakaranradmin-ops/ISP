// Package nastraffic polls NAS interface counters over SNMP and turns them
// into a traffic history the console graphs — FR-NET-006 | migration 073.
//
// The parts:
//
//   - Rate (this file) decides what a pair of counter readings means. Every
//     judgement about a bad reading is made here, once, before anything is
//     written: a counter that went backwards, a gap too long to average
//     across, a rate the link could not carry.
//   - SNMPClient (snmp.go) reads the counters from a router.
//   - Poller (poller.go) runs on a ticker, polls every enabled router with
//     bounded concurrency, and records what it found.
//
// A separate package from internal/nas, which builds RADIUS attributes and has
// no business carrying an SNMP stack and a network dependency with it.
package nastraffic

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// PollInterval is the nominal time between polls. Five minutes is the
	// long-standing convention for interface graphs (MRTG's default): fine
	// enough to see a busy hour, coarse enough that thirty days is 8,640 rows
	// per interface rather than 43,200.
	PollInterval = 5 * time.Minute

	// MaxGap is the longest interval a rate is averaged across. Beyond it the
	// poller was down, or the router was unreachable, and an average over the
	// whole gap would draw a calm flat line through what may have been an
	// outage. The reading becomes a new baseline instead and the graph shows
	// a hole — which is what actually happened: nothing was measured.
	MaxGap = 3 * PollInterval

	// MinInterval guards against dividing by a near-zero interval when two
	// polls land almost together (a restart straight after a cycle), which
	// would turn a few bytes of jitter into an enormous rate.
	MinInterval = 10 * time.Second

	// Retention is how long samples are kept. The longest range the console
	// offers is 30 days; a day's margin means the oldest bucket of a 30-day
	// graph is never half-purged.
	Retention = 31 * 24 * time.Hour

	// MaxInterfacesPerNAS caps what one router can add to the table. A
	// misclassified router — one whose per-session interfaces are not
	// reported as PPP — would otherwise add a row per subscriber session.
	MaxInterfacesPerNAS = 256

	// implausibleFactor: a rate above this multiple of the link speed is not
	// recorded. Real traffic cannot exceed the link; a reading that does is a
	// counter that jumped (some devices reset a counter without it ever
	// reading lower, e.g. a module hot-swap) and would otherwise become a
	// spike that sets the graph's scale and flattens a month of real data.
	// 1.5 rather than 1.0 because ifHighSpeed is rounded to whole Mbit/s and
	// a bonded or bursting interface can briefly report above its nominal.
	implausibleFactor = 1.5

	maxNameRunes = 255
)

// IANA ifType values the poller skips.
const (
	ifTypePPP              = 23  // every PPPoE/L2TP/PPTP session on a BRAS
	ifTypeSoftwareLoopback = 24  // never carries subscriber traffic
	ifTypeTunnel           = 131 // per-session tunnels on some vendors
)

// Eligible reports whether an interface of this IANA ifType is worth graphing.
//
// PPP is the one that matters. On a MikroTik BRAS every subscriber's PPPoE
// session is its own interface in IF-MIB, appearing and disappearing as
// people connect, so a router with 3,000 subscribers reports 3,000 extra
// interfaces with a fresh ifIndex each time. Per-subscriber traffic is already
// recorded from RADIUS accounting; what an operator needs from this is the
// uplinks and the access ports.
func Eligible(ifType int) bool {
	switch ifType {
	case ifTypePPP, ifTypeSoftwareLoopback, ifTypeTunnel:
		return false
	}
	return true
}

// Interface is one interface as a router reported it in a single poll.
type Interface struct {
	Index int
	// Name is ifName, falling back to ifDescr and then to "ifIndex N", since
	// some agents leave ifName empty. It is the history key: see migration
	// 073 on why not the index.
	Name      string
	Alias     string
	Type      int
	SpeedMbps *int64
	OperUp    *bool
	// HasHC is false when the device did not report ifHCInOctets and
	// ifHCOutOctets. Such an interface is listed but never graphed.
	HasHC     bool
	InOctets  uint64
	OutOctets uint64
}

// Previous is the reading a rate is measured against.
type Previous struct {
	InOctets, OutOctets uint64
	At                  time.Time
}

// Outcome says what a pair of readings amounts to.
type Outcome int

const (
	// OutcomeSample is a rate worth recording.
	OutcomeSample Outcome = iota
	// OutcomeBaseline is a first reading: there is nothing to measure
	// against yet, so it is stored as the baseline for the next poll.
	OutcomeBaseline
	// OutcomeReset is a counter that went backwards. A 64-bit octet counter
	// takes 46 years to wrap at 100 Gbit/s, so this is a reboot or a counter
	// clear, never a wrap — and the delta across it means nothing.
	OutcomeReset
	// OutcomeGap is an interval longer than MaxGap.
	OutcomeGap
	// OutcomeTooSoon is an interval shorter than MinInterval. The previous
	// baseline is kept, so the next poll measures across both.
	OutcomeTooSoon
	// OutcomeImplausible is a rate the link could not have carried.
	OutcomeImplausible
	// OutcomeNoCounters is an interface without 64-bit counters.
	OutcomeNoCounters
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSample:
		return "sample"
	case OutcomeBaseline:
		return "baseline"
	case OutcomeReset:
		return "reset"
	case OutcomeGap:
		return "gap"
	case OutcomeTooSoon:
		return "too-soon"
	case OutcomeImplausible:
		return "implausible"
	case OutcomeNoCounters:
		return "no-counters"
	}
	return "unknown"
}

// Sample is a recorded rate.
type Sample struct {
	At              time.Time
	IntervalSeconds int
	InBps, OutBps   int64
}

// Rate turns the previous reading and the current one into a Sample, or
// explains why there is none.
//
// The current reading becomes the new baseline for every outcome except
// OutcomeTooSoon and OutcomeNoCounters — see AdvancesBaseline.
func Rate(prev *Previous, cur Interface, at time.Time) (Sample, Outcome) {
	if !cur.HasHC {
		return Sample{}, OutcomeNoCounters
	}
	if prev == nil {
		return Sample{}, OutcomeBaseline
	}
	elapsed := at.Sub(prev.At)
	switch {
	case elapsed < MinInterval:
		return Sample{}, OutcomeTooSoon
	case elapsed > MaxGap:
		return Sample{}, OutcomeGap
	case cur.InOctets < prev.InOctets || cur.OutOctets < prev.OutOctets:
		return Sample{}, OutcomeReset
	}

	secs := elapsed.Seconds()
	inBps := bitsPerSecond(cur.InOctets-prev.InOctets, secs)
	outBps := bitsPerSecond(cur.OutOctets-prev.OutOctets, secs)

	if cur.SpeedMbps != nil && *cur.SpeedMbps > 0 {
		ceiling := float64(*cur.SpeedMbps) * 1e6 * implausibleFactor
		if float64(inBps) > ceiling || float64(outBps) > ceiling {
			return Sample{}, OutcomeImplausible
		}
	}
	return Sample{
		At:              at,
		IntervalSeconds: int(elapsed.Round(time.Second) / time.Second),
		InBps:           inBps,
		OutBps:          outBps,
	}, OutcomeSample
}

// AdvancesBaseline reports whether the current reading should replace the
// stored one. A too-soon reading keeps the old baseline, so the next poll
// measures across both intervals rather than losing one; a reading without
// counters has nothing to store.
func (o Outcome) AdvancesBaseline() bool {
	return o != OutcomeTooSoon && o != OutcomeNoCounters
}

// bitsPerSecond converts an octet delta over secs to bit/s, saturating rather
// than overflowing int64 — the column is BIGINT, and a delta that large is
// refused by the implausibility check on any interface that reports a speed.
func bitsPerSecond(octets uint64, secs float64) int64 {
	bps := float64(octets) * 8 / secs
	if bps >= 9.2e18 {
		return 9_200_000_000_000_000_000
	}
	return int64(bps + 0.5)
}

// CleanName makes device-reported text safe to store and show: control
// characters removed, whitespace trimmed, cut to the column's length in
// characters. Interface names and aliases come from the router, which is
// configured by whoever has access to it — not necessarily anyone with
// access to this console.
func CleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxNameRunes {
		s = string([]rune(s)[:maxNameRunes])
	}
	return s
}
