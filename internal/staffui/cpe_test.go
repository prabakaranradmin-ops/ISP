package staffui

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

type fakeCPE struct {
	device    *tr069.Device
	tasks     []tr069.Task
	err       error
	gotAction string
	gotNames  []string
	gotURL    string
	gotActor  string
	calls     int
}

func (f *fakeCPE) CPEDevice(_ context.Context, serial string) (*tr069.Device, error) {
	if f.device == nil || f.device.SerialNumber != serial {
		return nil, api.ErrCPENotFound
	}
	return f.device, nil
}

func (f *fakeCPE) CPETasks(context.Context, int, *string) ([]tr069.Task, error) { return f.tasks, nil }

func (f *fakeCPE) QueueCPEAction(ctx context.Context, _ *tr069.Device, action string, names []string, u string) (*tr069.Task, error) {
	f.calls++
	f.gotAction, f.gotNames, f.gotURL = action, names, u
	f.gotActor = middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &tr069.Task{ID: 1}, nil
}

func serveCPE(h *Handler, role, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 {
		r.SetPathValue("serial", parts[2])
	}
	w := httptest.NewRecorder()
	switch {
	case method == "POST":
		h.CPEActionForm(w, r)
	case len(parts) >= 3:
		h.CPEDetail(w, r)
	default:
		h.CPEList(w, r)
	}
	return w
}

func TestCPEScreen(t *testing.T) {
	seen := time.Now().Add(-48 * time.Hour)
	dev := &tr069.Device{ID: 4, SerialNumber: "SN-1", ProductClass: "HG8245", ProvisioningState: "provisioned", LastInformAt: &seen}

	t.Run("NOC, technician and owner reach it; CSR and billing do not", func(t *testing.T) {
		for role, want := range map[string]int{"noc_engineer": 200, "technician": 200, "isp_owner": 200, "csr": 403, "billing_admin": 403} {
			h := &Handler{console: &fakeConsole{}}
			if code := serveCPE(h, role, "GET", "/staff/cpe", nil).Code; code != want {
				t.Errorf("%s: want %d, got %d", role, want, code)
			}
		}
	})

	t.Run("list filters reach the store; unknown is reported", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		serveCPE(h, "noc_engineer", "GET", "/staff/cpe?show=silent&q=+HG82+", nil)
		if fc.gotCPEView != "silent" || fc.gotCPEQuery != "HG82" {
			t.Errorf("got view=%q q=%q", fc.gotCPEView, fc.gotCPEQuery)
		}
		body := serveCPE(h, "noc_engineer", "GET", "/staff/cpe?show=bogus", nil).Body.String()
		if fc.gotCPEView != "managed" || !strings.Contains(body, "Unknown list") {
			t.Error("an unknown view must be reported and fall back to managed")
		}
	})

	t.Run("detail shows silence and the queued-not-done explanation", func(t *testing.T) {
		h := &Handler{cpe: &fakeCPE{device: dev, tasks: []tr069.Task{{ID: 9, RPCType: "Reboot", Status: "pending", CreatedAt: time.Now()}}}}
		body := serveCPE(h, "technician", "GET", "/staff/cpe/SN-1", nil).Body.String()
		for _, want := range []string{"silent", "next time this device checks in", "Queue reboot", "Reboot"} {
			if !strings.Contains(body, want) {
				t.Errorf("detail missing %q", want)
			}
		}
	})

	t.Run("unknown serial is 404", func(t *testing.T) {
		h := &Handler{cpe: &fakeCPE{device: dev}}
		if code := serveCPE(h, "noc_engineer", "GET", "/staff/cpe/NOPE", nil).Code; code != 404 {
			t.Errorf("want 404, got %d", code)
		}
	})
}

func TestCPEActions(t *testing.T) {
	dev := &tr069.Device{ID: 4, SerialNumber: "SN-1", ProvisioningState: "provisioned"}

	t.Run("an action is queued, attributed, and confirmed", func(t *testing.T) {
		fc := &fakeCPE{device: dev}
		h := &Handler{cpe: fc}
		w := serveCPE(h, "technician", "POST", "/staff/cpe/SN-1/action", url.Values{"action": {"reboot"}})
		if w.Code != 303 || fc.gotAction != "reboot" || fc.gotActor != "technician_user" {
			t.Fatalf("got %d action=%q actor=%q", w.Code, fc.gotAction, fc.gotActor)
		}
		if loc := w.Header().Get("Location"); loc != "/staff/cpe/SN-1?queued=1" {
			t.Errorf("redirect: %q", loc)
		}
	})

	t.Run("parameter names and firmware URL are passed through", func(t *testing.T) {
		fc := &fakeCPE{device: dev}
		h := &Handler{cpe: fc}
		serveCPE(h, "noc_engineer", "POST", "/staff/cpe/SN-1/action", url.Values{"action": {"parameters"}, "arg": {"A.B, C.D"}})
		if len(fc.gotNames) != 1 || fc.gotNames[0] != "A.B, C.D" {
			t.Errorf("names: %v", fc.gotNames)
		}
		serveCPE(h, "noc_engineer", "POST", "/staff/cpe/SN-1/action", url.Values{"action": {"firmware"}, "arg": {"https://fw/x.bin"}})
		if fc.gotURL != "https://fw/x.bin" {
			t.Errorf("url: %q", fc.gotURL)
		}
	})

	t.Run("CSR posting directly is refused before anything is queued", func(t *testing.T) {
		fc := &fakeCPE{device: dev}
		h := &Handler{cpe: fc}
		if code := serveCPE(h, "csr", "POST", "/staff/cpe/SN-1/action", url.Values{"action": {"reboot"}}).Code; code != 403 || fc.calls != 0 {
			t.Errorf("want 403 and no call, got %d with %d calls", code, fc.calls)
		}
	})

	t.Run("a validation failure is shown on the device page", func(t *testing.T) {
		fc := &fakeCPE{device: dev, err: fmt.Errorf("%w: url must be an http:// or https:// address the device can reach", api.ErrCPEInvalid)}
		h := &Handler{cpe: fc}
		body := serveCPE(h, "noc_engineer", "POST", "/staff/cpe/SN-1/action", url.Values{"action": {"firmware"}, "arg": {"ftp://x"}}).Body.String()
		if !strings.Contains(body, "Not queued: url must be") {
			t.Error("the reason must be shown")
		}
	})

	t.Run("a serial with path characters cannot redirect elsewhere", func(t *testing.T) {
		if got := urlPathSegment("a/b?c#d e%"); got != "a%2Fb%3Fc%23d%20e%25" {
			t.Errorf("escaped: %q", got)
		}
	})
}

// TestCPEActionGateMatchesTheAPI pins canActOnCPE to the API's nocOrTech
// list. It is the only thing that would still stop a role if the CPE
// section were ever widened for viewing — today the section gate refuses
// the same roles first, which is why the CSR test above cannot tell the two
// apart (a negative control removing canActOnCPE showed exactly that).
func TestCPEActionGateMatchesTheAPI(t *testing.T) {
	for _, role := range []string{"isp_owner", "noc_engineer", "technician", "csr", "billing_admin", "lco", ""} {
		want := role == "isp_owner" || role == "noc_engineer" || role == "technician"
		if got := canActOnCPE(role); got != want {
			t.Errorf("canActOnCPE(%q) = %v, want %v (API nocOrTech)", role, got, want)
		}
	}
}

func TestRenderCPE(t *testing.T) {
	h := &Handler{}
	sub := 3
	now := time.Now()
	assertRendersOK(t, h, "cpe", pageData{Session: testSession(), Data: cpeData{Show: "managed"}})
	assertRendersOK(t, h, "cpe", pageData{Session: testSession(), Data: cpeData{Show: "all", Devices: []CPEDeviceRow{
		{Device: tr069.Device{SerialNumber: "SN-1", SubscriberID: &sub, LastInformAt: &now, ProvisioningState: "fault"}, Username: "u", PendingTasks: 2, Silent: true},
		{Device: tr069.Device{SerialNumber: "SN-2", ProvisioningState: "unknown"}},
	}}})
	assertRendersOK(t, h, "cpe", pageData{Session: testSession(), Data: cpeData{
		Device:   &tr069.Device{SerialNumber: "SN-1", SubscriberID: &sub, LastInformAt: &now, LastFault: "9002 internal"},
		Username: "u", CanAct: true,
		Tasks: []tr069.Task{{ID: 1, RPCType: "Download", Params: map[string]string{"url": "https://x"}, Status: "failed", FaultCode: "9010", FaultString: "download failed", CreatedAt: now, SentAt: &now}},
	}})
}
