package staffui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// The audit trail screen — FR-OBS-006.

type fakeAudit struct {
	records   []middleware.AuditRecord
	total     int
	actors    []string
	actions   []string
	gotFilter middleware.AuditFilter
	err       error
}

func (f *fakeAudit) ListAudit(_ context.Context, flt middleware.AuditFilter) ([]middleware.AuditRecord, error) {
	f.gotFilter = flt
	return f.records, f.err
}

func (f *fakeAudit) CountAudit(_ context.Context, _ middleware.AuditFilter) (int, error) {
	return f.total, nil
}

func (f *fakeAudit) ListAuditActors(context.Context) ([]string, error)  { return f.actors, nil }
func (f *fakeAudit) ListAuditActions(context.Context) ([]string, error) { return f.actions, nil }

func auditRecord(actor, action, target string, detail map[string]any) middleware.AuditRecord {
	return middleware.AuditRecord{
		ID: 1, CorrelationID: "abc123", ActorID: actor, ActorRole: "isp_owner",
		Action: action, Target: target, Detail: detail, CreatedAt: time.Now(),
	}
}

func TestFR_OBS_006_AuditScreenIsOwnerOnly(t *testing.T) {
	// Owner-only by section, as the requirement specifies: the trail names
	// every action every operator has taken.
	for _, role := range []string{"billing_admin", "csr", "noc_engineer", "technician"} {
		var found bool
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "audit" {
				found = true
			}
		}
		if found {
			t.Errorf("%s can reach the audit trail", role)
		}
	}
	var ownerHas bool
	for _, sec := range AllowedSections("isp_owner", false) {
		if sec.Key == "audit" {
			ownerHas = true
		}
	}
	if !ownerHas {
		t.Error("the owner cannot reach the audit trail")
	}
}

func TestFR_OBS_006_AuditScreenRendersTheTrail(t *testing.T) {
	f := &fakeAudit{
		records: []middleware.AuditRecord{
			auditRecord("meena", "subscriber.plan_change", "42",
				map[string]any{"new_plan_id": float64(7), "note": "customer asked"}),
		},
		total:   1,
		actors:  []string{"meena", "ravi"},
		actions: []string{"product.attached", "subscriber.plan_change"},
	}
	h := &Handler{audit: f}

	w := servePR(h, "isp_owner", "GET", "/staff/audit", nil, h.Audit)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"meena", "subscriber.plan_change", "42", "abc123",
		"note: customer asked",
		// A JSON number arrives as float64; printed as an integer rather
		// than 7e+00, which is what %v would give.
		"new_plan_id: 7",
		// The filter dropdowns are populated from what is in the trail.
		"product.attached", "ravi",
		// And the append-only property is stated, because an operator
		// needs to know the page is evidence rather than a view.
		"append-only",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// Detail is rendered in sorted key order. Go map iteration is random, so
// without sorting the same entry reshuffles on every refresh — which makes
// a page meant to be evidence look unreliable.
func TestFR_OBS_006_DetailIsRenderedInAStableOrder(t *testing.T) {
	detail := map[string]any{"zulu": "last", "alpha": "first", "mike": "middle"}

	first := detailLines(detail)
	for i := 0; i < 50; i++ {
		again := detailLines(detail)
		if len(again) != len(first) {
			t.Fatalf("line count changed between renders")
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("render %d differs: %v vs %v", i, again, first)
			}
		}
	}
	if first[0] != "alpha: first" || first[2] != "zulu: last" {
		t.Errorf("lines are not sorted by key: %v", first)
	}
}

func TestFR_OBS_006_DetailValueFormatting(t *testing.T) {
	cases := map[string]struct {
		in   any
		want string
	}{
		"a whole JSON number is an integer":    {float64(7), "7"},
		"a large whole number is not exponent": {float64(1234567), "1234567"},
		"a fractional number keeps its places": {float64(1.5), "1.5"},
		"a string passes through":              {"hello", "hello"},
		"true reads as yes":                    {true, "yes"},
		"false reads as no":                    {false, "no"},
		"nil is empty":                         {nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := formatDetailValue(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFR_OBS_006_AuditFiltersReachTheStore(t *testing.T) {
	t.Run("actor, action and dates are passed through", func(t *testing.T) {
		f := &fakeAudit{}
		h := &Handler{audit: f}
		servePR(h, "isp_owner", "GET",
			"/staff/audit?actor=meena&action=product.attached&from=2026-04-01&to=2026-04-30",
			nil, h.Audit)

		if f.gotFilter.Actor != "meena" || f.gotFilter.Action != "product.attached" {
			t.Errorf("store got %+v", f.gotFilter)
		}
		if f.gotFilter.From == nil || f.gotFilter.From.Format("2006-01-02") != "2026-04-01" {
			t.Errorf("from reached the store as %v", f.gotFilter.From)
		}
		// `to` is the end of the chosen day, not its start: filtering "to
		// the 30th" must include the 30th.
		if f.gotFilter.To == nil || f.gotFilter.To.Format("2006-01-02") != "2026-05-01" {
			t.Errorf("to reached the store as %v, want the start of 1 May so the 30th is included", f.gotFilter.To)
		}
	})

	t.Run("a malformed date is explained rather than ignored", func(t *testing.T) {
		f := &fakeAudit{}
		h := &Handler{audit: f}
		w := servePR(h, "isp_owner", "GET", "/staff/audit?from=last+Tuesday", nil, h.Audit)
		if !strings.Contains(w.Body.String(), "must be a date") {
			t.Error("the page does not say the date was unreadable")
		}
		if f.gotFilter.From != nil {
			t.Error("an unreadable date reached the store")
		}
	})

	t.Run("the page says when it is showing only part of the trail", func(t *testing.T) {
		f := &fakeAudit{
			records: []middleware.AuditRecord{auditRecord("meena", "a.b", "x", nil)},
			total:   5000,
		}
		h := &Handler{audit: f}
		body := servePR(h, "isp_owner", "GET", "/staff/audit", nil, h.Audit).Body.String()
		if !strings.Contains(body, "5000") || !strings.Contains(body, "Narrow the dates") {
			t.Error("the page does not say the trail is longer than what is shown")
		}
	})

	t.Run("an unconfigured trail says so rather than failing", func(t *testing.T) {
		h := &Handler{}
		w := servePR(h, "isp_owner", "GET", "/staff/audit", nil, h.Audit)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "not configured") {
			t.Error("the page does not explain that the trail is unconfigured")
		}
	})

	t.Run("an empty trail is explained rather than looking broken", func(t *testing.T) {
		h := &Handler{audit: &fakeAudit{}}
		body := servePR(h, "isp_owner", "GET", "/staff/audit", nil, h.Audit).Body.String()
		if !strings.Contains(body, "No audit entries yet") {
			t.Error("an empty trail renders no explanation")
		}
	})
}
