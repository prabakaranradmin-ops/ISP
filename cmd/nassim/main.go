// Command nassim simulates a MikroTik NAS end to end against
// aaa_core_daemon, for testing a real Routers-screen registration without a
// physical or virtual router: sends a real Access-Request, decodes the
// Access-Accept's Mikrotik-Rate-Limit vendor attribute, optionally sends an
// Accounting-Start so the session is live enough for CoA to target, then
// listens for and acknowledges one CoA-Request or Disconnect-Request — the
// same protocol exchange a real MikroTik speaks, without needing one.
//
// Usage:
//
//	nassim -secret <the-secret-registered-on-the-Routers-screen> \
//	       -username demo_priya -password 'DemoPassword#1'
//
// -secret, and the address this runs from, must match a device already
// registered on the console's Routers screen — aaa_core_daemon resolves the
// shared secret (and vendor) by the packet's source IP
// (internal/nas/resolver.go), exactly as it would for a real router. Running
// this from the same machine as the daemon and registering the NAS at
// 127.0.0.1 is the simplest way to get that match without real network
// hardware.
//
// After a successful auth, it waits for a CoA/PoD — trigger one from the
// console (a speed override, a plan change on an active session, or a FUP
// breach) while it's waiting, to verify mid-session control actually
// reaches something listening on the wire.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
)

// MikroTik-Rate-Limit: vendor 14988, attribute 8 — the same constants
// internal/nas/mikrotik.go builds, decoded here instead of built.
const (
	mikrotikVendorID      = 14988
	mikrotikRateLimitType = 8
)

func main() {
	var (
		authAddr      = flag.String("auth-addr", "127.0.0.1:1812", "RADIUS authentication address")
		acctAddr      = flag.String("acct-addr", "127.0.0.1:1813", "RADIUS accounting address")
		coaListenAddr = flag.String("coa-listen", "127.0.0.1:1700", "address to listen on for CoA/PoD — match the CoA port registered for this NAS")
		secret        = flag.String("secret", "", "RADIUS shared secret — must match the secret registered for this NAS on the Routers screen")
		username      = flag.String("username", "", "subscriber username")
		password      = flag.String("password", "", "subscriber password")
		sessionID     = flag.String("session-id", "", "Acct-Session-Id to use (default: generated)")
		sendAcctStart = flag.Bool("acct-start", true, "send an Accounting-Start after a successful auth, so the session is live enough for CoA to target it")
		listenCoA     = flag.Bool("listen-coa", true, "after authenticating, wait for one CoA/PoD packet and acknowledge it")
		coaTimeout    = flag.Duration("coa-timeout", 2*time.Minute, "how long to wait for a CoA/PoD packet before giving up")
		timeout       = flag.Duration("timeout", 5*time.Second, "per-request timeout for the Access-Request and Accounting-Start")
	)
	flag.Parse()

	if *secret == "" || *username == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "nassim: -secret, -username and the subscriber's login credential flag are all required")
		flag.Usage()
		os.Exit(1)
	}
	if *sessionID == "" {
		*sessionID = fmt.Sprintf("nassim-%d", time.Now().UnixNano())
	}
	secretBytes := []byte(*secret)

	fmt.Println("=== Access-Request ===")
	authReq := radius.New(radius.CodeAccessRequest, secretBytes)
	check(rfc2865.UserName_SetString(authReq, *username))
	check(rfc2865.UserPassword_SetString(authReq, *password))

	authResp, err := exchange(authReq, *authAddr, *timeout)
	if err != nil {
		fatalf("auth exchange with %s failed: %v", *authAddr, err)
	}
	fmt.Printf("  %s -> %v\n", *authAddr, authResp.Code)
	if authResp.Code != radius.CodeAccessAccept {
		fmt.Println("  stopping here — a rejected subscriber never receives a rate-limit attribute or a CoA")
		os.Exit(1)
	}
	printMikrotikRateLimit("  ", authResp)

	if *sendAcctStart {
		fmt.Println("\n=== Accounting-Start ===")
		acctReq := radius.New(radius.CodeAccountingRequest, secretBytes)
		check(rfc2865.UserName_SetString(acctReq, *username))
		check(rfc2866.AcctStatusType_Set(acctReq, rfc2866.AcctStatusType_Value_Start))
		check(rfc2866.AcctSessionID_SetString(acctReq, *sessionID))
		acctResp, err := exchange(acctReq, *acctAddr, *timeout)
		if err != nil {
			fatalf("accounting-start with %s failed: %v", *acctAddr, err)
		}
		fmt.Printf("  %s -> %v (session %s)\n", *acctAddr, acctResp.Code, *sessionID)
	}

	if *listenCoA {
		fmt.Printf("\n=== Waiting up to %v for a CoA/PoD on %s ===\n", *coaTimeout, *coaListenAddr)
		fmt.Println("  Trigger one from the console now — a speed override, a plan change, or a FUP breach.")
		if err := listenAndAck(*coaListenAddr, secretBytes, *coaTimeout); err != nil {
			fatalf("%v", err)
		}
	}
}

func exchange(pkt *radius.Packet, addr string, timeout time.Duration) (*radius.Packet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return radius.Exchange(ctx, pkt, addr)
}

// printMikrotikRateLimit decodes and prints the Mikrotik-Rate-Limit vendor
// attribute (14988/8) if the packet carries one — the value RouterOS itself
// would apply to the session's queue.
func printMikrotikRateLimit(indent string, p *radius.Packet) {
	raw := p.Get(radius.Type(26)) // Vendor-Specific, RFC 2865
	if raw == nil {
		fmt.Printf("%sno Vendor-Specific attribute in the response — the subscriber would connect with no bandwidth limit applied\n", indent)
		return
	}
	vendorID, value, err := radius.VendorSpecific(raw)
	if err != nil {
		fmt.Printf("%scould not parse the Vendor-Specific attribute: %v\n", indent, err)
		return
	}
	if vendorID != mikrotikVendorID || len(value) < 2 || value[0] != mikrotikRateLimitType {
		fmt.Printf("%svendor-specific attribute present, but not a MikroTik rate limit (vendor %d)\n", indent, vendorID)
		return
	}
	fmt.Printf("%sMikrotik-Rate-Limit: %s\n", indent, string(value[2:]))
}

// listenAndAck waits for one CoA-Request or Disconnect-Request, prints what
// it carries, and acknowledges it — exactly what a real NAS's RADIUS client
// does on receiving one, proving the daemon's mid-session control path
// actually reaches something listening on the wire, with the right secret.
func listenAndAck(addr string, secret []byte, timeout time.Duration) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer conn.Close() //nolint:errcheck

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}

	buf := make([]byte, radius.MaxPacketLength)
	n, from, err := conn.ReadFromUDP(buf)
	if err != nil {
		return fmt.Errorf("no CoA/PoD received within %v: %w", timeout, err)
	}

	req, err := radius.Parse(buf[:n], secret)
	if err != nil {
		return fmt.Errorf("parse packet from %s: %w (wrong shared secret?)", from, err)
	}

	fmt.Printf("  received %v from %s\n", req.Code, from)
	if sid := rfc2866.AcctSessionID_GetString(req); sid != "" {
		fmt.Printf("  Acct-Session-Id: %s\n", sid)
	}

	var ackCode radius.Code
	switch req.Code {
	case radius.CodeCoARequest:
		printMikrotikRateLimit("  ", req)
		ackCode = radius.CodeCoAACK
	case radius.CodeDisconnectRequest:
		ackCode = radius.CodeDisconnectACK
	default:
		return fmt.Errorf("unexpected packet code %v (wanted CoA-Request or Disconnect-Request)", req.Code)
	}

	resp := req.Response(ackCode)
	encoded, err := resp.Encode()
	if err != nil {
		return fmt.Errorf("encode %v: %w", ackCode, err)
	}
	if _, err := conn.WriteToUDP(encoded, from); err != nil {
		return fmt.Errorf("send %v: %w", ackCode, err)
	}
	fmt.Printf("  acknowledged with %v\n", ackCode)
	return nil
}

func check(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "nassim: "+format+"\n", args...)
	os.Exit(1)
}
