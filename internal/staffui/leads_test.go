package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/crm"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// fakeLeads records what the console asked of the lead pipeline.
type fakeLeads struct {
	leads      []crm.Lead
	err        error
	created    *crm.Lead
	moved      int
	gotStatus  *string
	gotLost    *string
	converted  int
	gotInput   api.ConvertLeadInput
	gotActor   string
	gotScope   *int
	scopeCalls int
}

func (f *fakeLeads) NewLead(ctx context.Context, l crm.Lead, scope *int) (*crm.Lead, error) {
	f.gotActor, f.gotScope = middleware.SubjectFromContext(ctx), scope
	if f.err != nil {
		return nil, f.err
	}
	l.ID, l.Status = 77, crm.StatusNew
	f.created = &l
	return &l, nil
}

func (f *fakeLeads) MoveLead(ctx context.Context, id int, status, _, _, lostReason *string, scope *int) (*crm.Lead, error) {
	f.moved++
	f.gotStatus, f.gotLost, f.gotScope = status, lostReason, scope
	f.gotActor = middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &crm.Lead{ID: id}, nil
}

func (f *fakeLeads) ConvertLeadToSubscriber(ctx context.Context, id int, in api.ConvertLeadInput, scope *int) (*api.ConvertLeadResult, error) {
	f.converted++
	f.gotInput, f.gotScope = in, scope
	f.gotActor = middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &api.ConvertLeadResult{
		Subscriber: &api.SubscriberRecord{ID: 501, Username: in.Username, Status: "pending_payment"},
		Lead:       &crm.Lead{ID: id, Status: crm.StatusConverted},
		CPEWarning: "device X is not in stock; the subscriber was created and a device can be issued separately",
	}, nil
}

func (f *fakeLeads) LeadList(_ context.Context, _, _ *string, _ *int) ([]crm.Lead, error) {
	return f.leads, nil
}

func (f *fakeLeads) LeadFunnelReport(context.Context, *int) (*crm.FunnelReport, error) {
	return &crm.FunnelReport{Stages: []crm.FunnelStage{{Source: "walk_in", Status: "new", LeadCount: 2}}, TotalLeads: 2, ConversionRatePct: "0.00"}, nil
}

func (f *fakeLeads) LeadByID(_ context.Context, id int, _ *int) (*crm.Lead, error) {
	for _, l := range f.leads {
		if l.ID == id {
			return &l, nil
		}
	}
	return nil, api.ErrLeadNotFound
}

func serveLeads(h *Handler, role, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 {
		r.SetPathValue("id", parts[2])
	}
	w := httptest.NewRecorder()
	switch {
	case method == "GET" && strings.HasSuffix(r.URL.Path, "/convert"):
		h.ConvertLeadPage(w, r)
	case method == "POST" && strings.HasSuffix(r.URL.Path, "/convert"):
		h.ConvertLeadForm(w, r)
	case method == "POST" && strings.HasSuffix(r.URL.Path, "/new"):
		h.CreateLeadForm(w, r)
	case method == "POST":
		h.UpdateLeadForm(w, r)
	default:
		h.Leads(w, r)
	}
	return w
}

// tableBody returns only the rows of a page's table. Asserting on the whole
// page matched the add-lead form's placeholder text, which happens to read
// "Ramesh Kumar" — a check that passed with the row filtered out.
func tableBody(page string) string {
	i, j := strings.Index(page, "<tbody>"), strings.Index(page, "</tbody>")
	if i < 0 || j < i {
		return ""
	}
	return page[i:j]
}

var sampleLeads = []crm.Lead{
	{ID: 1, FullName: "Ramesh Kumar", MobileNumber: "+919811100001", Source: "walk_in", Status: "new", CreatedAt: time.Now()},
	{ID: 2, FullName: "Lost Larry", MobileNumber: "+919811100002", Source: "website", Status: "lost", LostReason: "price", CreatedAt: time.Now()},
}

func TestLeadsScreen(t *testing.T) {
	t.Run("open view hides lost and converted; convert only for billing tier", func(t *testing.T) {
		for role, wantConvert := range map[string]bool{"isp_owner": true, "billing_admin": true, "csr": false, "technician": false} {
			h := &Handler{leads: &fakeLeads{leads: sampleLeads}}
			body := serveLeads(h, role, "GET", "/staff/leads", nil).Body.String()
			rows := tableBody(body)
			if !strings.Contains(rows, "Ramesh Kumar") || strings.Contains(rows, "Lost Larry") {
				t.Errorf("%s: open view should show only open leads", role)
			}
			if got := strings.Contains(body, "/staff/leads/1/convert"); got != wantConvert {
				t.Errorf("%s: convert link shown=%v, want %v", role, got, wantConvert)
			}
		}
	})

	t.Run("NOC and franchise roles are refused", func(t *testing.T) {
		for _, role := range []string{"noc_engineer", "lco", "franchise_admin"} {
			h := &Handler{leads: &fakeLeads{}}
			if code := serveLeads(h, role, "GET", "/staff/leads", nil).Code; code != 403 {
				t.Errorf("%s: want 403, got %d", role, code)
			}
		}
	})

	t.Run("search narrows by name or mobile", func(t *testing.T) {
		h := &Handler{leads: &fakeLeads{leads: sampleLeads}}
		rows := tableBody(serveLeads(h, "csr", "GET", "/staff/leads?stage=all&q=100002", nil).Body.String())
		if strings.Contains(rows, "Ramesh Kumar") || !strings.Contains(rows, "Lost Larry") {
			t.Error("mobile search should match only Larry")
		}
	})
}

func TestLeadWrites(t *testing.T) {
	t.Run("a new lead is filed ISP-wide and attributed", func(t *testing.T) {
		fl := &fakeLeads{}
		h := &Handler{leads: fl}
		body := serveLeads(h, "csr", "POST", "/staff/leads/new", url.Values{
			"full_name": {"Priya"}, "mobile_number": {"+919800000009"}, "source": {"referral"},
		}).Body.String()
		if fl.created == nil || fl.gotScope != nil || fl.gotActor != "csr_user" {
			t.Fatalf("created=%+v scope=%v actor=%q", fl.created, fl.gotScope, fl.gotActor)
		}
		if !strings.Contains(body, "Lead #77 added") {
			t.Error("the operator should be told it was saved")
		}
	})

	t.Run("a validation failure keeps what was typed and says why", func(t *testing.T) {
		fl := &fakeLeads{err: fmt.Errorf("%w: mobile_number must be E.164 format (e.g. +919876543210)", api.ErrLeadInvalid)}
		h := &Handler{leads: fl}
		body := serveLeads(h, "csr", "POST", "/staff/leads/new", url.Values{
			"full_name": {"Priya"}, "mobile_number": {"98000"}, "source": {"referral"},
		}).Body.String()
		if !strings.Contains(body, "E.164") || !strings.Contains(body, `value="Priya"`) {
			t.Error("the reason should be shown and the form refilled")
		}
	})

	t.Run("closing a lead as lost needs a reason", func(t *testing.T) {
		fl := &fakeLeads{}
		h := &Handler{leads: fl}
		body := serveLeads(h, "csr", "POST", "/staff/leads/1/update", url.Values{"status": {"lost"}, "lost_reason": {""}}).Body.String()
		if fl.moved != 0 || !strings.Contains(body, "why the lead was lost") {
			t.Errorf("want refusal without a write, got %d writes", fl.moved)
		}
	})

	t.Run("a stage move is applied and returns to the same view", func(t *testing.T) {
		fl := &fakeLeads{}
		h := &Handler{leads: fl}
		w := serveLeads(h, "technician", "POST", "/staff/leads/1/update", url.Values{
			"status": {"contacted"}, "lost_reason": {""}, "return_stage": {"new"},
		})
		if fl.moved != 1 || fl.gotStatus == nil || *fl.gotStatus != "contacted" || fl.gotActor != "technician_user" {
			t.Fatalf("move not applied as expected: %+v", fl)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "stage=new") {
			t.Errorf("should return to the same stage view, got %q", loc)
		}
	})
}

func TestConvertLead(t *testing.T) {
	form := url.Values{
		"caf_number": {"CAF-1"}, "username": {"ramesh.k"}, "password": {"s3cret-pass"},
		"plan_id": {"2"}, "registered_state": {"TN"}, "cpe_serial": {"X"},
	}

	t.Run("CSR and technician cannot convert, even by posting directly", func(t *testing.T) {
		for _, role := range []string{"csr", "technician"} {
			fl := &fakeLeads{leads: sampleLeads}
			h := &Handler{leads: fl}
			if code := serveLeads(h, role, "POST", "/staff/leads/1/convert", form).Code; code != 403 || fl.converted != 0 {
				t.Errorf("%s: want 403 and no conversion, got %d (%d conversions)", role, code, fl.converted)
			}
		}
	})

	t.Run("billing converts; the result says pending payment and shows the CPE warning", func(t *testing.T) {
		fl := &fakeLeads{leads: sampleLeads}
		h := &Handler{leads: fl}
		body := serveLeads(h, "billing_admin", "POST", "/staff/leads/1/convert", form).Body.String()
		if fl.converted != 1 || fl.gotInput.PlanID != 2 || fl.gotInput.Username != "ramesh.k" || fl.gotActor != "billing_admin_user" || fl.gotScope != nil {
			t.Fatalf("conversion input wrong: %+v actor=%q", fl.gotInput, fl.gotActor)
		}
		for _, want := range []string{"Subscriber created", "pending payment", "not in stock", "/staff/subscribers/501"} {
			if !strings.Contains(body, want) {
				t.Errorf("result page missing %q", want)
			}
		}
	})

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"already converted": {crm.ErrAlreadyConverted, "already been converted"},
		"lost":              {crm.ErrLeadLost, "marked lost"},
		"duplicate login":   {api.ErrLeadDuplicateLogin, "already in use"},
		"store failure":     {errors.New("boom"), "Nothing was created"},
	} {
		t.Run("error: "+name, func(t *testing.T) {
			fl := &fakeLeads{leads: sampleLeads, err: tc.err}
			h := &Handler{leads: fl}
			body := serveLeads(h, "isp_owner", "POST", "/staff/leads/1/convert", form).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("want message containing %q", tc.want)
			}
			if strings.Contains(body, "s3cret-pass") {
				t.Error("the password must never be echoed back into the page")
			}
		})
	}
}

func TestRenderLeadPages(t *testing.T) {
	h := &Handler{}
	sub := 9
	conv := time.Now()
	assertRendersOK(t, h, "leads", pageData{Session: testSession(), Data: leadsData{
		Stage: "all", Stages: leadStages, Movable: leadMovableStages, Sources: leadSources, CanConvert: true,
		Funnel:  &crm.FunnelReport{ConversionRatePct: "12.50"},
		ByStage: []leadStageCount{{Stage: "new", Count: 3}},
		Leads: append(sampleLeads, crm.Lead{ID: 3, FullName: "Done", Status: "converted",
			ConvertedSubscriberID: &sub, ConvertedAt: &conv, CreatedAt: conv}),
	}})
	assertRendersOK(t, h, "leads", pageData{Session: testSession(), Data: leadsData{Stage: "open"}})
	assertRendersOK(t, h, "lead_convert", pageData{Session: testSession(), Data: convertData{
		Lead: &sampleLeads[0], Plans: []Plan{{ID: 1, Name: "Home"}},
	}})
	assertRendersOK(t, h, "lead_convert", pageData{Session: testSession(), Data: convertData{
		Lead: &sampleLeads[0], Result: &api.ConvertLeadResult{
			Subscriber: &api.SubscriberRecord{ID: 1, Username: "u"}, Lead: &crm.Lead{ID: 1}, CPEIssued: "SN1",
		},
	}})
}
