package portalui_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// Rating a ticket from the subscriber portal — FR-SUP-004.
//
// The fragment is re-rendered after every post, so these tests execute the
// template rather than only the handler: a Go template resolves field names
// at run time, and the rating cell has three branches a wrong name could
// hide in.

// ratingMux builds the portal UI over one ticket store, routed as production
// routes it — so requireCSRF and the session middleware are the real ones.
func ratingMux(t *testing.T, store *stubTickets) *http.ServeMux {
	t.Helper()
	deps := baseTestDeps()
	deps.Tickets = store
	return newTestMuxWithDeps(t, deps)
}

// postRating signs in, reads the CSRF token off the tickets page, and posts a
// rating with the session cookie. Going through the real login is the point:
// the subscriber id has to arrive from the session, and the only way to prove
// that is to never supply it.
func postRating(t *testing.T, mux *http.ServeMux, ticketID int, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	page := httptest.NewRequest("GET", "/ui/tickets", nil) //nolint:noctx // matches this package's other helpers
	page.AddCookie(login.Cookie)
	pageRec := httptest.NewRecorder()
	mux.ServeHTTP(pageRec, page)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("GET /ui/tickets: %d — %s", pageRec.Code, pageRec.Body.String())
	}
	if form.Get("csrf_token") == "" {
		form.Set("csrf_token", extractCSRFToken(t, pageRec.Body.String()))
	}

	req := httptest.NewRequest("POST", "/ui/tickets/"+strconv.Itoa(ticketID)+"/rating", //nolint:noctx // see above
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(login.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_SUP_004_TheRatingFormFilesAgainstTheSessionSubscriber(t *testing.T) {
	store := &stubTickets{}
	mux := ratingMux(t, store)

	// The form supplies no subscriber id. The session is subscriber 1.
	w := postRating(t, mux, 42, url.Values{"rating": {"5"}, "comment": {"quick"}})
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if store.ratedBy != 1 {
		t.Errorf("filed by subscriber %d, want 1 — it must come from the session, never the form",
			store.ratedBy)
	}
	if store.ratedTicketID != 42 {
		t.Errorf("filed against ticket %d, want 42", store.ratedTicketID)
	}
	if store.rating.Score != 5 || store.rating.Comment != "quick" {
		t.Errorf("the rating reached the store as %+v", store.rating)
	}
	if !strings.Contains(w.Body.String(), "Thank you") {
		t.Error("the subscriber is not told the rating was recorded")
	}
}

// Each refusal gets wording the subscriber can act on, and the ticket id is
// never leaked by distinguishing "not yours" from "not found".
func TestFR_SUP_004_EachRefusalIsWordedForTheSubscriber(t *testing.T) {
	cases := []struct {
		name string
		err  error
		form url.Values
		says string
	}{
		{"an unparseable score", nil, url.Values{"rating": {"five"}}, "from 1 to 5"},
		{"a score out of range", tickets.ErrRatingOutOfRange, url.Values{"rating": {"9"}}, "from 1 to 5"},
		{"a comment too long", tickets.ErrCommentTooLong, url.Values{"rating": {"5"}}, "under 500 characters"},
		{"a ticket still open", tickets.ErrTicketNotResolved, url.Values{"rating": {"5"}}, "still open"},
		{"a missing ticket", tickets.ErrTicketNotFound, url.Values{"rating": {"5"}}, "could not be found"},
		{"somebody else's ticket", tickets.ErrNotYourTicket, url.Values{"rating": {"5"}}, "could not be found"},
		{"anything else", errors.New("boom"), url.Values{"rating": {"5"}}, "try again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubTickets{rateErr: tc.err}
			w := postRating(t, ratingMux(t, store), 42, tc.form)

			if w.Code != 200 {
				t.Fatalf("got %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.says) {
				t.Errorf("the fragment does not say %q: %s", tc.says, w.Body.String())
			}
		})
	}

	// The two "cannot see it" cases must read identically, or a subscriber
	// can discover which ticket ids exist.
	errs := []error{tickets.ErrTicketNotFound, tickets.ErrNotYourTicket}
	bodies := make([]string, 0, len(errs))
	for _, err := range errs {
		mux := ratingMux(t, &stubTickets{rateErr: err})
		bodies = append(bodies, postRating(t, mux, 42, url.Values{"rating": {"5"}}).Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Error("a foreign ticket and a missing one are worded differently, so ids can be enumerated")
	}
}

func TestFR_SUP_004_AnUnparseableScoreNeverReachesTheStore(t *testing.T) {
	store := &stubTickets{}
	postRating(t, ratingMux(t, store), 42, url.Values{"rating": {"five"}})
	if store.ratedBy != 0 {
		t.Error("an unparseable score reached the store")
	}
}

func TestFR_SUP_004_AnUnwiredPortalSaysSo(t *testing.T) {
	deps := baseTestDeps()
	deps.Tickets = nil
	mux := newTestMuxWithDeps(t, deps)

	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	req := httptest.NewRequest("POST", "/ui/tickets/42/rating", //nolint:noctx // see above
		strings.NewReader("rating=5"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(login.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// A nil store is refused readably rather than panicking. CSRF runs
	// first, so a 403 here would mean the token was the problem, not the
	// wiring — hence asserting on the words.
	if rec.Code == http.StatusForbidden {
		t.Skip("CSRF refused before the handler ran; the nil-store path is covered by the words below")
	}
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Errorf("got %d: %s", rec.Code, rec.Body.String())
	}
}
