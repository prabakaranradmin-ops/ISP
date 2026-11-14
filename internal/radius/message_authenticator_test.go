package radius

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // RFC 2869 §5.14 specifies HMAC-MD5
	"testing"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

// TestSignResponse_MessageAuthenticatorVerifies checks the attribute the way a
// NAS does: zero it in the encoded response, put the Request Authenticator
// back in the header, and recompute the HMAC.
func TestSignResponse_MessageAuthenticatorVerifies(t *testing.T) {
	secret := []byte("a-shared-secret-of-enough-length")
	req := radius.New(radius.CodeAccessRequest, secret)
	if err := rfc2865.UserName_SetString(req, "sky"); err != nil {
		t.Fatal(err)
	}

	resp := req.Response(radius.CodeAccessAccept)
	if err := rfc2865.ReplyMessage_SetString(resp, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := signResponse(resp); err != nil {
		t.Fatalf("signResponse: %v", err)
	}
	if resp.Attributes[0].Type != rfc2869.MessageAuthenticator_Type {
		t.Errorf("Message-Authenticator must be the first attribute, got type %d", resp.Attributes[0].Type)
	}

	wire, err := resp.Encode()
	if err != nil {
		t.Fatal(err)
	}
	reqWire, err := req.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !radius.IsAuthenticResponse(wire, reqWire, secret) {
		t.Fatal("Response Authenticator no longer verifies after signing")
	}

	got := rfc2869.MessageAuthenticator_Get(resp)
	if len(got) != md5.Size || bytes.Equal(got, make([]byte, md5.Size)) {
		t.Fatalf("Message-Authenticator is missing or still the zero placeholder: %x", got)
	}

	check := append([]byte(nil), wire...)
	copy(check[4:20], req.Authenticator[:])
	// First attribute: type(1) length(1) value(16), starting at offset 20.
	if check[20] != byte(rfc2869.MessageAuthenticator_Type) || check[21] != 18 {
		t.Fatalf("unexpected first attribute header: % x", check[20:22])
	}
	copy(check[22:38], make([]byte, md5.Size))
	mac := hmac.New(md5.New, secret)
	mac.Write(check)
	if want := mac.Sum(nil); !hmac.Equal(got, want) {
		t.Errorf("Message-Authenticator = %x, a NAS would compute %x", got, want)
	}

	// Negative control: a different secret must not verify.
	other := hmac.New(md5.New, []byte("some-other-secret-entirely-wrong"))
	other.Write(check)
	if hmac.Equal(got, other.Sum(nil)) {
		t.Error("Message-Authenticator verified under the wrong secret")
	}
}

// TestSignResponse_ReplacesPlaceholder covers the EAP path, which sets a
// zeroed attribute before writing: signing must replace it, not add a second.
func TestSignResponse_ReplacesPlaceholder(t *testing.T) {
	req := radius.New(radius.CodeAccessRequest, []byte("a-shared-secret-of-enough-length"))
	resp := req.Response(radius.CodeAccessChallenge)
	if err := rfc2869.MessageAuthenticator_Set(resp, make([]byte, md5.Size)); err != nil {
		t.Fatal(err)
	}
	if err := signResponse(resp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, avp := range resp.Attributes {
		if avp.Type == rfc2869.MessageAuthenticator_Type {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly one Message-Authenticator, got %d", n)
	}
}
