package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
)

// The console's LEA lookup — FR-NET-002 (lookup) and FR-OBS-003 (every LEA
// data access writes a tamper-evident audit record).
//
// The console had the same fail-open path as the API: when the audit row
// could not be written it logged the failure and showed the subscriber's
// identity anyway. That is an access with no record, which FR-OBS-003 rules
// out. It also passed any string to the lookup as an IP, where the API
// refuses a malformed one before anything is queried or audited.

type fakeLEA struct {
	result    *api.LEAResult
	lookupErr error
	auditErr  error

	lookups []string
	gotAt   time.Time
	audits  []api.LEAAuditEntry
}

func (f *fakeLEA) LookupByPublicIP(_ context.Context, ip string, _ *int, at time.Time) (*api.LEAResult, error) {
	f.lookups = append(f.lookups, ip)
	f.gotAt = at
	return f.result, f.lookupErr
}

func (f *fakeLEA) RecordLEAAudit(_ context.Context, e api.LEAAuditEntry) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

func leaSession() Session {
	return Session{StaffID: 3, Username: "noc_user", Role: "noc_engineer", LeaAccess: true}
}

func serveLEA(h *Handler, s Session, form url.Values) *httptest.ResponseRecorder {
	var r *http.Request
	if form == nil {
		r = httptest.NewRequest(http.MethodGet, "/staff/lea", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/staff/lea", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	w := httptest.NewRecorder()
	if form == nil {
		h.LEAPage(w, r)
	} else {
		h.LEALookup(w, r)
	}
	return w
}

func leaForm(ip string) url.Values {
	return url.Values{"public_ip": {ip}, "timestamp": {"2026-08-09T18:00"}}
}

var found = &api.LEAResult{SubscriberID: 3, Username: "found_subscriber", Source: "direct_ip"}

// A lookup that cannot be recorded shows nothing, hit or miss.
func TestFR_OBS_003_AConsoleLookupThatCannotBeRecordedShowsNothing(t *testing.T) {
	for name, result := range map[string]*api.LEAResult{"a hit": found, "a miss": nil} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLEA{result: result, auditErr: errors.New("db: lea_audit_log insert failed")}
			body := pageText(serveLEA(&Handler{lea: f}, leaSession(), leaForm("203.0.113.5")).Body.String())

			if strings.Contains(body, "found_subscriber") {
				t.Fatal("the subscriber's identity was shown with no audit record")
			}
			if strings.Contains(body, "No subscriber held that address") {
				t.Error("a miss was reported with no audit record")
			}
			if !strings.Contains(body, "could not be recorded") {
				t.Errorf("the operator is not told why: %s", firstLines(body))
			}
		})
	}
}

// A hit is shown and recorded, against the operator in the session.
func TestFR_OBS_003_AConsoleHitIsShownAndRecorded(t *testing.T) {
	f := &fakeLEA{result: found}
	body := pageText(serveLEA(&Handler{lea: f}, leaSession(), leaForm(" 203.0.113.5 ")).Body.String())

	if !strings.Contains(body, "found_subscriber") {
		t.Errorf("the result is missing: %s", firstLines(body))
	}
	if len(f.audits) != 1 {
		t.Fatalf("want one audit row, got %d", len(f.audits))
	}
	a := f.audits[0]
	if a.AccessorIdentity != "noc_user" || a.AccessorRole != "noc_engineer" ||
		a.QueriedPublicIP != "203.0.113.5" || a.ResultRowCount != 1 ||
		a.ResultSubscriberID == nil || *a.ResultSubscriberID != 3 {
		t.Errorf("audit row = %+v", a)
	}
	// The form is labelled "At (UTC)", so the typed time is UTC.
	if want := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC); !f.gotAt.Equal(want) {
		t.Errorf("looked up at %v, want %v", f.gotAt, want)
	}
}

// A miss and a failed lookup are both recorded; neither shows a subscriber.
func TestFR_OBS_003_AConsoleMissAndAFailureAreRecordedToo(t *testing.T) {
	f := &fakeLEA{}
	body := pageText(serveLEA(&Handler{lea: f}, leaSession(), leaForm("198.51.100.9")).Body.String())
	if !strings.Contains(body, "No subscriber held that address") {
		t.Errorf("a miss: %s", firstLines(body))
	}
	if len(f.audits) != 1 || f.audits[0].ResultRowCount != 0 || f.audits[0].ResultSubscriberID != nil {
		t.Errorf("a miss was recorded as %+v", f.audits)
	}

	f = &fakeLEA{lookupErr: errors.New("db: cgnat_allocations unreachable")}
	body = pageText(serveLEA(&Handler{lea: f}, leaSession(), leaForm("198.51.100.9")).Body.String())
	if !strings.Contains(body, "Lookup failed") || strings.Contains(body, "cgnat_allocations") {
		t.Errorf("a failed lookup: %s", firstLines(body))
	}
	if len(f.audits) != 1 {
		t.Errorf("an attempted lookup is as auditable as a successful one; got %d rows", len(f.audits))
	}
}

// A malformed address or time is refused before anything is queried or
// recorded, as the API does: no LEA data was accessed.
func TestFR_NET_002_AConsoleLookupValidatesFirst(t *testing.T) {
	for name, form := range map[string]url.Values{
		"not an IP":    leaForm("203.0.113"),
		"no IP":        leaForm(""),
		"a hostname":   leaForm("router.example.net"),
		"bad time":     {"public_ip": {"203.0.113.5"}, "timestamp": {"9 Aug 2026 6pm"}},
		"time missing": {"public_ip": {"203.0.113.5"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLEA{result: found}
			body := pageText(serveLEA(&Handler{lea: f}, leaSession(), form).Body.String())
			if len(f.lookups) != 0 || len(f.audits) != 0 {
				t.Errorf("a malformed request reached the lookup (%d) or the audit log (%d)",
					len(f.lookups), len(f.audits))
			}
			if strings.Contains(body, "found_subscriber") {
				t.Error("a result was shown for a malformed request")
			}
		})
	}
	// IPv6 is an address too.
	f := &fakeLEA{result: found}
	serveLEA(&Handler{lea: f}, leaSession(), leaForm("2001:db8::5"))
	if len(f.lookups) != 1 {
		t.Error("an IPv6 address was refused")
	}
}

// The page needs the LEA claim as well as the role, and says when lookup is
// not configured rather than pretending to search.
func TestFR_NET_002_TheConsoleLookupGate(t *testing.T) {
	f := &fakeLEA{result: found}
	for _, s := range []Session{
		{Username: "o", Role: "isp_owner"},            // the role, no claim
		{Username: "n", Role: "noc_engineer"},         // the role, no claim
		{Username: "c", Role: "csr", LeaAccess: true}, // the claim, wrong role
	} {
		if rec := serveLEA(&Handler{lea: f}, s, leaForm("203.0.113.5")); rec.Code != http.StatusForbidden {
			t.Errorf("%s (lea=%v) reached the lookup: %d", s.Role, s.LeaAccess, rec.Code)
		}
	}
	if len(f.lookups) != 0 {
		t.Error("a refused session reached the store")
	}

	page := pageText(serveLEA(&Handler{lea: f}, leaSession(), nil).Body.String())
	if !strings.Contains(page, "At (UTC)") {
		t.Errorf("the form does not say which zone it takes: %s", firstLines(page))
	}

	if body := pageText(serveLEA(&Handler{}, leaSession(), leaForm("203.0.113.5")).Body.String()); !strings.Contains(body, "not configured") {
		t.Errorf("an unconfigured lookup: %s", firstLines(body))
	}
}
