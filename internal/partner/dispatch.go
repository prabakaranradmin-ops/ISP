package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// Webhook dispatch — FR-API-002..003 | MDS §4.22.

const (
	// TaskTypeWebhook carries one event to one endpoint. Fan-out enqueues one
	// task per subscriber endpoint rather than one task per event, for the
	// same reason announcements do: a partner whose server is down must not
	// be able to fail or delay delivery to every other partner.
	TaskTypeWebhook = "partner:webhook"

	// QueueWebhooks is separate from the transactional notification queue. A
	// partner integration retrying against a dead host must never sit in
	// front of a payment receipt or a suspension notice.
	QueueWebhooks = "webhooks"

	// MaxAttempts before a delivery is abandoned. the queue's exponential backoff
	// spreads these across roughly a day, which is long enough to ride out a
	// partner's deploy or short outage and short enough that a permanently
	// dead endpoint stops consuming the queue.
	MaxAttempts = 8

	// deliveryTimeout is per attempt. A partner that accepts the connection
	// and then stalls must not hold a worker indefinitely.
	deliveryTimeout = 20 * time.Second

	// excerptLimit caps what we keep of a partner's response body. Their 500
	// page is a diagnostic hint, not our audit log.
	excerptLimit = 512
)

var (
	webhooksSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "partner_webhooks_sent_total",
		Help: "Webhook deliveries attempted, by event type and outcome",
	}, []string{"event_type", "outcome"})
	webhooksAbandoned = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "partner_webhooks_abandoned_total",
		Help: "Webhook deliveries that exhausted every retry",
	}, []string{"event_type"})
	// The gauge worth alerting on: a partner endpoint failing repeatedly is a
	// broken integration somebody should be told about, and it is otherwise
	// invisible until they complain.
	webhookBlocked = promauto.NewCounter(prometheus.CounterOpts{
		Name: "partner_webhooks_blocked_total",
		Help: "Webhook deliveries refused because the target resolved to a private or reserved address",
	})
)

// DeliveryStore is the persistence surface dispatch needs.
type DeliveryStore interface {
	SubscribersFor(ctx context.Context, eventType string) ([]EndpointSecret, error)
	RecordDeliveryAttempt(ctx context.Context, endpointID int, ev Event, status string,
		responseStatus *int, responseExcerpt, lastError string, nextAttempt *time.Time) error
}

// Decryptor turns a stored ciphertext back into a signing secret.
type Decryptor interface {
	Decrypt(versionedCiphertext string) (string, error)
}

// TaskPayload is the task payload for one endpoint's copy of one event.
type TaskPayload struct {
	EndpointID int    `json:"endpoint_id"`
	URL        string `json:"url"`
	// SecretEncrypted, not Secret: this is ciphertext sitting in Redis (the
	// queue backend) until ProcessTask decrypts it, and the field name
	// said otherwise — gosec's G117 flags struct fields named like a secret
	// that get marshaled, and here it was also just an inaccurate name, not
	// only a lint finding. The JSON tag is unchanged; only the Go-side name
	// now matches what the field actually holds.
	SecretEncrypted string `json:"secret_encrypted"`
	Event           Event  `json:"event"`
}

// Emitter fans an event out to every subscribed endpoint.
type Emitter struct {
	store  DeliveryStore
	client *jobqueue.Client
}

// NewEmitter constructs an Emitter. A nil client disables emission, which is
// how a deployment with no partner integrations runs unchanged.
func NewEmitter(store DeliveryStore, client *jobqueue.Client) *Emitter {
	return &Emitter{store: store, client: client}
}

// Emit enqueues one delivery per subscribed endpoint.
//
// Errors are logged rather than returned to the caller. Emission hangs off
// business operations — creating a subscriber, resolving a ticket — and
// failing one of those because a partner integration could not be queued would
// let a third party's configuration break the core product.
func (e *Emitter) Emit(ctx context.Context, eventType string, entityID int) {
	if e == nil || e.client == nil || e.store == nil {
		return
	}

	ev, err := NewEvent(eventType, entityID, time.Now())
	if err != nil {
		log.Error().Err(err).Str("event_type", eventType).Msg("partner: refusing to emit unknown event type")
		return
	}

	endpoints, err := e.store.SubscribersFor(ctx, eventType)
	if err != nil {
		log.Error().Err(err).Str("event_type", eventType).Msg("partner: could not load webhook subscribers")
		return
	}

	for _, ep := range endpoints {
		payload, err := json.Marshal(TaskPayload{
			EndpointID:      ep.EndpointID,
			URL:             ep.URL,
			SecretEncrypted: ep.SecretEncrypted,
			Event:           ev,
		})
		if err != nil {
			log.Error().Err(err).Int("endpoint_id", ep.EndpointID).Msg("partner: marshal webhook task")
			continue
		}
		task := jobqueue.NewTask(TaskTypeWebhook, payload,
			jobqueue.Queue(QueueWebhooks),
			jobqueue.MaxRetry(MaxAttempts),
			jobqueue.Timeout(deliveryTimeout+10*time.Second))
		if _, err := e.client.EnqueueContext(ctx, task); err != nil {
			log.Error().Err(err).Int("endpoint_id", ep.EndpointID).Msg("partner: enqueue webhook")
		}
	}
}

// Sender delivers one webhook. It is the queue handler.
type Sender struct {
	store     DeliveryStore
	decryptor Decryptor
	client    *http.Client
	now       func() time.Time
}

// NewSender constructs the delivery handler.
func NewSender(store DeliveryStore, decryptor Decryptor) *Sender {
	return &Sender{
		store:     store,
		decryptor: decryptor,
		client:    NewSafeHTTPClient(deliveryTimeout),
		now:       time.Now,
	}
}

// ProcessTask implements jobqueue.Handler for TaskTypeWebhook.
//
// Returning an error hands the task back to the queue for exponential backoff;
// returning nil retires it. jobqueue.SkipRetry is used for anything no retry can
// fix — a malformed payload, an undecryptable secret, a blocked address — so a
// permanent misconfiguration does not occupy the queue for a day.
func (s *Sender) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p TaskPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("partner: unmarshal webhook task: %w: %w", err, jobqueue.SkipRetry)
	}

	secret, err := s.decryptor.Decrypt(p.SecretEncrypted)
	if err != nil {
		s.record(ctx, p, StatusAbandoned, nil, "", "signing secret could not be decrypted")
		return fmt.Errorf("partner: decrypt webhook secret: %w: %w", err, jobqueue.SkipRetry)
	}

	body, err := json.Marshal(p.Event)
	if err != nil {
		return fmt.Errorf("partner: marshal webhook body: %w: %w", err, jobqueue.SkipRetry)
	}

	attempt, _ := jobqueue.RetryCount(ctx)
	outcome, status, excerpt, sendErr := s.send(ctx, p, secret, body)

	switch {
	case sendErr == nil:
		webhooksSent.WithLabelValues(p.Event.EventType, "delivered").Inc()
		s.record(ctx, p, StatusDelivered, status, excerpt, "")
		return nil

	case outcome == outcomeBlocked:
		// No retry will make a private address public. Recorded as abandoned
		// so an operator can see the endpoint is misconfigured rather than
		// merely slow.
		webhookBlocked.Inc()
		webhooksSent.WithLabelValues(p.Event.EventType, "blocked").Inc()
		s.record(ctx, p, StatusAbandoned, status, excerpt, sendErr.Error())
		return fmt.Errorf("partner: %w: %w", sendErr, jobqueue.SkipRetry)

	case attempt+1 >= MaxAttempts:
		webhooksAbandoned.WithLabelValues(p.Event.EventType).Inc()
		webhooksSent.WithLabelValues(p.Event.EventType, "abandoned").Inc()
		s.record(ctx, p, StatusAbandoned, status, excerpt, sendErr.Error())
		return fmt.Errorf("partner: webhook abandoned after %d attempts: %w", attempt+1, sendErr)

	default:
		webhooksSent.WithLabelValues(p.Event.EventType, "retry").Inc()
		next := s.now().Add(backoff(attempt))
		s.recordWithNext(ctx, p, StatusPending, status, excerpt, sendErr.Error(), &next)
		return sendErr
	}
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeBlocked
	outcomeFailed
)

// send performs one HTTP attempt.
func (s *Sender) send(ctx context.Context, p TaskPayload, secret string, body []byte) (outcome, *int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return outcomeFailed, nil, "", fmt.Errorf("build request: %w", err)
	}

	ts := s.now()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ISP-BSS-Webhooks/1.0")
	req.Header.Set(HeaderSignature, Sign(secret, ts, body))
	req.Header.Set(HeaderEventID, p.Event.EventID.String())
	req.Header.Set(HeaderEventType, p.Event.EventType)
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", ts.Unix()))

	resp, err := s.client.Do(req)
	if err != nil {
		// The dialler's Control hook returns ErrBlockedAddress, which arrives
		// here wrapped in *url.Error — errors.As is what unwraps it, and
		// getting this wrong would turn a permanent SSRF refusal into a day of
		// pointless retries.
		var blocked *ErrBlockedAddress
		if errors.As(err, &blocked) {
			return outcomeBlocked, nil, "", blocked
		}
		return outcomeFailed, nil, "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	excerptBytes, _ := io.ReadAll(io.LimitReader(resp.Body, excerptLimit))
	excerpt := string(excerptBytes)
	status := resp.StatusCode

	// 2xx is success. Everything else retries, including 4xx: a partner
	// returning 401 because their own auth is misconfigured is a transient
	// problem from our side, and giving up immediately would lose the event.
	if status >= 200 && status < 300 {
		return outcomeOK, &status, excerpt, nil
	}
	return outcomeFailed, &status, excerpt, fmt.Errorf("partner returned HTTP %d", status)
}

func (s *Sender) record(ctx context.Context, p TaskPayload, status string, respStatus *int, excerpt, errStr string) {
	s.recordWithNext(ctx, p, status, respStatus, excerpt, errStr, nil)
}

func (s *Sender) recordWithNext(ctx context.Context, p TaskPayload, status string,
	respStatus *int, excerpt, errStr string, next *time.Time,
) {
	if err := s.store.RecordDeliveryAttempt(ctx, p.EndpointID, p.Event, status, respStatus, excerpt, errStr, next); err != nil {
		// Logged, not returned: losing the audit row is bad, but failing the
		// delivery because we could not write about it would be worse.
		log.Error().Err(err).Int("endpoint_id", p.EndpointID).Msg("partner: record delivery attempt")
	}
}

// backoff mirrors the queue's schedule so next_attempt_at in the delivery log
// matches when the retry will actually run. A log that disagrees with reality
// is worse than no log, because somebody will trust it.
// backoffSchedule is written out rather than computed with a shift.
//
// A table cannot overflow — an unbounded `1 << attempt` can, producing a
// negative or near-zero duration that schedules the next attempt in the past
// and turns the backoff into a hot loop against a partner already struggling.
// It also makes the actual schedule readable, which matters when the whole
// point is to mirror what the queue will really do.
//
// Total elapsed across all 8 attempts is a little over two hours, then the 6h
// ceiling holds for anything beyond.
var backoffSchedule = [MaxAttempts]time.Duration{
	1 * time.Minute,
	2 * time.Minute,
	4 * time.Minute,
	8 * time.Minute,
	16 * time.Minute,
	32 * time.Minute,
	64 * time.Minute,
	2 * time.Hour,
}

func backoff(attempt int) time.Duration {
	if attempt < 0 {
		return backoffSchedule[0]
	}
	if attempt >= len(backoffSchedule) {
		return 6 * time.Hour
	}
	return backoffSchedule[attempt]
}
