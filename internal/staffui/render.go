package staffui

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
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
	"money":       func(d decimal.Decimal) string { return d.StringFixed(2) },
	"datetime":    func(t time.Time) string { return t.Format("02 Jan 2006 15:04") },
	"date":        func(t time.Time) string { return t.Format("02 Jan 2006") },
	"badge":       badgeClass,
	"gbytes":      gbytes,
	"shortcutFor": shortcutFor,
}

// sectionShortcuts maps a Section.Key to the letter console.js's "g <letter>"
// navigation binds to it. Kept here, next to the nav template that reads it,
// rather than on the Section struct itself — the shortcut is a presentation
// detail of the layout template, not part of the role/authorization model
// Section otherwise carries.
var sectionShortcuts = map[string]string{
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
}

func shortcutFor(key string) string { return sectionShortcuts[key] }

// badgeClass maps a status to its pill colour, so state reads at a glance in a
// list rather than having to be read word by word.
func badgeClass(status string) string {
	switch status {
	case "active", "resolved", "closed", "sent", "delivered", "in_stock":
		return "ok"
	case "grace_period", "open", "in_progress", "remind_7d", "remind_3d", "remind_1d", "issued", "returned":
		return "warn"
	case "soft_suspended", "hard_suspended", "terminated", "failed", "faulty":
		return "bad"
	default:
		return "neutral"
	}
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
	"inventory",
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
	return pageData{
		Title:    title,
		Session:  s,
		Sections: AllowedSections(s.Role, s.LeaAccess),
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
