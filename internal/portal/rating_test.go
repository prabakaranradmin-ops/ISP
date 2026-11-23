package portal_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// Rating a resolved ticket from the portal — FR-SUP-004.

// ratingMux builds a handler over one ticket store, routed as production
// routes it — so the {id} wildcard and the session middleware are the real
// ones rather than a shape invented for the test.
func ratingMux(store *stubTickets) *http.ServeMux {
	h := portal.NewHandler(&stubSubDB{}, &stubSessions{}, &stubNotifs{}, store, nil,
		"test-portal-secret")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

// loginToken gets a real token the way the other authenticated tests here
// do. stubSubDB authenticates testuser as subscriber 1, so that is the
// subscriber every rating below is filed by.
func loginToken(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/portal/login", //nolint:noctx // matches this file's other calls
		strings.NewReader(`{"username":"testuser","password":"testpass"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login for a token: %d — %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if resp["token"] == "" {
		t.Fatal("login returned no token")
	}
	return resp["token"]
}

func rateCall(t *testing.T, mux *http.ServeMux, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)) //nolint:noctx // see above
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// The security property the feature turns on: a rating is filed against the
// session's subscriber, never one named in the request. Without this a
// subscriber could rate — and so comment on — somebody else's ticket.
func TestFR_SUP_004_TheRatingIsFiledAgainstTheSessionNotTheBody(t *testing.T) {
	store := &stubTickets{}
	mux := ratingMux(store)
	token := loginToken(t, mux)

	// The body names subscriber 99. The session is subscriber 1.
	rec := rateCall(t, mux, token, "/portal/tickets/7/rating",
		`{"rating":5,"comment":"quick fix","subscriber_id":99}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}

	if store.ratedBy != 1 {
		t.Errorf("the rating was filed by subscriber %d; it must come from the session, "+
			"not from the body (which said 99)", store.ratedBy)
	}
	if store.ratedTicketID != 7 {
		t.Errorf("the rating was filed against ticket %d, want 7", store.ratedTicketID)
	}
	if store.rating.Score != 5 || store.rating.Comment != "quick fix" {
		t.Errorf("the rating reached the store as %+v", store.rating)
	}
}

func TestFR_SUP_004_AnUnauthenticatedCallerIsRefused(t *testing.T) {
	store := &stubTickets{}
	mux := ratingMux(store)

	// No Authorization header at all.
	rec := rateCall(t, mux, "", "/portal/tickets/7/rating", `{"rating":5}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rec.Code)
	}
	if store.ratedBy != 0 {
		t.Error("an unauthenticated request reached the store")
	}
}

// Each failure needs its own answer: a malformed score, an unfinished
// ticket and an unknown ticket are three different things for a client to
// do something about.
func TestFR_SUP_004_EachFailureIsReportedDistinctly(t *testing.T) {
	cases := []struct {
		name string
		err  error
		body string
		code int
		says string
	}{
		{"a score outside the range", tickets.ErrRatingOutOfRange, `{"rating":9}`, 422, "1 to 5"},
		{"a comment that is too long", tickets.ErrCommentTooLong, `{"rating":5}`, 422, "characters"},
		{"a ticket that is still open", tickets.ErrTicketNotResolved, `{"rating":5}`, 409, "resolved"},
		{"a ticket that does not exist", tickets.ErrTicketNotFound, `{"rating":5}`, 404, "no such ticket"},
		{"somebody else's ticket", tickets.ErrNotYourTicket, `{"rating":5}`, 404, "no such ticket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubTickets{rateErr: tc.err}
			mux := ratingMux(store)
			rec := rateCall(t, mux, loginToken(t, mux), "/portal/tickets/7/rating", tc.body)
			if rec.Code != tc.code {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(tc.says)) {
				t.Errorf("the response does not explain the problem (wanted %q): %s",
					tc.says, rec.Body.String())
			}
		})
	}
}

// An unknown ticket and somebody else's ticket must be indistinguishable, or
// a subscriber can enumerate which ticket ids exist by rating them.
func TestFR_SUP_004_AForeignTicketLooksExactlyLikeAMissingOne(t *testing.T) {
	errs := []error{tickets.ErrTicketNotFound, tickets.ErrNotYourTicket}
	bodies := make([]string, 0, len(errs))
	for _, err := range errs {
		store := &stubTickets{rateErr: err}
		mux := ratingMux(store)
		rec := rateCall(t, mux, loginToken(t, mux), "/portal/tickets/7/rating", `{"rating":5}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%v gave %d, want 404", err, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Errorf("the two answers differ, so ticket ids can be enumerated:\n%s\n%s",
			bodies[0], bodies[1])
	}
}

func TestFR_SUP_004_ABadTicketIdIsRefusedBeforeTheStore(t *testing.T) {
	for _, id := range []string{"abc", "0", "-1"} {
		store := &stubTickets{}
		mux := ratingMux(store)
		rec := rateCall(t, mux, loginToken(t, mux), "/portal/tickets/"+id+"/rating", `{"rating":5}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id %q gave %d, want 400", id, rec.Code)
		}
		if store.ratedBy != 0 {
			t.Errorf("id %q reached the store", id)
		}
	}
}

// ── The list ────────────────────────────────────────────────────────────────

// CanRate is computed from the status rather than stored, so it cannot go
// stale against a ticket that was reopened.
func TestFR_SUP_004_CanRateFollowsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"resolved", true}, {"closed", true},
		{"open", false}, {"in_progress", false},
	} {
		got := portal.TicketEntry{Status: tc.status}.CanRate()
		if got != tc.want {
			t.Errorf("CanRate for %q = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// A reopened ticket keeps the rating of the resolution that was undone, and
// stops being ratable until it is resolved again. Rated and CanRate are
// therefore different questions, and a screen that used one for the other
// would either hide the feedback or offer to re-rate work in progress.
func TestFR_SUP_004_RatedAndCanRateAreDifferentQuestions(t *testing.T) {
	five := 5
	reopened := portal.TicketEntry{Status: "open", Rating: &five}

	if !reopened.Rated() {
		t.Error("a reopened ticket lost the rating of its previous resolution")
	}
	if reopened.CanRate() {
		t.Error("a reopened ticket can be rated again before it is resolved")
	}

	fresh := portal.TicketEntry{Status: "resolved"}
	if fresh.Rated() {
		t.Error("an unrated ticket reports a rating")
	}
	if !fresh.CanRate() {
		t.Error("a resolved ticket cannot be rated")
	}
}

// The rating travels on the JSON the portal already returns, so a client
// does not need a second call to show it.
func TestFR_SUP_004_TheRatingIsCarriedOnTheTicketJSON(t *testing.T) {
	four := 4
	body, err := json.Marshal(portal.TicketEntry{
		ID: 1, Status: "resolved", Rating: &four, RatingComment: "sorted quickly",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"rating":4`, `"rating_comment":"sorted quickly"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the ticket JSON is missing %s: %s", want, body)
		}
	}

	// An unrated ticket omits them rather than reporting zero, which would
	// read as the worst possible score.
	body, _ = json.Marshal(portal.TicketEntry{ID: 2, Status: "open"})
	if strings.Contains(string(body), `"rating"`) {
		t.Errorf("an unrated ticket carries a rating field: %s", body)
	}
}
