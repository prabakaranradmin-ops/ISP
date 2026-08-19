package tr069

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
)

// The ACS session engine — FR-CPE-001..003 | MDS §4.19.
//
// CWMP is CPE-initiated. The ACS never pushes; it answers a session the
// device opens and, while that session is open, drains whatever RPCs are
// queued for it. Behind CGNAT a Connection Request usually cannot reach the
// device at all, so periodic Inform sessions are the delivery mechanism
// rather than a fallback (decision 2026-08-14).
//
// One session, in order:
//
//	POST Inform          -> InformResponse            [session opened]
//	POST (empty)         -> next queued RPC, or 204   [drain]
//	POST <RPC>Response   -> next queued RPC, or 204   [drain, loop]
//	POST (empty)         -> 204                        [session closed]

// sessionTTL bounds an in-flight CWMP session. Generous next to EAP's 60s
// because a firmware Download can legitimately keep a session open for
// minutes, but still bounded so an abandoned session cannot pin a task in
// 'sent' forever.
const sessionTTL = 10 * time.Minute

// maxBodyBytes caps a CWMP request body. Device-supplied and unauthenticated
// at the point of read, so an unbounded ReadAll would be a trivial memory
// exhaustion vector.
const maxBodyBytes = 1 << 20 // 1 MiB

// Session is the ACS's memory of one in-flight CWMP conversation.
type Session struct {
	DeviceID     int    `json:"device_id"`
	SerialNumber string `json:"serial_number"`
	// OutstandingTaskID is the task whose response we are waiting for, so a
	// device's reply can be matched back to what we asked. Zero when nothing
	// is in flight.
	OutstandingTaskID int `json:"outstanding_task_id"`
}

// Store is the persistence surface the ACS needs.
type Store interface {
	// UpsertDeviceFromInform records or refreshes a device on contact,
	// returning the device and whether it was newly discovered.
	UpsertDeviceFromInform(ctx context.Context, inform *Inform) (*Device, error)
	// ClaimNextTask atomically moves the highest-priority pending task for a
	// device to 'sent', returning (nil, nil) when there is nothing queued.
	ClaimNextTask(ctx context.Context, deviceID int) (*Task, error)
	CompleteTask(ctx context.Context, taskID int, status, faultCode, faultString string) error
	SetProvisioningState(ctx context.Context, deviceID int, state, lastFault string) error
	// GetProvisioningPlan returns the rendered parameter map for a device,
	// or an empty map when its model has no template configured.
	GetProvisioningPlan(ctx context.Context, deviceID int) (map[string]string, error)
	EnqueueTask(ctx context.Context, deviceID int, rpcType string, params map[string]string, priority int, createdBy string) (*Task, error)
}

// ACS is the CWMP HTTP handler.
type ACS struct {
	store    Store
	sessions *localcache.Store[*Session]
	ttl      time.Duration
}

// NewACS constructs an ACS.
func NewACS(store Store) *ACS {
	return &ACS{store: store, sessions: localcache.New[*Session](0), ttl: sessionTTL}
}

func sessionKey(id string) string { return "tr069_session:" + id }

// ServeHTTP handles a CWMP POST.
//
// Session identity is carried in a cookie the ACS sets on InformResponse —
// the standard CWMP mechanism, and the only one that works when a device is
// behind NAT and its source address changes between requests.
func (a *ACS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "CWMP requires POST", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		log.Warn().Err(err).Msg("tr069: could not read the CWMP body")
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}

	env, err := ParseEnvelope(raw)
	if err != nil {
		log.Warn().Err(err).Msg("tr069: malformed CWMP envelope")
		http.Error(w, "malformed CWMP envelope", http.StatusBadRequest)
		return
	}

	// An Inform opens a session and is the only message that can arrive
	// without one.
	if env != nil && env.Body.Inform != nil {
		a.handleInform(ctx, w, env)
		return
	}

	sess := a.loadSession(ctx, r)
	if sess == nil {
		// No session and no Inform: nothing can be done with this. Ending
		// cleanly rather than erroring lets a confused device restart.
		a.writeEmpty(w)
		return
	}

	// A response to whatever we last asked.
	if env != nil {
		a.recordTaskOutcome(ctx, sess, env)
	}

	a.sendNextTaskOrClose(ctx, w, r, sess)
}

// handleInform opens a session, records the contact, and decides whether the
// device needs provisioning.
func (a *ACS) handleInform(ctx context.Context, w http.ResponseWriter, env *Envelope) {
	inform := env.Body.Inform

	device, err := a.store.UpsertDeviceFromInform(ctx, inform)
	if err != nil {
		log.Error().Err(err).Str("serial", inform.DeviceID.SerialNumber).
			Msg("tr069: could not record the informing device")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if device.ACSDiscovered {
		UnknownDeviceInformsTotal.Inc()
	}

	primaryEvent := "unknown"
	if len(inform.Event) > 0 {
		primaryEvent = inform.Event[0].EventCode
	}
	InformsTotal.WithLabelValues(primaryEvent).Inc()

	// BOOTSTRAP means first contact ever (or post-factory-reset), and BOOT
	// means it restarted — both warrant re-pushing configuration, because in
	// neither case can we assume what is on the device. PERIODIC does not:
	// re-provisioning on every check-in would rewrite a subscriber's Wi-Fi
	// password every fifteen minutes.
	if inform.HasEvent(EventBootstrap) || inform.HasEvent(EventBoot) ||
		device.ProvisioningState == StateNeedsReprovision || device.ProvisioningState == StateUnknown {
		a.queueProvisioning(ctx, device)
	}

	sessionID, err := newSessionID()
	if err != nil {
		log.Error().Err(err).Msg("tr069: could not generate a session id")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.saveSession(ctx, sessionID, &Session{
		DeviceID: device.ID, SerialNumber: device.SerialNumber,
	}); err != nil {
		log.Error().Err(err).Msg("tr069: could not store the CWMP session")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Secure and SameSite added alongside HttpOnly: this endpoint is mounted on
	// the same mux as every JWT-protected route (cmd/api/main.go), so it never
	// actually receives a plain-HTTP request in this deployment — Caddy
	// terminates TLS in front of all of it — and Secure costs nothing here.
	// SameSite=Strict is the conservative default for a session that never
	// legitimately needs cross-site semantics: CWMP is a CPE-to-ACS protocol
	// session, not something a browser navigates to.
	http.SetCookie(w, &http.Cookie{
		Name: "cwmp-session", Value: sessionID, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	a.writeSOAP(w, BuildInformResponse(env.Header.ID))
}

// queueProvisioning enqueues the device's plan-derived configuration.
//
// Best-effort: a device that cannot be provisioned right now should still
// complete its session and get whatever else is queued, rather than have the
// whole contact fail.
func (a *ACS) queueProvisioning(ctx context.Context, device *Device) {
	params, err := a.store.GetProvisioningPlan(ctx, device.ID)
	if err != nil {
		log.Error().Err(err).Int("device_id", device.ID).
			Msg("tr069: could not build the provisioning plan")
		return
	}
	if len(params) == 0 {
		// No template for this model, or no subscriber attached yet. Not an
		// error: an unissued device in the warehouse legitimately has
		// nothing to be configured with.
		return
	}
	if _, err := a.store.EnqueueTask(ctx, device.ID, RPCSetParameterValues, params, 10, "acs:auto-provision"); err != nil {
		log.Error().Err(err).Int("device_id", device.ID).Msg("tr069: could not queue provisioning")
	}
}

// recordTaskOutcome matches a device's response to the outstanding task.
func (a *ACS) recordTaskOutcome(ctx context.Context, sess *Session, env *Envelope) {
	if sess.OutstandingTaskID == 0 {
		return // an unsolicited response, e.g. TransferComplete
	}

	status, faultCode, faultString := TaskCompleted, "", ""
	if env.Body.Fault != nil {
		status = TaskFailed
		faultCode = env.Body.Fault.Detail.Fault.FaultCode
		faultString = env.Body.Fault.Detail.Fault.FaultString
		if faultCode == "" {
			faultCode = env.Body.Fault.FaultCode
			faultString = env.Body.Fault.FaultString
		}
		TaskFaultsTotal.WithLabelValues(faultCode).Inc()

		// A device that rejected its own provisioning is in a state nobody
		// has verified, so it is marked rather than left looking healthy.
		if err := a.store.SetProvisioningState(ctx, sess.DeviceID, StateFault, faultString); err != nil {
			log.Error().Err(err).Msg("tr069: could not record the device fault")
		}
	}

	if err := a.store.CompleteTask(ctx, sess.OutstandingTaskID, status, faultCode, faultString); err != nil {
		log.Error().Err(err).Int("task_id", sess.OutstandingTaskID).
			Msg("tr069: could not record the task outcome")
	}

	// A successful SetParameterValues is what moves a device to provisioned.
	if status == TaskCompleted && env.Body.SetParameterValuesResp != nil {
		if err := a.store.SetProvisioningState(ctx, sess.DeviceID, StateProvisioned, ""); err != nil {
			log.Error().Err(err).Msg("tr069: could not mark the device provisioned")
		}
	}
}

// sendNextTaskOrClose drains one queued RPC, or ends the session.
func (a *ACS) sendNextTaskOrClose(ctx context.Context, w http.ResponseWriter, r *http.Request, sess *Session) {
	task, err := a.store.ClaimNextTask(ctx, sess.DeviceID)
	if err != nil {
		log.Error().Err(err).Int("device_id", sess.DeviceID).Msg("tr069: could not claim the next task")
		a.writeEmpty(w)
		return
	}
	if task == nil {
		// Nothing queued: the session is over. The device will be back at
		// its next periodic Inform, which is when anything queued in the
		// meantime gets delivered.
		a.clearSession(ctx, r)
		a.writeEmpty(w)
		return
	}

	sess.OutstandingTaskID = task.ID
	if id := sessionIDFrom(r); id != "" {
		if err := a.saveSession(ctx, id, sess); err != nil {
			log.Error().Err(err).Msg("tr069: could not update the CWMP session")
		}
	}

	body, err := renderTask(task)
	if err != nil {
		log.Error().Err(err).Int("task_id", task.ID).Msg("tr069: could not render the RPC")
		if cErr := a.store.CompleteTask(ctx, task.ID, TaskFailed, "9003", err.Error()); cErr != nil {
			log.Error().Err(cErr).Msg("tr069: could not fail the unrenderable task")
		}
		a.writeEmpty(w)
		return
	}

	TasksDeliveredTotal.WithLabelValues(task.RPCType).Inc()
	a.writeSOAP(w, body)
}

// renderTask turns a queued task into the CWMP envelope for it.
func renderTask(task *Task) (string, error) {
	commandKey := fmt.Sprintf("task-%d", task.ID)

	switch task.RPCType {
	case RPCSetParameterValues:
		if len(task.Params) == 0 {
			return "", fmt.Errorf("SetParameterValues with no parameters")
		}
		return BuildSetParameterValues("", task.Params, commandKey), nil

	case RPCGetParameterValues:
		names := make([]string, 0, len(task.Params))
		for _, v := range task.Params {
			names = append(names, v)
		}
		if len(names) == 0 {
			return "", fmt.Errorf("GetParameterValues with no parameter names")
		}
		return BuildGetParameterValues("", names), nil

	case RPCReboot:
		return BuildReboot("", commandKey), nil

	case RPCFactoryReset:
		return BuildFactoryReset(""), nil

	case RPCDownload:
		url := task.Params["url"]
		if url == "" {
			// Issuing a Download with no URL can leave a device in a
			// half-upgraded state, so it fails here rather than on the box.
			return "", fmt.Errorf("download RPC has no url parameter")
		}
		return BuildDownload("", url, commandKey, 0), nil

	default:
		return "", fmt.Errorf("unsupported RPC type %q", task.RPCType)
	}
}

// ── Session plumbing ────────────────────────────────────────────────────────

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tr069: generate session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func sessionIDFrom(r *http.Request) string {
	c, err := r.Cookie("cwmp-session")
	if err != nil {
		return ""
	}
	return c.Value
}

func (a *ACS) saveSession(_ context.Context, id string, sess *Session) error {
	a.sessions.Set(sessionKey(id), sess, a.ttl)
	return nil
}

func (a *ACS) loadSession(_ context.Context, r *http.Request) *Session {
	id := sessionIDFrom(r)
	if id == "" {
		return nil
	}
	sess, ok := a.sessions.Get(sessionKey(id))
	if !ok {
		return nil
	}
	return sess
}

func (a *ACS) clearSession(_ context.Context, r *http.Request) {
	if id := sessionIDFrom(r); id != "" {
		a.sessions.Delete(sessionKey(id))
	}
}

func (a *ACS) writeSOAP(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, body); err != nil {
		log.Error().Err(err).Msg("tr069: could not write the CWMP response")
	}
}

// writeEmpty ends a session: 204 with no body is how TR-069 says "I have
// nothing more for you".
func (a *ACS) writeEmpty(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
