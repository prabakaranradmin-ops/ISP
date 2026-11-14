package netflow

import (
	"context"
	"net"
	"testing"
	"time"
)

func sampleFlow() Flow {
	return Flow{
		Protocol:       6, // TCP
		SrcAddr:        net.IPv4(100, 64, 1, 20).To4(),
		SrcPort:        51000,
		DstAddr:        net.IPv4(142, 250, 183, 14).To4(),
		DstPort:        443,
		PostNATSrcAddr: net.IPv4(203, 0, 113, 10).To4(),
		PostNATSrcPort: 41234,
		Bytes:          8421,
		Packets:        37,
		FirstSwitched:  1_000_000,
		LastSwitched:   1_002_500,
	}
}

func sampleHeader() Header {
	return Header{SysUptime: 1_010_000, UnixSecs: 1_700_000_000, SequenceNo: 1, SourceID: 42}
}

// TestEncodeDecodeRoundTrip: a packet carrying a template then two data records
// decodes back to the same field values. This is the core guarantee — the
// decoder reads exactly what a real exporter would send.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	h := sampleHeader()
	f1 := sampleFlow()
	f2 := sampleFlow()
	f2.SrcAddr = net.IPv4(100, 64, 2, 30).To4()
	f2.PostNATSrcPort = 41299
	f2.Protocol = 17 // UDP

	pkt := EncodePacket(h, 3,
		EncodeTemplateFlowSet(CGNATTemplate),
		EncodeDataFlowSet(CGNATTemplateID, [][]byte{EncodeCGNATRecord(f1), EncodeCGNATRecord(f2)}),
	)

	gotH, flows, err := DecodePacket(pkt, NewTemplateCache())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotH.SourceID != h.SourceID {
		t.Errorf("source id = %d, want %d", gotH.SourceID, h.SourceID)
	}
	if len(flows) != 2 {
		t.Fatalf("decoded %d flows, want 2", len(flows))
	}

	g := flows[0]
	if g.Protocol != 6 || g.SrcPort != 51000 || g.DstPort != 443 {
		t.Errorf("tuple wrong: proto=%d src=%d dst=%d", g.Protocol, g.SrcPort, g.DstPort)
	}
	if !g.SrcAddr.Equal(f1.SrcAddr) || !g.DstAddr.Equal(f1.DstAddr) {
		t.Errorf("addrs wrong: src=%v dst=%v", g.SrcAddr, g.DstAddr)
	}
	if !g.PostNATSrcAddr.Equal(f1.PostNATSrcAddr) || g.PostNATSrcPort != f1.PostNATSrcPort {
		t.Errorf("post-NAT wrong: %v:%d", g.PostNATSrcAddr, g.PostNATSrcPort)
	}
	if g.Bytes != 8421 || g.Packets != 37 {
		t.Errorf("counters wrong: bytes=%d pkts=%d", g.Bytes, g.Packets)
	}
	if flows[1].Protocol != 17 || flows[1].PostNATSrcPort != 41299 {
		t.Errorf("second flow wrong: proto=%d natport=%d", flows[1].Protocol, flows[1].PostNATSrcPort)
	}
}

// TestTemplatePersistsAcrossPackets: a data packet that arrives after its
// template, in a separate packet, still decodes — the cache must carry the
// template forward. A collector depends on this because exporters send
// templates only periodically.
func TestTemplatePersistsAcrossPackets(t *testing.T) {
	cache := NewTemplateCache()
	h := sampleHeader()

	// Packet 1: template only.
	_, flows, err := DecodePacket(EncodePacket(h, 1, EncodeTemplateFlowSet(CGNATTemplate)), cache)
	if err != nil {
		t.Fatalf("template packet: %v", err)
	}
	if len(flows) != 0 {
		t.Fatalf("template-only packet produced %d flows, want 0", len(flows))
	}

	// Packet 2: data only, no template inline.
	_, flows, err = DecodePacket(
		EncodePacket(h, 1, EncodeDataFlowSet(CGNATTemplateID, [][]byte{EncodeCGNATRecord(sampleFlow())})), cache)
	if err != nil {
		t.Fatalf("data packet: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("data packet decoded %d flows, want 1 (template not carried forward)", len(flows))
	}
}

// TestDataWithoutTemplateIsSkipped: a data record whose template was never seen
// is dropped, not an error — a normal transient when a collector starts
// mid-stream before the next template refresh.
func TestDataWithoutTemplateIsSkipped(t *testing.T) {
	_, flows, err := DecodePacket(
		EncodePacket(sampleHeader(), 1, EncodeDataFlowSet(CGNATTemplateID, [][]byte{EncodeCGNATRecord(sampleFlow())})),
		NewTemplateCache())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(flows) != 0 {
		t.Errorf("want 0 flows with no known template, got %d", len(flows))
	}
}

// TestFlowStartEnd: boot-relative timestamps become wall-clock using the
// packet header, so an abuse query's "at this time" actually lines up.
func TestFlowStartEnd(t *testing.T) {
	_, flows, _ := DecodePacket(
		EncodePacket(sampleHeader(), 2,
			EncodeTemplateFlowSet(CGNATTemplate),
			EncodeDataFlowSet(CGNATTemplateID, [][]byte{EncodeCGNATRecord(sampleFlow())})),
		NewTemplateCache())
	// export at 1_700_000_000, uptime 1_010_000ms, first switched 1_000_000ms
	// => start = export - (1_010_000-1_000_000)ms = export - 10s
	wantStart := time.Unix(1_700_000_000, 0).UTC().Add(-10 * time.Second)
	if got := flows[0].FlowStart(); !got.Equal(wantStart) {
		t.Errorf("FlowStart = %v, want %v", got, wantStart)
	}
}

type memSink struct{ flows []Flow }

func (m *memSink) RecordFlows(_ context.Context, flows []Flow) error {
	m.flows = append(m.flows, flows...)
	return nil
}

// TestCollectorHandle: the full decode→sink path, exercised without a socket.
func TestCollectorHandle(t *testing.T) {
	sink := &memSink{}
	c := NewCollector(sink)
	h := sampleHeader()

	// Template packet first, then a data packet from the same exporter/source.
	if _, err := c.Handle(context.Background(), "203.0.113.1:9995",
		EncodePacket(h, 1, EncodeTemplateFlowSet(CGNATTemplate))); err != nil {
		t.Fatal(err)
	}
	n, err := c.Handle(context.Background(), "203.0.113.1:9995",
		EncodePacket(h, 1, EncodeDataFlowSet(CGNATTemplateID, [][]byte{EncodeCGNATRecord(sampleFlow())})))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(sink.flows) != 1 {
		t.Fatalf("recorded %d flows (handle returned %d), want 1", len(sink.flows), n)
	}
}
