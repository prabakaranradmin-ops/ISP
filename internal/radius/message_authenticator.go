package radius

import (
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // RFC 2869 §5.14 specifies HMAC-MD5 for Message-Authenticator
	"fmt"

	"github.com/rs/zerolog/log"
	"layeh.com/radius"
	"layeh.com/radius/rfc2869"
)

// signResponse adds a valid Message-Authenticator (RFC 2869 §5.14) to an
// Access-Accept, Access-Reject or Access-Challenge.
//
// layeh.com/radius never computes this attribute, so it has to be done here.
// A NAS hardened against BlastRADIUS (CVE-2024-3596) silently discards an
// Access response without it and retransmits the request until it gives up,
// which shows up on this side as several accepts for one login that never
// produces a session.
//
// resp must come from Request.Response, so that resp.Authenticator still holds
// the Request Authenticator the HMAC is defined over. The attribute goes first,
// as the BlastRADIUS mitigation guidance asks.
func signResponse(resp *radius.Packet) error {
	resp.Attributes.Del(rfc2869.MessageAuthenticator_Type)
	placeholder := &radius.AVP{
		Type:      rfc2869.MessageAuthenticator_Type,
		Attribute: make(radius.Attribute, md5.Size),
	}
	resp.Attributes = append(radius.Attributes{placeholder}, resp.Attributes...)

	raw, err := resp.MarshalBinary()
	if err != nil {
		return fmt.Errorf("radius: marshal response for Message-Authenticator: %w", err)
	}
	mac := hmac.New(md5.New, resp.Secret)
	mac.Write(raw) //nolint:errcheck // hash.Hash.Write never errors
	placeholder.Attribute = radius.Attribute(mac.Sum(nil))
	return nil
}

// signingWriter signs every Access response on its way out, so no handler
// path can send one unsigned.
type signingWriter struct {
	radius.ResponseWriter
}

func (s signingWriter) Write(resp *radius.Packet) error {
	if err := signResponse(resp); err != nil {
		// Sent unsigned rather than dropped: a NAS that does not require the
		// attribute still gets its answer.
		log.Warn().Err(err).Msg("radius: could not sign response")
	}
	return s.ResponseWriter.Write(resp)
}
