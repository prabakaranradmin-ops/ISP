package partner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// SSRF protection for outbound webhooks — FR-API-002 | SecD §9.x | MDS §4.22.
//
// A webhook URL is supplied by a third party and fetched by our server from
// inside the private network. That is a server-side request forgery primitive
// unless it is constrained: a partner who registers
// http://169.254.169.254/latest/meta-data/ is asking us to read cloud instance
// credentials and POST them somewhere, and one who registers
// https://bss_postgres_primary:5432/ is probing the internal service mesh.
//
// The check runs twice, and the second time is the one that matters.
// Validating only at registration is defeated by DNS rebinding: a hostname
// that resolves publicly when the endpoint is created can resolve to
// 169.254.169.254 by the time a delivery fires. So the dialler re-checks every
// address at connect time, after resolution, where the decision cannot be
// undone by a later DNS answer.

// blockedNets are ranges no legitimate partner endpoint lives in.
var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",      // "this network"
		"10.0.0.0/8",     // RFC1918
		"100.64.0.0/10",  // CGNAT — our own subscriber space
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local, incl. cloud metadata at .169.254
		"172.16.0.0/12",  // RFC1918
		"192.0.0.0/24",   // IETF protocol assignments
		"192.168.0.0/16", // RFC1918
		"198.18.0.0/15",  // benchmarking
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
		"2001:db8::/32",  // documentation
		// Deliberately NOT ::ffff:0:0/96. Go represents every parsed IPv4
		// address in 16-byte IPv4-mapped form, so that CIDR matches all of
		// them — listing it blocked 8.8.8.8 and every legitimate partner
		// endpoint. The mapped-address bypass is closed by normalising to
		// 4-byte form in IsBlockedIP instead, which then applies the IPv4
		// rules above to ::ffff:127.0.0.1 exactly as it would to 127.0.0.1.
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// ErrBlockedAddress is returned when a webhook target resolves somewhere it
// must not reach.
type ErrBlockedAddress struct{ Addr string }

func (e *ErrBlockedAddress) Error() string {
	return fmt.Sprintf("partner: webhook target %s is in a private or reserved range", e.Addr)
}

// IsBlockedIP reports whether an address is off limits.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Unspecified and loopback are covered by the CIDRs below, but checking
	// the helpers too means a future edit to the list cannot quietly reopen
	// the most obvious targets.
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// IPv4-mapped IPv6 (::ffff:10.0.0.1) needs no normalisation here:
	// net.IPNet.Contains calls To4 on its argument, so a mapped address is
	// already matched against the IPv4 CIDRs above. Verified rather than
	// assumed — an explicit To4 block was written here first, and removing it
	// changed no test outcome because it never did anything.
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}

	// IPv4-*compatible* IPv6 (::10.0.0.1, RFC 5156-deprecated — distinct from
	// the IPv4-*mapped* form above, ::ffff:10.0.0.1) is not caught by the loop
	// above. To4() recognises only the mapped form's 0xff,0xff marker at bytes
	// 10-11; the compatible form has 0x00,0x00 there instead, so To4() returns
	// nil, IPNet.Contains compares 16 bytes against a 4-byte network and never
	// matches, and this loop passes it through no matter what it embeds —
	// ::169.254.169.254 reads as "not blocked".
	//
	// No mainstream OS actually routes this deprecated form to its embedded
	// IPv4 target (confirmed by dialling one against a real listener during
	// review: RFC 5156, 2008), so this is not exploitable today. It is fixed
	// anyway, because "not exploitable on the OS this happened to be tested
	// against" is not a property this function can see or depend on, and the
	// fix is one bounds check.
	if v4 := embeddedIPv4Compatible(ip); v4 != nil {
		for _, n := range blockedNets {
			if n.Contains(v4) {
				return true
			}
		}
	}
	return false
}

// embeddedIPv4Compatible extracts the trailing 4 bytes of a 16-byte address
// whose first 10 bytes are zero — the IPv4-compatible IPv6 form, ::a.b.c.d —
// and nil for anything else, including plain IPv6 addresses like ::1 or ::2
// that happen to share that same all-zero prefix.
//
// Deliberately not folded into the mapped-form path above: mapped and
// compatible are bit-for-bit different only in bytes 10-11, and conflating
// the two checks would make it easy to silently stop checking one of them
// during a future edit.
func embeddedIPv4Compatible(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil || ip.To4() != nil {
		return nil // already plain IPv4, or not a valid 16-byte address
	}
	for i := 0; i < 10; i++ {
		if ip16[i] != 0 {
			return nil
		}
	}
	// ::0.0.0.0 and ::0.0.0.1 are the unspecified and loopback addresses under
	// this reading — both are indistinguishable from ::/128 and ::1, which
	// IsLoopback/IsUnspecified above already block by their real IPv6
	// semantics. Excluding them here avoids blocking on a coincidental byte
	// pattern rather than on an actual embedded address.
	if ip16[12] == 0 && ip16[13] == 0 && ip16[14] == 0 && (ip16[15] == 0 || ip16[15] == 1) {
		return nil
	}
	return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
}

// ValidateWebhookURL checks a URL at registration time.
//
// This is the friendly check — it gives an operator an immediate, readable
// error instead of a webhook that registers cleanly and then fails silently on
// every delivery. It is not the security boundary; the dialler is.
func ValidateWebhookURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("partner: webhook url is not a valid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("partner: webhook url must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("partner: webhook url has no host")
	}

	host := u.Hostname()
	// A literal IP can be judged immediately. A hostname cannot be judged
	// safely here at all — see the DNS rebinding note above — so resolution
	// failures are not fatal at registration.
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return &ErrBlockedAddress{Addr: host}
		}
		return nil
	}

	// Resolved through the caller's context so the lookup inherits the
	// request's deadline. net.LookupIP has none, and a hostname whose
	// nameserver blackholes packets would hang a console request for as long
	// as the resolver cares to wait.
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		// Deliberately not an error: a partner may register an endpoint whose
		// DNS is not live yet, and refusing would be a worse failure than
		// letting the dialler reject it later.
		return nil
	}
	for _, a := range addrs {
		if IsBlockedIP(a.IP) {
			return &ErrBlockedAddress{Addr: a.IP.String()}
		}
	}
	return nil
}

// NewSafeHTTPClient builds the client webhook deliveries go out on.
//
// Control determines the address AFTER resolution and immediately before the
// connection is made, which is the only place a rebinding attack cannot slip
// between the check and the use. Redirects are refused for the same reason: a
// 302 to 169.254.169.254 would otherwise walk straight past the checks above.
func NewSafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("partner: cannot parse dial address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if IsBlockedIP(ip) {
				return &ErrBlockedAddress{Addr: host}
			}
			return nil
		},
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConnsPerHost:   2,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// DialIsBlocked reports whether a dial to this address would be refused.
// Exported for tests that assert the boundary without opening a socket.
func DialIsBlocked(_ context.Context, address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	return IsBlockedIP(net.ParseIP(host))
}
