// RADIUS accounting tests — FR-AAA-003 | DDS §5.2.
//
// Deliberately not behind the `integration` build tag, unlike this package's
// other internal tests. Everything here runs fully in-process — no external
// store at all since the move off Redis — and the defect these cover
// (accounting acknowledged and thrown away) survived precisely because the
// checks that would have caught it were not in the default `go test ./...`
// run.
package radius

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

var acctSecret = []byte("acct-testing123")

// ── Test doubles ────────────────────────────────────────────────────────────

// acctRecorder captures what the daemon asked the store to persist.
type acctRecorder struct {
	mu sync.Mutex

	starts  []acctStartCall
	updates []acctOctetCall
	stops   []acctStopCall

	// matched is what Update/Stop report; false models a record arriving for a
	// session this system has no open row for.
	matched bool
	err     error
}

type acctStartCall struct {
	SubscriberID int
	SessionID    string
	NASIP        string
	AssignedIP   string
}

type acctOctetCall struct {
	SessionID string
	In, Out   int64
}

type acctStopCall struct {
	SessionID string
	In, Out   int64
	Cause     string
}

func (s *acctRecorder) StartSession(_ context.Context, subscriberID int, sessionID, nasIP, assignedIP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.starts = append(s.starts, acctStartCall{subscriberID, sessionID, nasIP, assignedIP})
	return nil
}

func (s *acctRecorder) UpdateSessionOctets(_ context.Context, sessionID string, in, out int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	s.updates = append(s.updates, acctOctetCall{sessionID, in, out})
	return s.matched, nil
}

func (s *acctRecorder) StopSession(_ context.Context, sessionID string, in, out int64, cause string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	s.stops = append(s.stops, acctStopCall{sessionID, in, out, cause})
	return s.matched, nil
}

func (s *acctRecorder) snapshot() ([]acctStartCall, []acctOctetCall, []acctStopCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]acctStartCall(nil), s.starts...),
		append([]acctOctetCall(nil), s.updates...),
		append([]acctStopCall(nil), s.stops...)
}

// acctSubscriberDB resolves usernames to subscribers.
type acctSubscriberDB struct {
	subs map[string]*Subscriber
}

func (db *acctSubscriberDB) GetSubscriberByUsername(_ context.Context, username string) (*Subscriber, error) {
	return db.subs[username], nil
}

// acctMABDB resolves MACs, standing in for the hotspot store.
type acctMABDB struct {
	byMAC map[string]*Subscriber
}

func (db *acctMABDB) AuthorizeMAC(_ context.Context, mac string, _ int) (*Subscriber, error) {
	return db.byMAC[mac], nil
}

// acctWriter captures the responses the handler writes.
type acctWriter struct {
	mu      sync.Mutex
	packets []*radius.Packet
}

func (w *acctWriter) Write(p *radius.Packet) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.packets = append(w.packets, p)
	return nil
}

func (w *acctWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.packets)
}

func (w *acctWriter) last() *radius.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.packets) == 0 {
		return nil
	}
	return w.packets[len(w.packets)-1]
}

// ── Harness ─────────────────────────────────────────────────────────────────

func acctDaemon(t *testing.T, store AccountingStore) *RadiusDaemon {
	t.Helper()

	d := NewRadiusDaemon(":0", acctSecret, &acctSubscriberDB{
		subs: map[string]*Subscriber{
			"pppoe@isp": {ID: 42, Username: "pppoe@isp", Status: "active"},
		},
	}, []byte("verifier-secret-32-bytes-minimum-len"))
	d.SetAccountingStore(store)
	d.SetMABQuerier(&acctMABDB{byMAC: map[string]*Subscriber{
		"AA:BB:CC:DD:EE:FF": {ID: 77, Username: "hotspot@isp", Status: "active"},
		// A voucher-backed grant: authorised, but with no subscriber row behind
		// it, which AuthorizeMAC reports as id 0.
		"11:22:33:44:55:66": {ID: 0, Username: "voucher:9", Status: "active"},
	}})
	return d
}

// acctOpts describes one accounting packet.
type acctOpts struct {
	SessionID       string
	Username        string
	Status          rfc2866.AcctStatusType
	InputOctets     uint32
	OutputOctets    uint32
	InputGigawords  uint32
	OutputGigawords uint32
	NASIP           string
	FramedIP        string
	Cause           rfc2866.AcctTerminateCause
	OmitSessionID   bool
}

func acctRequest(t *testing.T, o acctOpts) *radius.Request {
	t.Helper()
	pkt := radius.New(radius.CodeAccountingRequest, acctSecret)

	if err := rfc2866.AcctStatusType_Set(pkt, o.Status); err != nil {
		t.Fatalf("set Acct-Status-Type: %v", err)
	}
	if !o.OmitSessionID {
		if err := rfc2866.AcctSessionID_SetString(pkt, o.SessionID); err != nil {
			t.Fatalf("set Acct-Session-Id: %v", err)
		}
	}
	if o.Username != "" {
		if err := rfc2865.UserName_SetString(pkt, o.Username); err != nil {
			t.Fatalf("set User-Name: %v", err)
		}
	}
	if err := rfc2866.AcctInputOctets_Set(pkt, rfc2866.AcctInputOctets(o.InputOctets)); err != nil {
		t.Fatalf("set Acct-Input-Octets: %v", err)
	}
	if err := rfc2866.AcctOutputOctets_Set(pkt, rfc2866.AcctOutputOctets(o.OutputOctets)); err != nil {
		t.Fatalf("set Acct-Output-Octets: %v", err)
	}
	if o.InputGigawords > 0 {
		if err := rfc2869.AcctInputGigawords_Set(pkt, rfc2869.AcctInputGigawords(o.InputGigawords)); err != nil {
			t.Fatalf("set Acct-Input-Gigawords: %v", err)
		}
	}
	if o.OutputGigawords > 0 {
		if err := rfc2869.AcctOutputGigawords_Set(pkt, rfc2869.AcctOutputGigawords(o.OutputGigawords)); err != nil {
			t.Fatalf("set Acct-Output-Gigawords: %v", err)
		}
	}
	if o.NASIP != "" {
		if err := rfc2865.NASIPAddress_Set(pkt, net.ParseIP(o.NASIP)); err != nil {
			t.Fatalf("set NAS-IP-Address: %v", err)
		}
	}
	if o.FramedIP != "" {
		if err := rfc2865.FramedIPAddress_Set(pkt, net.ParseIP(o.FramedIP)); err != nil {
			t.Fatalf("set Framed-IP-Address: %v", err)
		}
	}
	if o.Cause != 0 {
		if err := rfc2866.AcctTerminateCause_Set(pkt, o.Cause); err != nil {
			t.Fatalf("set Acct-Terminate-Cause: %v", err)
		}
	}

	return &radius.Request{
		Packet:     pkt,
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 41000},
	}
}

func acctCounterVec(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("counter vec %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func acctCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// ── The session lifecycle actually reaches storage ──────────────────────────

// TestFR_AAA_003_SessionLifecycleIsPersisted is the regression this whole file
// exists for. The daemon used to answer Accounting-Response and record nothing,
// so subscriber_session_history stayed empty and every feature reading it — FUP
// enforcement, CoA targeting, LEA lookups, portal usage — silently had no data.
func TestFR_AAA_003_SessionLifecycleIsPersisted(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)
	ctx := context.Background()

	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-1", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_Start,
		NASIP:  "10.10.0.1", FramedIP: "100.64.0.5",
	}))
	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-1", Username: "pppoe@isp",
		Status:      rfc2866.AcctStatusType_Value_InterimUpdate,
		InputOctets: 1_000_000, OutputOctets: 2_000_000,
	}))
	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-1", Username: "pppoe@isp",
		Status:      rfc2866.AcctStatusType_Value_Stop,
		InputOctets: 5_000_000, OutputOctets: 6_000_000,
		Cause: rfc2866.AcctTerminateCause_Value_UserRequest,
	}))

	starts, updates, stops := store.snapshot()

	if len(starts) != 1 {
		t.Fatalf("Accounting-Start must open a session row, got %d calls", len(starts))
	}
	if starts[0].SubscriberID != 42 {
		t.Errorf("session must be attributed to the authenticated subscriber: want 42, got %d", starts[0].SubscriberID)
	}
	if starts[0].SessionID != "sess-1" {
		t.Errorf("session id: want sess-1, got %q", starts[0].SessionID)
	}
	// The NAS's own address, not the packet source: CoA has to be sent back to
	// the former, and behind NAT they differ.
	if starts[0].NASIP != "10.10.0.1" {
		t.Errorf("nas ip: want the NAS-IP-Address attribute 10.10.0.1, got %q", starts[0].NASIP)
	}
	if starts[0].AssignedIP != "100.64.0.5" {
		t.Errorf("assigned ip: want 100.64.0.5, got %q — LEA lookups resolve against this", starts[0].AssignedIP)
	}

	if len(updates) != 1 || updates[0].In != 1_000_000 || updates[0].Out != 2_000_000 {
		t.Errorf("interim update must carry both counters, got %+v", updates)
	}
	if len(stops) != 1 || stops[0].In != 5_000_000 || stops[0].Out != 6_000_000 {
		t.Errorf("stop must carry final counters, got %+v", stops)
	}
	if len(stops) == 1 && stops[0].Cause != "User-Request" {
		t.Errorf("terminate cause: want User-Request, got %q", stops[0].Cause)
	}
}

// TestFR_AAA_003_GigawordsAreCombined covers the counter wrap.
//
// Acct-Input-Octets is 32 bits and wraps every 4 GiB, with the wrap count in
// Acct-Input-Gigawords (RFC 2869 §5.1). Reading only the low word makes usage
// appear to reset every 4 GiB — which would disable FUP enforcement for exactly
// the heavy users it exists to catch, and in the direction that never throttles.
func TestFR_AAA_003_GigawordsAreCombined(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)

	// 3 gigawords + 1000 bytes ≈ 12 GiB.
	d.handleAccounting(context.Background(), &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-big", Username: "pppoe@isp",
		Status:         rfc2866.AcctStatusType_Value_InterimUpdate,
		InputOctets:    1000,
		InputGigawords: 3,
		OutputOctets:   500,
		// 2^32 * 1 + 500
		OutputGigawords: 1,
	}))

	_, updates, _ := store.snapshot()
	if len(updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(updates))
	}
	const wantIn = int64(3)<<32 | 1000
	const wantOut = int64(1)<<32 | 500
	if updates[0].In != wantIn {
		t.Errorf("input octets: want %d (3 gigawords + 1000), got %d — a 32-bit read would "+
			"report %d and hide 12 GiB of usage", wantIn, updates[0].In, 1000)
	}
	if updates[0].Out != wantOut {
		t.Errorf("output octets: want %d, got %d", wantOut, updates[0].Out)
	}
}

// TestFR_AAA_003_HotspotSessionIsAttributedViaMAC — a MAB session's User-Name
// is the MAC, so resolving it as a username would find nobody and the session
// would go unrecorded. That would leave hotspot usage invisible to FUP, which
// is the enforcement FR-HSP-003 promises.
func TestFR_AAA_003_HotspotSessionIsAttributedViaMAC(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)

	d.handleAccounting(context.Background(), &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-hs", Username: "aa-bb-cc-dd-ee-ff", // NAS spelling
		Status: rfc2866.AcctStatusType_Value_Start,
		NASIP:  "10.10.0.2",
	}))

	starts, _, _ := store.snapshot()
	if len(starts) != 1 {
		t.Fatalf("a MAB session must be recorded, got %d starts", len(starts))
	}
	if starts[0].SubscriberID != 77 {
		t.Errorf("subscriber: want 77 (resolved through the hotspot MAC lookup), got %d", starts[0].SubscriberID)
	}
}

// acctGrantMeter records voucher-session metering.
type acctGrantMeter struct {
	mu      sync.Mutex
	calls   []acctGrantCall
	matched bool
	err     error
}

type acctGrantCall struct {
	MAC       string
	SessionID string
	NASIP     string
	Bytes     int64
}

func (m *acctGrantMeter) RecordGrantUsage(_ context.Context, mac, sessionID, nasIP string, bytesUsed int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	m.calls = append(m.calls, acctGrantCall{mac, sessionID, nasIP, bytesUsed})
	return m.matched, nil
}

func (m *acctGrantMeter) snapshot() []acctGrantCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]acctGrantCall(nil), m.calls...)
}

// TestFR_HSP_001_VoucherSessionIsMeteredOnItsGrant — a voucher grant has no
// subscriber row, so session history cannot hold it (its subscriber_id is NOT
// NULL with a foreign key). Before migration 035 the usage was simply
// discarded, which is why a voucher's data cap went unenforced.
func TestFR_HSP_001_VoucherSessionIsMeteredOnItsGrant(t *testing.T) {
	store := &acctRecorder{matched: true}
	meter := &acctGrantMeter{matched: true}
	d := acctDaemon(t, store)
	d.SetGrantUsageDB(meter)
	w := &acctWriter{}

	d.handleAccounting(context.Background(), w, acctRequest(t, acctOpts{
		SessionID: "sess-voucher", Username: "11:22:33:44:55:66",
		Status:      rfc2866.AcctStatusType_Value_InterimUpdate,
		InputOctets: 700_000_000, OutputOctets: 300_000_000,
		NASIP: "10.10.0.4",
	}))

	calls := meter.snapshot()
	if len(calls) != 1 {
		t.Fatalf("a voucher session must be metered on its grant, got %d calls", len(calls))
	}
	if calls[0].MAC != "11:22:33:44:55:66" {
		t.Errorf("mac: got %q", calls[0].MAC)
	}
	// Total, not per-direction: a voucher's cap is a single volume.
	if calls[0].Bytes != 1_000_000_000 {
		t.Errorf("want the combined total 1000000000, got %d", calls[0].Bytes)
	}
	// The session and NAS are captured so an exhausted voucher has something to
	// disconnect — a Disconnect-Request is addressed by Acct-Session-Id.
	if calls[0].SessionID != "sess-voucher" || calls[0].NASIP != "10.10.0.4" {
		t.Errorf("the grant must capture where to send a disconnect, got %+v", calls[0])
	}

	// It must not also be written to session history, which would violate the
	// foreign key.
	starts, updates, _ := store.snapshot()
	if len(starts) != 0 || len(updates) != 0 {
		t.Errorf("a voucher session must not reach subscriber session history: %+v %+v", starts, updates)
	}
	if w.count() != 1 {
		t.Error("the NAS must still be acknowledged")
	}
}

// TestFR_HSP_001_SubscriberMABSessionIsNotMeteredAsAVoucher — a registered
// device belongs to a subscriber and is metered the ordinary way, so that FUP
// throttles it rather than the quota scanner disconnecting it.
func TestFR_HSP_001_SubscriberMABSessionIsNotMeteredAsAVoucher(t *testing.T) {
	store := &acctRecorder{matched: true}
	meter := &acctGrantMeter{matched: true}
	d := acctDaemon(t, store)
	d.SetGrantUsageDB(meter)

	// AA:BB:CC:DD:EE:FF resolves to subscriber 77 in the harness.
	d.handleAccounting(context.Background(), &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-mab", Username: "AA:BB:CC:DD:EE:FF",
		Status: rfc2866.AcctStatusType_Value_Start, NASIP: "10.10.0.2",
	}))

	if got := meter.snapshot(); len(got) != 0 {
		t.Errorf("a subscriber's MAB session must not be metered as a voucher, got %+v — it "+
			"would be disconnected on cap instead of throttled by FUP", got)
	}
	starts, _, _ := store.snapshot()
	if len(starts) != 1 || starts[0].SubscriberID != 77 {
		t.Errorf("it must go to session history against its subscriber, got %+v", starts)
	}
}

// TestFR_HSP_001_VoucherSessionWithNoLiveGrantIsCounted — the voucher expired
// or was revoked mid-session. Counted rather than dropped, since a rising
// number means sessions are outliving their authorisation.
func TestFR_HSP_001_VoucherSessionWithNoLiveGrantIsCounted(t *testing.T) {
	meter := &acctGrantMeter{matched: false}
	d := acctDaemon(t, &acctRecorder{matched: true})
	d.SetGrantUsageDB(meter)

	before := acctCounter(t, radiusAcctUnmatched)
	d.handleAccounting(context.Background(), &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-gone", Username: "11:22:33:44:55:66",
		Status: rfc2866.AcctStatusType_Value_InterimUpdate, InputOctets: 10,
	}))

	if got := acctCounter(t, radiusAcctUnmatched); got != before+1 {
		t.Errorf("radius_acct_unmatched_total: want +1, got %v", got-before)
	}
}

// TestFR_HSP_001_NoGrantMeterFallsBackToTheOldBehaviour — without the meter
// wired, a voucher session is simply unrecorded, as before migration 035.
func TestFR_HSP_001_NoGrantMeterFallsBackToTheOldBehaviour(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)
	w := &acctWriter{}

	d.handleAccounting(context.Background(), w, acctRequest(t, acctOpts{
		SessionID: "sess-voucher", Username: "11:22:33:44:55:66",
		Status: rfc2866.AcctStatusType_Value_Start,
	}))

	starts, _, _ := store.snapshot()
	if len(starts) != 0 {
		t.Errorf("a voucher session has no subscriber to attribute to, got %+v", starts)
	}
	if w.count() != 1 {
		t.Error("the NAS must be acknowledged even when the record cannot be attributed")
	}
}

// ── The NAS is always answered ──────────────────────────────────────────────

// TestFR_AAA_003_NASIsAlwaysAcknowledged — RADIUS accounting has no "try later"
// reply. A NAS that gets no response retransmits, and on repeated failure many
// implementations discard the record or drop the session, so withholding the
// ACK turns a database blip into lost usage and disconnected customers.
func TestFR_AAA_003_NASIsAlwaysAcknowledged(t *testing.T) {
	tests := []struct {
		name  string
		store *acctRecorder
		opts  acctOpts
	}{
		{"store failure", &acctRecorder{err: errAcctBoom}, acctOpts{
			SessionID: "s", Username: "pppoe@isp", Status: rfc2866.AcctStatusType_Value_Start}},
		{"no session id", &acctRecorder{matched: true}, acctOpts{
			Username: "pppoe@isp", Status: rfc2866.AcctStatusType_Value_Start, OmitSessionID: true}},
		{"unknown user", &acctRecorder{matched: true}, acctOpts{
			SessionID: "s", Username: "ghost@isp", Status: rfc2866.AcctStatusType_Value_Start}},
		{"unmatched stop", &acctRecorder{matched: false}, acctOpts{
			SessionID: "s", Username: "pppoe@isp", Status: rfc2866.AcctStatusType_Value_Stop}},
		{"accounting-on", &acctRecorder{matched: true}, acctOpts{
			SessionID: "s", Status: rfc2866.AcctStatusType_Value_AccountingOn}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := acctDaemon(t, tc.store)
			w := &acctWriter{}
			d.handleAccounting(context.Background(), w, acctRequest(t, tc.opts))

			if w.count() != 1 {
				t.Fatalf("want exactly 1 response, got %d", w.count())
			}
			if got := w.last(); got == nil || got.Code != radius.CodeAccountingResponse {
				t.Errorf("want Accounting-Response, got %v", got)
			}
		})
	}
}

var errAcctBoom = &acctError{"database is down"}

type acctError struct{ msg string }

func (e *acctError) Error() string { return e.msg }

// TestFR_AAA_003_UnmatchedRecordsAreCounted — a Stop for a session that was
// never opened means usage is being lost, which must be visible rather than a
// silent no-op. This is the shape of a daemon restart mid-session.
func TestFR_AAA_003_UnmatchedRecordsAreCounted(t *testing.T) {
	store := &acctRecorder{matched: false}
	d := acctDaemon(t, store)

	before := acctCounter(t, radiusAcctUnmatched)
	d.handleAccounting(context.Background(), &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-orphan", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_Stop, InputOctets: 10,
	}))

	if got := acctCounter(t, radiusAcctUnmatched); got != before+1 {
		t.Errorf("radius_acct_unmatched_total: want +1, got %v — a Stop with no open session "+
			"loses that session's usage and must not pass silently", got-before)
	}
}

// ── Deduplication ───────────────────────────────────────────────────────────

// TestFR_AAA_003_RetransmitIsDedupedPerSession covers the retransmit a NAS
// sends when it sees no Accounting-Response.
//
// The key is per session, per record type, per counter value. The earlier
// implementation keyed on NAS-Identifier, which is per *device*: two
// subscribers on one NAS with equal octet counts would suppress each other's
// records.
func TestFR_AAA_003_RetransmitIsDedupedPerSession(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)
	ctx := context.Background()

	rec := func() *radius.Request {
		return acctRequest(t, acctOpts{
			SessionID: "sess-dup", Username: "pppoe@isp",
			Status: rfc2866.AcctStatusType_Value_InterimUpdate, InputOctets: 4242,
		})
	}

	before := acctCounter(t, radiusDedupSkipped)
	d.handleAccounting(ctx, &acctWriter{}, rec())
	d.handleAccounting(ctx, &acctWriter{}, rec())

	_, updates, _ := store.snapshot()
	if len(updates) != 1 {
		t.Errorf("an exact retransmit must reach the store once, got %d", len(updates))
	}
	if got := acctCounter(t, radiusDedupSkipped); got != before+1 {
		t.Errorf("radius_acct_dedup_skipped_total: want +1, got %v", got-before)
	}

	// A different session with identical counters is a different record, not a
	// duplicate — this is what the NAS-Identifier key got wrong.
	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-other", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_InterimUpdate, InputOctets: 4242,
	}))
	_, updates, _ = store.snapshot()
	if len(updates) != 2 {
		t.Errorf("a second session with identical counters must not be deduped away, got %d updates", len(updates))
	}

	// And a counter advance on the same session is a new record.
	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-dup", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_InterimUpdate, InputOctets: 9999,
	}))
	_, updates, _ = store.snapshot()
	if len(updates) != 3 {
		t.Errorf("an advanced counter must not be deduped, got %d updates", len(updates))
	}

	if got := d.acctDedupSize(); got != 3 {
		t.Errorf("want one dedup key per distinct record, got %d", got)
	}
}

// TestFR_AAA_003_StartAndStopAreDistinctRecords — a Start and a Stop for one
// session can carry identical counters (a session that moved no traffic).
// Without the record type in the key, the Stop would be swallowed as a
// duplicate of the Start and the session would never close.
func TestFR_AAA_003_StartAndStopAreDistinctRecords(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)
	ctx := context.Background()

	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-idle", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_Start,
	}))
	d.handleAccounting(ctx, &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-idle", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_Stop,
	}))

	starts, _, stops := store.snapshot()
	if len(starts) != 1 {
		t.Errorf("want 1 start, got %d", len(starts))
	}
	if len(stops) != 1 {
		t.Error("a Stop carrying the same counters as its Start must not be deduped away — " +
			"the session would stay open forever and keep counting toward FUP")
	}
}

// TestFR_AAA_003_NoStoreStillAcknowledges — a deployment that has not wired a
// store must not leave its NAS retransmitting forever.
func TestFR_AAA_003_NoStoreStillAcknowledges(t *testing.T) {
	d := acctDaemon(t, nil)
	d.SetAccountingStore(nil)

	w := &acctWriter{}
	d.handleAccounting(context.Background(), w, acctRequest(t, acctOpts{
		SessionID: "s", Username: "pppoe@isp", Status: rfc2866.AcctStatusType_Value_Start,
	}))

	if got := w.last(); got == nil || got.Code != radius.CodeAccountingResponse {
		t.Errorf("want Accounting-Response even with no store, got %v", got)
	}
}

// TestFR_AAA_003_StatusLabelsAreBounded keeps an exotic NAS from creating a new
// Prometheus time series per status value it invents.
func TestFR_AAA_003_StatusLabelsAreBounded(t *testing.T) {
	seen := map[string]bool{}
	for v := 0; v < 300; v++ {
		seen[acctStatusLabel(rfc2866.AcctStatusType(v))] = true //nolint:gosec // bounded loop
	}
	if len(seen) > 6 {
		t.Errorf("status labels must stay a bounded set, got %d distinct: %v", len(seen), seen)
	}
}

// TestFR_AAA_003_NASAddressFallsBackToPacketSource — some NAS firmware omits
// NAS-IP-Address. Falling back to the source address keeps the session
// attributable to a device rather than storing an empty string the CoA sender
// cannot dial.
func TestFR_AAA_003_NASAddressFallsBackToPacketSource(t *testing.T) {
	store := &acctRecorder{matched: true}
	d := acctDaemon(t, store)

	d.handleAccounting(context.Background(), &acctWriter{}, acctRequest(t, acctOpts{
		SessionID: "sess-nonas", Username: "pppoe@isp",
		Status: rfc2866.AcctStatusType_Value_Start, // no NASIP set
	}))

	starts, _, _ := store.snapshot()
	if len(starts) != 1 {
		t.Fatalf("want 1 start, got %d", len(starts))
	}
	if starts[0].NASIP != "203.0.113.7" {
		t.Errorf("nas ip must fall back to the packet source, want 203.0.113.7, got %q", starts[0].NASIP)
	}
	// Counters exist to make the metric name real, not just the behaviour.
	if acctCounterVec(t, radiusAcctProcessed, "start", "persisted") == 0 {
		t.Error("a persisted start must be counted")
	}
}
