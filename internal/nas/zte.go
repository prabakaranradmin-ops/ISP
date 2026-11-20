package nas

import "fmt"

// ZTE vendor ID (3902, IANA-registered) is certain. As with Huawei, the
// sub-attribute type numbers are model-dependent (BRAS vs OLT product
// lines use different dictionaries) and are placeholders pending
// confirmation against the deployed hardware — MDS §4.11.
const (
	zteVendorID          = 3902
	zteInputAvgRateType  = 201 // TODO-VERIFY against deployed firmware
	zteOutputAvgRateType = 202 // TODO-VERIFY against deployed firmware
)

type zteBuilder struct{}

// plainRateLimit for the same reason as Huawei above: these are integer
// VSAs and the burst form does not parse.
func (zteBuilder) BuildAccept(p RateProfile) ([]Attr, error) { return zteRateAttrs(p.plainRateLimit()) }
func (zteBuilder) BuildCoA(p RateProfile) ([]Attr, error)    { return zteRateAttrs(p.plainRateLimit()) }

func zteRateAttrs(rateLimit string) ([]Attr, error) {
	in, out, err := parseRateLimitString(rateLimit)
	if err != nil {
		return nil, fmt.Errorf("nas: zte: %w", err)
	}
	inAttr, err := buildVendorSubAttr(zteVendorID, zteInputAvgRateType, uint32BE(in))
	if err != nil {
		return nil, fmt.Errorf("nas: zte: %w", err)
	}
	outAttr, err := buildVendorSubAttr(zteVendorID, zteOutputAvgRateType, uint32BE(out))
	if err != nil {
		return nil, fmt.Errorf("nas: zte: %w", err)
	}
	return []Attr{inAttr, outAttr}, nil
}
