package billing

import (
	"errors"
	"fmt"
	"time"
)

// Configurable dunning — FR-BIL-017 | migration 055.

// DunningPolicy is the ladder's timing: when each reminder goes out before
// expiry, how long grace lasts after it, and how long soft suspension lasts
// before it hardens. Every figure is in days, measured from plan_expiry.
type DunningPolicy struct {
	RemindFirstDays  int
	RemindSecondDays int
	RemindLastDays   int
	GraceDays        int
	SoftSuspendDays  int
	UpdatedBy        string
	UpdatedAt        time.Time
}

// DefaultDunningPolicy is the ladder this system shipped with, and what the
// scanner falls back to if the settings row cannot be read: the timing that
// was correct yesterday is a safer default than skipping a pass.
func DefaultDunningPolicy() DunningPolicy {
	return DunningPolicy{RemindFirstDays: 7, RemindSecondDays: 3, RemindLastDays: 1, GraceDays: 3, SoftSuspendDays: 3}
}

// ErrPolicyInvalid wraps an operator-facing sentence.
var ErrPolicyInvalid = errors.New("billing: invalid dunning policy")

// Validate mirrors migration 055's CHECK constraints as sentences.
func (p DunningPolicy) Validate() error {
	switch {
	case p.RemindLastDays < 0:
		return fmt.Errorf("%w: reminders cannot be after expiry", ErrPolicyInvalid)
	case p.RemindFirstDays < p.RemindSecondDays || p.RemindSecondDays < p.RemindLastDays:
		return fmt.Errorf("%w: reminders must count down: first, then second, then last", ErrPolicyInvalid)
	case p.RemindFirstDays > 30:
		return fmt.Errorf("%w: the first reminder can be at most 30 days before expiry", ErrPolicyInvalid)
	case p.GraceDays < 0 || p.GraceDays > 30:
		return fmt.Errorf("%w: grace must be between 0 and 30 days", ErrPolicyInvalid)
	case p.SoftSuspendDays < 0 || p.SoftSuspendDays > 60:
		return fmt.Errorf("%w: soft suspension must be between 0 and 60 days", ErrPolicyInvalid)
	}
	return nil
}

// NextDunningStateWith returns the stage a subscriber belongs in under p,
// or the current state if no change is due.
//
// graceUntil is a per-subscriber grace extension (FR-BIL-016): while it is
// in the future, a subscriber past expiry is held in grace — service on —
// however far the ladder would otherwise have taken them, including back
// from a suspension. Once it passes, the ordinary ladder applies again from
// plan_expiry, which may move them straight to a suspension stage.
func NextDunningStateWith(p DunningPolicy, current DunningState, planExpiry, now time.Time, graceUntil *time.Time) DunningState {
	// A future expiry means they have paid. Restore, or stay put if already active.
	if planExpiry.After(now) {
		switch current {
		case DunningGracePeriod, DunningSoftSuspended, DunningHardSuspended:
			return DunningActive
		}
	}

	daysUntilExpiry := planExpiry.Sub(now).Hours() / 24

	if graceUntil != nil && graceUntil.After(now) && daysUntilExpiry <= 0 {
		return DunningGracePeriod
	}

	switch {
	case daysUntilExpiry <= -float64(p.GraceDays+p.SoftSuspendDays):
		return DunningHardSuspended
	case daysUntilExpiry <= -float64(p.GraceDays):
		return DunningSoftSuspended
	case daysUntilExpiry <= 0:
		return DunningGracePeriod
	case daysUntilExpiry <= float64(p.RemindLastDays):
		return DunningRemind1d
	case daysUntilExpiry <= float64(p.RemindSecondDays):
		return DunningRemind3d
	case daysUntilExpiry <= float64(p.RemindFirstDays):
		return DunningRemind7d
	default:
		return DunningActive
	}
}

// Grace extension errors (FR-BIL-016). ErrGraceInvalid wraps an
// operator-facing sentence.
var (
	ErrGraceInvalid       = errors.New("billing: invalid grace extension")
	ErrGraceNotApplicable = errors.New("billing: grace can only be extended for a subscriber past their plan expiry")
)

// GraceInvalidReason returns the sentence inside an ErrGraceInvalid, or "".
func GraceInvalidReason(err error) string {
	if !errors.Is(err, ErrGraceInvalid) {
		return ""
	}
	msg := err.Error()
	prefix := ErrGraceInvalid.Error() + ": "
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}
