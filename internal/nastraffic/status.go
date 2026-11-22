package nastraffic

import (
	"errors"
	"time"
)

// What the console reads: each router's polling state, its interfaces, and a
// bucketed series for one interface.

// RouterStatus is one router's SNMP polling state.
type RouterStatus struct {
	NASID       int
	IP          string
	Vendor      string
	Description string
	Enabled     bool
	// HasCommunity is whether a community is stored. The community itself
	// is never read back out for display.
	HasCommunity bool
	Port         int
	LastPolledAt *time.Time
	LastOKAt     *time.Time
	LastError    string
	Interfaces   int
}

// Healthy reports whether the last poll succeeded.
func (r RouterStatus) Healthy() bool {
	return r.LastError == "" && r.LastOKAt != nil
}

// Stale reports whether the router has gone more than three intervals without
// a successful poll despite being enabled — the state worth noticing, since a
// single failed poll is often a blip.
func (r RouterStatus) Stale(now time.Time) bool {
	if !r.Enabled || r.LastPolledAt == nil {
		return false
	}
	if r.LastOKAt == nil {
		return true
	}
	return now.Sub(*r.LastOKAt) > MaxGap
}

// InterfaceStatus is one interface with its most recent rate.
type InterfaceStatus struct {
	ID         int
	Name       string
	Alias      string
	Index      int
	SpeedMbps  *int64
	OperUp     *bool
	HasHC      bool
	LastSeenAt time.Time
	// The most recent sample, if any.
	LatestAt     *time.Time
	LatestInBps  int64
	LatestOutBps int64
}

// Point is one bucket of a series: the time-weighted average rate in each
// direction, and the highest single sample in the bucket. The peak is kept
// because a 2-hour average on a 30-day graph would flatten a saturated hour
// into a moderate one — exactly the event the graph is there to show.
type Point struct {
	At         time.Time
	InAvgBps   int64
	OutAvgBps  int64
	InPeakBps  int64
	OutPeakBps int64
}

// Range is one of the windows the console offers.
type Range struct {
	Key    string
	Label  string
	Span   time.Duration
	Bucket time.Duration
}

// Ranges are the three windows FR-NET-006 asks for. Buckets are chosen so
// each graph has roughly 300 points: enough to draw a smooth line at the
// width the screen gives it, few enough that the page stays small.
var Ranges = []Range{
	{Key: "24h", Label: "Last 24 hours", Span: 24 * time.Hour, Bucket: 5 * time.Minute},
	{Key: "7d", Label: "Last 7 days", Span: 7 * 24 * time.Hour, Bucket: 30 * time.Minute},
	{Key: "30d", Label: "Last 30 days", Span: 30 * 24 * time.Hour, Bucket: 2 * time.Hour},
}

// LookupRange returns the range for key, defaulting to the first.
func LookupRange(key string) Range {
	for _, r := range Ranges {
		if r.Key == key {
			return r
		}
	}
	return Ranges[0]
}

// ErrSNMPNeedsCommunity is returned when polling is enabled on a router that
// has no community stored and none was given.
var ErrSNMPNeedsCommunity = errors.New("enter the router's SNMP community to turn on traffic monitoring")
