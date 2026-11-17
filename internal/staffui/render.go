package staffui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

//go:embed templates static
var assets embed.FS

var staticFS = func() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

func staticHandler() http.Handler { return http.FileServer(http.FS(staticFS)) }

var funcMap = template.FuncMap{
	"money":        func(d decimal.Decimal) string { return d.StringFixed(2) },
	"datetime":     func(t time.Time) string { return t.Format("02 Jan 2006 15:04") },
	"date":         func(t time.Time) string { return t.Format("02 Jan 2006") },
	"badge":        badgeClass,
	"humanize":     humanizeStatus,
	"leadBadge":    leadBadge,
	"cpeBadge":     cpeBadge,
	"cpeTaskBadge": cpeTaskBadge,
	"annBadge":     annBadge,
	"msgBadge":     msgBadge,
	"methodLabel":  methodLabel,
	"hasString":    inStrings,
	"channelLabel": func(c string) string {
		return map[string]string{"whatsapp": "WhatsApp", "sms": "SMS", "email": "Email", "push": "App push"}[c]
	},
	"ticketView":  func(v string) string { return ticketViewLabels[v] },
	"pct":         func(f float64) string { return fmt.Sprintf("%.2f", f) },
	"gbytes":      gbytes,
	"bytes":       humanBytes,
	"elapsed":     elapsed,
	"shortcutFor": shortcutFor,
	// optHours/optPct format a nil-able *float64 straight to one decimal
	// place, or an em dash — used by the reports screen, where nil means
	// "nothing in this group was resolved/billed" rather than zero (see
	// internal/reporting/rows.go's own comments on why those fields are
	// pointers, not bare float64s).
	"optHours": optFloat1,
	"optPct":   optFloat1,
	// optMoney formats a nil-able *decimal.Decimal (workflow.ApprovalRequest's
	// Amount is nil for a terminate request, which has no money attached).
	"optMoney": optMoney,
	// optDate/optString support the Tasks screen's nilable due-date and
	// decided-by/reason fields without a template-side type mismatch (a
	// template function can't accept a pointer where it declared a value
	// type, unlike printing a pointer directly).
	"optDate":     optDate,
	"optDatetime": optDatetime,
	"optString":   optString,
	// deref reads a *int for a template-side equality check (e.g. matching
	// a staff account's bound franchise against a dropdown option) — a bare
	// {{eq .ID $ptr}} cannot compare an int to a *int, and printableValue's
	// auto-indirection only applies when printing a value directly, not
	// when passing it as an argument to another action.
	"deref": derefInt,
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func optMoney(d *decimal.Decimal) string {
	if d == nil {
		return "—"
	}
	return d.StringFixed(2)
}

func optDate(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("02 Jan 2006")
}

func optDatetime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("02 Jan 2006 15:04")
}

func optString(s *string) string {
	if s == nil || *s == "" {
		return "—"
	}
	return *s
}

func optFloat1(f *float64) string {
	if f == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *f)
}

// sectionShortcuts maps a Section.Key to the letter console.js's "g <letter>"
// navigation binds to it. Kept here, next to the nav template that reads it,
// rather than on the Section struct itself — the shortcut is a presentation
// detail of the layout template, not part of the role/authorization model
// Section otherwise carries.
var sectionShortcuts = map[string]string{
	"dashboard":   "h",
	"sessions":    "o",
	"invoices":    "v",
	"subscribers": "s",
	"catalogue":   "c",
	"billing":     "b",
	"nas":         "n",
	"revenue":     "r",
	"tickets":     "t",
	"lea":         "l",
	"demo":        "d",
	"franchise":   "f",
	"inventory":   "i",
	"reports":     "p",
	"tasks":       "k",
	"ledger":      "g",
	"network":     "w",
}

func shortcutFor(key string) string { return sectionShortcuts[key] }

// navGroups arranges the sections into the top bar's menus. Like the
// shortcuts above, this is presentation only: which sections a role may open
// is still decided by AllowedSections alone, and grouping can neither grant
// nor hide anything. An entry with an empty Label is a plain top-level link.
//
// Every Section key must appear exactly once; TestNavGroupsCoverEverySection
// fails otherwise, so a new section cannot silently drop out of the menus.
var navGroups = []struct {
	Label string
	Keys  []string
}{
	{"", []string{"dashboard"}},
	{"Subscribers", []string{"subscribers", "leads", "my-subscribers", "catalogue"}},
	{"Finance", []string{"invoices", "billing", "revenue", "ledger", "dunning", "reports", "procurement", "franchise", "my-pnl"}},
	{"Service", []string{"tickets", "tasks", "announcements", "logs", "inventory"}},
	{"Network", []string{"sessions", "cpe", "nas", "network", "lea"}},
	{"Admin", []string{"accounts", "demo"}},
}

// NavEntry is one item in the top bar: a single link (len(Items) == 1) or a
// menu of them.
type NavEntry struct {
	Label  string
	Items  []Section
	Active bool // the current page is one of Items
}

// buildNav groups the sections an operator may open. A menu left with one
// item is shown as that item's own link, so a role with little reach (a
// technician, a franchise partner) gets a short row of links rather than a
// row of one-item menus. Any allowed section missing from navGroups is
// appended as its own link rather than dropped.
func buildNav(allowed []Section, active string) []NavEntry {
	byKey := make(map[string]Section, len(allowed))
	for _, s := range allowed {
		byKey[s.Key] = s
	}
	placed := make(map[string]bool, len(allowed))
	var out []NavEntry
	add := func(label string, items []Section) {
		if len(items) == 0 {
			return
		}
		e := NavEntry{Label: label, Items: items}
		for _, it := range items {
			if it.Key == active {
				e.Active = true
			}
		}
		if len(items) == 1 {
			e.Label = items[0].Label
		}
		out = append(out, e)
	}
	for _, g := range navGroups {
		var items []Section
		for _, k := range g.Keys {
			if s, ok := byKey[k]; ok {
				items = append(items, s)
				placed[k] = true
			}
		}
		if g.Label == "" {
			for _, it := range items {
				add("", []Section{it})
			}
			continue
		}
		add(g.Label, items)
	}
	for _, s := range allowed {
		if !placed[s.Key] {
			add("", []Section{s})
		}
	}
	return out
}

// badgeClass maps a status to its pill colour, so state reads at a glance in a
// list rather than having to be read word by word.
func badgeClass(status string) string {
	switch status {
	case "active", "resolved", "closed", "sent", "delivered", "in_stock", "completed", "executed", "approved", "low":
		return "ok"
	case "grace_period", "open", "in_progress", "remind_7d", "remind_3d", "remind_1d", "issued", "returned", "pending", "medium", "high":
		return "warn"
	case "soft_suspended", "hard_suspended", "terminated", "failed", "faulty", "cancelled", "rejected", "execution_failed", "critical":
		return "bad"
	default:
		return "neutral"
	}
}

// ticketViewLabels names the ticket queue's preset lists.
var ticketViewLabels = map[string]string{
	"open": "Open", "overdue": "Past target", "unassigned": "Unassigned",
	"mine": "Assigned to me", "closed": "Closed (30 days)", "all": "All",
}

// msgBadge colours a message or webhook delivery status.
func msgBadge(status string) string {
	switch status {
	case "delivered", "read", "sent":
		return "ok"
	case "pending":
		return "warn"
	case "failed", "abandoned":
		return "bad"
	default:
		return "neutral" // suppressed_dnd: deliberately not sent
	}
}

// annBadge colours an announcement's status.
func annBadge(status string) string {
	switch status {
	case "sent":
		return "ok"
	case "draft", "sending":
		return "warn"
	case "failed":
		return "bad"
	default:
		return "neutral"
	}
}

// cpeBadge colours a provisioning state.
func cpeBadge(state string) string {
	switch state {
	case "provisioned":
		return "ok"
	case "registered", "needs_reprovision":
		return "warn"
	case "fault":
		return "bad"
	default:
		return "neutral"
	}
}

// cpeTaskBadge colours a queued RPC's status.
func cpeTaskBadge(status string) string {
	switch status {
	case "completed":
		return "ok"
	case "pending", "sent":
		return "warn"
	case "failed", "expired":
		return "bad"
	default:
		return "neutral"
	}
}

// leadBadge colours a lead stage: neutral while untouched, amber while being
// worked, green once converted, red once lost.
func leadBadge(stage string) string {
	switch stage {
	case "converted":
		return "ok"
	case "contacted", "qualified":
		return "warn"
	case "lost":
		return "bad"
	default:
		return "neutral"
	}
}

// humanizeStatus turns a stored status ("soft_suspended") into the words an
// operator would say ("Soft suspended").
func humanizeStatus(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// humanBytes formats a byte count in the largest unit that keeps it >= 1,
// the way an operator reads usage ("287.97 MB", "3.50 GB").
func humanBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(b) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}

// elapsed formats the time between two instants as h:mm:ss, or "Nd h:mm:ss"
// past a day — PPPoE sessions routinely run for weeks.
func elapsed(from, to time.Time) string {
	d := to.Sub(from)
	if d < 0 {
		d = 0
	}
	secs := int64(d / time.Second)
	days, secs := secs/86400, secs%86400
	h, m, sec := secs/3600, (secs%3600)/60, secs%60
	if days > 0 {
		return fmt.Sprintf("%dd %d:%02d:%02d", days, h, m, sec)
	}
	return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
}

func gbytes(b int64) string {
	if b <= 0 {
		return "0.00 GB"
	}
	return decimal.NewFromInt(b).
		Div(decimal.NewFromInt(1024*1024*1024)).
		StringFixed(2) + " GB"
}

// Each page is parsed into its own template set rooted at layout.html, so
// pages can define their own "content" block without colliding — html/template
// has no inheritance and this is the standard workaround.
var pageNames = []string{
	"login", "subscribers", "subscriber_detail", "subscriber_new",
	"billing", "tickets", "revenue", "catalogue", "nas", "lea", "demo",
	"accounts", "change_password", "error", "franchise", "franchise_detail",
	"inventory", "reports", "tasks", "my_subscribers", "procurement", "ledger", "network",
	"dashboard", "sessions", "money", "leads", "lead_convert", "cpe", "announcements", "logs", "credit_note", "dunning",
}

var pages = func() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t := template.Must(template.New("layout").Funcs(funcMap).
			ParseFS(assets, "templates/layout.html", "templates/"+name+".html"))
		out[name] = t
	}
	return out
}()

// pageData is the envelope every screen renders inside. The nav is derived
// from the session rather than passed per handler, so a new screen cannot
// accidentally show a menu the operator has no right to.
type pageData struct {
	Title    string
	Session  Session
	Sections []Section
	Nav      []NavEntry
	Active   string
	CSRF     string
	Message  string
	Error    string
	Data     any
}

func (h *Handler) render(w http.ResponseWriter, name string, d pageData) {
	t, ok := pages[name]
	if !ok {
		log.Error().Str("page", name).Msg("staffui: unknown template")
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	// Rendered to a buffer first, then written in one go. Executing straight to
	// the ResponseWriter means a template error — a mistyped field name, say —
	// leaves a half-written page carrying a 200 status, with nothing to say
	// anything went wrong. That is precisely how a field that does not exist on
	// health.SubscriberRecord first showed up here: as an empty panel that
	// looked like missing data rather than the error it was.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", d); err != nil {
		log.Error().Err(err).Str("page", name).Msg("staffui: render failed")
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil {
		log.Warn().Err(err).Str("page", name).Msg("staffui: response write failed")
	}
}

// page builds the envelope for a signed-in operator.
func (h *Handler) page(s Session, title, active string) pageData {
	allowed := AllowedSections(s.Role, s.LeaAccess)
	return pageData{
		Title:    title,
		Session:  s,
		Sections: allowed,
		Nav:      buildNav(allowed, active),
		Active:   active,
		CSRF:     h.csrfToken(s.Token),
	}
}

func (h *Handler) renderLogin(w http.ResponseWriter, _ *http.Request, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := pages["login"]
	if err := t.ExecuteTemplate(w, "layout", pageData{Title: "Sign in", Error: message}); err != nil {
		log.Error().Err(err).Msg("staffui: login render failed")
	}
}

func (h *Handler) renderError(w http.ResponseWriter, _ *http.Request, s Session, code int, msg string) {
	w.WriteHeader(code)
	d := h.page(s, "Not available", "")
	d.Error = msg
	h.render(w, "error", d)
}

// methodLabel names a payment method the way a cashier would.
func methodLabel(m string) string {
	if l, ok := methodLabels[m]; ok {
		return l
	}
	return m
}
