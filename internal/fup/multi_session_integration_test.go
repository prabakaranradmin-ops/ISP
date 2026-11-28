//go:build integration

package fup

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"sync"
	"testing"

	"layeh.com/radius"
	"layeh.com/radius/rfc3576"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// A CoA or a disconnect reaches every session the subscriber holds —
// FR-FUP-002, FR-NET-002, FR-BIL-004.
//
// Both used to address one session: GetSubscriberNASSession takes the newest
// open one. Nothing limits a subscriber to one session, and FR-HSP-002 gives
// them more on purpose (a PPPoE line at home, phones registered for MAC Auth
// Bypass), so every enforcement action missed the rest. A FUP throttle
// slowed one connection; a hard suspension cut one and left the others
// online.

// multiDB answers like the store: the newest session with the rate, and the
// whole list when asked.
type multiDB struct {
	sessions []NASSession
	listErr  error

	mu     sync.Mutex
	closed []string
}

func (d *multiDB) CloseGoneSession(_ context.Context, sessionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = append(d.closed, sessionID)
	return nil
}

func (d *multiDB) closedSessions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.closed...)
	sort.Strings(out)
	return out
}

func (d *multiDB) GetSubscriberNASSession(context.Context, int) (string, string, string, int, error) {
	if len(d.sessions) == 0 {
		return "", "", "", 0, errors.New("no active session")
	}
	return d.sessions[0].NasIP, d.sessions[0].SessionID, "10M/10M", 3, nil
}

func (d *multiDB) ListSubscriberNASSessions(context.Context, int) ([]NASSession, error) {
	return d.sessions, d.listErr
}

func sessionsOn(n int) []NASSession {
	out := make([]NASSession, n)
	for i := range out {
		out[i] = NASSession{NasIP: "127.0.0.1", SessionID: "sess-" + strconv.Itoa(i+1)}
	}
	return out
}

// recordingNAS answers every request with code, carrying Error-Cause when
// cause is non-zero, and records the Acct-Session-Id of each.
type recordingNAS struct {
	mu   sync.Mutex
	seen []string
}

func (n *recordingNAS) sessions() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := append([]string(nil), n.seen...)
	sort.Strings(out)
	return out
}

func startRecordingNAS(t *testing.T, secret []byte, code radius.Code, cause rfc3576.ErrorCause) (int, *recordingNAS) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	rec := &recordingNAS{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req, err := radius.Parse(buf[:n], secret)
			if err != nil {
				continue
			}
			rec.mu.Lock()
			rec.seen = append(rec.seen, string(req.Get(radius.Type(44))))
			rec.mu.Unlock()
			resp := req.Response(code)
			if cause != 0 {
				_ = rfc3576.ErrorCause_Set(resp, cause)
			}
			if encoded, err := resp.Encode(); err == nil {
				_, _ = conn.WriteToUDP(encoded, addr)
			}
		}
	}()
	return port(conn), rec
}

func port(c *net.UDPConn) int { return c.LocalAddr().(*net.UDPAddr).Port }

func podTask(t *testing.T, subscriberID int) *jobqueue.Task {
	t.Helper()
	payload, _ := json.Marshal(PoDPayload{SubscriberID: subscriberID})
	return jobqueue.NewTask(TaskTypePoD, payload)
}

func coaTask(t *testing.T, subscriberID int) *jobqueue.Task {
	t.Helper()
	payload, _ := json.Marshal(CoAPayload{SubscriberID: subscriberID})
	return jobqueue.NewTask(TaskTypeCoA, payload)
}

func TestFR_BIL_004_ADisconnectEndsEverySessionTheSubscriberHolds(t *testing.T) {
	secret := []byte("pod-secret")
	p, nasRec := startRecordingNAS(t, secret, radius.CodeDisconnectACK, 0)
	h := NewPoDHandler(&multiDB{sessions: sessionsOn(3)}, secret)
	h.SetPort(p)
	if err := h.ProcessTask(context.Background(), podTask(t, 40)); err != nil {
		t.Fatal(err)
	}
	if got := nasRec.sessions(); len(got) != 3 || got[0] != "sess-1" || got[2] != "sess-3" {
		t.Errorf("disconnects reached %v; every open session must end, not only the newest", got)
	}
}

func TestFR_FUP_002_AThrottleReachesEverySessionTheSubscriberHolds(t *testing.T) {
	secret := []byte("coa-secret")
	p, nasRec := startRecordingNAS(t, secret, radius.CodeCoAACK, 0)
	h := NewCoAHandler(&multiDB{sessions: sessionsOn(2)}, secret)
	h.SetPort(p)
	if err := h.ProcessTask(context.Background(), coaTask(t, 41)); err != nil {
		t.Fatal(err)
	}
	if got := nasRec.sessions(); len(got) != 2 {
		t.Errorf("CoAs reached %v; a throttle on one connection leaves the rest at full speed", got)
	}
}

// RFC 5176 Error-Cause 503: the NAS has no such session. For a disconnect
// that is done; for a CoA it is not worth retrying. A plain NAK still is.
func TestFR_NET_002_ASessionAlreadyGoneIsNotRetriedForever(t *testing.T) {
	secret := []byte("pod-secret")
	p, _ := startRecordingNAS(t, secret, radius.CodeDisconnectNAK, rfc3576.ErrorCause_Value_SessionContextNotFound)
	h := NewPoDHandler(&multiDB{sessions: sessionsOn(2)}, secret)
	h.SetPort(p)
	if err := h.ProcessTask(context.Background(), podTask(t, 42)); err != nil {
		t.Errorf("disconnecting sessions the NAS says are gone: %v; that is the outcome wanted", err)
	}

	p, _ = startRecordingNAS(t, secret, radius.CodeCoANAK, rfc3576.ErrorCause_Value_SessionContextNotFound)
	c := NewCoAHandler(&multiDB{sessions: sessionsOn(1)}, secret)
	c.SetPort(p)
	err := c.ProcessTask(context.Background(), coaTask(t, 42))
	if !errors.Is(err, ErrSessionNotFound) || !errorIsSkipRetry(err) {
		t.Errorf("a CoA to a gone session: %v, want ErrSessionNotFound and no retry", err)
	}

	// Any other NAK is a failure, and is retried.
	p, _ = startRecordingNAS(t, secret, radius.CodeDisconnectNAK, rfc3576.ErrorCause_Value_AdministrativelyProhibited)
	h = NewPoDHandler(&multiDB{sessions: sessionsOn(1)}, secret)
	h.SetPort(p)
	err = h.ProcessTask(context.Background(), podTask(t, 43))
	if err == nil || errorIsSkipRetry(err) || errors.Is(err, ErrSessionNotFound) {
		t.Errorf("a refused disconnect: %v, want a retried failure", err)
	}
}

// A session the NAS says does not exist is closed in session history, not
// left open: an open row shows the subscriber online, sums into their usage
// and counts against their connection limit, and one whose Accounting-Stop
// was lost would do that for good. Only that answer closes it; a refusal or
// an acknowledgement leaves the row to accounting.
func TestFR_AAA_003_ASessionTheNASDisownsIsClosed(t *testing.T) {
	secret := []byte("pod-secret")
	for name, tc := range map[string]struct {
		code   radius.Code
		cause  rfc3576.ErrorCause
		closed bool
	}{
		"gone":    {radius.CodeDisconnectNAK, rfc3576.ErrorCause_Value_SessionContextNotFound, true},
		"refused": {radius.CodeDisconnectNAK, rfc3576.ErrorCause_Value_AdministrativelyProhibited, false},
		"ended":   {radius.CodeDisconnectACK, 0, false},
	} {
		p, _ := startRecordingNAS(t, secret, tc.code, tc.cause)
		db := &multiDB{sessions: sessionsOn(2)}
		h := NewPoDHandler(db, secret)
		h.SetPort(p)
		_ = h.ProcessTask(context.Background(), podTask(t, 46))
		got := db.closedSessions()
		if tc.closed && (len(got) != 2 || got[0] != "sess-1" || got[1] != "sess-2") {
			t.Errorf("%s: closed %v, want both sessions", name, got)
		}
		if !tc.closed && len(got) != 0 {
			t.Errorf("%s: closed %v; only a NAS saying the session does not exist closes the row", name, got)
		}
	}

	// A CoA the NAS disowns closes the row the same way.
	p, _ := startRecordingNAS(t, secret, radius.CodeCoANAK, rfc3576.ErrorCause_Value_SessionContextNotFound)
	db := &multiDB{sessions: sessionsOn(1)}
	c := NewCoAHandler(db, secret)
	c.SetPort(p)
	_ = c.ProcessTask(context.Background(), coaTask(t, 47))
	if got := db.closedSessions(); len(got) != 1 {
		t.Errorf("a CoA to a gone session closed %v", got)
	}
}

// If the list cannot be read, the newest session is still reached, as
// before; and a task does not address more sessions than it can finish.
func TestFR_BIL_004_SessionListingDegradesAndIsBounded(t *testing.T) {
	secret := []byte("pod-secret")
	p, nasRec := startRecordingNAS(t, secret, radius.CodeDisconnectACK, 0)
	h := NewPoDHandler(&multiDB{sessions: sessionsOn(3), listErr: errors.New("db: timeout")}, secret)
	h.SetPort(p)
	if err := h.ProcessTask(context.Background(), podTask(t, 44)); err != nil {
		t.Fatal(err)
	}
	if got := nasRec.sessions(); len(got) != 1 || got[0] != "sess-1" {
		t.Errorf("with the list unreadable, reached %v; want the newest session", got)
	}

	p, nasRec = startRecordingNAS(t, secret, radius.CodeDisconnectACK, 0)
	h = NewPoDHandler(&multiDB{sessions: sessionsOn(maxSessionsPerControl + 4)}, secret)
	h.SetPort(p)
	if err := h.ProcessTask(context.Background(), podTask(t, 45)); err != nil {
		t.Fatal(err)
	}
	if got := len(nasRec.sessions()); got != maxSessionsPerControl {
		t.Errorf("reached %d sessions, want the newest %d", got, maxSessionsPerControl)
	}
}
