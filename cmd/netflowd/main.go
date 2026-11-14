// Command netflowd runs the NetFlow v9 collector as a standalone process,
// decoding incoming export packets and reporting throughput. It uses a counting
// sink so the whole wire path (exporter → UDP → decode → sink) can be exercised
// end to end with cmd/netflowsim and no database.
//
// The next slice replaces the counting sink with a database FlowSink that writes
// to flow_records (migration 052) and wires this into the service stack; the
// decode and collector code it runs is already the production code.
//
// Usage:
//
//	netflowd -listen :9995
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"flag"

	"github.com/maaransoft/isp-bss-oss/internal/netflow"
)

// countingSink tallies flows and remembers the most recent one, so a run can be
// summarized without a database.
type countingSink struct {
	flows atomic.Int64
	last  atomic.Pointer[netflow.Flow]
}

func (s *countingSink) RecordFlows(_ context.Context, flows []netflow.Flow) error {
	s.flows.Add(int64(len(flows)))
	if len(flows) > 0 {
		f := flows[len(flows)-1]
		s.last.Store(&f)
	}
	return nil
}

func main() {
	listen := flag.String("listen", ":9995", "UDP address to receive NetFlow v9 on")
	flag.Parse()

	sink := &countingSink{}
	collector := netflow.NewCollector(sink)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Periodic throughput line, so a running collector shows it is receiving.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var prev int64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := sink.flows.Load()
				if n == prev {
					continue
				}
				line := fmt.Sprintf("netflowd: %d flows decoded", n)
				if f := sink.last.Load(); f != nil {
					line += fmt.Sprintf(" (last: %s:%d via %s:%d proto %d, %d bytes)",
						f.SrcAddr, f.SrcPort, f.PostNATSrcAddr, f.PostNATSrcPort, f.Protocol, f.Bytes)
				}
				fmt.Println(line)
				prev = n
			}
		}
	}()

	fmt.Printf("netflowd: listening on %s (Ctrl+C to stop)\n", *listen)
	if err := collector.ListenAndServe(ctx, *listen); err != nil {
		fmt.Fprintf(os.Stderr, "netflowd: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("netflowd: stopped after %d flows\n", sink.flows.Load())
}
