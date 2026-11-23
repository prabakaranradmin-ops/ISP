package staffui

import (
	"context"
	"errors"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Field payment collection — FR-COL-001.
//
// CollectionManager is satisfied by *api.Handler, so the console validates
// and audits exactly as the API does.
type CollectionManager interface {
	AssignCollector(ctx context.Context, subscriberID int, collectorID *int) error
	CollectionRoundFor(ctx context.Context, collectorID, windowDays int) ([]billing.CollectionItem, error)
	MyCollectionRound(ctx context.Context, windowDays int) ([]billing.CollectionItem, error)
	ListCollectors(ctx context.Context) ([]billing.CollectorLoad, error)
	CollectorTakings(ctx context.Context, from, to time.Time) ([]billing.CollectorTakings, error)
	RecordCollection(ctx context.Context, subscriberID int, amount decimal.Decimal, pd billing.PaymentDetail, unscoped bool) (*billing.Transaction, error)
}

var collectionMessages = map[string]string{
	"collected": "Payment recorded and credited. The subscriber's service resumes on their next authentication, within a minute.",
	"assigned":  "Subscriber added to that collector's round.",
	"cleared":   "Subscriber taken off the collection round.",

	"notyours": "Not recorded: that subscriber is not on your round. Ask the owner or billing to assign them, or to record it themselves.",
	"missing":  "No subscriber with that id.",
	"failed":   "That did not go through. Nothing was credited; try again.",
}

var collectionSuccess = map[string]bool{"collected": true, "assigned": true, "cleared": true}

// collectionRow is one subscriber as the round shows them.
type collectionRow struct {
	billing.CollectionItem
	Due       decimal.Decimal
	Overdue   bool
	Covered   bool
	ExpiryDay string
}

type collectionData struct {
	Rows    []collectionRow
	Window  int
	Methods []methodOption

	// Mine is true when the round shown is the signed-in user's own, which
	// is the only case where they may record a payment without the wider
	// billing grant.
	Mine        bool
	ViewingID   int
	ViewingName string

	// CanAssign and CanRecordAny are the owner/billing powers. A collector
	// sees neither: they work their round and record what they take.
	CanAssign    bool
	CanRecordAny bool

	Collectors []billing.CollectorLoad
	Takings    []billing.CollectorTakings
	TakingsDay string

	TotalDue decimal.Decimal
}

type methodOption struct{ Key, Label string }

// Who may work a round at all is the "collections" entry in
// staffui.sections (isp_owner, billing_admin, technician), enforced by
// requireSection. Technicians are included because they are the field staff
// who actually knock on doors; CSRs are not, being desk-based. Owner and
// billing are included because they both assign rounds and reconcile the
// cash, and because they can already record a payment from the subscriber's
// page.
//
// A canCollect(role) helper used to repeat that list here and was never
// called, so it was a second copy of the policy that could drift from the
// one being enforced.

// canAssignCollector: putting a subscriber on somebody's round decides whose
// pocket the cash passes through, which is a management act, not a field one.
func canAssignCollector(role string) bool {
	return role == "isp_owner" || role == "billing_admin"
}

func (h *Handler) collectionGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "collections")
	if !ok {
		return s, false
	}
	if h.collection == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Field collection is not configured on this deployment.")
		return s, false
	}
	return s, true
}

// Collections handles GET /staff/collections.
func (h *Handler) Collections(w http.ResponseWriter, r *http.Request) {
	s, ok := h.collectionGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Collections", "collections")
	q := r.URL.Query()

	pd := collectionData{
		Window:       collectionWindow(q.Get("window")),
		CanAssign:    canAssignCollector(s.Role),
		CanRecordAny: canRecordPayment(s.Role),
		Mine:         true,
	}
	for _, m := range billing.PaymentMethods {
		pd.Methods = append(pd.Methods, methodOption{Key: m, Label: methodLabels[m]})
	}

	// The subject is needed on the read path too, not only on the writes:
	// MyCollectionRound resolves the signed-in username to a staff id, and
	// without it the lookup is for an empty username and the screen answers
	// 500. The console fake ignored the context, so no unit test could see
	// this — it took loading the page.
	ctx := middleware.WithSubject(r.Context(), s.Username)

	// Owner and billing may look at somebody else's round; a collector only
	// ever sees their own, which is enforced by ignoring the parameter
	// rather than by hiding the control.
	var (
		rows []billing.CollectionItem
		err  error
	)
	if pd.CanAssign {
		if raw := strings.TrimSpace(q.Get("collector")); raw != "" {
			if id, convErr := strconv.Atoi(raw); convErr == nil {
				pd.Mine, pd.ViewingID = false, id
				rows, err = h.collection.CollectionRoundFor(ctx, id, pd.Window)
			}
		}
	}
	if pd.Mine {
		rows, err = h.collection.MyCollectionRound(ctx, pd.Window)
	}
	if err != nil {
		log.Error().Err(err).Str("actor", s.Username).Msg("staffui: collection round unavailable")
		h.renderError(w, r, s, http.StatusInternalServerError,
			"Could not read the collection round.")
		return
	}

	now := time.Now()
	for _, item := range rows {
		row := collectionRow{
			CollectionItem: item,
			Due:            item.AmountDue(),
			Overdue:        item.IsOverdue(now),
			Covered:        item.AlreadyCovered(),
		}
		if item.PlanExpiry != nil {
			row.ExpiryDay = item.PlanExpiry.Local().Format("2 Jan 2006")
		}
		pd.TotalDue = pd.TotalDue.Add(row.Due)
		pd.Rows = append(pd.Rows, row)
	}

	if pd.CanAssign {
		if collectors, cErr := h.collection.ListCollectors(ctx); cErr == nil {
			pd.Collectors = collectors
			for _, c := range collectors {
				if c.StaffID == pd.ViewingID {
					pd.ViewingName = c.Username
				}
			}
		} else {
			log.Warn().Err(cErr).Msg("staffui: collector list unavailable")
		}

		// Today's takings, for the hand-in at the end of a round. Cash
		// collection without reconciliation is where money goes missing, so
		// this sits on the same screen rather than in a report nobody opens.
		day := startOfDay(now)
		pd.TakingsDay = day.Format("2 Jan 2006")
		if takings, tErr := h.collection.CollectorTakings(ctx, day, day.Add(24*time.Hour)); tErr == nil {
			pd.Takings = takings
		} else {
			log.Warn().Err(tErr).Msg("staffui: collector takings unavailable")
		}
	}

	if code := q.Get("m"); code != "" {
		if msg, ok := collectionMessages[code]; ok {
			if collectionSuccess[code] {
				d.Message = msg
			} else {
				d.Error = msg
			}
		}
	}
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}

	d.Data = pd
	h.render(w, "collections", d)
}

// collectionWindow reads the look-ahead in days.
//
// Zero is a meaningful value — "only those already lapsed" — so an empty or
// unparseable parameter falls back to a default rather than to zero, which
// would silently show a collector a shorter round than they asked for.
func collectionWindow(raw string) int {
	const defaultWindow = 7
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultWindow
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 90 {
		return defaultWindow
	}
	return n
}

func startOfDay(t time.Time) time.Time {
	t = t.Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// RecordCollectionForm handles POST /staff/collections/{id}/payment.
func (h *Handler) RecordCollectionForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.collectionGate(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("amount")))
	if err != nil {
		h.redirectCollections(w, r, "", "Enter the amount as a number, for example 799.00.")
		return
	}
	if amount.IsNegative() || amount.IsZero() {
		h.redirectCollections(w, r, "", "A collection must be more than zero.")
		return
	}

	pd := billing.PaymentDetail{
		Method:       r.PostFormValue("method"),
		Reference:    strings.TrimSpace(r.PostFormValue("reference")),
		ChequeNumber: strings.TrimSpace(r.PostFormValue("cheque_number")),
		ChequeBank:   strings.TrimSpace(r.PostFormValue("cheque_bank")),
	}
	if v := r.PostFormValue("cheque_date"); v != "" {
		day, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			h.redirectCollections(w, r, "", "The cheque date is not a date.")
			return
		}
		pd.ChequeDate = &day
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	_, err = h.collection.RecordCollection(ctx, id, amount, pd, canRecordPayment(s.Role))
	switch {
	case errors.Is(err, billing.ErrNotYourCollection):
		h.redirectCollections(w, r, "notyours", "")
		return
	case err != nil:
		// A method or cheque problem is already an operator-facing
		// sentence from FR-BIL-013's own validation, so it is shown rather
		// than flattened.
		if reason := billing.PaymentInvalidReason(err); reason != "" {
			h.redirectCollections(w, r, "", reason+".")
			return
		}
		if strings.Contains(err.Error(), "not found") {
			h.redirectCollections(w, r, "missing", "")
			return
		}
		log.Error().Err(err).Int("subscriber_id", id).Str("actor", s.Username).
			Msg("staffui: record collection failed")
		h.redirectCollections(w, r, "failed", "")
		return
	}
	h.redirectCollections(w, r, "collected", "")
}

// AssignCollectorForm handles POST /staff/collections/assign.
func (h *Handler) AssignCollectorForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.collectionGate(w, r)
	if !ok {
		return
	}
	if !canAssignCollector(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only the owner and billing staff can change who collects from a subscriber.")
		return
	}
	subID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("subscriber_id")))
	if err != nil {
		h.redirectCollections(w, r, "", "That is not a valid subscriber id.")
		return
	}

	var collectorID *int
	if raw := strings.TrimSpace(r.PostFormValue("collector_id")); raw != "" && raw != "0" {
		id, convErr := strconv.Atoi(raw)
		if convErr != nil {
			h.redirectCollections(w, r, "failed", "")
			return
		}
		collectorID = &id
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.collection.AssignCollector(ctx, subID, collectorID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			h.redirectCollections(w, r, "missing", "")
			return
		}
		log.Error().Err(err).Int("subscriber_id", subID).Msg("staffui: assign collector failed")
		h.redirectCollections(w, r, "failed", "")
		return
	}
	if collectorID == nil {
		h.redirectCollections(w, r, "cleared", "")
		return
	}
	h.redirectCollections(w, r, "assigned", "")
}

func (h *Handler) redirectCollections(w http.ResponseWriter, r *http.Request, code, problem string) {
	url := "/staff/collections"
	switch {
	case problem != "":
		url += "?err=" + neturl.QueryEscape(problem)
	case code != "":
		url += "?m=" + code
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}
