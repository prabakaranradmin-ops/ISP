package portalui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

//go:embed templates static
var assets embed.FS

// staticFS roots the embedded filesystem at static/ so /ui/static/<name>
// serves static/<name> from the binary, not the templates alongside it.
var staticFS = func() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

var funcMap = template.FuncMap{
	"money":       func(d decimal.Decimal) string { return d.StringFixed(2) },
	"gb":          func(d decimal.Decimal) string { return d.StringFixed(2) },
	"date":        formatDate,
	"datetime":    formatDateTime,
	"statusBadge": statusBadgeClass,
	"pctWidth":    pctWidth,
	"methodLabel": billing.MethodLabel,
}

// pageNames lists every full page template (layout + content). Extended as
// each phase adds a page.
var pageNames = []string{"login", "dashboard", "usage", "invoices", "renew", "tickets", "notifications"}

// pages holds one isolated *template.Template per page: each is parsed from
// its own template.New("layout") root together with layout.html and the
// shared partials, so pages can each define their own "content" block
// without colliding in a single shared namespace (html/template has no
// built-in template inheritance — this is the standard workaround).
//
// Parsing happens at package init, so a template syntax error anywhere
// panics at process startup rather than surfacing later as a broken page —
// a deliberate trade-off given this runs in the same process as the
// production API; catch it with `go test ./internal/portalui/...`, which
// exercises this init on every run, before it reaches a real deploy.
var pages = mustParsePages()

func mustParsePages() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		out[name] = template.Must(
			template.New("layout").Funcs(funcMap).ParseFS(assets,
				"templates/layout.html",
				"templates/"+name+".html",
				"templates/partials/*.html",
			),
		)
	}
	return out
}

// fragments holds the HTMX partial-swap targets, rendered standalone (no
// layout) for polling/swap endpoints like GET /ui/dashboard/session.
var fragments = template.Must(
	template.New("fragments").Funcs(funcMap).ParseFS(assets, "templates/partials/*.html"),
)

// baseData carries the fields every page template needs regardless of its
// own page-specific data; page data structs embed it so its fields are
// promoted and directly addressable from the template (e.g. .Authenticated).
type baseData struct {
	Authenticated bool
	Active        string
	Error         string
}

func renderPage(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages[name].ExecuteTemplate(w, "layout", data); err != nil {
		log.Error().Err(err).Str("page", name).Msg("portalui: template render failed")
	}
}

func renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := fragments.ExecuteTemplate(w, name, data); err != nil {
		log.Error().Err(err).Str("fragment", name).Msg("portalui: fragment render failed")
	}
}

func formatDate(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("02 Jan 2006")
}

// formatDateTime accepts both time.Time (e.g. SessionHistoryEntry.StartTime)
// and *time.Time (e.g. its StopTime) so the same "datetime" template func
// works on both without the caller needing to dereference in-template, which
// html/template's function-call reflection cannot do implicitly.
func formatDateTime(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.Format("02 Jan 2006 15:04")
	case *time.Time:
		if t == nil {
			return "—"
		}
		return t.Format("02 Jan 2006 15:04")
	default:
		return "—"
	}
}

func statusBadgeClass(status string) string {
	switch status {
	case "active":
		return "badge-active"
	case "grace_period":
		return "badge-pending"
	case "soft_suspended", "hard_suspended":
		return "badge-suspended"
	case "terminated":
		return "badge-failed"
	default:
		return ""
	}
}

// pctWidth clamps a usage percentage into a CSS width value — Redis-derived
// usage math can transiently overshoot 100% between a plan change and the
// next FUP recalculation, and a >100% width would blow out the progress bar.
func pctWidth(pct float64) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return strconv.FormatFloat(pct, 'f', 1, 64)
}
