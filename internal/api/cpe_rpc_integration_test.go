//go:build integration

// TR-069 control endpoint tests — FR-CPE-003 | MDS §4.19.
//
// Until these existed nothing exercised the HTTP control routes at all; the
// ACS's own tests cover delivery, and the db tests cover the queue. What is
// defended here is the contract an operator relies on: the right RPC is
// queued at the right priority and attributed, malformed requests never
// reach the queue, and reach matches nocOrTech.
package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

type stubCPE struct {
	device   *tr069.Device
	queued   []tr069.Task
	state    string
	failNext bool
}

func (s *stubCPE) GetDeviceBySerialACS(_ context.Context, serial string) (*tr069.Device, error) {
	if s.device != nil && s.device.SerialNumber == serial {
		return s.device, nil
	}
	return nil, nil
}

func (s *stubCPE) EnqueueTask(_ context.Context, deviceID int, rpcType string, params map[string]string, priority int, createdBy string) (*tr069.Task, error) {
	t := tr069.Task{ID: len(s.queued) + 1, DeviceID: deviceID, RPCType: rpcType, Params: params,
		Priority: priority, CreatedBy: createdBy, Status: "pending"}
	s.queued = append(s.queued, t)
	return &t, nil
}

func (s *stubCPE) ListTasks(context.Context, int, *string) ([]tr069.Task, error) {
	return s.queued, nil
}

func (s *stubCPE) SetProvisioningState(_ context.Context, _ int, state, _ string) error {
	s.state = state
	return nil
}

func newCPEMux(t *testing.T, cpe api.CPEControlQuerier) *http.ServeMux {
	t.Helper()
	deps := api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})}
	if cpe != nil {
		deps.CPEControl = cpe
	}
	h := api.NewHandler(deps)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func cpeDo(t *testing.T, mux *http.ServeMux, path, body, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, role+"-op"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_CPE_003_ControlEndpoints(t *testing.T) {
	newStub := func() *stubCPE {
		return &stubCPE{device: &tr069.Device{ID: 4, SerialNumber: "SN-1", ProvisioningState: tr069.StateProvisioned}}
	}

	t.Run("reboot is queued at priority 10, attributed, and answered 202", func(t *testing.T) {
		cpe := newStub()
		rec := cpeDo(t, newCPEMux(t, cpe), "/api/v1/cpe/devices/SN-1/reboot", "", "technician")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body.String())
		}
		if len(cpe.queued) != 1 || cpe.queued[0].RPCType != tr069.RPCReboot || cpe.queued[0].Priority != 10 || cpe.queued[0].CreatedBy != "technician-op" {
			t.Errorf("queued: %+v", cpe.queued)
		}
		if !strings.Contains(rec.Body.String(), `"delivery":"queued"`) {
			t.Error("the response must say the action is queued, not done")
		}
	})

	t.Run("a CSR cannot queue anything", func(t *testing.T) {
		cpe := newStub()
		if rec := cpeDo(t, newCPEMux(t, cpe), "/api/v1/cpe/devices/SN-1/reboot", "", "csr"); rec.Code != http.StatusForbidden || len(cpe.queued) != 0 {
			t.Errorf("want 403 and nothing queued, got %d with %d queued", rec.Code, len(cpe.queued))
		}
	})

	for name, body := range map[string]string{
		"missing url": `{}`,
		"ftp url":     `{"url":"ftp://x/fw.bin"}`,
	} {
		t.Run("firmware refused: "+name, func(t *testing.T) {
			cpe := newStub()
			rec := cpeDo(t, newCPEMux(t, cpe), "/api/v1/cpe/devices/SN-1/firmware", body, "noc_engineer")
			if rec.Code != http.StatusUnprocessableEntity || len(cpe.queued) != 0 {
				t.Errorf("want 422 and nothing queued, got %d with %d queued", rec.Code, len(cpe.queued))
			}
		})
	}

	t.Run("firmware with an https url is queued as a Download", func(t *testing.T) {
		cpe := newStub()
		rec := cpeDo(t, newCPEMux(t, cpe), "/api/v1/cpe/devices/SN-1/firmware", `{"url":"https://fw.example/v2.bin"}`, "noc_engineer")
		if rec.Code != http.StatusAccepted || len(cpe.queued) != 1 || cpe.queued[0].RPCType != tr069.RPCDownload || cpe.queued[0].Params["url"] != "https://fw.example/v2.bin" {
			t.Errorf("got %d %+v", rec.Code, cpe.queued)
		}
	})

	t.Run("parameter read keeps the names in order; none is refused", func(t *testing.T) {
		cpe := newStub()
		mux := newCPEMux(t, cpe)
		if rec := cpeDo(t, mux, "/api/v1/cpe/devices/SN-1/parameters", `{"names":[]}`, "isp_owner"); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("empty names: want 422, got %d", rec.Code)
		}
		rec := cpeDo(t, mux, "/api/v1/cpe/devices/SN-1/parameters", `{"names":["A.B","C.D"]}`, "isp_owner")
		if rec.Code != http.StatusAccepted || len(cpe.queued) != 1 || cpe.queued[0].Params["0"] != "A.B" || cpe.queued[0].Params["1"] != "C.D" {
			t.Errorf("got %d %+v", rec.Code, cpe.queued)
		}
	})

	t.Run("reprovision flags the device and queues no RPC", func(t *testing.T) {
		cpe := newStub()
		rec := cpeDo(t, newCPEMux(t, cpe), "/api/v1/cpe/devices/SN-1/reprovision", "", "noc_engineer")
		if rec.Code != http.StatusAccepted || cpe.state != tr069.StateNeedsReprovision || len(cpe.queued) != 0 {
			t.Errorf("got %d state=%q queued=%d", rec.Code, cpe.state, len(cpe.queued))
		}
	})

	t.Run("unknown serial is 404; no ACS is 503", func(t *testing.T) {
		if rec := cpeDo(t, newCPEMux(t, newStub()), "/api/v1/cpe/devices/NOPE/reboot", "", "noc_engineer"); rec.Code != http.StatusNotFound {
			t.Errorf("unknown serial: want 404, got %d", rec.Code)
		}
		if rec := cpeDo(t, newCPEMux(t, nil), "/api/v1/cpe/devices/SN-1/reboot", "", "noc_engineer"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("no ACS: want 503, got %d", rec.Code)
		}
	})
}
