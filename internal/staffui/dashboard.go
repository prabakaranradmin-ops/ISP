package staffui

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// ConsoleStore backs the home dashboard and the filterable subscriber
// directory. Satisfied by *db.ConsoleStore.
//
// The types it returns are declared here rather than in internal/db because
// internal/db already imports this package (catalogue, demo and staff stores
// implement interfaces declared here), so the reverse import would cycle.
type ConsoleStore interface {
	DashboardCounts(ctx context.Context) (*DashboardCounts, error)
	ListDirectory(ctx context.Context, f DirectoryFilter) (*DirectoryPage, error)
	ListLiveSessions(ctx context.Context, f SessionFilter) ([]LiveSession, error)
	ListCompletedSessions(ctx context.Context, f SessionFilter) ([]CompletedSession, error)
	ListTicketQueue(ctx context.Context, f TicketFilter) ([]QueueTicket, error)
	HelpdeskSummary(ctx context.Context) (*HelpdeskSummary, error)
	// ListTicketAssignees returns active staff whose role can work tickets.
	ListTicketAssignees(ctx context.Context) ([]Assignee, error)
	ListInvoices(ctx context.Context, f MoneyFilter) ([]InvoiceRow, *InvoiceTotals, error)
	ListPayments(ctx context.Context, f MoneyFilter) ([]PaymentRow, *PaymentTotals, error)
}

// SubscriberStatuses is every value subscribers.status may hold (migration
// 048's CHECK), in lifecycle order.
var SubscriberStatuses = []string{
	"pending_payment", "active", "grace_period",
	"soft_suspended", "hard_suspended", "terminated",
}

// DirectoryViews are the dashboard drill-downs the directory accepts.
var DirectoryViews = []string{"expiring", "expired", "new", "churned", "online"}

// directoryViewLabels names each drill-down the way the dashboard tile did,
// so the list the operator lands on is titled by what they clicked.
var directoryViewLabels = map[string]string{
	"expiring": "Expiring in the next 7 days",
	"expired":  "Plan expired, not terminated",
	"new":      "Joined in the last 7 days",
	"churned":  "Terminated in the last 30 days",
	"online":   "Online now",
}

// DashboardCounts is the home dashboard's snapshot.
type DashboardCounts struct {
	Online int
	Total  int
	// ByStatus is keyed by subscribers.status. Every status in
	// SubscriberStatuses is present, zero or not.
	ByStatus map[string]int

	ExpiringWeek int // active, plan_expiry within the next 7 days
	Expired      int // plan_expiry passed, not terminated
	NewWeek      int // created in the last 7 days
	Churned30d   int // moved to terminated in the last 30 days

	OpenTickets       int // open or in progress
	OverdueTickets    int // open or in progress, past the SLA resolution deadline
	UnassignedTickets int // open or in progress, nobody assigned
}

// DirectoryFilter narrows the subscriber directory. The zero value lists
// everybody.
type DirectoryFilter struct {
	// Query matches username, phone or email (substring), id or CAF number
	// (exact), or the assigned IP of a live session (exact). One box, because
	// a CSR does not know in advance which of these a caller will read out.
	Query string
	// Status is one of SubscriberStatuses, or "" for any.
	Status string
	// View is one of DirectoryViews, or "" for none. An unknown value is an
	// error, not "everybody", so a broken tile link is visible as one.
	View string
	// Limit caps the rows returned; <= 0 means the store's default.
	Limit int
}

// DirectoryRow is one subscriber in the directory.
type DirectoryRow struct {
	ID           int
	Username     string
	Status       string
	MobileNumber string
	PlanName     string
	PlanExpiry   *time.Time
	CreatedAt    time.Time
	Online       bool
}

// DirectoryPage is the rows matching a filter, how many matched in all, and
// the unfiltered total, so the screen can say "12 of 340".
type DirectoryPage struct {
	Rows      []DirectoryRow
	Matched   int
	Total     int
	Truncated bool
}

// statusSegment is one coloured run of the dashboard's status bar.
type statusSegment struct {
	Status string
	Count  int
	Pct    float64
}

type dashboardData struct {
	Counts      *DashboardCounts
	Segments    []statusSegment
	ShowTickets bool
	UpdatedAt   time.Time
}

// Dashboard is the console's home screen: who is online, who is about to
// lapse, how the base is growing, and — for roles that work tickets — the
// state of the support queue. Every figure links to the list behind it.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "dashboard")
	if !ok {
		return
	}
	d := h.page(s, "Dashboard", "dashboard")
	if h.console == nil {
		d.Error = "The dashboard is not configured on this deployment."
		h.render(w, "dashboard", d)
		return
	}

	counts, err := h.console.DashboardCounts(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: dashboard counts failed")
		d.Error = "Could not load the dashboard figures."
		h.render(w, "dashboard", d)
		return
	}

	d.Data = dashboardData{
		Counts:      counts,
		Segments:    statusSegments(counts),
		ShowTickets: canAccess("tickets", s.Role, s.LeaAccess),
		UpdatedAt:   time.Now(),
	}
	h.render(w, "dashboard", d)
}

// statusSegments turns the per-status counts into bar widths. Statuses with
// no subscribers are dropped so the bar has no zero-width slivers, and the
// order follows the lifecycle so neighbouring colours mean neighbouring
// states.
func statusSegments(c *DashboardCounts) []statusSegment {
	if c == nil || c.Total == 0 {
		return nil
	}
	out := make([]statusSegment, 0, len(SubscriberStatuses))
	for _, st := range SubscriberStatuses {
		n := c.ByStatus[st]
		if n == 0 {
			continue
		}
		out = append(out, statusSegment{
			Status: st,
			Count:  n,
			Pct:    float64(n) * 100 / float64(c.Total),
		})
	}
	return out
}

// ── Sessions ────────────────────────────────────────────────────────────────

// LiveSession is one subscriber online right now.
type LiveSession struct {
	SubscriberID int
	Username     string
	SessionID    string
	NasIP        string
	AssignedIP   string
	BytesIn      int64
	BytesOut     int64
	SpeedProfile string
	FUPThrottled bool
	StartedAt    time.Time
	UpdatedAt    time.Time
}

// CompletedSession is one ended RADIUS session from the accounting history.
type CompletedSession struct {
	SubscriberID   int
	Username       string
	SessionID      string
	NasIP          string
	AssignedIP     string
	StartTime      time.Time
	StopTime       time.Time
	InputOctets    int64
	OutputOctets   int64
	TerminateCause string
}

// SessionFilter narrows either session list.
type SessionFilter struct {
	// Query matches username (substring), or assigned IP, NAS IP or
	// session id (exact).
	Query string
	// FUPOnly limits the live list to throttled sessions. Ignored for
	// completed sessions.
	FUPOnly bool
	// From/To bound completed sessions by stop time. Ignored for live ones.
	From, To time.Time
	Limit    int
}

// ── Ticket queue (FR-CON-005) ───────────────────────────────────────────────

// TicketViews are the queue's preset filters.
//
//	open        open or in progress (the default)
//	overdue     open or in progress, past the SLA resolution deadline
//	unassigned  open or in progress, nobody assigned
//	mine        open or in progress, assigned to the viewing operator
//	closed      resolved or closed, updated in the last 30 days
//	all         everything, newest first
var TicketViews = []string{"open", "overdue", "unassigned", "mine", "closed", "all"}

// TicketPriorities are the values migration 023's CHECK allows.
var TicketPriorities = []string{"critical", "high", "medium", "low"}

// TicketFilter narrows the queue.
type TicketFilter struct {
	View     string // one of TicketViews; "" means "open"
	Priority string // one of TicketPriorities, or "" for any
	// Query matches a ticket id or subscriber id (exact), or the username or
	// description (substring).
	Query string
	// StaffUsername is the viewing operator, used by the "mine" view. A
	// username rather than an id because that is what the session carries:
	// the console JWT's subject is the username, and Session.StaffID is
	// never populated from it.
	StaffUsername string
	// SubscriberID, when non-zero, limits the queue to one subscriber.
	SubscriberID int
	Limit        int
}

// QueueTicket is one row of the ticket queue.
type QueueTicket struct {
	ID            int
	SubscriberID  int
	Username      string
	Category      string
	Description   string
	Priority      string
	Status        string
	AssignedTo    *int
	AssigneeName  string
	ResponseDue   *time.Time
	ResolutionDue *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// Overdue is computed by the store against its own clock, so the badge
	// and the "overdue" view can never disagree about a ticket.
	Overdue bool
}

// HelpdeskSummary is the figures strip above the queue.
type HelpdeskSummary struct {
	Open       int
	Overdue    int
	Unassigned int
	Closed30d  int
	// MedianCloseHours is nil when nothing was resolved in the window — "no
	// data", which a zero would misreport as "instant".
	MedianCloseHours *float64
}

// Assignee is a staff member a ticket can be assigned to.
type Assignee struct {
	ID       int
	Username string
	FullName string
	Role     string
}

// ── Invoices and payments (FR-CON-006) ──────────────────────────────────────

// MoneyFilter narrows the invoice and payment lists to a half-open
// [From, To) window plus an optional search.
type MoneyFilter struct {
	From, To time.Time
	// Query matches a subscriber's username (substring), a subscriber id or
	// invoice id (exact), an invoice number written INV-000042, or a
	// payment's transaction reference (exact).
	Query string
	Limit int
}

// InvoiceRow is one invoice in the cross-subscriber list. Amounts are
// decimal strings exactly as PostgreSQL rendered the NUMERIC column, so no
// float ever touches money between the database and the page.
type InvoiceRow struct {
	ID           int
	SubscriberID int
	Username     string
	CreatedAt    time.Time
	Base         string
	Tax          string
	Total        string
	Credited     string // sum of credit notes against it; "0.00" when none
}

// InvoiceTotals sums every invoice matching the filter, not just the rows
// shown, so a truncated list still reports the true total.
type InvoiceTotals struct {
	Count    int
	Base     string
	Tax      string
	Total    string
	Credited string
}

// PaymentRow is one wallet top-up: a credit to the subscriber wallet with no
// staff actor. Staff adjustments and approved credits (which carry
// adjusted_by_username) are not payments and are excluded.
type PaymentRow struct {
	ID           int
	SubscriberID int
	Username     string
	CreatedAt    time.Time
	Amount       string
	BalanceAfter string
	// Method is what the recharge was described as ("cash", "razorpay
	// webhook"). It lives in free text today; FR-BIL-013 makes it a column.
	Method    string
	Reference string // the idempotency token: a gateway payment id, or blank
}

// PaymentTotals sums every payment matching the filter.
type PaymentTotals struct {
	Count  int
	Amount string
}
