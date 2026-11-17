//go:build integration

// Per-subscriber grace and dunning timing — FR-BIL-016/017 | migration 055.
//
// Defended here: an extension needs a reason and a short, future end date;
// who set it is taken from the token, not the body; a CSR cannot set either;
// and a policy the ladder could not follow is refused before it is stored.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

type stubDunning struct {
	policy        billing.DunningPolicy
	policySavedBy string
	policySaves   int
	graceID       int
	graceUntil    time.Time
	graceReason   string
	graceBy       string
	graceErr      error
	cleared       int
}

func (s *stubDunning) DunningPolicy(context.Context) (billing.DunningPolicy, error) {
	return s.policy, nil
}

func (s *stubDunning) SetDunningPolicy(_ context.Context, p billing.DunningPolicy, by string) error {
	s.policy, s.policySavedBy = p, by
	s.policySaves++
	return nil
}

func (s *stubDunning) SetGraceExtension(_ context.Context, id int, until time.Time, reason, by string) error {
	if s.graceErr != nil {
		return s.graceErr
	}
	s.graceID, s.graceUntil, s.graceReason, s.graceBy = id, until, reason, by
	return nil
}

func (s *stubDunning) ClearGraceExtension(_ context.Context, id int) error {
	s.cleared = id
	return nil
}

func newGraceHarness(t *testing.T) (*http.ServeMux, *stubDunning) {
	t.Helper()
	d := &stubDunning{policy: billing.DefaultDunningPolicy()}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Dunning: d,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux, d
}

func TestFR_BIL_016_GraceExtensionAPI(t *testing.T) {
	inFour := time.Now().AddDate(0, 0, 4).Format("2006-01-02")

	t.Run("an admin extends grace; who set it comes from the token", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		body := `{"until":"` + inFour + `","reason":"salary on Friday","set_by":"someone-else"}`
		code, resp := cnDo(t, mux, "POST", "/api/v1/subscribers/7/grace", body, "billing_admin", "meena")
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, resp)
		}
		if d.graceID != 7 || d.graceReason != "salary on Friday" || d.graceBy != "meena" {
			t.Fatalf("stored: id=%d reason=%q by=%q", d.graceID, d.graceReason, d.graceBy)
		}
		// A bare date means the end of that day, not its first second.
		if h, m := d.graceUntil.Hour(), d.graceUntil.Minute(); h != 23 || m != 59 {
			t.Errorf("a date should run to the end of the day, got %s", d.graceUntil)
		}
	})

	for name, body := range map[string]string{
		"no reason":    `{"until":"` + inFour + `","reason":"  "}`,
		"in the past":  `{"until":"2020-01-01","reason":"x"}`,
		"too long":     `{"until":"` + time.Now().AddDate(0, 0, 45).Format("2006-01-02") + `","reason":"x"}`,
		"not a date":   `{"until":"friday","reason":"x"}`,
		"missing date": `{"reason":"x"}`,
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			mux, d := newGraceHarness(t)
			code, resp := cnDo(t, mux, "POST", "/api/v1/subscribers/7/grace", body, "billing_admin", "meena")
			if code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d: %s", code, resp)
			}
			if d.graceID != 0 {
				t.Error("nothing may be stored")
			}
		})
	}

	t.Run("a subscriber not in dunning is a conflict, not a server error", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		d.graceErr = billing.ErrGraceNotApplicable
		code, resp := cnDo(t, mux, "POST", "/api/v1/subscribers/7/grace", `{"until":"`+inFour+`","reason":"x"}`, "billing_admin", "meena")
		if code != http.StatusConflict || !strings.Contains(resp, "ERR_NOT_IN_DUNNING") {
			t.Errorf("want 409 ERR_NOT_IN_DUNNING, got %d: %s", code, resp)
		}
	})

	t.Run("ending an extension clears it", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		if code, resp := cnDo(t, mux, "DELETE", "/api/v1/subscribers/7/grace", ``, "billing_admin", "meena"); code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, resp)
		}
		if d.cleared != 7 {
			t.Errorf("cleared %d", d.cleared)
		}
	})

	t.Run("a CSR can do neither", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		if code, _ := cnDo(t, mux, "POST", "/api/v1/subscribers/7/grace", `{"until":"`+inFour+`","reason":"x"}`, "csr", "divya"); code != http.StatusForbidden {
			t.Errorf("extend: want 403, got %d", code)
		}
		if code, _ := cnDo(t, mux, "DELETE", "/api/v1/subscribers/7/grace", ``, "csr", "divya"); code != http.StatusForbidden {
			t.Errorf("end: want 403, got %d", code)
		}
		if d.graceID != 0 || d.cleared != 0 {
			t.Error("a CSR changed grace")
		}
	})
}

func TestFR_BIL_017_DunningPolicyAPI(t *testing.T) {
	t.Run("read returns the ladder in force", func(t *testing.T) {
		mux, _ := newGraceHarness(t)
		code, resp := cnDo(t, mux, "GET", "/api/v1/billing/dunning-policy", ``, "billing_admin", "meena")
		if code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, resp)
		}
		var got map[string]any
		_ = json.Unmarshal([]byte(resp), &got)
		if got["remind_first_days"] != float64(7) || got["grace_days"] != float64(3) {
			t.Errorf("policy: %v", got)
		}
	})

	t.Run("a valid change is stored, attributed to the caller", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		body := `{"remind_first_days":10,"remind_second_days":5,"remind_last_days":2,"grace_days":7,"soft_suspend_days":5}`
		if code, resp := cnDo(t, mux, "PUT", "/api/v1/billing/dunning-policy", body, "billing_admin", "meena"); code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", code, resp)
		}
		if d.policy.GraceDays != 7 || d.policy.RemindFirstDays != 10 || d.policySavedBy != "meena" {
			t.Errorf("stored %+v by %q", d.policy, d.policySavedBy)
		}
	})

	t.Run("a ladder that does not count down is refused with the reason", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		body := `{"remind_first_days":3,"remind_second_days":7,"remind_last_days":1,"grace_days":3,"soft_suspend_days":3}`
		code, resp := cnDo(t, mux, "PUT", "/api/v1/billing/dunning-policy", body, "billing_admin", "meena")
		if code != http.StatusUnprocessableEntity || !strings.Contains(resp, "count down") {
			t.Errorf("want 422 naming the rule, got %d: %s", code, resp)
		}
		if d.policySaves != 0 {
			t.Error("an invalid policy was stored")
		}
	})

	t.Run("a CSR cannot change it", func(t *testing.T) {
		mux, d := newGraceHarness(t)
		body := `{"remind_first_days":7,"remind_second_days":3,"remind_last_days":1,"grace_days":30,"soft_suspend_days":3}`
		if code, _ := cnDo(t, mux, "PUT", "/api/v1/billing/dunning-policy", body, "csr", "divya"); code != http.StatusForbidden {
			t.Errorf("want 403, got %d", code)
		}
		if d.policySaves != 0 {
			t.Error("a CSR changed the policy")
		}
	})
}
