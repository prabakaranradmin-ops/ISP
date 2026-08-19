package fup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"layeh.com/radius"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

var podAckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fup_pod_ack_total",
	Help: "PoD (Disconnect-Request) response counts by result",
}, []string{"result"})

// PoDPayload is the task payload for a forced disconnect.
//
// Either the session is named directly, or it is resolved from a subscriber.
// The direct form exists for voucher-backed hotspot sessions (FR-HSP-001),
// which have no subscriber row to look up — see migration 034's
// chk_grant_has_exactly_one_source.
type PoDPayload struct {
	SubscriberID int `json:"subscriber_id,omitempty"`
	// NasIP and SessionID, when both set, are used as-is and no subscriber
	// lookup happens.
	NasIP     string `json:"nas_ip,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// direct reports whether the payload names its own session.
func (p PoDPayload) direct() bool { return p.NasIP != "" && p.SessionID != "" }

// PoDHandler processes forced-disconnect tasks with exponential backoff via
// queue retry, sharing CoAQuerier since both need the same NAS session lookup.
//
// FR: FR-NET-002, FR-NAS-002 | DDS §5.3 | MDS §4.11
type PoDHandler struct {
	db          CoAQuerier
	secret      []byte
	port        int
	nasResolver *nas.Resolver
}

// NewPoDHandler constructs a PoDHandler targeting DefaultCoAPort.
func NewPoDHandler(db CoAQuerier, secret []byte) *PoDHandler {
	return &PoDHandler{db: db, secret: secret, port: DefaultCoAPort}
}

// SetPort overrides the NAS PoD destination port.
func (h *PoDHandler) SetPort(port int) {
	h.port = port
}

// SetNASResolver enables per-NAS secret/port resolution (FR-NAS-002, MDS
// §4.11). PoD carries no vendor-specific attribute for any vendor, so
// unlike CoAHandler this only affects which secret/port is used, never
// packet contents. Optional: unset behaves exactly as before.
func (h *PoDHandler) SetNASResolver(r *nas.Resolver) {
	h.nasResolver = r
}

// ProcessTask implements jobqueue.Handler.
func (h *PoDHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p PoDPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("pod: unmarshal payload: %w", err)
	}

	nasIP, sessionID := p.NasIP, p.SessionID
	if !p.direct() {
		var err error
		nasIP, sessionID, _, _, err = h.db.GetSubscriberNASSession(ctx, p.SubscriberID)
		if err != nil {
			// No live session means there is nothing left to disconnect.
			// Retrying cannot change that, so the task should not be retried.
			return fmt.Errorf("pod: get NAS session for sub %d: %w: %w", p.SubscriberID, err, jobqueue.SkipRetry)
		}
	}

	secret, port := h.secret, h.port
	if port == 0 {
		port = DefaultCoAPort
	}
	if h.nasResolver != nil {
		device := h.nasResolver.Resolve(nasIP)
		secret, port = device.Secret, device.PoDPort
	}
	return SendReliablePoD(nasIP, port, sessionID, secret)
}

// NewDirectPoDTask builds a disconnect task for a session named outright,
// used where there is no subscriber to resolve it from.
func NewDirectPoDTask(nasIP, sessionID string) (*jobqueue.Task, error) {
	payload, err := json.Marshal(PoDPayload{NasIP: nasIP, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("pod: marshal payload: %w", err)
	}
	return jobqueue.NewTask(TaskTypePoD, payload,
		jobqueue.Queue(QueueNetCommands),
		jobqueue.MaxRetry(3)), nil
}

// SendReliablePoD sends a Disconnect-Request to the NAS and waits for
// Disconnect-ACK. Returns an error on NAK or timeout — the queue will retry with
// exponential backoff.
//
// DDS §5.3
func SendReliablePoD(nasIP string, port int, sessionID string, secret []byte) error {
	pkt := radius.New(radius.CodeDisconnectRequest, secret)
	pkt.Set(radius.Type(44), []byte(sessionID)) // Acct-Session-Id

	return sendReliableControl(nasIP, port, secret, pkt, radius.CodeDisconnectACK, podAckTotal)
}
