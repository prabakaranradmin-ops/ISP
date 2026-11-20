package nas

import (
	"fmt"

	"layeh.com/radius"
)

// MikroTik-Rate-Limit: vendor 14988, attribute 8. This is the reference
// implementation the other builders follow the same interface as — it is
// the exact logic that used to live inline in internal/radius/handlers.go
// and internal/fup/coa_task.go, moved here unchanged.
const (
	mikrotikVendorID      = 14988
	mikrotikRateLimitType = 8
)

type mikrotikBuilder struct{}

func (mikrotikBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	rl, err := p.mikrotikRateLimit()
	if err != nil {
		return nil, fmt.Errorf("nas: mikrotik: %w", err)
	}
	return mikrotikRateLimitAttrs(rl)
}

func (mikrotikBuilder) BuildCoA(p RateProfile) ([]Attr, error) {
	rl, err := p.mikrotikRateLimit()
	if err != nil {
		return nil, fmt.Errorf("nas: mikrotik: %w", err)
	}
	return mikrotikRateLimitAttrs(rl)
}

func mikrotikRateLimitAttrs(rateLimit string) ([]Attr, error) {
	if rateLimit == "" {
		return nil, fmt.Errorf("nas: mikrotik: empty rate limit string")
	}

	rlBytes := []byte(rateLimit)
	vsaData := make([]byte, 2+len(rlBytes))
	vsaData[0] = mikrotikRateLimitType
	if 2+len(rlBytes) <= 255 { // RADIUS attribute value max 253 bytes
		vsaData[1] = byte(2 + len(rlBytes)) //nolint:gosec
	} else {
		vsaData[1] = 255
	}
	copy(vsaData[2:], rlBytes)

	vsAttr, err := radius.NewVendorSpecific(mikrotikVendorID, radius.Attribute(vsaData))
	if err != nil {
		return nil, fmt.Errorf("nas: mikrotik: build VSA: %w", err)
	}
	return []Attr{{Type: 26, Value: vsAttr}}, nil // 26 = Vendor-Specific
}
