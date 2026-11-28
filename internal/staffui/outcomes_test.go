package staffui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Every outcome a catalogue screen can redirect with has a sentence to show.
//
// The IP pool, product, bandwidth and fair-use screens report a write's
// outcome by redirecting with a short code (?p=exhausted, ?m=inuse) that the
// list page looks up in its own message table. The mapper and the table are
// two lists kept by hand. A code the mapper returns and the table lacks
// redirects back to a page that says nothing at all — the write failed, and
// the operator sees the list as though it had not been submitted. That is
// the registry-drift shape this codebase has been bitten by before.
//
// Each mapper is fed every error it names, wrapped the way the store wraps
// them, plus an unknown one; every code that comes back must be in the
// table, or carry its own sentence ("invalid:...").

// wrapped is how the db layer returns a sentinel.
func wrapped(err error) error { return fmt.Errorf("db: doing the thing: %w", err) }

func TestEveryIPPoolOutcomeHasASentence(t *testing.T) {
	_, badRange := nas.ParseAddrRange("203.0.113.9", "203.0.113.1")
	for _, err := range []error{
		nas.ErrPoolNameTaken, nas.ErrPoolInUse, nas.ErrPoolNotStatic, nas.ErrPoolExhausted,
		nas.ErrAddressTaken, nas.ErrAddressExcluded, nas.ErrAddressOutsidePool,
		nas.ErrSubscriberHasAddress, nas.ErrAssignmentNotActive, nas.ErrPoolNotFound,
		nas.ErrAssignmentNotFound, badRange, errors.New("connection reset"),
	} {
		code := ipPoolErrCode(wrapped(err))
		if reason, ok := strings.CutPrefix(code, "invalid:"); ok {
			if reason == "" {
				t.Errorf("%v maps to an empty validation sentence", err)
			}
			continue
		}
		if _, ok := ipPoolMessages[code]; !ok {
			t.Errorf("%v maps to %q, which ipPoolMessages has no sentence for", err, code)
		}
		// And the sentence an operator reads is that one, not the fallback.
		if code != "failed" && ipPoolReason(wrapped(err)) != ipPoolMessages[code] {
			t.Errorf("%v: ipPoolReason does not use the table's sentence", err)
		}
	}
	if releaseCode(nil) != "released" || ipPoolMessages["released"] == "" {
		t.Error("a successful release has no sentence")
	}
	if got := ipPoolReason(errors.New("connection reset")); !strings.Contains(got, "Nothing was changed") {
		t.Errorf("an unknown failure reads %q", got)
	}
}

func TestEveryProductOutcomeHasASentence(t *testing.T) {
	_, invalid := billing.Product{Code: "", Name: ""}.Normalise()
	for _, err := range []error{
		billing.ErrProductCodeTaken, billing.ErrProductInactive, billing.ErrProductInUse,
		billing.ErrAttachmentNotActive, billing.ErrProductNotFound, billing.ErrAttachmentNotFound,
		invalid, errors.New("connection reset"),
	} {
		code := productErrCode(wrapped(err))
		if reason, ok := strings.CutPrefix(code, "invalid:"); ok {
			if reason == "" {
				t.Errorf("%v maps to an empty validation sentence", err)
			}
			continue
		}
		if _, ok := productMessages[code]; !ok {
			t.Errorf("%v maps to %q, which productMessages has no sentence for", err, code)
		}
	}
	if endCode(nil) != "ended" || productMessages["ended"] == "" {
		t.Error("an ended charge has no sentence")
	}
}

// outcomeOf runs a redirecting failure mapper and returns the code it chose.
func outcomeOf(t *testing.T, fail func(http.ResponseWriter, *http.Request)) string {
	t.Helper()
	w := httptest.NewRecorder()
	fail(w, httptest.NewRequest(http.MethodPost, "/staff/x", nil))
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != http.StatusSeeOther {
		t.Fatalf("not a redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	return loc.Query().Get("m")
}

func TestEveryBandwidthAndFairUseOutcomeHasASentence(t *testing.T) {
	h := &Handler{}
	for _, err := range []error{
		nas.ErrProfileNameTaken, nas.ErrProfileInUse,
		errors.New("db: bandwidth profile 9 not found"), errors.New("connection reset"),
	} {
		code := outcomeOf(t, func(w http.ResponseWriter, r *http.Request) { h.bandwidthFailure(w, r, wrapped(err)) })
		if _, ok := bandwidthMessages[code]; !ok {
			t.Errorf("bandwidth: %v maps to %q, which has no sentence", err, code)
		}
		if bandwidthSuccess[code] {
			t.Errorf("bandwidth: the failure %v is reported as a success (%q)", err, code)
		}
	}
	for _, err := range []error{
		fup.ErrPolicyNameTaken, fup.ErrPolicyInUse,
		errors.New("db: fup policy 9 not found"), errors.New("connection reset"),
	} {
		code := outcomeOf(t, func(w http.ResponseWriter, r *http.Request) { h.fupPolicyFailure(w, r, wrapped(err)) })
		if _, ok := fupPolicyMessages[code]; !ok {
			t.Errorf("fair use: %v maps to %q, which has no sentence", err, code)
		}
		if fupPolicySuccess[code] {
			t.Errorf("fair use: the failure %v is reported as a success (%q)", err, code)
		}
	}
}

// Every success code is in its table too, and no failure code is marked a
// success: the success set decides whether the banner is green.
func TestSuccessCodesAreSentencesAndOnlySuccesses(t *testing.T) {
	for name, tc := range map[string]struct {
		messages map[string]string
		success  map[string]bool
	}{
		"ip pools":  {ipPoolMessages, ipPoolSuccess},
		"products":  {productMessages, productSuccess},
		"bandwidth": {bandwidthMessages, bandwidthSuccess},
		"fair use":  {fupPolicyMessages, fupPolicySuccess},
	} {
		for code := range tc.success {
			if tc.messages[code] == "" {
				t.Errorf("%s: success code %q has no sentence", name, code)
			}
		}
		for _, failure := range []string{"failed", "missing", "duplicate", "inuse"} {
			if tc.success[failure] {
				t.Errorf("%s: %q is marked a success", name, failure)
			}
		}
	}
}
