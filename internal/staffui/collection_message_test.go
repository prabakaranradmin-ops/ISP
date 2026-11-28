package staffui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// What a collector is told after taking a payment at the door — FR-COL-001,
// FR-BIL-009.
//
// It said service "resumes on their next authentication, within a minute",
// and a collector would repeat that to the customer. Recording the payment
// only credits the wallet; service comes back when the renewal pass charges
// the cycle, which runs every billing.RenewalScanInterval, and only if the
// balance covers it.
func TestFR_COL_001_TheCollectorIsNotPromisedAMinute(t *testing.T) {
	msg := collectionMessages["collected"]
	if strings.Contains(msg, "minute,") || strings.Contains(msg, "within a minute") {
		t.Errorf("the message still promises a minute: %q", msg)
	}
	want := fmt.Sprintf("within %d minutes", int(billing.RenewalScanInterval.Minutes()))
	if !strings.Contains(msg, want) {
		t.Errorf("the message does not give the renewal pass's own wait (%q): %q", want, msg)
	}
	if !strings.Contains(msg, "If the balance now covers their renewal") {
		t.Errorf("the message does not say a part payment restores nothing: %q", msg)
	}
}
