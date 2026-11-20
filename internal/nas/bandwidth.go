package nas

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Shaping is a resolved bandwidth profile — FR-CAT-002.
//
// Rates are bits per second throughout. The database stores them that way
// (migration 065) rather than as MikroTik's "50M" text, because the text is
// one vendor's dialect: Huawei and ZTE want a 32-bit integer in a VSA and
// Cisco wants the name of a policy the NAS already holds. Keeping the
// numbers numeric also keeps a string parse off the authentication hot path,
// which NFR-PERF-001 holds to a 15ms p99 at 5,000 requests a second.
type Shaping struct {
	Name string // operator-facing, for logs and errors only

	MaxDownBPS int64
	MaxUpBPS   int64

	// The committed floor. Zero means no guarantee, which is the honest
	// default for a plan sold without one.
	MinDownBPS int64
	MinUpBPS   int64

	// Burst. These four are one feature: a burst rate with no threshold is
	// just a higher ceiling, and a threshold with no time never ends. The
	// database refuses a partial set (chk_bw_burst_complete_down/up), and
	// HasBurst below is the only thing callers should branch on.
	BurstDownBPS       int64
	BurstUpBPS         int64
	BurstThresholdDown int64
	BurstThresholdUp   int64
	BurstTimeSeconds   int

	// MikroTik queue priority, 1 (served first) to 8 (last).
	Priority int
}

// HasBurst reports whether this profile bursts in either direction.
func (s Shaping) HasBurst() bool {
	return s.BurstDownBPS > 0 || s.BurstUpBPS > 0
}

// HasFloor reports whether this profile commits a minimum rate.
func (s Shaping) HasFloor() bool {
	return s.MinDownBPS > 0 || s.MinUpBPS > 0
}

// MikrotikRateLimit renders the full MikroTik Rate-Limit form:
//
//	rx-rate/tx-rate rx-burst-rate/tx-burst-rate \
//	  rx-burst-threshold/tx-burst-threshold rx-burst-time/tx-burst-time \
//	  priority rx-rate-min/tx-rate-min
//
// Trailing fields are omitted when they carry no information, so a profile
// with no burst and no floor renders exactly the "50M/50M" that
// plans.rate_limit_string holds today. That is not cosmetic: it means
// adopting a plain profile cannot change the attribute a NAS already
// receives, so the change is observable only where an operator asked for
// burst or a floor.
//
// The fields are positional, so a gap cannot be skipped — asking for a
// floor with no burst still has to emit the burst fields, and MikroTik's own
// documented way to say "no burst" in that position is zeros.
func (s Shaping) MikrotikRateLimit() (string, error) {
	if s.MaxDownBPS <= 0 || s.MaxUpBPS <= 0 {
		return "", fmt.Errorf("nas: bandwidth profile %q: max rate must be positive (down=%d up=%d)",
			s.Name, s.MaxDownBPS, s.MaxUpBPS)
	}

	var b strings.Builder
	b.WriteString(rateToken(s.MaxDownBPS))
	b.WriteByte('/')
	b.WriteString(rateToken(s.MaxUpBPS))

	// Priority and the floor both sit after the burst fields, so any of the
	// three forces the burst group to be emitted.
	needTail := s.HasFloor() || (s.Priority != 0 && s.Priority != 8)
	if !s.HasBurst() && !needTail {
		return b.String(), nil
	}

	b.WriteByte(' ')
	b.WriteString(rateToken(s.BurstDownBPS) + "/" + rateToken(s.BurstUpBPS))
	b.WriteByte(' ')
	b.WriteString(rateToken(s.BurstThresholdDown) + "/" + rateToken(s.BurstThresholdUp))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(s.BurstTimeSeconds) + "/" + strconv.Itoa(s.BurstTimeSeconds))

	if !needTail {
		return b.String(), nil
	}

	prio := s.Priority
	if prio == 0 {
		prio = 8 // the column's default; 0 is not a value MikroTik accepts
	}
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(prio))

	if s.HasFloor() {
		b.WriteByte(' ')
		b.WriteString(rateToken(s.MinDownBPS) + "/" + rateToken(s.MinUpBPS))
	}
	return b.String(), nil
}

// rateToken renders bits per second the way MikroTik writes it, using the
// largest suffix that divides exactly. Inexact values stay as raw bits
// rather than being rounded: a profile an operator entered as 1,500,000 bps
// must not silently become "1M".
func rateToken(bps int64) string {
	switch {
	case bps == 0:
		return "0"
	case bps%1_000_000_000 == 0:
		return strconv.FormatInt(bps/1_000_000_000, 10) + "G"
	case bps%1_000_000 == 0:
		return strconv.FormatInt(bps/1_000_000, 10) + "M"
	case bps%1_000 == 0:
		return strconv.FormatInt(bps/1_000, 10) + "k"
	default:
		return strconv.FormatInt(bps, 10)
	}
}

// ErrProfileInUse is returned when deleting a profile is refused because
// plans still reference it. Retiring is the supported path.
//
// This and ErrProfileNameTaken live here rather than in internal/db because
// the console has to tell these apart from a generic failure, and it cannot
// import internal/db — internal/db imports internal/staffui.
var ErrProfileInUse = errors.New("nas: bandwidth profile is still used by plans")

// ErrProfileNameTaken is returned when another active profile holds the name.
var ErrProfileNameTaken = errors.New("nas: another active bandwidth profile already has that name")

// BandwidthProfile is a stored profile: the shaping values plus the identity
// and attribution the console needs. Shaping alone is what the RADIUS path
// carries, so the hot path never holds the audit fields.
type BandwidthProfile struct {
	ID int
	Shaping
	Active    bool
	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt *time.Time

	// InUseByPlans is how many plans reference this profile. Read-only,
	// populated by listings so the console can say what retiring it would
	// re-shape — and refuse a delete rather than let the ON DELETE RESTRICT
	// surface as a 500.
	InUseByPlans int
}

// Normalise trims the name and settles the defaults the database would
// otherwise apply, so that what Validate checks is what gets stored.
func (p BandwidthProfile) Normalise() BandwidthProfile {
	p.Name = strings.TrimSpace(p.Name)
	if p.Priority == 0 {
		p.Priority = 8
	}
	// Burst is all-or-nothing per direction. A rate with no threshold would
	// burst forever and a threshold with no rate does nothing, so an
	// incomplete pair is cleared rather than stored half-set — the database
	// refuses it either way (chk_bw_burst_complete_down/up), and clearing
	// here means the console shows the operator what will actually happen.
	if p.BurstDownBPS == 0 || p.BurstThresholdDown == 0 {
		p.BurstDownBPS, p.BurstThresholdDown = 0, 0
	}
	if p.BurstUpBPS == 0 || p.BurstThresholdUp == 0 {
		p.BurstUpBPS, p.BurstThresholdUp = 0, 0
	}
	if !p.HasBurst() {
		p.BurstTimeSeconds = 0
	}
	return p
}

// Validate restates the table's CHECK constraints in Go so the console can
// refuse a bad profile with a sentence instead of letting a constraint
// violation surface as a 500. The database remains the authority — these are
// the same rules, not a replacement for them.
func (p BandwidthProfile) Validate() error {
	switch {
	case p.Name == "":
		return fmt.Errorf("a bandwidth profile needs a name")
	case len(p.Name) > 100:
		return fmt.Errorf("a profile name may be at most 100 characters")
	case p.MaxDownBPS <= 0 || p.MaxUpBPS <= 0:
		return fmt.Errorf("both maximum rates must be above zero: " +
			"a zero rate means unlimited to a MikroTik NAS, which is the opposite of what it looks like")
	case p.MinDownBPS > p.MaxDownBPS || p.MinUpBPS > p.MaxUpBPS:
		return fmt.Errorf("the committed minimum cannot exceed the maximum")
	case p.Priority < 1 || p.Priority > 8:
		return fmt.Errorf("queue priority must be between 1 (served first) and 8 (last), not %d", p.Priority)
	}

	if p.BurstDownBPS > 0 {
		switch {
		case p.BurstDownBPS < p.MaxDownBPS:
			return fmt.Errorf("the download burst rate must be at least the maximum rate, or it is not a burst")
		case p.BurstThresholdDown > p.MaxDownBPS:
			return fmt.Errorf("the download burst threshold is above the maximum rate, " +
				"so it could never be crossed and the burst would never engage")
		}
	}
	if p.BurstUpBPS > 0 {
		switch {
		case p.BurstUpBPS < p.MaxUpBPS:
			return fmt.Errorf("the upload burst rate must be at least the maximum rate, or it is not a burst")
		case p.BurstThresholdUp > p.MaxUpBPS:
			return fmt.Errorf("the upload burst threshold is above the maximum rate, " +
				"so it could never be crossed and the burst would never engage")
		}
	}
	if p.HasBurst() && p.BurstTimeSeconds <= 0 {
		return fmt.Errorf("a burst needs a duration in seconds, or it would never end")
	}

	// The rendering is the point of the record, so a profile that cannot be
	// rendered must not be storable.
	if _, err := p.MikrotikRateLimit(); err != nil {
		return err
	}
	return nil
}

// PlainRateLimit renders only the ceiling, as "down/up".
//
// This is what every non-MikroTik builder must be given. The reason is a
// trap worth naming: parseRateLimitString, which the Huawei and ZTE builders
// use to turn the string into their integer VSAs, splits on the first "/"
// and parses both halves as a single rate token. Hand it the full burst form
// and the second half is "50M 100M/100M 40M/40M 8 10M/10M", which fails to
// parse — so a plan that merely gained a burst would stop authenticating on
// every Huawei and ZTE NAS in the estate. Burst is a MikroTik queue
// behaviour; the other dialects either express it NAS-side in a named policy
// (Cisco, Juniper, the wireless controllers) or not at all.
func (s Shaping) PlainRateLimit() string {
	return rateToken(s.MaxDownBPS) + "/" + rateToken(s.MaxUpBPS)
}
