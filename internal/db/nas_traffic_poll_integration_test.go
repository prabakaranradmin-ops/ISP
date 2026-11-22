//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
	"github.com/maaransoft/isp-bss-oss/internal/nastraffic"
	"github.com/maaransoft/isp-bss-oss/internal/nastraffic/snmptest"
)

// The whole FR-NET-006 pipeline, end to end: the production poller, the
// production SNMP client and the production store, polling an SNMP agent over
// a real UDP socket into a real database. Everything except a real router —
// which this machine does not have, and which is the one part this cannot
// prove. See HANDOFF.

func TestFR_NET_006_ThePollerRecordsARoutersTrafficEndToEnd(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()

	agent, err := snmptest.Listen("127.0.0.1:0", "public")
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	agent.Interface(1, "ether1", "ether1", 6, 1000, true, 1_000_000, 2_000_000, true)
	agent.Interface(2, "<pppoe-asha>", "<pppoe-asha>", 23, 0, true, 5, 5, true)

	seedNAS(ctx, t, pool, 1, "127.0.0.1", false)
	ct := "test:public"
	if err := store.SetNASSNMP(ctx, 1, true, &ct, int(agent.Port())); err != nil {
		t.Fatal(err)
	}

	decrypt := func(s string) (string, error) { return strings.TrimPrefix(s, "test:"), nil }
	p := nastraffic.NewPoller(store, &nastraffic.GoSNMPClient{Timeout: time.Second}, decrypt)
	clock := time.Now().Truncate(time.Minute)
	p.SetClock(func() time.Time { return clock })

	p.PollOnce(ctx)
	// Five minutes on, 37.5 MB more in and 75 MB more out: 1 and 2 Mbit/s.
	clock = clock.Add(5 * time.Minute)
	agent.Set(snmptest.OIDIfHCInOctets+".1", gosnmp.Counter64, uint64(1_000_000+37_500_000))
	agent.Set(snmptest.OIDIfHCOutOctets+".1", gosnmp.Counter64, uint64(2_000_000+75_000_000))
	p.PollOnce(ctx)

	ifaces, err := store.ListNASInterfaces(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ifaces) != 1 || ifaces[0].Name != "ether1" {
		t.Fatalf("interfaces %+v — want ether1 alone, the PPPoE session skipped", ifaces)
	}
	if ifaces[0].LatestInBps != 1_000_000 || ifaces[0].LatestOutBps != 2_000_000 {
		t.Errorf("latest rate %d/%d bit/s, want 1 and 2 Mbit/s", ifaces[0].LatestInBps, ifaces[0].LatestOutBps)
	}
	routers, _ := store.ListTrafficRouters(ctx)
	if routers[0].LastOKAt == nil || routers[0].LastError != "" {
		t.Errorf("router status after a good poll: %+v", routers[0])
	}

	// The router stops answering this community. The failure is recorded
	// with a sentence pointing at the community, and the last success kept.
	ct = "test:wrong"
	if err := store.SetNASSNMP(ctx, 1, true, &ct, int(agent.Port())); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Minute)
	p.PollOnce(ctx)
	routers, _ = store.ListTrafficRouters(ctx)
	if !strings.Contains(routers[0].LastError, "community") {
		t.Errorf("a wrong community recorded %q", routers[0].LastError)
	}
	if routers[0].LastOKAt == nil {
		t.Error("a failed poll erased the last success")
	}
}
