package ledger

import "testing"

func TestNormalBalanceFor(t *testing.T) {
	cases := []struct {
		accountType string
		want        string
	}{
		{TypeAsset, BalanceDebit},
		{TypeExpense, BalanceDebit},
		{TypeLiability, BalanceCredit},
		{TypeEquity, BalanceCredit},
		{TypeIncome, BalanceCredit},
	}
	for _, tc := range cases {
		if got := NormalBalanceFor(tc.accountType); got != tc.want {
			t.Errorf("NormalBalanceFor(%q) = %q, want %q", tc.accountType, got, tc.want)
		}
	}
}

func TestValidAccountType(t *testing.T) {
	for _, ok := range []string{TypeAsset, TypeLiability, TypeEquity, TypeIncome, TypeExpense} {
		if !ValidAccountType(ok) {
			t.Errorf("ValidAccountType(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "revenue", "ASSET", "made_up"} {
		if ValidAccountType(bad) {
			t.Errorf("ValidAccountType(%q) = true, want false", bad)
		}
	}
}
