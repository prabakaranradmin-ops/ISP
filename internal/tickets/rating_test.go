package tickets

import (
	"errors"
	"strings"
	"testing"
)

// Subscriber ratings — FR-SUP-004.

func TestFR_SUP_004_AScoreMustBeOneToFive(t *testing.T) {
	for _, score := range []int{1, 2, 3, 4, 5} {
		if err := (Rating{Score: score}).Validate(); err != nil {
			t.Errorf("score %d was refused: %v", score, err)
		}
	}
	// Zero especially: it would be indistinguishable from "not rated" in
	// every average, and an unrated ticket is already NULL.
	for _, score := range []int{0, -1, 6, 100} {
		if err := (Rating{Score: score}).Validate(); !errors.Is(err, ErrRatingOutOfRange) {
			t.Errorf("score %d was accepted (err = %v)", score, err)
		}
	}
}

func TestFR_SUP_004_TheCommentLimitIsCharactersNotBytes(t *testing.T) {
	// A Tamil comment is three bytes per character. A byte limit would
	// refuse a short complaint in the customer's own language while
	// accepting a long one in English — which is the wrong way round for an
	// ISP in Tamil Nadu.
	tamil := strings.Repeat("அ", MaxRatingComment)
	if n := len(tamil); n <= MaxRatingComment {
		t.Fatalf("the fixture is only %d bytes; it cannot prove anything", n)
	}
	if err := (Rating{Score: 5, Comment: tamil}).Validate(); err != nil {
		t.Errorf("a %d-character Tamil comment was refused: %v", MaxRatingComment, err)
	}

	tooLong := strings.Repeat("a", MaxRatingComment+1)
	if err := (Rating{Score: 5, Comment: tooLong}).Validate(); !errors.Is(err, ErrCommentTooLong) {
		t.Errorf("a comment one character over the limit was accepted: %v", err)
	}
}

func TestFR_SUP_004_NormaliseTrimsTheComment(t *testing.T) {
	r := Rating{Score: 4, Comment: "  slow but sorted  "}.Normalise()
	if r.Comment != "slow but sorted" {
		t.Errorf("Comment = %q", r.Comment)
	}
	// A comment of only whitespace is no comment, so it must not be stored
	// as one — the store's NULLIF depends on this having happened first.
	if got := (Rating{Score: 4, Comment: "   "}).Normalise().Comment; got != "" {
		t.Errorf("a whitespace-only comment normalised to %q", got)
	}
}

// Closed as well as resolved. A ticket a CSR closed after the customer
// stopped replying is still work the customer can have an opinion about, and
// refusing the rating there would lose the feedback most worth having.
func TestFR_SUP_004_OnlyFinishedTicketsCanBeRated(t *testing.T) {
	for _, status := range []string{"resolved", "closed"} {
		if !Ratable(status) {
			t.Errorf("a %s ticket cannot be rated", status)
		}
	}
	for _, status := range []string{"open", "in_progress", "", "nonsense"} {
		if Ratable(status) {
			t.Errorf("a %q ticket can be rated, which rates a resolution that has not happened", status)
		}
	}
}

// ── The summary ─────────────────────────────────────────────────────────────

func TestFR_SUP_004_TheResponseRateIsNilWhenThereWasNothingToRate(t *testing.T) {
	// Not zero: 0% claims nobody replied, where the truth is that nobody was
	// asked.
	if got := (RatingSummary{}).ResponseRate(); got != nil {
		t.Errorf("ResponseRate = %v for an empty window, want nil", *got)
	}
	s := RatingSummary{Rated: 7, Ratable: 28}
	got := s.ResponseRate()
	if got == nil || *got < 24.9 || *got > 25.1 {
		t.Errorf("ResponseRate = %v, want 25", got)
	}
}

// Ones and twos together: a two is a complaint, not a middling review, and
// grouping them is what makes the follow-up list the right length.
func TestFR_SUP_004_NegativeCountsOnesAndTwos(t *testing.T) {
	s := RatingSummary{Distribution: [MaxRating]int{3, 2, 10, 20, 40}}
	if got := s.Negative(); got != 5 {
		t.Errorf("Negative = %d, want 5", got)
	}
	if got := (RatingSummary{}).Negative(); got != 0 {
		t.Errorf("Negative = %d for an empty summary", got)
	}
}

// The distribution is the point: twenty 5s and five 1s averages the same as
// twenty-five 4s and is a completely different support operation.
func TestFR_SUP_004_TheDistributionIsKeptBesideTheAverage(t *testing.T) {
	polarised := RatingSummary{Distribution: [MaxRating]int{5, 0, 0, 0, 20}}
	steady := RatingSummary{Distribution: [MaxRating]int{0, 0, 0, 25, 0}}

	if polarised.Negative() == steady.Negative() {
		t.Error("a polarised month and a steady one report the same negative count, so the " +
			"distribution adds nothing")
	}
	var total int
	for _, n := range polarised.Distribution {
		total += n
	}
	if total != 25 {
		t.Errorf("the distribution sums to %d, want 25", total)
	}
}
