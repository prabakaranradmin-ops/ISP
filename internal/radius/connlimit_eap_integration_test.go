//go:build integration

package radius

import (
	"testing"

	"layeh.com/radius"
)

// EAP is held to the connection limit as PAP is, and its refusal is logged
// as the limit rather than as a failed EAP exchange — FR-AAA-002,
// FR-AAA-006.
func TestFR_AAA_002_EAPIsHeldToTheConnectionLimit(t *testing.T) {
	sub := wgSubscriber(t, "active")
	sub.NTHash = NTPasswordHash(wgPassword)
	sub.ConnectionLimit = 1
	d := wgDaemon(t, sub)
	d.SetEAPSessionStore(NewEAPSessionStore())
	d.SetConnectionLimit(ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter}})
	l := NewAuthLogger(nil)
	d.SetAuthLogger(l)

	resp := eapLogin(t, d, wgUser, wgPassword, wgMikrotikIP)
	if resp.Code != radius.CodeAccessReject {
		t.Fatalf("EAP over the limit: %v", resp.Code)
	}
	var last AuthAttempt
	for {
		select {
		case a := <-l.ch:
			last = a
			continue
		default:
		}
		break
	}
	if last.RejectReason != RejectConnectionLimit || last.AuthMethod != MethodEAP {
		t.Errorf("logged as %q over %q, want %q over eap", last.RejectReason, last.AuthMethod, RejectConnectionLimit)
	}

	// Under the limit, EAP is let in.
	d.SetConnectionLimit(ConnectionLimitEnforce, &fakeConnections{})
	if resp := eapLogin(t, d, wgUser, wgPassword, wgMikrotikIP); resp.Code != radius.CodeAccessAccept {
		t.Errorf("EAP under the limit: %v", resp.Code)
	}
}
