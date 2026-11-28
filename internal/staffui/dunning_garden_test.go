package staffui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// The dunning screen promises "Browsing redirected to the payment page" for
// soft suspension — FR-BIL-004. It now says what that depends on, and whether
// this server's half of it is running, so the promise can be checked rather
// than taken on trust.

func dunningPage(t *testing.T, garden WalledGardenInfo) string {
	t.Helper()
	h := &Handler{dunning: &fakeDunning{policy: billing.DefaultDunningPolicy()}, walledGarden: garden}
	r := httptest.NewRequest("GET", "/staff/dunning", nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: "owner", Role: "isp_owner"}))
	w := httptest.NewRecorder()
	h.DunningSettings(w, r)
	if w.Code != 200 {
		t.Fatalf("dunning screen: %d", w.Code)
	}
	return visibleText(w.Body.String())
}

func TestFR_BIL_004_TheDunningScreenSaysWhatSoftSuspensionDependsOn(t *testing.T) {
	off := dunningPage(t, WalledGardenInfo{})
	for _, want := range []string{
		"Browsing redirected to the payment page",
		nas.WalledGardenAddressList,
		"deploy/mikrotik/walled-garden.rsc",
		"soft suspension restricts nothing",
		"refused, the same as at hard suspension",
		"The payment-page redirector is off on this server",
	} {
		if !strings.Contains(off, want) {
			t.Errorf("with the redirector off, the screen does not say %q", want)
		}
	}

	on := dunningPage(t, WalledGardenInfo{Addr: ":8088", RedirectURL: "https://portal.example.net/ui/login?suspended=1"})
	if strings.Contains(on, "redirector is off") {
		t.Error("the screen says the redirector is off while it is configured")
	}
	if !strings.Contains(on, ":8088") || !strings.Contains(on, "https://portal.example.net/ui/login?suspended=1") {
		t.Errorf("the screen does not say where redirected traffic goes: %s", on)
	}
}
