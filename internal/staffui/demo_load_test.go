package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// The Demo Data screen: load a presentable console, and remove it again.
//
// demo.go was at 30%. Writing this found that the load reported what it
// meant to create rather than what it created:
//
//   - "1 support ticket, 1 invoice and 1 sample router" whatever happened to
//     the ticket and the router — a failure, or no ticket store or NAS store
//     wired at all — and "active, soft-suspended, hard-suspended" whether or
//     not activating them worked. Only the invoice was checked. An owner
//     showing a client the console was told a router existed that did not.
//   - A row that could not be flagged as demo data was logged and forgotten.
//     Remove demo data deletes by that flag, so the row stays behind as real
//     data — for a subscriber, an active account with the walkthrough's
//     published password.

var errDemo = errors.New("store: failed")

type demoStoreFake struct {
	status    DemoStatus
	statusErr error
	removeErr error
	markErr   error
	removed   bool
	marked    map[string]int
}

func (f *demoStoreFake) mark(kind string) error {
	if f.marked == nil {
		f.marked = map[string]int{}
	}
	if f.markErr != nil {
		return f.markErr
	}
	f.marked[kind]++
	return nil
}
func (f *demoStoreFake) MarkSubscriberDemo(context.Context, int) error { return f.mark("subscriber") }
func (f *demoStoreFake) MarkPlanDemo(context.Context, int) error       { return f.mark("plan") }
func (f *demoStoreFake) MarkNASDemo(context.Context, int) error        { return f.mark("nas") }
func (f *demoStoreFake) Status(context.Context) (DemoStatus, error)    { return f.status, f.statusErr }
func (f *demoStoreFake) Remove(context.Context) error {
	f.removed = f.removeErr == nil
	return f.removeErr
}

type fakeProvisioner struct {
	created []api.CreateSubscriberRequest
	failAt  int // 1-based; 0 = never
}

func (f *fakeProvisioner) ProvisionSubscriber(_ context.Context, req api.CreateSubscriberRequest) (*api.SubscriberRecord, error) {
	if f.failAt > 0 && len(f.created)+1 == f.failAt {
		return nil, errDemo
	}
	f.created = append(f.created, req)
	return &api.SubscriberRecord{ID: 200 + len(f.created), Username: req.Username, Status: "pending_payment"}, nil
}

type statusRecorder struct {
	fakeSubscribers
	set map[int]string
	exp map[int]time.Time
}

func (s *statusRecorder) UpdateSubscriber(_ context.Context, id int, _ *int, status *string, expiry *time.Time) (*api.SubscriberRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.set == nil {
		s.set, s.exp = map[int]string{}, map[int]time.Time{}
	}
	s.set[id], s.exp[id] = *status, *expiry
	return &api.SubscriberRecord{ID: id, Status: *status}, nil
}

type fakeTicketCreator struct {
	calls int
	err   error
}

func (f *fakeTicketCreator) CreateTicketAdmin(context.Context, int, string, string, *string) (*api.TicketRecord, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &api.TicketRecord{ID: 1}, nil
}

type fakeInvoiceSeeder struct {
	rateErr error
	got     *billing.Invoice
}

func (f *fakeInvoiceSeeder) GetActiveGstRate(context.Context) (billing.GstRate, error) {
	return billing.GstRate{ID: 1, CgstRate: d("0.09"), SgstRate: d("0.09"), IgstRate: d("0.18")}, f.rateErr
}

func (f *fakeInvoiceSeeder) CreateInvoice(_ context.Context, inv billing.Invoice) (int, error) {
	f.got = &inv
	return 55, nil
}

type demoRig struct {
	h       *Handler
	demo    *demoStoreFake
	cat     *fakeCatalogue
	prov    *fakeProvisioner
	subs    *statusRecorder
	tickets *fakeTicketCreator
	inv     *fakeInvoiceSeeder
	nas     *fakeNASStore
	enc     *recordingEncryptor
}

func newDemoRig() *demoRig {
	r := &demoRig{
		demo: &demoStoreFake{}, cat: newFakeCatalogue(), prov: &fakeProvisioner{},
		subs: &statusRecorder{}, tickets: &fakeTicketCreator{}, inv: &fakeInvoiceSeeder{},
		nas: &fakeNASStore{}, enc: &recordingEncryptor{},
	}
	r.h = &Handler{
		demo: r.demo, catalogue: r.cat, subscriberCreator: r.prov, subscribers: r.subs,
		ticketCreator: r.tickets, invoiceSeeder: r.inv, nas: r.nas, secretEncryptor: r.enc,
		gstSupplier: billing.Supplier{State: "TN"},
	}
	return r
}

func serveDemo(h *Handler, s Session, method, target string) (int, string) {
	r := httptest.NewRequest(method, target, nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	w := httptest.NewRecorder()
	switch target {
	case "/staff/demo/load":
		h.LoadDemoData(w, r)
	case "/staff/demo/remove":
		h.RemoveDemoData(w, r)
	default:
		h.Demo(w, r)
	}
	return w.Code, visibleText(w.Body.String())
}

func TestLoadingDemoDataCreatesEverythingItClaims(t *testing.T) {
	rig := newDemoRig()
	_, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load")

	want := "Demo data loaded: 2 plans, 5 subscribers (active, soft-suspended, hard-suspended), " +
		"1 support ticket, 1 invoice and 1 sample router."
	if !strings.Contains(body, want) {
		t.Fatalf("want the full confirmation: %s", firstLines(body))
	}
	if len(rig.cat.created) != 2 || len(rig.prov.created) != 5 || rig.tickets.calls != 1 ||
		rig.inv.got == nil || rig.nas.created == nil {
		t.Errorf("created plans=%d subscribers=%d tickets=%d invoice=%v router=%v",
			len(rig.cat.created), len(rig.prov.created), rig.tickets.calls, rig.inv.got != nil, rig.nas.created != nil)
	}
	// Everything it made is flagged, which is what Remove deletes by.
	if rig.demo.marked["plan"] != 2 || rig.demo.marked["subscriber"] != 5 || rig.demo.marked["nas"] != 1 {
		t.Errorf("flagged %v", rig.demo.marked)
	}
	// Each status agrees with its expiry, as migration 048 requires: a
	// suspended account is past its expiry, an active one is not.
	now := time.Now()
	for id, status := range rig.subs.set {
		past := rig.subs.exp[id].Before(now)
		if (status == "active") == past {
			t.Errorf("subscriber %d is %s with expiry %s", id, status, rig.subs.exp[id].Format("2006-01-02"))
		}
	}
	// The router's secret went to the encryptor, not the store.
	if rig.enc.got != demoSecret || rig.nas.created.SecretEncrypted == demoSecret {
		t.Error("the demo router's secret reached the store unencrypted")
	}
	// The invoice is intrastate, so it takes the CGST+SGST branch.
	if rig.inv.got.CgstAmount.IsZero() || !rig.inv.got.IgstAmount.IsZero() {
		t.Errorf("the demo invoice is not intrastate: %+v", rig.inv.got)
	}
}

// What did not happen is not reported as having happened.
func TestLoadingDemoDataReportsWhatWasSkipped(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*demoRig)
		says  string
		not   string
	}{
		"no GST rate":         {func(r *demoRig) { r.inv.rateErr = errDemo }, "the sample invoice", "1 invoice"},
		"no invoice store":    {func(r *demoRig) { r.h.invoiceSeeder = nil }, "the sample invoice", "1 invoice"},
		"ticket failed":       {func(r *demoRig) { r.tickets.err = errDemo }, "the support ticket", "1 support ticket"},
		"no ticket store":     {func(r *demoRig) { r.h.ticketCreator = nil }, "the support ticket", "1 support ticket"},
		"router failed":       {func(r *demoRig) { r.nas.createErr = errDemo }, "the sample router", "1 sample router"},
		"no NAS store":        {func(r *demoRig) { r.h.nas = nil }, "the sample router", "1 sample router"},
		"no encryptor":        {func(r *demoRig) { r.h.secretEncryptor = nil }, "the sample router", "1 sample router"},
		"encrypt failed":      {func(r *demoRig) { r.enc.err = errDemo }, "the sample router", "1 sample router"},
		"activation failed":   {func(r *demoRig) { r.subs.err = errDemo }, "still pending payment", "(active, soft-suspended"},
		"no subscriber store": {func(r *demoRig) { r.h.subscribers = nil }, "still pending payment", "(active, soft-suspended"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newDemoRig()
			tc.setup(rig)
			_, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load")
			if !strings.Contains(body, tc.says) {
				t.Errorf("want %q named as skipped: %s", tc.says, body)
			}
			if strings.Contains(body, tc.not) {
				t.Errorf("the page still claims %q", tc.not)
			}
		})
	}
}

// A row that could not be flagged survives Remove, so the operator is told.
func TestDemoRowsThatCouldNotBeFlaggedAreReported(t *testing.T) {
	rig := newDemoRig()
	rig.demo.markErr = errDemo
	_, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load")
	if !strings.Contains(body, "could not be marked as demo data") || !strings.Contains(body, "will not delete them") {
		t.Errorf("unflagged rows are not reported: %s", body)
	}
}

func TestLoadingDemoDataRefusals(t *testing.T) {
	rig := newDemoRig()
	rig.demo.status = DemoStatus{Subscribers: 5}
	if _, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load"); !strings.Contains(body, "already loaded") {
		t.Errorf("a second load: %s", firstLines(body))
	}
	if len(rig.prov.created) != 0 {
		t.Error("a second load created subscribers")
	}

	rig = newDemoRig()
	rig.demo.statusErr = errDemo
	if _, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load"); !strings.Contains(body, "Could not check") {
		t.Errorf("status failure: %s", firstLines(body))
	}

	rig = newDemoRig()
	rig.cat.err = errDemo
	if _, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load"); !strings.Contains(body, "Could not create demo plans") {
		t.Errorf("plan failure: %s", firstLines(body))
	}

	// A subscriber that fails part-way leaves what was made so far, which
	// Remove can clear; the page says so.
	rig = newDemoRig()
	rig.prov.failAt = 3
	_, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/load")
	if !strings.Contains(body, "Could not create demo subscribers") || !strings.Contains(body, "Remove demo data") {
		t.Errorf("a part-way failure: %s", firstLines(body))
	}

	if code, _ := serveDemo(&Handler{}, owner(), http.MethodPost, "/staff/demo/load"); code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured load: %d", code)
	}
}

func TestRemovingDemoData(t *testing.T) {
	rig := newDemoRig()
	if _, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/remove"); !strings.Contains(body, "Demo data removed") || !rig.demo.removed {
		t.Errorf("remove: %s", firstLines(body))
	}

	rig = newDemoRig()
	rig.demo.removeErr = ErrDemoDataInUse
	if _, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/remove"); !strings.Contains(body, "a real subscriber is on a demo plan") {
		t.Errorf("in use: %s", firstLines(body))
	}

	rig = newDemoRig()
	rig.demo.removeErr = errDemo
	if _, body := serveDemo(rig.h, owner(), http.MethodPost, "/staff/demo/remove"); !strings.Contains(body, "Could not remove demo data") {
		t.Errorf("failure: %s", firstLines(body))
	}
	if code, _ := serveDemo(&Handler{}, owner(), http.MethodPost, "/staff/demo/remove"); code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured remove: %d", code)
	}
}

func TestTheDemoScreen(t *testing.T) {
	rig := newDemoRig()
	if _, body := serveDemo(rig.h, owner(), http.MethodGet, "/staff/demo"); !strings.Contains(body, "No demo data loaded") {
		t.Errorf("empty: %s", firstLines(body))
	}
	rig.demo.status = DemoStatus{Subscribers: 5, Plans: 2, NASDevices: 1}
	if _, body := serveDemo(rig.h, owner(), http.MethodGet, "/staff/demo"); !strings.Contains(body, "Remove demo data") {
		t.Errorf("loaded: %s", firstLines(body))
	}
	rig.demo.statusErr = errDemo
	if _, body := serveDemo(rig.h, owner(), http.MethodGet, "/staff/demo"); !strings.Contains(body, "Could not read demo data status") {
		t.Errorf("status failure: %s", firstLines(body))
	}
	if _, body := serveDemo(&Handler{}, owner(), http.MethodGet, "/staff/demo"); !strings.Contains(body, "not configured") {
		t.Errorf("unconfigured: %s", firstLines(body))
	}
	// Owner only: seeding subscribers is not a billing task.
	for _, role := range []string{"billing_admin", "csr", "technician", "noc_engineer"} {
		if code, _ := serveDemo(rig.h, sess(role), http.MethodPost, "/staff/demo/load"); code != http.StatusForbidden {
			t.Errorf("%s could load demo data: %d", role, code)
		}
	}
}
