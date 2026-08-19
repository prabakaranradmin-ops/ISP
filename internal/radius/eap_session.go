package radius

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
)

// EAP-MSCHAPv2 session state — FR-AAA-006 | MDS §4.18.
//
// Unlike PAP, which is one request and one response, EAP is a multi-packet
// conversation. RADIUS carries no connection, so the only thread tying the
// packets together is the State attribute the server issues and the NAS
// echoes back. That makes State the session key, and it makes the server
// responsible for remembering the challenge it issued — a challenge the
// server forgets is a challenge it cannot verify a response against.

// EAPStage is where a conversation has reached.
type EAPStage string

const (
	// StageIdentityRequested: we asked who they are and are waiting.
	StageIdentityRequested EAPStage = "identity_requested"
	// StageChallengeIssued: we sent a challenge and are waiting for the
	// NT response.
	StageChallengeIssued EAPStage = "challenge_issued"
	// StageAwaitingSuccessAck: the response verified; MS-CHAPv2 requires the
	// peer to acknowledge our Success before we send EAP-Success.
	StageAwaitingSuccessAck EAPStage = "awaiting_success_ack"
)

// eapSessionTTL bounds how long a half-finished conversation is remembered.
//
// Deliberately short. The window is the few round trips a supplicant needs,
// not a user session — and every stored entry holds a challenge that must
// never be reusable, so expiring aggressively is the safe direction. A
// supplicant that takes longer than this simply restarts, which is the
// normal EAP recovery path.
const eapSessionTTL = 60 * time.Second

var (
	eapSessionsStarted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_eap_sessions_started_total",
		Help: "EAP-MSCHAPv2 conversations begun",
	})
	eapSessionsCompleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "radius_eap_sessions_completed_total",
		Help: "EAP-MSCHAPv2 conversations that reached a terminal state, by result",
	}, []string{"result"})
	// eapSessionsLost counts responses arriving with a State we no longer
	// hold — expired, or a NAS echoing a stale value. Distinct from a
	// rejection: it means the conversation has to restart, and a rising
	// value points at a TTL that is too short for the network's latency.
	eapSessionsLost = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_eap_sessions_lost_total",
		Help: "EAP responses whose State no longer maps to a stored session",
	})
)

// EAPSession is the server-side memory of one in-flight conversation.
type EAPSession struct {
	Stage    EAPStage `json:"stage"`
	Username string   `json:"username"`
	// AuthenticatorChallenge is the 16 random bytes we issued. Verifying a
	// response means recomputing it against exactly these bytes, so losing
	// them loses the conversation.
	AuthenticatorChallenge []byte `json:"authenticator_challenge"`
	// PeerChallenge and NTResponse are carried into the success-ack stage so
	// the authenticator response can be recomputed without re-deriving them.
	PeerChallenge []byte `json:"peer_challenge,omitempty"`
	NTResponse    []byte `json:"nt_response,omitempty"`
	// MSCHAPv2ID echoes the peer's MS-CHAPv2 identifier, which must be
	// mirrored in our replies or supplicants discard them.
	MSCHAPv2ID uint8 `json:"mschapv2_id"`
	// EAPIdentifier is the EAP-layer identifier; each Request we send uses
	// the previous Response's identifier plus one.
	EAPIdentifier uint8 `json:"eap_identifier"`
	// AuthenticatorResponse is computed once, at verification time, and
	// replayed if the peer's ack needs it.
	AuthenticatorResponse string `json:"authenticator_response,omitempty"`
}

// EAPSessionStore persists in-flight conversations.
//
// Backed by an in-process TTL map. This used to require Redis specifically
// because radiusd was designed to run more than one instance behind a NAS
// that may send consecutive packets of one conversation to different
// servers — an in-memory map would authenticate only when the round trips
// happened to land on the same process. A single-machine install runs
// exactly one radiusd process, which retires that requirement — see
// internal/localcache's package doc.
type EAPSessionStore struct {
	store *localcache.Store[*EAPSession]
	ttl   time.Duration
}

// NewEAPSessionStore constructs an EAPSessionStore.
func NewEAPSessionStore() *EAPSessionStore {
	return &EAPSessionStore{store: localcache.New[*EAPSession](0), ttl: eapSessionTTL}
}

func eapSessionKey(state string) string { return "radius_eap:" + state }

// NewState mints an unpredictable State value.
//
// Unpredictability matters: State is the only handle on a session that holds
// a live challenge, so a guessable value would let an attacker attach to
// somebody else's half-finished conversation.
func NewState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("radius: generate EAP state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NewChallenge mints a 16-byte authenticator challenge.
//
// crypto/rand, never math/rand: a predictable challenge lets an attacker
// precompute responses, which defeats the entire challenge-response exchange.
func NewChallenge() ([]byte, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("radius: generate EAP challenge: %w", err)
	}
	return buf, nil
}

// Save stores a session against its State, refreshing the TTL.
func (s *EAPSessionStore) Save(_ context.Context, state string, sess *EAPSession) error {
	s.store.Set(eapSessionKey(state), sess, s.ttl)
	return nil
}

// Load fetches a session. A missing or expired entry returns (nil, nil):
// the conversation must restart, which is different from an error and is
// counted separately.
func (s *EAPSessionStore) Load(_ context.Context, state string) (*EAPSession, error) {
	sess, ok := s.store.Get(eapSessionKey(state))
	if !ok {
		eapSessionsLost.Inc()
		return nil, nil
	}
	return sess, nil
}

// Delete removes a finished conversation.
//
// Called on every terminal outcome, success or failure. Leaving a completed
// session behind would keep a used challenge alive until its TTL, and a
// challenge that outlives its single use is exactly what replay protection
// exists to prevent.
func (s *EAPSessionStore) Delete(_ context.Context, state string) error {
	s.store.Delete(eapSessionKey(state))
	return nil
}
