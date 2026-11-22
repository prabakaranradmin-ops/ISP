package nastraffic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// IF-MIB columns read on each poll. The 64-bit counters and ifName live in
// ifXTable (.31.1.1.1); the type, description and status in the older ifTable
// (.2.2.1), which every agent has.
const (
	oidIfDescr       = "1.3.6.1.2.1.2.2.1.2"
	oidIfType        = "1.3.6.1.2.1.2.2.1.3"
	oidIfOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	oidIfName        = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCInOctets  = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOutOctets = "1.3.6.1.2.1.31.1.1.1.10"
	oidIfHighSpeed   = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfAlias       = "1.3.6.1.2.1.31.1.1.1.18"
)

// Target is a router to poll.
type Target struct {
	Host      string
	Port      uint16
	Community string
}

// Result is what one poll of one router found.
type Result struct {
	Interfaces []Interface
	// Skipped counts interfaces left out by type (PPP sessions, loopback,
	// tunnels); Truncated counts eligible ones beyond MaxInterfacesPerNAS.
	// Reported so a router whose interfaces are mostly missing can be told
	// apart from one that has few.
	Skipped, Truncated int
}

// SNMPClient reads a router's interfaces. Satisfied by *GoSNMPClient, and by a
// fake in the poller's tests.
type SNMPClient interface {
	Poll(ctx context.Context, t Target) (Result, error)
}

// GoSNMPClient polls over SNMPv2c with github.com/gosnmp/gosnmp.
type GoSNMPClient struct {
	// Timeout is per request, and Retries is how many times each is resent.
	// A poll is several bulk walks, so the worst case for a router that does
	// not answer is roughly Timeout × (Retries+1) for the first walk, which
	// then fails the whole poll — the remaining walks are never attempted.
	Timeout time.Duration
	Retries int
}

// NewGoSNMPClient returns a client with timeouts suited to routers on a LAN or
// a backhaul link: long enough for a busy CPU, short enough that one dead
// router does not hold a worker for most of a poll interval.
func NewGoSNMPClient() *GoSNMPClient {
	return &GoSNMPClient{Timeout: 3 * time.Second, Retries: 1}
}

// ErrNoInterfaces is returned when the agent answered but reported no
// interfaces at all — a community scoped to a view without IF-MIB, usually,
// which looks like success unless it is called out.
var ErrNoInterfaces = errors.New("the router answered but reported no interfaces — check the SNMP community's view includes IF-MIB")

// Poll walks the IF-MIB columns and assembles the interfaces.
func (c *GoSNMPClient) Poll(ctx context.Context, t Target) (Result, error) {
	g := &gosnmp.GoSNMP{
		Target:    t.Host,
		Port:      t.Port,
		Community: t.Community,
		Version:   gosnmp.Version2c,
		Timeout:   c.Timeout,
		Retries:   c.Retries,
		Context:   ctx,
		// Ten rows per GETBULK keeps each response well under a typical
		// 1500-byte MTU even with long interface names, avoiding IP
		// fragmentation, which some firewalls in front of routers drop.
		MaxRepetitions: 10,
	}
	if err := g.Connect(); err != nil {
		return Result{}, fmt.Errorf("connect: %w", err)
	}
	defer g.Conn.Close()

	cols := map[string][]gosnmp.SnmpPDU{}
	for _, oid := range []string{oidIfDescr, oidIfType, oidIfOperStatus, oidIfName,
		oidIfHCInOctets, oidIfHCOutOctets, oidIfHighSpeed, oidIfAlias} {
		pdus, err := g.BulkWalkAll(oid)
		if err != nil {
			return Result{}, fmt.Errorf("walk %s: %w", oid, err)
		}
		cols[oid] = pdus
	}
	return assemble(cols)
}

// assemble joins the walked columns into interfaces by index. Separated from
// Poll so it can be tested on hand-built columns, including the ones real
// agents produce: missing ifXTable, empty ifName, a counter column present
// for some interfaces and not others.
func assemble(cols map[string][]gosnmp.SnmpPDU) (Result, error) {
	type row struct {
		descr, name, alias string
		ifType             int
		hasType            bool
		oper               *bool
		speed              *int64
		in, out            uint64
		hasIn, hasOut      bool
	}
	rows := map[int]*row{}
	get := func(idx int) *row {
		if rows[idx] == nil {
			rows[idx] = &row{}
		}
		return rows[idx]
	}

	for oid, pdus := range cols {
		for _, p := range pdus {
			idx, ok := indexOf(p.Name, oid)
			if !ok {
				continue
			}
			r := get(idx)
			switch oid {
			case oidIfDescr:
				r.descr = asString(p)
			case oidIfName:
				r.name = asString(p)
			case oidIfAlias:
				r.alias = asString(p)
			case oidIfType:
				if v, ok := asUint(p); ok {
					r.ifType, r.hasType = int(v), true
				}
			case oidIfOperStatus:
				if v, ok := asUint(p); ok {
					up := v == 1 // 1 = up; everything else is some kind of not-up
					r.oper = &up
				}
			case oidIfHighSpeed:
				if v, ok := asUint(p); ok {
					s := int64(v)
					r.speed = &s
				}
			case oidIfHCInOctets:
				if v, ok := asUint(p); ok && p.Type == gosnmp.Counter64 {
					r.in, r.hasIn = v, true
				}
			case oidIfHCOutOctets:
				if v, ok := asUint(p); ok && p.Type == gosnmp.Counter64 {
					r.out, r.hasOut = v, true
				}
			}
		}
	}
	if len(rows) == 0 {
		return Result{}, ErrNoInterfaces
	}

	indexes := make([]int, 0, len(rows))
	for idx := range rows {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)

	var res Result
	seen := map[string]bool{}
	for _, idx := range indexes {
		r := rows[idx]
		if r.hasType && !Eligible(r.ifType) {
			res.Skipped++
			continue
		}
		name := CleanName(r.name)
		if name == "" {
			name = CleanName(r.descr)
		}
		if name == "" {
			name = "ifIndex " + strconv.Itoa(idx)
		}
		// Two rows with one name would collide on the history key. Some
		// agents report duplicate ifDescr values ("Ethernet"); the index
		// disambiguates them, and a stable one at that, since the
		// duplication is the agent's own.
		if seen[name] {
			name = name + " (ifIndex " + strconv.Itoa(idx) + ")"
		}
		seen[name] = true

		if len(res.Interfaces) >= MaxInterfacesPerNAS {
			res.Truncated++
			continue
		}
		res.Interfaces = append(res.Interfaces, Interface{
			Index:     idx,
			Name:      name,
			Alias:     CleanName(r.alias),
			Type:      r.ifType,
			SpeedMbps: r.speed,
			OperUp:    r.oper,
			// Both directions or neither: a rate with one side missing would
			// graph as a link with no traffic in that direction.
			HasHC:     r.hasIn && r.hasOut,
			InOctets:  r.in,
			OutOctets: r.out,
		})
	}
	return res, nil
}

// indexOf returns the ifIndex a column entry is for: the single component
// after the column OID. Anything else — a deeper OID, or one from outside the
// column that a misbehaving agent returned past the end of the walk — is
// ignored rather than misread as an interface.
func indexOf(name, column string) (int, bool) {
	name = strings.TrimPrefix(name, ".")
	rest, ok := strings.CutPrefix(name, column+".")
	if !ok || strings.Contains(rest, ".") {
		return 0, false
	}
	idx, err := strconv.Atoi(rest)
	if err != nil || idx <= 0 {
		return 0, false
	}
	return idx, true
}

func asString(p gosnmp.SnmpPDU) string {
	switch v := p.Value.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	}
	return ""
}

func asUint(p gosnmp.SnmpPDU) (uint64, bool) {
	switch p.Type {
	case gosnmp.Integer, gosnmp.Counter32, gosnmp.Gauge32, gosnmp.Counter64, gosnmp.TimeTicks, gosnmp.Uinteger32:
	default:
		return 0, false
	}
	b := gosnmp.ToBigInt(p.Value)
	if b == nil || b.Sign() < 0 || !b.IsUint64() {
		return 0, false
	}
	return b.Uint64(), true
}
