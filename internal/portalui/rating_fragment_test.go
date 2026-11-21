package portalui

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// The rating cell of the tickets fragment — FR-SUP-004.
//
// In the internal test package because renderFragment is unexported. These
// execute the template with real data: a Go template resolves field names at
// run time, so a wrong name compiles cleanly and fails only here, and the
// rating cell has three branches to hide in.

func itoa(n int) string { return strconv.Itoa(n) }

// Three states, each rendered with real data. A wrong field name in any
// branch compiles cleanly and fails only here.
func TestFR_SUP_004_TheFragmentRendersEveryRatingState(t *testing.T) {
	four := 4
	now := time.Now()

	cases := []struct {
		name     string
		ticket   portal.TicketEntry
		wants    []string
		notWants []string
	}{
		{
			name: "resolved and unrated offers the form",
			ticket: portal.TicketEntry{
				ID: 1, Category: "billing", Description: "overcharged",
				Status: "resolved", CreatedAt: now,
			},
			wants:    []string{"/ui/tickets/1/rating", "How did we do?", `name="rating"`},
			notWants: []string{"once it is resolved"},
		},
		{
			name: "rated shows the stars and a way to change them",
			ticket: portal.TicketEntry{
				ID: 2, Category: "billing", Description: "overcharged",
				Status: "resolved", CreatedAt: now,
				Rating: &four, RatedAt: &now, RatingComment: "sorted quickly",
			},
			// Four filled, one empty.
			wants:    []string{"★★★★☆", "sorted quickly", "Update rating"},
			notWants: []string{"How did we do?"},
		},
		{
			name: "open offers nothing and says why",
			ticket: portal.TicketEntry{
				ID: 3, Category: "billing", Description: "overcharged",
				Status: "open", CreatedAt: now,
			},
			wants:    []string{"once it is resolved"},
			notWants: []string{"/ui/tickets/3/rating"},
		},
		{
			name: "rated then reopened keeps the rating and offers no form",
			ticket: portal.TicketEntry{
				ID: 4, Category: "billing", Description: "overcharged",
				Status: "open", CreatedAt: now,
				Rating: &four, RatedAt: &now,
			},
			wants:    []string{"★★★★☆", "once it is resolved"},
			notWants: []string{"/ui/tickets/4/rating", "Update rating"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			renderFragment(w, "tickets-list", ticketsListData{
				Tickets:   []portal.TicketEntry{tc.ticket},
				CSRFToken: "tok",
			})
			body := w.Body.String()

			if strings.Contains(body, "<no value>") {
				t.Error("the fragment rendered <no value>, so a template field name does not resolve")
			}
			for _, want := range tc.wants {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q:\n%s", want, body)
				}
			}
			for _, not := range tc.notWants {
				if strings.Contains(body, not) {
					t.Errorf("unexpectedly contains %q", not)
				}
			}
			// The form posts, so it needs the token — and the token has to be
			// on the fragment, which replaces itself after every post.
			if strings.Contains(body, "/rating") && !strings.Contains(body, `value="tok"`) {
				t.Error("the rating form carries no CSRF token, so the second rating would be refused")
			}
		})
	}
}

// The score options come from the domain rather than the markup, so the form
// cannot offer a score the store would refuse.
func TestFR_SUP_004_TheFormOffersExactlyTheValidScores(t *testing.T) {
	w := httptest.NewRecorder()
	renderFragment(w, "tickets-list", ticketsListData{
		Tickets:   []portal.TicketEntry{{ID: 1, Status: "resolved", CreatedAt: time.Now()}},
		CSRFToken: "tok",
	})
	body := w.Body.String()

	for n := tickets.MinRating; n <= tickets.MaxRating; n++ {
		if !strings.Contains(body, `<option value="`+itoa(n)+`">`) {
			t.Errorf("the form does not offer a score of %d", n)
		}
	}
	for _, bad := range []int{0, 6} {
		if strings.Contains(body, `<option value="`+itoa(bad)+`">`) {
			t.Errorf("the form offers %d, which the store would refuse", bad)
		}
	}
}

// Unrated must not render as zero stars, which reads as a score given.
func TestFR_SUP_004_AnUnratedTicketRendersNoStars(t *testing.T) {
	w := httptest.NewRecorder()
	renderFragment(w, "tickets-list", ticketsListData{
		Tickets: []portal.TicketEntry{{ID: 1, Status: "resolved", CreatedAt: time.Now()}},
	})
	if strings.Contains(w.Body.String(), "☆") {
		t.Error("an unrated ticket rendered empty stars, which reads as a rating of zero")
	}
}
