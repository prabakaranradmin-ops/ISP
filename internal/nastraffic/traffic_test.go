package nastraffic

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

// NAS interface traffic — FR-NET-006.

func i64(v int64) *int64 { return &v }

// ── The client, over real UDP ────────────────────────────────────────────────

// A router with more interfaces than one GETBULK returns, so the walk has to
// page; PPP session and loopback interfaces it must leave out; and one with no
// ifName, which has to fall back to its description.
func TestFR_NET_006_TheClientReadsARoutersInterfacesOverSNMP(t *testing.T) {
	a := startAgent(t, "s3cret")
	for i := 1; i <= 24; i++ {
		a.Interface(i, "ether"+strconv.Itoa(i), "ether"+strconv.Itoa(i), 6, 1000, i%2 == 1,
			uint64(i)*1_000_000, uint64(i)*2_000_000, true)
	}
	a.Interface(25, "<pppoe-subscriber1>", "<pppoe-subscriber1>", 23, 0, true, 5, 5, true)
	a.Interface(26, "lo", "lo", 24, 0, true, 0, 0, true)
	a.Interface(27, "", "sfp-sfpplus1", 6, 10000, true, 123, 456, true)
	// A counter above 2^63, so the unsigned path is exercised.
	a.Interface(28, "uplink", "uplink", 6, 100000, true, 1<<63+5, 7, true)

	c := &GoSNMPClient{Timeout: 2 * time.Second, Retries: 0}
	res, err := c.Poll(context.Background(), Target{Host: "127.0.0.1", Port: a.Port(), Community: "s3cret"})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}

	if len(res.Interfaces) != 26 || res.Skipped != 2 {
		t.Fatalf("got %d interfaces and %d skipped, want 26 and 2 (the PPP session and loopback)",
			len(res.Interfaces), res.Skipped)
	}
	byName := map[string]Interface{}
	for _, i := range res.Interfaces {
		byName[i.Name] = i
	}
	e3 := byName["ether3"]
	if e3.Index != 3 || e3.Type != 6 || !e3.HasHC || e3.InOctets != 3_000_000 || e3.OutOctets != 6_000_000 ||
		e3.SpeedMbps == nil || *e3.SpeedMbps != 1000 || e3.OperUp == nil || !*e3.OperUp {
		t.Errorf("ether3 read as %+v", e3)
	}
	if e4 := byName["ether4"]; e4.OperUp == nil || *e4.OperUp {
		t.Error("a down interface was read as up")
	}
	if _, ok := byName["sfp-sfpplus1"]; !ok {
		t.Error("an interface with no ifName did not fall back to its description")
	}
	if up := byName["uplink"]; up.InOctets != 1<<63+5 {
		t.Errorf("a counter above 2^63 was read as %d", up.InOctets)
	}
	for name := range byName {
		if strings.Contains(name, "pppoe") || name == "lo" {
			t.Errorf("%q was polled; per-session and loopback interfaces must be skipped", name)
		}
	}
	// More than one round trip per column proves the walk paged rather than
	// stopping at the first GETBULK's ten rows.
	if a.Requests() <= 8 {
		t.Errorf("%d requests for 28 interfaces across 8 columns — the walk did not page", a.Requests())
	}
}

// SNMPv2c gives no answer at all for a wrong community, so the failure is a
// timeout, and the message has to say the community is the first suspect.
func TestFR_NET_006_AWrongCommunityIsATimeoutThatSaysToCheckTheCommunity(t *testing.T) {
	a := startAgent(t, "right")
	a.Interface(1, "ether1", "ether1", 6, 1000, true, 1, 1, true)

	c := &GoSNMPClient{Timeout: 300 * time.Millisecond, Retries: 0}
	_, err := c.Poll(context.Background(), Target{Host: "127.0.0.1", Port: a.Port(), Community: "wrong"})
	if err == nil {
		t.Fatal("a wrong community produced a successful poll")
	}
	if msg := describePollError(err); !strings.Contains(msg, "community") || !strings.Contains(msg, "No answer") {
		t.Errorf("the failure reads %q", msg)
	}
}

// An agent without ifXTable — no ifName, no 64-bit counters — still lists its
// interfaces, but none of them can be graphed.
func TestFR_NET_006_ARouterWithoutHCCountersIsListedButNotGraphed(t *testing.T) {
	a := startAgent(t, "public")
	a.Set(oidIfDescr+".1", gosnmp.OctetString, []byte("FastEthernet0/0"))
	a.Set(oidIfType+".1", gosnmp.Integer, 6)
	a.Set(oidIfOperStatus+".1", gosnmp.Integer, 1)

	c := &GoSNMPClient{Timeout: 2 * time.Second}
	res, err := c.Poll(context.Background(), Target{Host: "127.0.0.1", Port: a.Port(), Community: "public"})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(res.Interfaces) != 1 || res.Interfaces[0].Name != "FastEthernet0/0" || res.Interfaces[0].HasHC {
		t.Fatalf("got %+v", res.Interfaces)
	}
	if _, o := Rate(&Previous{At: time.Now().Add(-5 * time.Minute)}, res.Interfaces[0], time.Now()); o != OutcomeNoCounters {
		t.Errorf("an interface without 64-bit counters gave %v, want no-counters", o)
	}
}

// An agent whose community is scoped to a view without IF-MIB answers with
// nothing. That must not look like a router with no traffic.
func TestFR_NET_006_AnAgentThatReportsNoInterfacesSaysSo(t *testing.T) {
	a := startAgent(t, "public")
	a.Set("1.3.6.1.2.1.1.5.0", gosnmp.OctetString, []byte("sysName only"))

	c := &GoSNMPClient{Timeout: 2 * time.Second}
	_, err := c.Poll(context.Background(), Target{Host: "127.0.0.1", Port: a.Port(), Community: "public"})
	if !errors.Is(err, ErrNoInterfaces) {
		t.Errorf("got %v, want ErrNoInterfaces", err)
	}
}

// ── Assembly edge cases ──────────────────────────────────────────────────────

func TestFR_NET_006_AssemblyHandlesWhatRealAgentsSend(t *testing.T) {
	pdu := func(col string, idx int, typ gosnmp.Asn1BER, v any) gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Name: "." + col + "." + strconv.Itoa(idx), Type: typ, Value: v}
	}
	res, err := assemble(map[string][]gosnmp.SnmpPDU{
		oidIfDescr: {
			pdu(oidIfDescr, 1, gosnmp.OctetString, []byte("Ethernet")),
			pdu(oidIfDescr, 2, gosnmp.OctetString, []byte("Ethernet")),
			// A deeper OID returned past the column's end is not an interface.
			{Name: "." + oidIfDescr + ".3.1", Type: gosnmp.OctetString, Value: []byte("junk")},
		},
		// Only the in counter for ifIndex 2: half a pair is no pair.
		oidIfHCInOctets:  {pdu(oidIfHCInOctets, 1, gosnmp.Counter64, uint64(10)), pdu(oidIfHCInOctets, 2, gosnmp.Counter64, uint64(10))},
		oidIfHCOutOctets: {pdu(oidIfHCOutOctets, 1, gosnmp.Counter64, uint64(20))},
		// A control character in a device-supplied alias.
		oidIfAlias: {pdu(oidIfAlias, 1, gosnmp.OctetString, []byte("ISP uplink\x1b[31m"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interfaces) != 2 {
		t.Fatalf("got %d interfaces, want 2: %+v", len(res.Interfaces), res.Interfaces)
	}
	// Duplicate descriptions are disambiguated by index, not collapsed.
	if res.Interfaces[0].Name != "Ethernet" || res.Interfaces[1].Name != "Ethernet (ifIndex 2)" {
		t.Errorf("names %q, %q", res.Interfaces[0].Name, res.Interfaces[1].Name)
	}
	if !res.Interfaces[0].HasHC || res.Interfaces[1].HasHC {
		t.Error("an interface with only one 64-bit counter was treated as graphable")
	}
	if strings.ContainsRune(res.Interfaces[0].Alias, 0x1b) {
		t.Error("a control character from the device reached the stored alias")
	}
}

func TestFR_NET_006_TooManyInterfacesAreCappedAndCounted(t *testing.T) {
	var descr []gosnmp.SnmpPDU
	for i := 1; i <= MaxInterfacesPerNAS+10; i++ {
		descr = append(descr, gosnmp.SnmpPDU{
			Name: "." + oidIfDescr + "." + strconv.Itoa(i), Type: gosnmp.OctetString, Value: []byte("vlan" + strconv.Itoa(i)),
		})
	}
	res, err := assemble(map[string][]gosnmp.SnmpPDU{oidIfDescr: descr})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interfaces) != MaxInterfacesPerNAS || res.Truncated != 10 {
		t.Errorf("got %d kept and %d truncated", len(res.Interfaces), res.Truncated)
	}
}

// ── Rates ────────────────────────────────────────────────────────────────────

func TestFR_NET_006_RatesAndTheReadingsThatAreNotRecorded(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	gig := i64(1000)
	cur := func(in, out uint64) Interface {
		return Interface{Name: "ether1", HasHC: true, SpeedMbps: gig, InOctets: in, OutOctets: out}
	}
	prev := &Previous{InOctets: 1_000_000, OutOctets: 2_000_000, At: t0}

	cases := []struct {
		name    string
		prev    *Previous
		cur     Interface
		at      time.Time
		want    Outcome
		inBps   int64
		outBps  int64
		advance bool
	}{
		// 37.5 MB in over 300 s is 1 Mbit/s; 75 MB out is 2 Mbit/s.
		{"an ordinary interval", prev, cur(1_000_000+37_500_000, 2_000_000+75_000_000), t0.Add(5 * time.Minute), OutcomeSample, 1_000_000, 2_000_000, true},
		{"an idle link is a real zero", prev, cur(1_000_000, 2_000_000), t0.Add(5 * time.Minute), OutcomeSample, 0, 0, true},
		{"the first reading", nil, cur(5, 5), t0, OutcomeBaseline, 0, 0, true},
		{"a counter that went backwards", prev, cur(10, 2_000_100), t0.Add(5 * time.Minute), OutcomeReset, 0, 0, true},
		{"a gap longer than three intervals", prev, cur(2_000_000, 3_000_000), t0.Add(16 * time.Minute), OutcomeGap, 0, 0, true},
		{"two polls a moment apart", prev, cur(1_000_100, 2_000_100), t0.Add(2 * time.Second), OutcomeTooSoon, 0, 0, false},
		// 1 Gbit/s link: 60 GB in five minutes is 1.6 Gbit/s.
		{"faster than the link can carry", prev, cur(1_000_000+60_000_000_000, 2_000_000), t0.Add(5 * time.Minute), OutcomeImplausible, 0, 0, true},
		{"no 64-bit counters", prev, Interface{Name: "fa0/0"}, t0.Add(5 * time.Minute), OutcomeNoCounters, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, o := Rate(tc.prev, tc.cur, tc.at)
			if o != tc.want {
				t.Fatalf("outcome %v, want %v", o, tc.want)
			}
			if o == OutcomeSample && (s.InBps != tc.inBps || s.OutBps != tc.outBps || s.IntervalSeconds != 300) {
				t.Errorf("sample %+v, want %d/%d bit/s over 300 s", s, tc.inBps, tc.outBps)
			}
			if o.AdvancesBaseline() != tc.advance {
				t.Errorf("advances baseline = %v, want %v", o.AdvancesBaseline(), tc.advance)
			}
		})
	}

	// With no speed reported there is no ceiling to judge against.
	noSpeed := cur(1_000_000+60_000_000_000, 2_000_000)
	noSpeed.SpeedMbps = nil
	if _, o := Rate(prev, noSpeed, t0.Add(5*time.Minute)); o != OutcomeSample {
		t.Errorf("an interface with no reported speed gave %v", o)
	}
}

func TestFR_NET_006_PPPLoopbackAndTunnelsAreNotEligible(t *testing.T) {
	for typ, want := range map[int]bool{6: true, 135: true, 161: true, 53: true, 23: false, 24: false, 131: false} {
		if Eligible(typ) != want {
			t.Errorf("Eligible(%d) = %v, want %v", typ, !want, want)
		}
	}
}

func TestFR_NET_006_DeviceTextIsCleanedAndCut(t *testing.T) {
	if got := CleanName("  ether1\x00\r\n "); got != "ether1" {
		t.Errorf("got %q", got)
	}
	// Characters, not bytes: 300 Tamil characters cut to 255 characters.
	long := strings.Repeat("அ", 300)
	if got := CleanName(long); len([]rune(got)) != maxNameRunes {
		t.Errorf("cut to %d characters", len([]rune(got)))
	}
}
