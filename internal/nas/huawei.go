package nas

import "fmt"

// Huawei vendor ID (2011) is IANA-registered and certain. The sub-attribute
// type numbers below are NOT: Huawei's RADIUS dictionary differs across
// BRAS product lines and firmware versions. The TLV structure (vendor 2011,
// sub-attribute, 4-byte big-endian bps value) is standard Huawei VSA
// encoding and is what this builder gets right; confirm the exact sub-type
// numbers against the deployed hardware's RADIUS dictionary before
// production use (MDS §4.11 — flagged there for the same reason).
const (
	huaweiVendorID          = 2011
	huaweiInputAvgRateType  = 24 // TODO-VERIFY against deployed firmware
	huaweiOutputAvgRateType = 25 // TODO-VERIFY against deployed firmware
)

type huaweiBuilder struct{}

// Note plainRateLimit, not the burst form: huaweiRateAttrs parses the
// string into two integer VSAs, and the positional burst form does not
// parse. Burst is a MikroTik queue behaviour with no Huawei equivalent
// expressible over RADIUS.
func (huaweiBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	return huaweiRateAttrs(p.plainRateLimit())
}
func (huaweiBuilder) BuildCoA(p RateProfile) ([]Attr, error) {
	return huaweiRateAttrs(p.plainRateLimit())
}

func huaweiRateAttrs(rateLimit string) ([]Attr, error) {
	in, out, err := parseRateLimitString(rateLimit)
	if err != nil {
		return nil, fmt.Errorf("nas: huawei: %w", err)
	}
	inAttr, err := buildVendorSubAttr(huaweiVendorID, huaweiInputAvgRateType, uint32BE(in))
	if err != nil {
		return nil, fmt.Errorf("nas: huawei: %w", err)
	}
	outAttr, err := buildVendorSubAttr(huaweiVendorID, huaweiOutputAvgRateType, uint32BE(out))
	if err != nil {
		return nil, fmt.Errorf("nas: huawei: %w", err)
	}
	return []Attr{inAttr, outAttr}, nil
}
