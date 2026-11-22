// Package snmptest is a minimal SNMPv2c agent on a real UDP socket, for
// exercising the NAS traffic poller end to end on a machine with no router to
// poll.
//
// It answers GETBULK and GETNEXT from a sorted table of values, and, like a
// real v2c agent, stays silent on a wrong community. It is built on gosnmp's
// own encoder and decoder, so it proves the poller's OIDs, walks, types and
// timeouts against a well-behaved agent — not that any particular router's
// agent behaves like this one.
//
// It does not import internal/nastraffic, whose own tests use it; the IF-MIB
// column OIDs below are the standard ones, defined here for that reason.
package snmptest

import (
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gosnmp/gosnmp"
)

// IF-MIB columns (RFC 2863).
const (
	OIDIfDescr       = "1.3.6.1.2.1.2.2.1.2"
	OIDIfType        = "1.3.6.1.2.1.2.2.1.3"
	OIDIfOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	OIDIfName        = "1.3.6.1.2.1.31.1.1.1.1"
	OIDIfHCInOctets  = "1.3.6.1.2.1.31.1.1.1.6"
	OIDIfHCOutOctets = "1.3.6.1.2.1.31.1.1.1.10"
	OIDIfHighSpeed   = "1.3.6.1.2.1.31.1.1.1.15"
)

// Agent is a minimal SNMPv2c agent on a real UDP socket, for exercising
// GoSNMPClient end to end — packets encoded, sent, received and decoded — on
// a machine with no router to poll.
//
// It is built on gosnmp's own encoder and decoder, so what it proves is that
// the client's OIDs, walks, type handling and timeouts are right against a
// well-behaved agent. It cannot prove that a particular router's agent
// behaves like this one; that needs a real device. See HANDOFF.
//
// Like a real v2c agent, it does not answer a request with the wrong
// community: SNMPv2c has no authentication-failure response, so a bad
// community is indistinguishable from a router that is not there.
type Agent struct {
	conn      *net.UDPConn
	community string

	mu       sync.Mutex
	oids     []agentOID
	requests int
}

type agentOID struct {
	oid []int
	typ gosnmp.Asn1BER
	val any
}

func parseOID(s string) []int {
	parts := strings.Split(strings.TrimPrefix(s, "."), ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func oidString(o []int) string {
	parts := make([]string, len(o))
	for i, v := range o {
		parts[i] = strconv.Itoa(v)
	}
	return "." + strings.Join(parts, ".")
}

func oidLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// Listen starts an agent on addr ("127.0.0.1:0" for any free port) that
// answers only community. Close stops it.
func Listen(addr, community string) (*Agent, error) {
	udp, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", udp)
	if err != nil {
		return nil, err
	}
	a := &Agent{conn: conn, community: community}
	go a.serve()
	return a, nil
}

// Close stops the agent.
func (a *Agent) Close() { _ = a.conn.Close() }

func (a *Agent) Port() uint16 { return uint16(a.conn.LocalAddr().(*net.UDPAddr).Port) } //nolint:gosec // a UDP port

// set adds or replaces one value.
func (a *Agent) Set(oid string, typ gosnmp.Asn1BER, val any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := parseOID(oid)
	for i := range a.oids {
		if oidString(a.oids[i].oid) == oidString(o) {
			a.oids[i] = agentOID{o, typ, val}
			return
		}
	}
	a.oids = append(a.oids, agentOID{o, typ, val})
	sort.Slice(a.oids, func(i, j int) bool { return oidLess(a.oids[i].oid, a.oids[j].oid) })
}

// iface publishes one interface across ifTable and ifXTable.
func (a *Agent) Interface(idx int, name, descr string, ifType int, speedMbps uint, up bool, in, out uint64, hc bool) {
	i := "." + strconv.Itoa(idx)
	a.Set(OIDIfDescr+i, gosnmp.OctetString, []byte(descr))
	a.Set(OIDIfType+i, gosnmp.Integer, ifType)
	status := 2
	if up {
		status = 1
	}
	a.Set(OIDIfOperStatus+i, gosnmp.Integer, status)
	if name != "" {
		a.Set(OIDIfName+i, gosnmp.OctetString, []byte(name))
	}
	a.Set(OIDIfHighSpeed+i, gosnmp.Gauge32, speedMbps)
	if hc {
		a.Set(OIDIfHCInOctets+i, gosnmp.Counter64, in)
		a.Set(OIDIfHCOutOctets+i, gosnmp.Counter64, out)
	}
}

func (a *Agent) Requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests
}

func (a *Agent) serve() {
	decoder := &gosnmp.GoSNMP{Version: gosnmp.Version2c, Community: a.community, Logger: gosnmp.Default.Logger}
	buf := make([]byte, 65535)
	for {
		n, from, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		req, err := decoder.SnmpDecodePacket(buf[:n])
		if err != nil || req.Community != a.community {
			continue // a real v2c agent stays silent
		}
		a.mu.Lock()
		a.requests++
		vars := a.respond(req)
		a.mu.Unlock()

		resp := &gosnmp.SnmpPacket{
			Version: gosnmp.Version2c, Community: req.Community,
			PDUType: gosnmp.GetResponse, RequestID: req.RequestID,
			Variables: vars, Logger: gosnmp.Default.Logger,
		}
		out, err := resp.MarshalMsg()
		if err != nil {
			continue
		}
		_, _ = a.conn.WriteToUDP(out, from)
	}
}

// respond answers GETBULK and GETNEXT: for each requested OID, the next
// MaxRepetitions values in lexicographic order, then endOfMibView.
func (a *Agent) respond(req *gosnmp.SnmpPacket) []gosnmp.SnmpPDU {
	reps := 1
	if req.PDUType == gosnmp.GetBulkRequest && req.MaxRepetitions > 0 {
		reps = int(req.MaxRepetitions)
	}
	var vars []gosnmp.SnmpPDU
	for _, v := range req.Variables {
		cur := parseOID(v.Name)
		for i := 0; i < reps; i++ {
			next := -1
			for j, o := range a.oids {
				if oidLess(cur, o.oid) {
					next = j
					break
				}
			}
			if next < 0 {
				vars = append(vars, gosnmp.SnmpPDU{Name: oidString(cur), Type: gosnmp.EndOfMibView})
				break
			}
			o := a.oids[next]
			vars = append(vars, gosnmp.SnmpPDU{Name: oidString(o.oid), Type: o.typ, Value: o.val})
			cur = o.oid
		}
	}
	return vars
}
