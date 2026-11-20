package staffui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// The staff audit trail — FR-OBS-006 | migration 062.
//
// AuditReader reads the append-only trail. Satisfied by *db.AuditStore.
// Read-only by design: the table has UPDATE and DELETE revoked from the
// role the services run as, so there is no write method to offer.
type AuditReader interface {
	ListAudit(ctx context.Context, f middleware.AuditFilter) ([]middleware.AuditRecord, error)
	CountAudit(ctx context.Context, f middleware.AuditFilter) (int, error)
	ListAuditActors(ctx context.Context) ([]string, error)
	ListAuditActions(ctx context.Context) ([]string, error)
}

// auditPageSize is how many entries one page shows. The table only grows,
// so this is a cap rather than a preference.
const auditPageSize = 200

type auditRow struct {
	middleware.AuditRecord
	// Detail rendered as sorted "key: value" lines, because a Go map
	// renders in a random order and an audit page that reshuffles between
	// refreshes is not something anybody trusts.
	DetailLines []string
}

type auditData struct {
	Rows    []auditRow
	Actors  []string
	Actions []string
	// Filter values as given, so the form comes back filled in.
	Actor  string
	Action string
	From   string
	To     string
	// Total is how many entries match, so the page can say whether what
	// it shows is all of them.
	Total    int
	Shown    int
	Capped   bool
	PageSize int
}

// Audit handles GET /staff/audit.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "audit")
	if !ok {
		return
	}
	d := h.page(s, "Audit trail", "audit")

	if h.audit == nil {
		d.Error = "The audit trail is not configured on this deployment."
		h.render(w, "audit", d)
		return
	}

	q := r.URL.Query()
	ad := auditData{
		Actor:    strings.TrimSpace(q.Get("actor")),
		Action:   strings.TrimSpace(q.Get("action")),
		From:     strings.TrimSpace(q.Get("from")),
		To:       strings.TrimSpace(q.Get("to")),
		PageSize: auditPageSize,
	}

	f := middleware.AuditFilter{Actor: ad.Actor, Action: ad.Action, Limit: auditPageSize}
	// Dates are parsed in UTC as bare days, matching how they are stored
	// and how every other date filter in this console reads them. `to` is
	// the end of the chosen day, not its start, because an operator
	// filtering "to the 5th" means including the 5th.
	if ad.From != "" {
		if t, err := time.ParseInLocation("2006-01-02", ad.From, time.UTC); err == nil {
			f.From = &t
		} else {
			d.Error = "The 'from' date must be a date, for example 2026-04-01."
		}
	}
	if ad.To != "" {
		if t, err := time.ParseInLocation("2006-01-02", ad.To, time.UTC); err == nil {
			end := t.AddDate(0, 0, 1)
			f.To = &end
		} else {
			d.Error = "The 'to' date must be a date, for example 2026-04-30."
		}
	}

	entries, err := h.audit.ListAudit(r.Context(), f)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list audit entries failed")
		d.Error = "Could not load the audit trail."
		h.render(w, "audit", d)
		return
	}
	for _, e := range entries {
		ad.Rows = append(ad.Rows, auditRow{AuditRecord: e, DetailLines: detailLines(e.Detail)})
	}
	ad.Shown = len(ad.Rows)

	if total, err := h.audit.CountAudit(r.Context(), f); err == nil {
		ad.Total = total
		ad.Capped = total > ad.Shown
	} else {
		log.Warn().Err(err).Msg("staffui: count audit entries failed")
	}

	// The filter dropdowns. A failure here leaves them empty rather than
	// taking the page down: the trail itself is what the operator came
	// for, and the filters can still be typed into the URL.
	if actors, err := h.audit.ListAuditActors(r.Context()); err == nil {
		ad.Actors = actors
	} else {
		log.Warn().Err(err).Msg("staffui: audit actor list unavailable")
	}
	if actions, err := h.audit.ListAuditActions(r.Context()); err == nil {
		ad.Actions = actions
	} else {
		log.Warn().Err(err).Msg("staffui: audit action list unavailable")
	}

	d.Data = ad
	h.render(w, "audit", d)
}

// detailLines renders a detail map as sorted "key: value" lines.
//
// Sorted, because Go map iteration is deliberately random: without this
// the same audit entry renders its detail in a different order on every
// refresh, which makes a page meant to be evidence look unreliable.
func detailLines(detail map[string]any) []string {
	if len(detail) == 0 {
		return nil
	}
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+": "+formatDetailValue(detail[k]))
	}
	return out
}

// formatDetailValue renders one detail value for display.
//
// JSON numbers arrive as float64, so an integer id would otherwise print
// as "1.2345e+04". Whole numbers are printed as integers, which is what
// every id, count and plan number in these details actually is.
func formatDetailValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}
