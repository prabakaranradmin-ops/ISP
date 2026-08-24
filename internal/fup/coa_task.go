package fup

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"layeh.com/radius"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

var (
	coaAckTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fup_coa_ack_total",
		Help: "CoA response counts by result",
	}, []string{"result"})
)

const (
	// DefaultCoAPort is MikroTik's CoA/PoD listener port. RFC 5176 specifies
	// 3799, so vendors differ and the port has to be configurable per deployment.
	DefaultCoAPort     = 1700
	controlDialTimeout = 3 * time.Second
)

// CoAPayload is the task payload for CoA sends.
type CoAPayload struct {
	SubscriberID int    `json:"subscriber_id"`
	NasIP        string `json:"nas_ip"`
}

// CoAQuerier retrieves subscriber state needed to build the CoA packet.
type CoAQuerier interface {
	GetSubscriberNASSession(ctx context.Context, subscriberID int) (nasIP, sessionID, rateLimit string, planID int, err error)
}

// LiveSessionRateWriter updates the cached rate a live session's portal
// panel reports. Satisfied by *cache.SessionStore.
type LiveSessionRateWriter interface {
	UpdateSpeedProfile(ctx context.Context, sessionID, rateLimit string) error
}

// CoAHandler processes CoA send tasks with exponential backoff via queue retry.
//
// FR: FR-FUP-002, FR-NAS-001..004 | DDS §5.3 | MDS §4.11
type CoAHandler struct {
	db           CoAQuerier
	secret       []byte
	port         int
	nasResolver  *nas.Resolver
	liveSessions LiveSessionRateWriter
}

// NewCoAHandler constructs a CoAHandler targeting DefaultCoAPort.
func NewCoAHandler(db CoAQuerier, secret []byte) *CoAHandler {
	return &CoAHandler{db: db, secret: secret, port: DefaultCoAPort}
}

// SetPort overrides the NAS CoA destination port.
func (h *CoAHandler) SetPort(port int) {
	h.port = port
}

// SetNASResolver enables per-NAS secret/port and vendor-aware CoA attributes
// (FR-NAS-001..004, MDS §4.11). Optional: with no resolver set, CoAHandler
// behaves exactly as before — the constructor's global secret and port,
// MikroTik VSA unconditionally.
func (h *CoAHandler) SetNASResolver(r *nas.Resolver) {
	h.nasResolver = r
}

// SetLiveSessions enables refreshing the portal's live-usage panel with the
// rate a CoA actually applied. Optional: without it, CoA sends behave
// exactly as before — the NAS still gets the right rate, only the console's
// and portal's cached display lags until the subscriber's next session.
func (h *CoAHandler) SetLiveSessions(w LiveSessionRateWriter) {
	h.liveSessions = w
}

// ProcessTask implements jobqueue.Handler.
//
// The NAS session is resolved fresh at execution time rather than trusting the
// payload's snapshot: the queue retries with backoff, and a subscriber may have
// reconnected — to a different NAS IP — between enqueue and this run.
func (h *CoAHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p CoAPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("coa: unmarshal payload: %w", err)
	}

	nasIP, sessionID, rateLimit, planID, err := h.db.GetSubscriberNASSession(ctx, p.SubscriberID)
	if err != nil {
		return fmt.Errorf("coa: get NAS session for sub %d: %w", p.SubscriberID, err)
	}

	secret, port := h.secret, h.effectivePort()
	vendor := nas.VendorMikrotik
	profileName := ""
	if h.nasResolver != nil {
		device := h.nasResolver.Resolve(nasIP)
		secret, port, vendor = device.Secret, device.CoAPort, device.Vendor
		profileName = h.nasResolver.ResolveProfile(planID, vendor)
	}

	attrs, err := nas.BuildCoAAttrs(vendor, nas.RateProfile{RateLimitString: rateLimit, ProfileName: profileName})
	if err != nil {
		// Unlike Access-Accept, a CoA with no bandwidth attribute is a no-op
		// enforcement action wearing a success — retrying (queue backoff,
		// eventual dead-letter + alert) is the right failure mode, not a
		// best-effort send.
		return fmt.Errorf("coa: build vendor attributes for sub %d (%s): %w", p.SubscriberID, vendor, err)
	}

	if err := SendReliableCoA(nasIP, port, sessionID, attrs, secret); err != nil {
		return err
	}

	// Best-effort: this is a read-surface refresh for the console/portal, not
	// the enforcement action itself, which the NAS has already acknowledged
	// above. A failure here must not turn a successful CoA into a retried
	// task — that would re-send an already-applied rate change.
	if h.liveSessions != nil {
		if err := h.liveSessions.UpdateSpeedProfile(ctx, sessionID, rateLimit); err != nil {
			log.Warn().Err(err).Str("session_id", sessionID).Msg("coa: live session rate refresh failed")
		}
	}
	return nil
}

func (h *CoAHandler) effectivePort() int {
	if h.port == 0 {
		return DefaultCoAPort
	}
	return h.port
}

// SendReliableCoA sends a CoA-Request to the NAS and waits for CoA-ACK.
// Returns an error on NAK or timeout — the queue will retry with exponential backoff.
//
// DDS §5.3 | MDS §4.11
func SendReliableCoA(nasIP string, port int, sessionID string, attrs []nas.Attr, secret []byte) error {
	pkt := radius.New(radius.CodeCoARequest, secret)
	pkt.Set(radius.Type(44), []byte(sessionID)) // Acct-Session-Id
	for _, a := range attrs {
		pkt.Add(a.Type, a.Value)
	}

	return sendReliableControl(nasIP, port, secret, pkt, radius.CodeCoAACK, coaAckTotal)
}

// sendReliableControl sends a pre-built control packet (CoA-Request or
// Disconnect-Request) to the NAS and waits for the matching ACK code.
// A non-matching response, an error, or a timeout are all reported as errors so
// the queue retries with exponential backoff.
func sendReliableControl(nasIP string, port int, secret []byte, pkt *radius.Packet, ackCode radius.Code, result *prometheus.CounterVec) error {
	addr := &net.UDPAddr{IP: net.ParseIP(nasIP), Port: port}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("radius control: dial %s: %w", nasIP, err)
	}
	defer conn.Close() //nolint:errcheck

	if err := conn.SetDeadline(time.Now().Add(controlDialTimeout)); err != nil {
		return fmt.Errorf("radius control: set deadline: %w", err)
	}

	encoded, err := pkt.Encode()
	if err != nil {
		return fmt.Errorf("radius control: encode packet: %w", err)
	}
	if _, err := conn.Write(encoded); err != nil {
		return fmt.Errorf("radius control: write packet: %w", err)
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("radius control: await response from %s: %w", nasIP, err)
	}

	response, err := radius.Parse(buf[:n], secret)
	if err != nil {
		return fmt.Errorf("radius control: parse response: %w", err)
	}

	if response.Code != ackCode {
		result.WithLabelValues("nak").Inc()
		return fmt.Errorf("radius control: NAK (code %v) received from %s (will retry)", response.Code, nasIP)
	}

	result.WithLabelValues("ack").Inc()
	return nil
}
