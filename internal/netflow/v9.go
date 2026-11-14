// Package netflow decodes Cisco NetFlow v9 (RFC 3954) export packets into flow
// records, and encodes the same wire format so a mock exporter and the tests can
// produce real packets to feed back in.
//
// This is the collector half of Epic 5 (CGNAT/IPFIX auditing): a BNG or CGNAT
// gateway exports one flow per translated connection, and these records are what
// answers a later abuse complaint — "who was behind public IP:port at this time".
// NetFlow v9 is template-driven: the exporter first sends a Template FlowSet
// describing the field layout, then Data FlowSets packed to that layout, so a
// decoder has to remember templates across packets (see TemplateCache).
//
// Only the fixed-length field types a CGNAT/translation export actually carries
// are interpreted; any other field is skipped by its declared length, so an
// exporter that sends extra fields still decodes rather than failing.
package netflow

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// NetFlow v9 field type numbers (RFC 3954 §8 / IANA IPFIX registry, which v9
// shares). Only the ones this collector interprets are named.
const (
	fieldInBytes        = 1
	fieldInPkts         = 2
	fieldProtocol       = 4
	fieldL4SrcPort      = 7
	fieldIPv4SrcAddr    = 8
	fieldL4DstPort      = 11
	fieldIPv4DstAddr    = 12
	fieldLastSwitched   = 21
	fieldFirstSwitched  = 22
	fieldPostNATSrcIP   = 225 // CGNAT: public (translated) source address
	fieldPostNATSrcPort = 227 // CGNAT: public (translated) source port
)

const (
	version9    = 9
	headerLen   = 20  // NetFlow v9 packet header
	flowsetHdr  = 4   // FlowSetID(2) + Length(2)
	templateSet = 0   // FlowSet ID 0 is a Template FlowSet
	optionsSet  = 1   // FlowSet ID 1 is an Options Template FlowSet (skipped)
	minDataSet  = 256 // FlowSet IDs >= 256 are Data FlowSets
)

// Field is one entry in a template: a field type and its fixed byte length.
type Field struct {
	Type   uint16
	Length uint16
}

// Template is an exporter's declared layout for a set of data records.
type Template struct {
	ID     uint16
	Fields []Field
}

// recordSize is the byte length of one data record under this template.
func (t Template) recordSize() int {
	n := 0
	for _, f := range t.Fields {
		n += int(f.Length)
	}
	return n
}

// TemplateCache remembers the templates one exporter stream has sent. It must
// persist across packets: a data record is undecodable until its template has
// arrived, and exporters re-send templates only periodically. The collector
// keeps one cache per (exporter address, source id).
type TemplateCache map[uint16]Template

// NewTemplateCache returns an empty cache.
func NewTemplateCache() TemplateCache { return make(TemplateCache) }

// Header is the decoded NetFlow v9 packet header.
type Header struct {
	Count      uint16 // number of records (template + data + options) in the packet
	SysUptime  uint32 // ms since the exporter booted
	UnixSecs   uint32 // export time, seconds since epoch
	SequenceNo uint32
	SourceID   uint32 // the exporter's observation domain
}

// ExportTime is the wall-clock time the exporter stamped the packet with.
func (h Header) ExportTime() time.Time { return time.Unix(int64(h.UnixSecs), 0).UTC() }

// Flow is one decoded flow record. Fields left zero were not present in the
// template. Addresses are IPv4 (4-byte net.IP) to match the INET storage column.
type Flow struct {
	SourceID   uint32
	TemplateID uint16

	SrcAddr  net.IP
	DstAddr  net.IP
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8

	// CGNAT translation, when the exporter is a NAT gateway: the public address
	// and port the private source was translated to. Zero when the export is a
	// plain (non-NAT) flow.
	PostNATSrcAddr net.IP
	PostNATSrcPort uint16

	Bytes   uint64
	Packets uint64

	// FirstSwitched/LastSwitched are ms since the exporter booted. ExportTime
	// plus the uptime delta turns them into wall-clock times (see FlowStart).
	FirstSwitched uint32
	LastSwitched  uint32

	ExportTime time.Time
	SysUptime  uint32
}

// FlowStart converts the boot-relative FirstSwitched into wall-clock time using
// the packet's export time and uptime. A NetFlow timestamp is only meaningful
// relative to the same packet's header, which is why it is derived here rather
// than stored raw.
func (f Flow) FlowStart() time.Time {
	return f.ExportTime.Add(-time.Duration(f.SysUptime-f.FirstSwitched) * time.Millisecond)
}

// FlowEnd converts LastSwitched the same way.
func (f Flow) FlowEnd() time.Time {
	return f.ExportTime.Add(-time.Duration(f.SysUptime-f.LastSwitched) * time.Millisecond)
}

// DecodePacket decodes one NetFlow v9 UDP payload. Templates it carries are
// stored into cache (and used for data records later in the same packet);
// data records whose template is not yet known — in cache or earlier in this
// packet — are skipped rather than erroring, because a template-less data
// record is a normal transient, not corruption.
//
// cache must be the same instance across packets from one exporter/source, so
// a data packet that arrives after its template packet still decodes.
func DecodePacket(data []byte, cache TemplateCache) (Header, []Flow, error) {
	var h Header
	if len(data) < headerLen {
		return h, nil, fmt.Errorf("netflow: packet shorter than header (%d bytes)", len(data))
	}
	if v := binary.BigEndian.Uint16(data[0:2]); v != version9 {
		return h, nil, fmt.Errorf("netflow: unsupported version %d (want 9)", v)
	}
	h.Count = binary.BigEndian.Uint16(data[2:4])
	h.SysUptime = binary.BigEndian.Uint32(data[4:8])
	h.UnixSecs = binary.BigEndian.Uint32(data[8:12])
	h.SequenceNo = binary.BigEndian.Uint32(data[12:16])
	h.SourceID = binary.BigEndian.Uint32(data[16:20])

	var flows []Flow
	pos := headerLen
	for pos+flowsetHdr <= len(data) {
		setID := binary.BigEndian.Uint16(data[pos : pos+2])
		setLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if setLen < flowsetHdr || pos+setLen > len(data) {
			return h, flows, fmt.Errorf("netflow: flowset at offset %d has bad length %d", pos, setLen)
		}
		body := data[pos+flowsetHdr : pos+setLen]

		switch {
		case setID == templateSet:
			parseTemplates(body, cache)
		case setID == optionsSet:
			// Options templates describe exporter metadata, not flows. Skipped.
		case setID >= minDataSet:
			tmpl, ok := cache[setID]
			if ok {
				flows = append(flows, parseDataRecords(body, tmpl, h)...)
			}
			// Unknown template: skip this data flowset and wait for the template.
		}
		pos += setLen
	}
	return h, flows, nil
}

// parseTemplates reads one or more templates from a Template FlowSet body and
// stores them into cache, replacing any earlier template with the same id (an
// exporter may re-issue a template with a changed layout).
func parseTemplates(body []byte, cache TemplateCache) {
	for len(body) >= 4 {
		id := binary.BigEndian.Uint16(body[0:2])
		count := int(binary.BigEndian.Uint16(body[2:4]))
		body = body[4:]
		if len(body) < count*4 {
			return // truncated template; stop rather than read past the end
		}
		fields := make([]Field, count)
		for i := 0; i < count; i++ {
			fields[i] = Field{
				Type:   binary.BigEndian.Uint16(body[i*4 : i*4+2]),
				Length: binary.BigEndian.Uint16(body[i*4+2 : i*4+4]),
			}
		}
		cache[id] = Template{ID: id, Fields: fields}
		body = body[count*4:]
	}
}

// parseDataRecords unpacks every record in a Data FlowSet body under tmpl.
func parseDataRecords(body []byte, tmpl Template, h Header) []Flow {
	size := tmpl.recordSize()
	if size == 0 {
		return nil
	}
	n := len(body) / size // trailing bytes are padding to a 4-byte boundary
	flows := make([]Flow, 0, n)
	for i := 0; i < n; i++ {
		rec := body[i*size : (i+1)*size]
		f := Flow{
			SourceID:   h.SourceID,
			TemplateID: tmpl.ID,
			ExportTime: h.ExportTime(),
			SysUptime:  h.SysUptime,
		}
		off := 0
		for _, field := range tmpl.Fields {
			val := rec[off : off+int(field.Length)]
			applyField(&f, field.Type, val)
			off += int(field.Length)
		}
		flows = append(flows, f)
	}
	return flows
}

// applyField writes one field's bytes into the flow. Unknown field types are
// ignored, so an exporter that sends more than this collector interprets still
// decodes cleanly.
func applyField(f *Flow, typ uint16, val []byte) {
	switch typ {
	case fieldInBytes:
		f.Bytes = uintN(val)
	case fieldInPkts:
		f.Packets = uintN(val)
	case fieldProtocol:
		if len(val) >= 1 {
			f.Protocol = val[0]
		}
	case fieldL4SrcPort:
		f.SrcPort = uint16(uintN(val))
	case fieldL4DstPort:
		f.DstPort = uint16(uintN(val))
	case fieldIPv4SrcAddr:
		f.SrcAddr = ipv4(val)
	case fieldIPv4DstAddr:
		f.DstAddr = ipv4(val)
	case fieldPostNATSrcIP:
		f.PostNATSrcAddr = ipv4(val)
	case fieldPostNATSrcPort:
		f.PostNATSrcPort = uint16(uintN(val))
	case fieldFirstSwitched:
		f.FirstSwitched = uint32(uintN(val))
	case fieldLastSwitched:
		f.LastSwitched = uint32(uintN(val))
	}
}

// uintN reads a big-endian unsigned integer of 1..8 bytes. NetFlow counters are
// sent in the smallest width the exporter chose (often 4 or 8 bytes), so the
// width is taken from the template, not assumed.
func uintN(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// ipv4 copies a 4-byte address, returning nil for any other length.
func ipv4(b []byte) net.IP {
	if len(b) != 4 {
		return nil
	}
	ip := make(net.IP, 4)
	copy(ip, b)
	return ip
}
