package reporting

import "strings"

// The report catalogue — FR-RPT-004.
//
// One table describing every report: its name on the wire, its heading, the
// area it belongs to, what it answers, and which filters apply to it. The
// console's grouping and filter box, the API's validation, the CSV dispatch
// and the scheduled exporter all read this rather than each keeping a list.
//
// That is the point, not tidiness. Adding the roster report for FR-GRP-001
// meant touching five separate places — a const, Reports(), the WriteCSV
// switch, a JSON switch that silently had no default and would have served
// {"rows": null} with a 200, and the console's tab bar. Four were found by
// tests and one by a browser. A single registry plus
// TestFR_RPT_004_EveryCatalogueEntryIsServedByEveryPath makes that class of
// omission fail at `go test` instead.

// Areas group the reports on the catalogue screen. Ordered as an operator
// would look for them rather than alphabetically: who the customers are,
// then what they owe, then what went wrong, then the network underneath.
const (
	AreaSubscribers = "Subscribers"
	AreaMoney       = "Money"
	AreaSupport     = "Support"
	AreaNetwork     = "Network"
)

// Window describes which time filter a report takes.
type Window int

const (
	// WindowNone is a point-in-time snapshot. Offering a month window would
	// imply a history the report does not have.
	WindowNone Window = iota
	// WindowMonths is a trailing window of whole months.
	WindowMonths
	// WindowDays is a forward-looking window in days — only the expiring
	// report, where "the next 7 days" is the question and "the last 12
	// months" is meaningless.
	WindowDays
)

// ReportDef describes one report.
type ReportDef struct {
	// Name is the string in URLs and stored export filenames: kebab-case,
	// never the raw view name.
	Name  string
	Title string
	Area  string
	// Summary says what question the report answers. Shown under the title
	// and searched by the filter box, so it carries the words an operator
	// would actually type — "expiry", "renewal", "GST" — rather than
	// restating the title.
	Summary string
	Window  Window
	// Group is true when a subscriber group narrows the report. Only reports
	// with one row per subscriber can honour it; see
	// ErrGroupFilterUnsupported.
	Group bool
}

// catalogue is the single list. Order within an area is the order shown.
var catalogue = []ReportDef{
	{
		Name: ReportPlanMix, Title: "Plan mix", Area: AreaSubscribers,
		Summary: "How many subscribers are on each tariff, and the monthly revenue each plan carries.",
		Window:  WindowNone,
	},
	{
		Name: ReportGrowth, Title: "Growth & churn", Area: AreaSubscribers,
		Summary: "New connections, reactivations, churn and suspensions by month.",
		Window:  WindowMonths,
	},
	{
		Name: ReportSubscribers, Title: "Subscribers", Area: AreaSubscribers,
		Summary: "A roster of the base, filterable by named group — the list to work from for a locality or a cohort.",
		Window:  WindowNone, Group: true,
	},
	{
		Name: ReportExpiring, Title: "Expiring subscribers", Area: AreaSubscribers,
		Summary: "Whose plan expires in the next few days, and what they owe to renew. The renewal call list.",
		Window:  WindowDays,
	},
	{
		Name: ReportPlanChanges, Title: "Plan change history", Area: AreaSubscribers,
		Summary: "Scheduled upgrades and downgrades: what was asked for, what fired at renewal, what was withdrawn.",
		Window:  WindowMonths,
	},
	{
		Name: ReportCollection, Title: "Collections by franchise", Area: AreaMoney,
		Summary: "Billed against collected per franchise per month, with commission and collection rate.",
		Window:  WindowMonths,
	},
	{
		Name: ReportPaymentsByMethod, Title: "Payments by method", Area: AreaMoney,
		Summary: "Cash, UPI, cheque, gateway and wallet adjustments by month — what arrived and how.",
		Window:  WindowMonths,
	},
	{
		Name: ReportCollectionByStaff, Title: "Collection by staff", Area: AreaMoney,
		Summary: "Who took which payments, separating cash that must be handed in from UPI already at the bank.",
		Window:  WindowMonths,
	},
	{
		Name: ReportInvoiceRegister, Title: "Invoice register", Area: AreaMoney,
		Summary: "Every invoice raised with its number, date, subscriber, taxable value, tax and status.",
		Window:  WindowMonths,
	},
	{
		Name: ReportTaxCharged, Title: "Tax charged", Area: AreaMoney,
		Summary: "CGST, SGST and IGST charged by month and rate. Business analytics, not the GSTR-1 return itself.",
		Window:  WindowMonths,
	},
	{
		Name: ReportTicketResolution, Title: "Ticket resolution", Area: AreaSupport,
		Summary: "Raised, resolved, reopened and within-SLA by category and priority, per month.",
		Window:  WindowMonths,
	},
	{
		Name: ReportTicketTurnaround, Title: "Ticket turnaround", Area: AreaSupport,
		Summary: "How long tickets actually took: first response, time to resolve, and the slowest in the window.",
		Window:  WindowMonths,
	},
	{
		Name: ReportTopDataUsers, Title: "Top data users", Area: AreaNetwork,
		Summary: "Heaviest consumers by volume in the window, with their plan's allowance for comparison.",
		Window:  WindowMonths,
	},
	{
		Name: ReportFUPExceeded, Title: "Over fair-use", Area: AreaNetwork,
		Summary: "Who is over their fair-use allowance now, which tier applies, and what they have been throttled to.",
		Window:  WindowNone,
	},
}

// Catalogue returns every report, in display order.
//
// The one accessor for the table: Areas, InArea, Reports and Lookup all read
// it rather than the package variable, so there is a single place where the
// copy is made and a single thing to change if the catalogue ever stops being
// a literal. A caller cannot reorder or truncate the real table by mistake,
// because every one of them gets a copy.
func Catalogue() []ReportDef {
	out := make([]ReportDef, len(catalogue))
	copy(out, catalogue)
	return out
}

// Areas returns the area names in display order, including only areas that
// actually hold a report — so removing the last report in an area does not
// leave an empty heading on the screen.
func Areas() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range Catalogue() {
		if !seen[d.Area] {
			seen[d.Area] = true
			out = append(out, d.Area)
		}
	}
	return out
}

// InArea returns the reports of one area, in display order.
func InArea(area string) []ReportDef {
	var out []ReportDef
	for _, d := range Catalogue() {
		if d.Area == area {
			out = append(out, d)
		}
	}
	return out
}

// Lookup finds a report by name.
func Lookup(name string) (ReportDef, bool) {
	for _, d := range Catalogue() {
		if d.Name == name {
			return d, true
		}
	}
	return ReportDef{}, false
}

// Reports lists every exportable report name, for validation and error
// messages. Derived from the catalogue so the two cannot disagree.
func Reports() []string {
	out := make([]string, 0, len(catalogue))
	for _, d := range Catalogue() {
		out = append(out, d.Name)
	}
	return out
}

// ValidReport reports whether name is exportable.
func ValidReport(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// GroupFilterable reports whether report honours Filter.GroupID.
func GroupFilterable(name string) bool {
	d, ok := Lookup(name)
	return ok && d.Group
}

// Matches reports whether a report should survive the catalogue screen's
// filter box.
//
// Title, area and summary, not the name: an operator types "GST" or
// "renewal", and the words they reach for are in the summary rather than in a
// kebab-case identifier. Case-insensitive substring rather than fuzzy
// matching — a filter box that guesses is worse than one that does not, since
// a report quietly excluded reads as a report that does not exist.
func (d ReportDef) Matches(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	for _, field := range []string{d.Title, d.Area, d.Summary, d.Name} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

// TakesMonths and TakesDays let a template ask which window applies without
// comparing against the Window constants' numeric values — html/template's
// eq refuses a typed Window against an untyped literal, and "eq .Window 1" in
// a page would be unreadable even if it worked.
func (d ReportDef) TakesMonths() bool { return d.Window == WindowMonths }
func (d ReportDef) TakesDays() bool   { return d.Window == WindowDays }
