//go:build integration

// End-to-end ACS session tests — FR-CPE-001..003 | MDS §4.19.
//
// A simulated CPE walks the real handler through a whole CWMP session:
// Inform, drain the queue, close. The device is simulated because the
// alternative is a physical router on a bench — which is the field test this
// cannot replace, and which the module notes as still outstanding. Session
// state used to require a real (in-process) Redis here; it is now held in
// process by the ACS itself.
package tr069_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

// ── Stub store ───────────────────────────────────────────────────────────────

// stubStore is an in-memory tr069.Store. ClaimNextTask holds a mutex across
// the check-and-claim so it reproduces the real store's conditional UPDATE —
// a stub that let two sessions claim the same task would make the race test
// pass against an implementation that could never behave that way.
type stubStore struct {
	mu sync.Mutex

	device       *tr069.Device
	tasks        []*tr069.Task
	nextTaskID   int
	provisioning map[string]string

	completed  []completedTask
	stateSets  []string
	enqueued   []string
	upsertErr  error
	claimCalls int
}

type completedTask struct {
	id          int
	status      string
	faultCode   string
	faultString string
}

func newStubStore() *stubStore {
	return &stubStore{
		device: &tr069.Device{
			ID: 1, SerialNumber: "SN-ACS-001", DeviceTypeID: 1,
			ProvisioningState: tr069.StateRegistered,
		},
		nextTaskID:   1,
		provisioning: map[string]string{},
	}
}

func (s *stubStore) UpsertDeviceFromInform(_ context.Context, inform *tr069.Inform) (*tr069.Device, error) {
	if s.upsertErr != nil {
		return nil, s.upsertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device.SerialNumber = inform.DeviceID.SerialNumber
	s.device.LastInformEvent = inform.EventCodes()
	cp := *s.device
	return &cp, nil
}

func (s *stubStore) ClaimNextTask(_ context.Context, deviceID int) (*tr069.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	for _, t := range s.tasks {
		if t.DeviceID == deviceID && t.Status == tr069.TaskPending {
			t.Status = tr069.TaskSent
			cp := *t
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *stubStore) CompleteTask(_ context.Context, taskID int, status, faultCode, faultString string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, completedTask{taskID, status, faultCode, faultString})
	for _, t := range s.tasks {
		if t.ID == taskID {
			t.Status = status
		}
	}
	return nil
}

func (s *stubStore) SetProvisioningState(_ context.Context, _ int, state, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateSets = append(s.stateSets, state)
	return nil
}

func (s *stubStore) GetProvisioningPlan(_ context.Context, _ int) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provisioning, nil
}

func (s *stubStore) EnqueueTask(_ context.Context, deviceID int, rpcType string,
	params map[string]string, priority int, createdBy string,
) (*tr069.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &tr069.Task{
		ID: s.nextTaskID, DeviceID: deviceID, RPCType: rpcType,
		Params: params, Status: tr069.TaskPending, Priority: priority, CreatedBy: createdBy,
	}
	s.nextTaskID++
	s.tasks = append(s.tasks, t)
	s.enqueued = append(s.enqueued, rpcType)
	return t, nil
}

// ── Harness ──────────────────────────────────────────────────────────────────

func newACS(t *testing.T) (*tr069.ACS, *stubStore) {
	t.Helper()
	store := newStubStore()
	return tr069.NewACS(store), store
}

// cpe simulates a device holding a session cookie across requests.
type cpe struct {
	acs    *tr069.ACS
	cookie *http.Cookie
	t      *testing.T
}

func (c *cpe) post(body string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tr069", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/xml")
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	rec := httptest.NewRecorder()
	c.acs.ServeHTTP(rec, req)

	// Devices keep the session cookie the ACS sets on InformResponse.
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "cwmp-session" {
			c.cookie = ck
		}
	}
	return rec
}

func informBody(serial, event string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
  <soap:Header><cwmp:ID>s1</cwmp:ID></soap:Header>
  <soap:Body><cwmp:Inform>
    <DeviceId><Manufacturer>M</Manufacturer><OUI>001122</OUI>
      <ProductClass>PC</ProductClass><SerialNumber>%s</SerialNumber></DeviceId>
    <Event><EventStruct><EventCode>%s</EventCode><CommandKey></CommandKey></EventStruct></Event>
    <MaxEnvelopes>1</MaxEnvelopes><CurrentTime>2026-08-14T10:00:00Z</CurrentTime><RetryCount>0</RetryCount>
    <ParameterList></ParameterList>
  </cwmp:Inform></soap:Body>
</soap:Envelope>`, serial, event)
}

const setParamResponse = `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
  <soap:Header><cwmp:ID>s1</cwmp:ID></soap:Header>
  <soap:Body><cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse></soap:Body>
</soap:Envelope>`

const faultResponse = `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
  <soap:Header><cwmp:ID>s1</cwmp:ID></soap:Header>
  <soap:Body><soap:Fault>
    <faultcode>Client</faultcode><faultstring>CWMP fault</faultstring>
    <detail><cwmp:Fault><FaultCode>9003</FaultCode><FaultString>Invalid arguments</FaultString></cwmp:Fault></detail>
  </soap:Fault></soap:Body>
</soap:Envelope>`

// ── Tests ────────────────────────────────────────────────────────────────────

// TestFR_CPE_001_InformOpensASessionAndAcknowledges is the entry point of
// every CWMP conversation.
func TestFR_CPE_001_InformOpensASessionAndAcknowledges(t *testing.T) {
	acs, store := newACS(t)
	device := &cpe{acs: acs, t: t}

	rec := device.post(informBody("SN-ACS-001", tr069.EventPeriodic))

	if rec.Code != http.StatusOK {
		t.Fatalf("Inform: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "InformResponse") {
		t.Errorf("want an InformResponse, got:\n%s", rec.Body.String())
	}
	if device.cookie == nil {
		t.Fatal("the ACS must set a session cookie — it is how the next POST is correlated")
	}
	if store.device.LastInformEvent != tr069.EventPeriodic {
		t.Errorf("last_inform_event = %q", store.device.LastInformEvent)
	}
}

// TestFR_CPE_001_BootstrapTriggersProvisioning: a device on first contact
// (or after a factory reset) must be configured.
func TestFR_CPE_001_BootstrapTriggersProvisioning(t *testing.T) {
	acs, store := newACS(t)
	store.provisioning = map[string]string{"Device.WiFi.SSID": "ISP-Fibre"}
	device := &cpe{acs: acs, t: t}

	device.post(informBody("SN-ACS-001", tr069.EventBootstrap))

	if len(store.enqueued) != 1 || store.enqueued[0] != tr069.RPCSetParameterValues {
		t.Fatalf("BOOTSTRAP must queue provisioning, got %v", store.enqueued)
	}
}

// TestFR_CPE_001_PeriodicDoesNotReprovision is the counterpart, and the more
// important one: re-pushing configuration on every check-in would rewrite a
// subscriber's Wi-Fi settings every fifteen minutes.
func TestFR_CPE_001_PeriodicDoesNotReprovision(t *testing.T) {
	acs, store := newACS(t)
	store.provisioning = map[string]string{"Device.WiFi.SSID": "ISP-Fibre"}
	store.device.ProvisioningState = tr069.StateProvisioned
	device := &cpe{acs: acs, t: t}

	device.post(informBody("SN-ACS-001", tr069.EventPeriodic))

	if len(store.enqueued) != 0 {
		t.Errorf("a routine PERIODIC on a provisioned device must not re-push config, got %v", store.enqueued)
	}
}

// TestFR_CPE_003_QueuedRPCIsDeliveredWithinTheSession is the delivery
// mechanism the CGNAT decision rests on: nothing is pushed, everything is
// drained inside a session the device opened.
func TestFR_CPE_003_QueuedRPCIsDeliveredWithinTheSession(t *testing.T) {
	acs, store := newACS(t)
	if _, err := store.EnqueueTask(context.Background(), 1, tr069.RPCReboot, nil, 10, "noc1"); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	device := &cpe{acs: acs, t: t}

	// 1. Inform opens the session.
	device.post(informBody("SN-ACS-001", tr069.EventPeriodic))

	// 2. The device asks for more work with an empty POST.
	rec := device.post("")
	if rec.Code != http.StatusOK {
		t.Fatalf("want the queued RPC, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cwmp:Reboot") {
		t.Fatalf("want a Reboot RPC, got:\n%s", rec.Body.String())
	}

	// 3. The device acknowledges, and the session closes with 204.
	rec = device.post(`<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
  <soap:Header><cwmp:ID>s1</cwmp:ID></soap:Header>
  <soap:Body><cwmp:RebootResponse></cwmp:RebootResponse></soap:Body>
</soap:Envelope>`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("with the queue drained the session must close with 204, got %d:\n%s", rec.Code, rec.Body.String())
	}
	if len(store.completed) != 1 || store.completed[0].status != tr069.TaskCompleted {
		t.Errorf("the RPC outcome must be recorded, got %+v", store.completed)
	}
}

// TestFR_CPE_003_MultipleRPCsDrainInOneSession: a device that has been
// offline may have several tasks waiting, and all of them should go out
// while it is reachable rather than one per check-in.
func TestFR_CPE_003_MultipleRPCsDrainInOneSession(t *testing.T) {
	acs, store := newACS(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := store.EnqueueTask(ctx, 1, tr069.RPCReboot, nil, 10, "noc1"); err != nil {
			t.Fatalf("EnqueueTask: %v", err)
		}
	}
	device := &cpe{acs: acs, t: t}
	device.post(informBody("SN-ACS-001", tr069.EventPeriodic))

	delivered := 0
	for i := 0; i < 5; i++ {
		rec := device.post("")
		if rec.Code == http.StatusNoContent {
			break
		}
		if strings.Contains(rec.Body.String(), "cwmp:Reboot") {
			delivered++
		}
	}
	if delivered != 3 {
		t.Errorf("want all 3 queued RPCs delivered in one session, got %d", delivered)
	}
}

// TestFR_CPE_003_DeviceFaultIsRecordedAndMarksTheDevice: a device that
// rejected its own provisioning is in a state nobody has verified, so it
// must not be left looking healthy.
func TestFR_CPE_003_DeviceFaultIsRecordedAndMarksTheDevice(t *testing.T) {
	acs, store := newACS(t)
	if _, err := store.EnqueueTask(context.Background(), 1,
		tr069.RPCSetParameterValues, map[string]string{"Device.WiFi.SSID": "X"}, 10, "noc1"); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	device := &cpe{acs: acs, t: t}
	device.post(informBody("SN-ACS-001", tr069.EventPeriodic))
	device.post("") // collect the RPC

	device.post(faultResponse)

	if len(store.completed) != 1 {
		t.Fatalf("want the task recorded, got %+v", store.completed)
	}
	got := store.completed[0]
	if got.status != tr069.TaskFailed {
		t.Errorf("status = %q, want failed", got.status)
	}
	if got.faultCode != "9003" {
		t.Errorf("fault code = %q, want 9003", got.faultCode)
	}

	sawFault := false
	for _, s := range store.stateSets {
		if s == tr069.StateFault {
			sawFault = true
		}
	}
	if !sawFault {
		t.Errorf("a device that faulted must be marked, states seen: %v", store.stateSets)
	}
}

// TestFR_CPE_001_SuccessfulSetMarksTheDeviceProvisioned closes the loop.
func TestFR_CPE_001_SuccessfulSetMarksTheDeviceProvisioned(t *testing.T) {
	acs, store := newACS(t)
	if _, err := store.EnqueueTask(context.Background(), 1,
		tr069.RPCSetParameterValues, map[string]string{"Device.WiFi.SSID": "X"}, 10, "acs"); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	device := &cpe{acs: acs, t: t}
	device.post(informBody("SN-ACS-001", tr069.EventPeriodic))
	device.post("")
	device.post(setParamResponse)

	sawProvisioned := false
	for _, s := range store.stateSets {
		if s == tr069.StateProvisioned {
			sawProvisioned = true
		}
	}
	if !sawProvisioned {
		t.Errorf("a successful SetParameterValues must mark the device provisioned, states: %v", store.stateSets)
	}
}

// TestACS_RequestWithoutASessionEndsCleanly: a device POSTing without a
// session (expired, or a confused client) must be told to stop rather than
// erroring, so it restarts from Inform.
func TestACS_RequestWithoutASessionEndsCleanly(t *testing.T) {
	acs, _ := newACS(t)
	device := &cpe{acs: acs, t: t}

	rec := device.post("")
	if rec.Code != http.StatusNoContent {
		t.Errorf("want 204 for a session-less POST, got %d", rec.Code)
	}
}

func TestACS_RejectsNonPOST(t *testing.T) {
	acs, _ := newACS(t)
	req := httptest.NewRequest(http.MethodGet, "/tr069", nil)
	rec := httptest.NewRecorder()
	acs.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("want 405 for GET, got %d", rec.Code)
	}
}

func TestACS_RejectsMalformedEnvelope(t *testing.T) {
	acs, _ := newACS(t)
	device := &cpe{acs: acs, t: t}

	rec := device.post(`<soap:Envelope><unclosed>`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a malformed envelope, got %d", rec.Code)
	}
}

// TestACS_NoTemplateIsNotAFailure: an unissued warehouse device legitimately
// has nothing to be configured with, and its Inform must still succeed.
func TestACS_NoTemplateIsNotAFailure(t *testing.T) {
	acs, store := newACS(t)
	store.provisioning = map[string]string{} // no template / no subscriber
	device := &cpe{acs: acs, t: t}

	rec := device.post(informBody("SN-ACS-001", tr069.EventBootstrap))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if len(store.enqueued) != 0 {
		t.Errorf("nothing should be queued when there is no template, got %v", store.enqueued)
	}
}

// TestACS_ConcurrentSessionsDoNotDoubleDeliverATask is the race the atomic
// claim exists for: two overlapping sessions must not both be handed the
// same reboot.
func TestACS_ConcurrentSessionsDoNotDoubleDeliverATask(t *testing.T) {
	acs, store := newACS(t)
	if _, err := store.EnqueueTask(context.Background(), 1, tr069.RPCReboot, nil, 10, "noc1"); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	// Two devices (or two overlapping sessions from one) open concurrently.
	const sessions = 6
	devices := make([]*cpe, sessions)
	for i := range devices {
		devices[i] = &cpe{acs: acs, t: t}
		devices[i].post(informBody("SN-ACS-001", tr069.EventPeriodic))
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		delivered int
	)
	start := make(chan struct{})
	for _, d := range devices {
		wg.Add(1)
		go func(d *cpe) {
			defer wg.Done()
			<-start
			if strings.Contains(d.post("").Body.String(), "cwmp:Reboot") {
				mu.Lock()
				delivered++
				mu.Unlock()
			}
		}(d)
	}
	close(start)
	wg.Wait()

	if delivered != 1 {
		t.Errorf("DOUBLE DELIVERY: %d sessions were handed the same reboot, want exactly 1", delivered)
	}
}
