package revenue

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Named subscriber groups — FR-GRP-001 | migration 069.
//
// Beside Franchise in this package because the two are the same kind of
// thing: a named set of subscribers used to scope a list, a report or a
// message. A franchise is one the business structure imposes; a group is one
// an operator names themselves — a locality, a building, a campaign cohort.

// SubscriberGroup is a named set of subscribers.
type SubscriberGroup struct {
	ID          int
	Name        string
	Description string
	Active      bool

	// Members is how many subscribers are in the group, excluding
	// terminated ones. Read-only, populated by listings: a group's size is
	// what an operator checks before sending to it, and counting the
	// members who have left would overstate the reach of every message.
	Members int

	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt *time.Time
}

// ErrGroupNameTaken is returned when another live group holds the name.
var ErrGroupNameTaken = errors.New("revenue: another active subscriber group already has that name")

// ErrGroupInUse is returned when deleting a group is refused because an
// announcement still targets it.
//
// segment_group_id is ON DELETE RESTRICT precisely so this cannot happen
// quietly: a NULL segment means "no restriction on this dimension", so had it
// been SET NULL like the other three segment columns, deleting a targeted
// group would have turned a message sent to twenty people into one appearing
// in every subscriber's portal. The store counts first only so the operator
// gets this sentence instead of a constraint name; the constraint refuses it
// either way. Retiring is the supported path and changes nothing about who
// the announcement reaches.
var ErrGroupInUse = errors.New("revenue: an announcement still targets this group")

// Normalise tidies the text so what Validate checks is what gets stored.
//
// Runs of whitespace inside the name collapse to one space, not only the
// surrounding space. The unique index is on lower(btrim(name)), which strips
// the ends and nothing else, so without this "Anna  Nagar" and "Anna Nagar"
// are different names to the database and identical to every operator who
// reads them — two live groups one typo apart, which is the mistake the index
// exists to prevent.
func (g SubscriberGroup) Normalise() SubscriberGroup {
	g.Name = strings.Join(strings.Fields(g.Name), " ")
	g.Description = strings.TrimSpace(g.Description)
	return g
}

// Validate restates the table's constraints, so the console can refuse a bad
// group with a sentence instead of a constraint name.
func (g SubscriberGroup) Validate() error {
	switch {
	case g.Name == "":
		return fmt.Errorf("a group needs a name")
	case utf8.RuneCountInString(g.Name) > 100:
		// Characters, not bytes: the column is VARCHAR(100), which counts
		// characters. A byte check would refuse a 40-character Tamil or Hindi
		// group name that the database accepts without complaint — and the
		// localities an operator names groups after are exactly the text most
		// likely not to be ASCII.
		return fmt.Errorf("a group name may be at most 100 characters")
	}
	return nil
}
