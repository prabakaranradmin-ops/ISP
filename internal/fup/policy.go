package fup

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Multi-tier FUP policies and purchasable data top-ups — FR-CAT-003.
//
// A plan had one threshold and one throttle: cross 500GB and drop to
// 10M/10M for the rest of the cycle. Real tariffs are rarely that flat —
// the common Indian retail shape is a gentle first step and a harder one
// for the heaviest users, so that someone 10% over their quota is not
// treated the same as someone at triple it.
//
// Policies are reusable for the same reason bandwidth profiles are
// (FR-CAT-002): an operator runs a handful of fair-use shapes across many
// tariffs that differ only in price, quota and validity, and editing the
// policy then re-shapes every plan on it.

// Tier is one step of a policy: cross ThresholdBytes and the throttle
// becomes ThrottleRate.
type Tier struct {
	ID       int
	PolicyID int
	// ThresholdBytes is cumulative usage in the cycle, not an increment
	// above the previous tier. Cumulative because that is what the scanner
	// compares against and what an operator reads off a tariff sheet
	// ("throttle at 500GB, harder at 1TB"); increments would have to be
	// summed before either could happen.
	ThresholdBytes int64
	// ThrottleRate is a MikroTik-format "down/up" rate, the same form
	// plans.fup_throttle_string holds and the same form the CoA path
	// already sends. Deliberately not a bandwidth profile (FR-CAT-002): a
	// throttle is a flat ceiling, and giving a throttled subscriber a burst
	// would hand back the speed the throttle exists to remove.
	ThrottleRate string
}

// Policy is a named set of tiers.
type Policy struct {
	ID        int
	Name      string
	Tiers     []Tier
	Active    bool
	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt *time.Time

	// InUseByPlans is how many plans reference this policy. Read-only,
	// populated by listings so the console can say what editing it would
	// re-shape and refuse a delete rather than let the foreign key surface
	// as a 500.
	InUseByPlans int
}

// Normalise sorts the tiers by threshold and trims the text, so what
// Validate checks is what gets stored and TierFor can rely on the order.
func (p Policy) Normalise() Policy {
	p.Name = strings.TrimSpace(p.Name)
	tiers := make([]Tier, len(p.Tiers))
	copy(tiers, p.Tiers)
	for i := range tiers {
		tiers[i].ThrottleRate = strings.TrimSpace(tiers[i].ThrottleRate)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].ThresholdBytes < tiers[j].ThresholdBytes })
	p.Tiers = tiers
	return p
}

// Validate restates the table's constraints so the console can refuse a bad
// policy with a sentence instead of letting a constraint violation surface
// as a 500. The database remains the authority.
func (p Policy) Validate() error {
	switch {
	case p.Name == "":
		return fmt.Errorf("a fair-use policy needs a name")
	case len(p.Name) > 100:
		return fmt.Errorf("a policy name may be at most 100 characters")
	case len(p.Tiers) == 0:
		return fmt.Errorf("a fair-use policy needs at least one tier, or it throttles nobody")
	case len(p.Tiers) > MaxTiers:
		return fmt.Errorf("a policy may have at most %d tiers", MaxTiers)
	}

	for i, t := range p.Tiers {
		if t.ThresholdBytes <= 0 {
			return fmt.Errorf("tier %d needs a threshold above zero: a zero threshold throttles "+
				"every subscriber from their first byte", i+1)
		}
		if t.ThrottleRate == "" {
			return fmt.Errorf("tier %d needs a throttle rate", i+1)
		}
		if err := validateRate(t.ThrottleRate); err != nil {
			return fmt.Errorf("tier %d: %w", i+1, err)
		}
		// Normalise sorts, so a duplicate shows up as an equal neighbour.
		// Two tiers at one threshold is ambiguous: both apply, and which
		// throttle a subscriber gets would depend on row order.
		if i > 0 && t.ThresholdBytes == p.Tiers[i-1].ThresholdBytes {
			return fmt.Errorf("two tiers share the threshold %d bytes; which throttle applies "+
				"would depend on the order the rows came back in", t.ThresholdBytes)
		}
	}
	return nil
}

// MaxTiers is a sanity bound, not a technical limit. A policy with dozens of
// steps is not a fair-use rule anyone can explain to a customer, and the
// scanner compares every tier on each pass. Exported so the console can
// offer exactly that many rows rather than hardcoding its own number.
const MaxTiers = 8

// validateRate checks a "down/up" throttle rate.
//
// Deliberately a shape check and not a full parse: the authority on what a
// rate string means is internal/nas, and importing it here would couple the
// FUP scanner to the vendor attribute builders. What this catches is the
// mistake an operator actually makes — a single number with no slash, or a
// unit the dialect does not use.
func validateRate(rate string) error {
	parts := strings.Split(rate, "/")
	if len(parts) != 2 {
		return fmt.Errorf("a throttle rate is written \"down/up\", like \"10M/10M\"; %q is not", rate)
	}
	for _, half := range parts {
		half = strings.TrimSpace(half)
		if half == "" {
			return fmt.Errorf("a throttle rate is written \"down/up\", like \"10M/10M\"; %q is not", rate)
		}
		last := half[len(half)-1]
		switch last {
		case 'k', 'K', 'M', 'G', 'm', 'g':
			if len(half) == 1 {
				return fmt.Errorf("%q has a unit but no number", half)
			}
			half = half[:len(half)-1]
		}
		for _, r := range half {
			if r < '0' || r > '9' {
				return fmt.Errorf("%q is not a rate: expected digits and an optional k, M or G",
					strings.TrimSpace(rate))
			}
		}
	}
	return nil
}

// TierFor returns the tier that applies at bytesUsed, or nil when the
// subscriber is under every threshold and should run at plan speed.
//
// The highest crossed tier wins, not the first: a subscriber at triple
// their quota has crossed every threshold, and giving them the gentlest
// throttle would make the harder tiers unreachable.
//
// Tiers must be sorted ascending, which Normalise guarantees and the store
// guarantees by reading them ORDER BY threshold_bytes.
func (p Policy) TierFor(bytesUsed int64) *Tier {
	var applied *Tier
	for i := range p.Tiers {
		if bytesUsed >= p.Tiers[i].ThresholdBytes {
			applied = &p.Tiers[i]
			continue
		}
		// Sorted, so the first threshold not reached ends the search.
		break
	}
	return applied
}

// FirstThreshold is the usage at which this policy starts throttling, used
// for the 80% warning (FR-FUP-004) and for the portal's usage bar.
//
// The first tier, not the last: the subscriber's allowance is the point at
// which their speed first changes, and warning them at 80% of the hardest
// tier would arrive long after they had already been throttled.
func (p Policy) FirstThreshold() int64 {
	if len(p.Tiers) == 0 {
		return 0
	}
	return p.Tiers[0].ThresholdBytes
}

// Topup is a purchased data allowance that raises a subscriber's thresholds
// for the rest of the billing cycle — FR-CAT-003.
//
// Modelled as added allowance rather than as "clear the throttle", which is
// the tempting shortcut. Clearing a flag would un-throttle a subscriber who
// then carries on using data and never throttles again that cycle, however
// much they use: the tiers above them would be unreachable because nothing
// moved the thresholds. Raising the thresholds keeps every tier live, so a
// subscriber who buys 50GB and then uses 50GB more is throttled again,
// which is what both the operator and the customer expect of a top-up.
type Topup struct {
	ID           int
	SubscriberID int
	Bytes        int64
	// ExpiresAt is the end of the cycle the top-up was bought in. A top-up
	// does not roll over: it lifts the throttle "for the rest of the
	// cycle", and carrying it forward would let somebody bank allowance
	// against a plan they may no longer be on.
	ExpiresAt time.Time
	// TransactionID links the top-up to the wallet debit that paid for it,
	// so a subscriber disputing a charge can be shown what it bought and a
	// top-up can never exist without a payment behind it.
	TransactionID *int
	PurchasedBy   string
	PurchasedAt   time.Time
}

// Allowance is a subscriber's effective thresholds after top-ups.
type Allowance struct {
	// Policy is the plan's tiers, or a single synthetic tier built from the
	// plan's own fup_threshold_bytes and fup_throttle_string when the plan
	// has no policy — so a plan that predates policies behaves exactly as
	// it did, and that fallback is a supported steady state rather than a
	// migration window.
	Policy Policy
	// ToppedUpBytes is the sum of the subscriber's unexpired top-ups.
	ToppedUpBytes int64
}

// TierFor returns the throttle that applies at bytesUsed, accounting for
// top-ups by shifting every threshold up rather than by exempting the
// subscriber — see Topup's doc comment for why that distinction matters.
func (a Allowance) TierFor(bytesUsed int64) *Tier {
	if a.ToppedUpBytes <= 0 {
		return a.Policy.TierFor(bytesUsed)
	}
	// Equivalent to raising every threshold, done by lowering the usage
	// compared against them. A top-up larger than the usage leaves nothing
	// crossed, which is the un-throttled case.
	effective := bytesUsed - a.ToppedUpBytes
	if effective < 0 {
		effective = 0
	}
	return a.Policy.TierFor(effective)
}

// PolicyFromPlanColumns builds a one-tier policy from the legacy per-plan
// columns, so every caller can work in tiers whether or not the plan has a
// policy row.
//
// Returns a policy with no tiers when the plan sets no threshold, which is
// an unlimited plan: TierFor then returns nil and nobody is throttled.
func PolicyFromPlanColumns(thresholdBytes int64, throttleRate string) Policy {
	if thresholdBytes <= 0 || strings.TrimSpace(throttleRate) == "" {
		return Policy{}
	}
	return Policy{
		Name: "plan default",
		Tiers: []Tier{{
			ThresholdBytes: thresholdBytes,
			ThrottleRate:   strings.TrimSpace(throttleRate),
		}},
	}
}

// ErrPolicyInUse is returned when deleting a policy is refused because plans
// still reference it. Retiring is the supported path.
//
// This and ErrPolicyNameTaken live here rather than in internal/db because
// the console has to tell them apart from a generic failure and cannot
// import internal/db — internal/db imports internal/staffui.
var ErrPolicyInUse = errors.New("fup: fair-use policy is still used by plans")

// ErrPolicyNameTaken is returned when another active policy holds the name.
var ErrPolicyNameTaken = errors.New("fup: another active fair-use policy already has that name")

// ThrottleChange is one subscriber whose applied throttle is not the one
// their usage now calls for — FR-CAT-003.
//
// Carries both the applied and the target rate because the scanner logs the
// transition, and "throttled" on its own does not say whether a subscriber
// was stepped down a tier, eased back after a top-up, or released.
type ThrottleChange struct {
	SubscriberID int
	Username     string
	NasIP        string

	BytesUsed     int64
	ToppedUpBytes int64

	// AppliedRate is what the subscriber is on now, empty for plan speed.
	AppliedRate string
	// TargetRate is what they should be on, empty for plan speed.
	TargetRate string
	// TargetThreshold is the tier's threshold, for the notification that
	// tells the subscriber which limit they crossed.
	TargetThreshold int64
}

// IsRelease reports whether this change lifts a throttle rather than
// applying or tightening one. The two are different events to a subscriber
// and are notified differently (FR-NOTIF-003 against FR-NOTIF-006).
func (c ThrottleChange) IsRelease() bool { return c.TargetRate == "" }

// IsTightening reports whether the subscriber is moving to a harder tier
// from an existing throttle, rather than being throttled for the first time.
func (c ThrottleChange) IsTightening() bool {
	return c.AppliedRate != "" && c.TargetRate != ""
}
