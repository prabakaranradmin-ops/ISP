package partner_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/partner"
)

// Partner API unit tests — FR-API-001..003 | MDS §4.22.
//
// The properties here are the ones an attacker probes: whether a key can be
// forged or replayed, whether a signature can be reused, and whether a webhook
// URL can be pointed at the inside of our own network.

func TestFR_API_001_KeyRoundTrip(t *testing.T) {
	gen, err := partner.GenerateKey(partner.KeyEnvLive)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	if !strings.HasPrefix(gen.Plaintext, "pk_live_") {
		t.Errorf("plaintext should carry its environment: %q", gen.Plaintext)
	}
	// The prefix must be derivable from the presented key without the hash,
	// or lookup is impossible and every request would have to compare against
	// every stored key.
	prefix, ok := partner.ParsePrefix(gen.Plaintext)
	if !ok {
		t.Fatal("a freshly generated key must parse")
	}
	if prefix != gen.Prefix {
		t.Errorf("parsed prefix %q != generated prefix %q", prefix, gen.Prefix)
	}
	if !partner.VerifyKey(gen.Plaintext, gen.Hash) {
		t.Error("a key must verify against its own hash")
	}
	// The stored form must not contain the key.
	if strings.Contains(gen.Hash, gen.Plaintext) {
		t.Error("the hash must not embed the plaintext")
	}
}

func TestFR_API_001_ForgedKeysAreRejected(t *testing.T) {
	gen, err := partner.GenerateKey(partner.KeyEnvLive)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	// Same prefix, different secret — the case that matters, because an
	// attacker can read a prefix from a log or a console screenshot.
	other, _ := partner.GenerateKey(partner.KeyEnvLive)
	forged := gen.Prefix + "_" + strings.Split(other.Plaintext, "_")[3]
	if partner.VerifyKey(forged, gen.Hash) {
		t.Error("a key sharing only the prefix must not verify — the prefix is a lookup handle, not a secret")
	}

	for _, bad := range []string{
		"", "garbage", "pk_live_short", "Bearer pk_live_aaaaaaaa_" + strings.Repeat("b", 48),
		"sk_live_aaaaaaaa_" + strings.Repeat("b", 48),
	} {
		if _, ok := partner.ParsePrefix(bad); ok {
			t.Errorf("malformed key %q must not parse — it would cost a database round trip per probe", bad)
		}
	}
}

func TestFR_API_001_KeyUsability(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name string
		key  partner.APIKey
		want bool
	}{
		{"active, no expiry", partner.APIKey{Active: true}, true},
		{"active, future expiry", partner.APIKey{Active: true, ExpiresAt: &future}, true},
		{"expired", partner.APIKey{Active: true, ExpiresAt: &past}, false},
		{"inactive", partner.APIKey{Active: false}, false},
		{"revoked but still flagged active", partner.APIKey{Active: true, RevokedAt: &past}, false},
	}
	for _, tc := range cases {
		if got := tc.key.Usable(now); got != tc.want {
			t.Errorf("%s: Usable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFR_API_001_ScopeValidation(t *testing.T) {
	if err := partner.ValidateScopes(nil); err == nil {
		t.Error("a key with no scopes authorises nothing and must be refused at creation")
	}
	if err := partner.ValidateScopes([]string{"read:everything"}); err == nil {
		t.Error("an unknown scope must be refused: no route checks it, so it would read as working " +
			"right up until somebody relied on it")
	}
	if err := partner.ValidateScopes([]string{partner.ScopeReadSubscribers}); err != nil {
		t.Errorf("a known scope must be accepted: %v", err)
	}
}

// TestFR_API_002_SignatureRoundTripAndReplay covers the property a partner
// depends on: a signature they can verify, that an attacker cannot reuse.
func TestFR_API_002_SignatureRoundTripAndReplay(t *testing.T) {
	const secret = "whsec_testsecret"
	body := []byte(`{"event_id":"abc","event_type":"ticket.created"}`)
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tolerance := 5 * time.Minute

	sig := partner.Sign(secret, now, body)
	if !partner.VerifySignature(secret, sig, body, now, tolerance) {
		t.Fatal("a signature must verify against the body it was computed over")
	}

	// Wrong secret.
	if partner.VerifySignature("whsec_wrong", sig, body, now, tolerance) {
		t.Error("a signature must not verify under a different secret")
	}
	// Tampered body — the whole point of signing.
	if partner.VerifySignature(secret, sig, []byte(`{"event_type":"payment.received"}`), now, tolerance) {
		t.Error("a modified body must invalidate the signature")
	}
	// Replay: the captured header presented an hour later.
	if partner.VerifySignature(secret, sig, body, now.Add(time.Hour), tolerance) {
		t.Error("a signature outside the freshness window must be refused, or a captured " +
			"delivery can be replayed indefinitely")
	}
	// The timestamp is bound into the signature, so swapping it invalidates
	// rather than merely shifting the window.
	forged := strings.Replace(sig, "t="+itoa(now.Unix()), "t="+itoa(now.Add(time.Hour).Unix()), 1)
	if partner.VerifySignature(secret, forged, body, now.Add(time.Hour), tolerance) {
		t.Error("rewriting the timestamp must invalidate the signature — otherwise a replay " +
			"only needs a fresh clock value")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func TestFR_API_002_EventValidation(t *testing.T) {
	if _, err := partner.NewEvent("subscriber.exploded", 1, time.Now()); err == nil {
		t.Error("an unknown event type must be refused at emission")
	}
	ev, err := partner.NewEvent(partner.EventTicketCreated, 42, time.Now())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.EntityID != 42 || ev.EventType != partner.EventTicketCreated {
		t.Error("event fields must round-trip")
	}
	// Idempotency: every event carries a distinct key so a partner can
	// recognise a retry of one it already processed.
	other, _ := partner.NewEvent(partner.EventTicketCreated, 42, time.Now())
	if ev.EventID == other.EventID {
		t.Error("two events must not share an id, or a partner cannot tell a retry from a new event")
	}
	if err := partner.ValidateEvents([]string{"nope"}); err == nil {
		t.Error("subscribing to an unknown event must be refused: it would register cleanly " +
			"and then silently deliver nothing")
	}
}

// TestFR_API_002_SSRFBlocklist is the security boundary. Each of these is a
// real target an attacker would try.
func TestFR_API_002_SSRFBlocklist(t *testing.T) {
	blocked := []struct{ ip, why string }{
		{"169.254.169.254", "cloud instance metadata — the classic SSRF prize"},
		{"127.0.0.1", "loopback"},
		{"10.1.2.3", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"172.16.0.5", "RFC1918"},
		{"100.64.7.9", "our own CGNAT subscriber space"},
		{"::1", "IPv6 loopback"},
		{"fd00::1", "IPv6 unique local"},
		{"fe80::1", "IPv6 link-local"},
		{"::ffff:127.0.0.1", "IPv4-mapped loopback"},
		// The mapped case that actually exercises CIDR matching: unlike the
		// loopback above, IsLoopback does not catch this one, so it can only
		// be blocked by the IPv4 CIDRs matching a 16-byte address.
		{"::ffff:10.0.0.1", "IPv4-mapped RFC1918 — the bypass when only 4-byte addresses are checked"},
		{"::ffff:169.254.169.254", "IPv4-mapped cloud metadata"},
		{"0.0.0.0", "unspecified"},
		// IPv4-*compatible* IPv6 (RFC 5156-deprecated, ::a.b.c.d — distinct from
		// the IPv4-*mapped* form above, ::ffff:a.b.c.d). To4() recognises only
		// the mapped form's marker bytes, so this form previously passed
		// IsBlockedIP's CIDR loop unmatched regardless of what it embedded: a
		// partner could register https://[::169.254.169.254]/ and reach cloud
		// metadata past a blocklist that visibly listed 169.254.0.0/16.
		{"::169.254.169.254", "IPv4-compatible cloud metadata — the bypass this fix closes"},
		{"::10.0.0.1", "IPv4-compatible RFC1918"},
		{"::127.0.0.1", "IPv4-compatible loopback"},
		{"::192.168.1.1", "IPv4-compatible RFC1918"},
	}
	for _, tc := range blocked {
		if !partner.IsBlockedIP(net.ParseIP(tc.ip)) {
			t.Errorf("%s must be blocked (%s)", tc.ip, tc.why)
		}
	}

	for _, ip := range []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"} {
		if partner.IsBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s is a legitimate public address and must be allowed", ip)
		}
	}

	// ::1 and :: are ordinary IPv6 loopback/unspecified, already caught by
	// IsLoopback/IsUnspecified before the compatible-form check ever runs. A
	// regression guard: embeddedIPv4Compatible's byte pattern for "no
	// embedded address" (trailing 0.0.0.0/0.0.0.1) shares its all-zero prefix
	// with exactly these two addresses, so it is worth pinning that both stay
	// blocked after the fix, even though this API cannot show which check did it.
	for _, ip := range []string{"::1", "::"} {
		if !partner.IsBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s must still be blocked, via its real IPv6 semantics", ip)
		}
	}

	// An unparseable address fails closed.
	if !partner.IsBlockedIP(nil) {
		t.Error("an address that could not be parsed must be treated as blocked")
	}
}

func TestFR_API_002_WebhookURLValidation(t *testing.T) {
	refused := []struct{ url, why string }{
		{"http://example.com/hook", "plain HTTP is readable and forgeable in transit"},
		{"https://169.254.169.254/latest/meta-data/", "cloud metadata"},
		{"https://127.0.0.1:8080/hook", "loopback"},
		{"https://10.0.0.1/hook", "RFC1918"},
		{"ftp://example.com/hook", "not http(s)"},
		{"https://", "no host"},
	}
	for _, tc := range refused {
		if err := partner.ValidateWebhookURL(context.Background(), tc.url); err == nil {
			t.Errorf("%s must be refused (%s)", tc.url, tc.why)
		}
	}

	if err := partner.ValidateWebhookURL(context.Background(), "https://hooks.example.com/isp"); err != nil {
		t.Errorf("a normal https endpoint must be accepted: %v", err)
	}
}

// TestFR_API_002_DialerBlocksAfterResolution is the half that holds against
// DNS rebinding: registration-time validation cannot, because the answer can
// change before delivery.
func TestFR_API_002_DialerBlocksAfterResolution(t *testing.T) {
	client := partner.NewSafeHTTPClient(2 * time.Second)
	if client == nil {
		t.Fatal("NewSafeHTTPClient returned nil")
	}

	// A literal private address reaches the dialler's Control hook, which is
	// the only checkpoint a rebinding attack cannot slip past.
	resp, err := client.Get("https://169.254.169.254/latest/meta-data/")
	if err == nil {
		// Only reachable if the guard failed, but leaving the body open
		// would leak a connection on the way to reporting it.
		_ = resp.Body.Close()
		t.Fatal("a request to cloud metadata must fail at the dialler")
	}
	if !strings.Contains(err.Error(), "private or reserved range") {
		t.Errorf("the failure must come from the SSRF guard, not from a timeout that "+
			"would pass on a host where that address happens to answer: %v", err)
	}
}
