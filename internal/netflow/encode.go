package netflow

import (
	"encoding/binary"
	"net"
)

// Encoding of NetFlow v9 packets. Used by the mock exporter (cmd/netflowsim)
// and the tests to produce real wire-format packets, so the decoder is always
// exercised against bytes an actual exporter could send rather than a stub.

// CGNATTemplateID is the template id the mock CGNAT exporter uses. Any id >= 256
// is a valid data-template id in NetFlow v9.
const CGNATTemplateID = 256

// CGNATTemplate is the field layout a CGNAT/translation export carries: the
// private 5-tuple, the public (post-NAT) source address and port, byte/packet
// counters, and the flow's first/last uptime stamps.
var CGNATTemplate = Template{
	ID: CGNATTemplateID,
	Fields: []Field{
		{fieldProtocol, 1},
		{fieldIPv4SrcAddr, 4},
		{fieldL4SrcPort, 2},
		{fieldIPv4DstAddr, 4},
		{fieldL4DstPort, 2},
		{fieldPostNATSrcIP, 4},
		{fieldPostNATSrcPort, 2},
		{fieldInBytes, 4},
		{fieldInPkts, 4},
		{fieldFirstSwitched, 4},
		{fieldLastSwitched, 4},
	},
}

// EncodeCGNATRecord packs one flow under CGNATTemplate, field order matching it
// exactly.
func EncodeCGNATRecord(f Flow) []byte {
	b := make([]byte, 0, CGNATTemplate.recordSize())
	b = append(b, f.Protocol)
	b = append(b, to4(f.SrcAddr)...)
	b = appendU16(b, f.SrcPort)
	b = append(b, to4(f.DstAddr)...)
	b = appendU16(b, f.DstPort)
	b = append(b, to4(f.PostNATSrcAddr)...)
	b = appendU16(b, f.PostNATSrcPort)
	b = appendU32(b, uint32(f.Bytes))
	b = appendU32(b, uint32(f.Packets))
	b = appendU32(b, f.FirstSwitched)
	b = appendU32(b, f.LastSwitched)
	return b
}

// EncodeTemplateFlowSet builds a Template FlowSet (id 0) carrying one template.
func EncodeTemplateFlowSet(t Template) []byte {
	body := make([]byte, 0, 4+len(t.Fields)*4)
	body = appendU16(body, t.ID)
	body = appendU16(body, uint16(len(t.Fields)))
	for _, f := range t.Fields {
		body = appendU16(body, f.Type)
		body = appendU16(body, f.Length)
	}
	return wrapFlowSet(templateSet, body)
}

// EncodeDataFlowSet builds a Data FlowSet (id = templateID) from pre-encoded
// records, padded to a 4-byte boundary as RFC 3954 requires.
func EncodeDataFlowSet(templateID uint16, records [][]byte) []byte {
	var body []byte
	for _, r := range records {
		body = append(body, r...)
	}
	return wrapFlowSet(templateID, body)
}

// wrapFlowSet prepends the FlowSetID + Length header and pads the whole flowset
// to a 4-byte boundary.
func wrapFlowSet(id uint16, body []byte) []byte {
	total := flowsetHdr + len(body)
	pad := (4 - total%4) % 4
	out := make([]byte, 0, total+pad)
	out = appendU16(out, id)
	out = appendU16(out, uint16(total+pad))
	out = append(out, body...)
	out = append(out, make([]byte, pad)...)
	return out
}

// EncodePacket assembles a full NetFlow v9 packet: the header followed by the
// given flowsets. count is the record count the exporter claims (templates plus
// data records); it is advisory and this decoder does not rely on it.
func EncodePacket(h Header, count uint16, flowsets ...[]byte) []byte {
	out := make([]byte, 0, headerLen)
	out = appendU16(out, version9)
	out = appendU16(out, count)
	out = appendU32(out, h.SysUptime)
	out = appendU32(out, h.UnixSecs)
	out = appendU32(out, h.SequenceNo)
	out = appendU32(out, h.SourceID)
	for _, fs := range flowsets {
		out = append(out, fs...)
	}
	return out
}

func to4(ip net.IP) []byte {
	if v := ip.To4(); v != nil {
		return v
	}
	return make([]byte, 4)
}

func appendU16(b []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(b, v) }
func appendU32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }
