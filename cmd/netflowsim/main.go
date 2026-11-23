// Command netflowsim is a mock NetFlow v9 / CGNAT exporter. It stands in for a
// BNG or CGNAT gateway: it sends a Template FlowSet, then Data FlowSets of
// synthetic translated flows, to a collector over UDP — the Epic 5 testing-plan
// simulator for "streaming mock NetFlow records into the collector".
//
// Usage:
//
//	netflowsim -target 127.0.0.1:9995 -flows 200 -rate 50
//
// It re-sends the template every -template-every packets, the way a real
// exporter refreshes templates so a collector that starts mid-stream catches up.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/netflow"
)

func main() {
	var (
		target        = flag.String("target", "127.0.0.1:9995", "collector UDP address")
		total         = flag.Int("flows", 100, "total flow records to send")
		perPacket     = flag.Int("per-packet", 20, "flow records per data packet")
		rate          = flag.Int("rate", 50, "packets per second")
		templateEvery = flag.Int("template-every", 10, "re-send the template every N data packets")
		sourceID      = flag.Uint("source-id", 1, "exporter observation domain (source id)")
	)
	flag.Parse()

	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "udp", *target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netflowsim: dial %s: %v\n", *target, err)
		os.Exit(1)
	}
	defer conn.Close() //nolint:errcheck

	start := time.Now()
	ticker := time.NewTicker(time.Second / time.Duration(max(1, *rate)))
	defer ticker.Stop()

	seq := uint32(0)
	sent := 0
	packetNo := 0
	fmt.Printf("netflowsim: sending %d flows to %s (%d/packet, %d pkt/s)\n", *total, *target, *perPacket, *rate)

	for sent < *total {
		<-ticker.C
		n := min(*perPacket, *total-sent)

		records := make([][]byte, n)
		for i := 0; i < n; i++ {
			records[i] = netflow.EncodeCGNATRecord(randomFlow())
		}
		h := netflow.Header{
			SysUptime:  uint32(time.Since(start).Milliseconds()) + 1_000_000,
			UnixSecs:   uint32(time.Now().Unix()),
			SequenceNo: seq,
			SourceID:   uint32(*sourceID),
		}

		var flowsets [][]byte
		if packetNo%*templateEvery == 0 {
			flowsets = append(flowsets, netflow.EncodeTemplateFlowSet(netflow.CGNATTemplate))
		}
		flowsets = append(flowsets, netflow.EncodeDataFlowSet(netflow.CGNATTemplateID, records))

		pkt := netflow.EncodePacket(h, uint16(n+1), flowsets...)
		if _, err := conn.Write(pkt); err != nil {
			fmt.Fprintf(os.Stderr, "netflowsim: write: %v\n", err)
			os.Exit(1)
		}
		seq++
		sent += n
		packetNo++
	}
	fmt.Printf("netflowsim: sent %d flows in %d packets over %s\n", sent, packetNo, time.Since(start).Round(time.Millisecond))
}

// (min and max are Go 1.21 builtins.)

// randomFlow builds one synthetic CGNAT-translated flow: a private 100.64/10
// source (RFC 6598 CGNAT space) translated to a public 203.0.113.0/24 address.
func randomFlow() netflow.Flow {
	now := uint32(1_000_000)
	protos := []uint8{6, 17} // TCP, UDP
	return netflow.Flow{
		Protocol:       protos[rand.Intn(len(protos))],
		SrcAddr:        net.IPv4(100, 64, byte(rand.Intn(64)), byte(1+rand.Intn(254))).To4(),
		SrcPort:        uint16(1024 + rand.Intn(64000)),
		DstAddr:        net.IPv4(142, 250, byte(rand.Intn(255)), byte(rand.Intn(255))).To4(),
		DstPort:        uint16([]int{80, 443, 53}[rand.Intn(3)]),
		PostNATSrcAddr: net.IPv4(203, 0, 113, byte(1+rand.Intn(254))).To4(),
		PostNATSrcPort: uint16(20000 + rand.Intn(40000)),
		Bytes:          uint64(500 + rand.Intn(2_000_000)),
		Packets:        uint64(1 + rand.Intn(2000)),
		FirstSwitched:  now,
		LastSwitched:   now + uint32(rand.Intn(5000)),
	}
}
