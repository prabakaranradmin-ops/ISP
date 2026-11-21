package staffui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Printing a Customer Application Form from the console — FR-DOC-002.

type fakeCAF struct {
	pdf    []byte
	err    error
	gotID  int
	gotWho string
	calls  int
}

func (f *fakeCAF) RenderCAFPDF(ctx context.Context, subscriberID int) ([]byte, error) {
	f.calls++
	f.gotID = subscriberID
	f.gotWho = middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return f.pdf, nil
}

func cafScreen() (*Handler, *fakeCAF) {
	f := &fakeCAF{pdf: []byte("%PDF-1.4 application form")}
	return &Handler{caf: f}, f
}

// Served as an attachment with the subscriber's id in the filename: a CAF
// carries contact details, and opening it inline leaves it in a browser's
// cache and history.
func TestFR_DOC_002_TheFormIsServedAsAnAttachment(t *testing.T) {
	h, f := cafScreen()

	w := servePR(h, "csr", "GET", "/staff/subscribers/42/caf", nil, h.SubscriberCAF)
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if f.gotID != 42 {
		t.Errorf("the form was printed for subscriber %d, want 42", f.gotID)
	}
	if !strings.Contains(w.Body.String(), "%PDF") {
		t.Errorf("the response is not a PDF: %q", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type is %q", got)
	}
	disp := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disp, "attachment;") {
		t.Errorf("Content-Disposition is %q, want an attachment", disp)
	}
	if !strings.Contains(disp, "application-form-42.pdf") {
		t.Errorf("the filename does not identify the subscriber: %q", disp)
	}
}

// Who printed it is recorded, because the form carries contact details and
// which identity documents are held. Without the subject on the context the
// audit entry would name nobody.
func TestFR_DOC_002_WhoPrintedTheFormIsPassedThrough(t *testing.T) {
	h, f := cafScreen()
	servePR(h, "billing_admin", "GET", "/staff/subscribers/42/caf", nil, h.SubscriberCAF)
	if f.gotWho == "" {
		t.Error("the printer's identity did not reach the service, so the audit entry would name nobody")
	}
}

// The front desk signs customers up and needs the form in hand; the network
// roles have no reason to hold a customer's contact details and KYC status.
func TestFR_DOC_002_OnlyTheRolesThatSignCustomersUpCanPrintIt(t *testing.T) {
	for _, role := range []string{"isp_owner", "billing_admin", "csr"} {
		h, f := cafScreen()
		w := servePR(h, role, "GET", "/staff/subscribers/42/caf", nil, h.SubscriberCAF)
		if w.Code != 200 {
			t.Errorf("%s printing a form got %d, want 200", role, w.Code)
		}
		if f.calls != 1 {
			t.Errorf("%s: the service was called %d times", role, f.calls)
		}
	}

	for _, role := range []string{"noc_engineer", "technician"} {
		h, f := cafScreen()
		w := servePR(h, role, "GET", "/staff/subscribers/42/caf", nil, h.SubscriberCAF)
		if w.Code != 403 {
			t.Errorf("%s printing a form got %d, want 403", role, w.Code)
		}
		if f.calls != 0 {
			t.Errorf("%s reached the service despite the refusal", role)
		}
	}
}

func TestFR_DOC_002_FailuresAreReportedDistinctly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		says string
	}{
		{"an unknown subscriber", api.ErrCAFSubscriberNotFound, 404, "No subscriber with that id"},
		{"no browser on the server", api.ErrCAFUnavailable, 503, "no Chromium-based browser"},
		{"anything else", errors.New("boom"), 500, "could not be printed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := cafScreen()
			h.caf.(*fakeCAF).err = tc.err
			w := servePR(h, "csr", "GET", "/staff/subscribers/42/caf", nil, h.SubscriberCAF)
			if w.Code != tc.code {
				t.Errorf("got %d, want %d", w.Code, tc.code)
			}
			if !strings.Contains(w.Body.String(), tc.says) {
				t.Errorf("the page does not explain the problem (wanted %q)", tc.says)
			}
		})
	}
}

func TestFR_DOC_002_BadIdsAndAnUnwiredPrinterAreRefusedCleanly(t *testing.T) {
	for _, bad := range []string{"notanumber", "0", "-1"} {
		h, f := cafScreen()
		w := servePR(h, "csr", "GET", "/staff/subscribers/"+bad+"/caf", nil, h.SubscriberCAF)
		if w.Code != 400 {
			t.Errorf("id %q got %d, want 400", bad, w.Code)
		}
		if f.calls != 0 {
			t.Errorf("id %q reached the service", bad)
		}
	}

	// No printer wired: a readable 503 rather than a nil dereference.
	h := &Handler{}
	w := servePR(h, "csr", "GET", "/staff/subscribers/42/caf", nil, h.SubscriberCAF)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}
