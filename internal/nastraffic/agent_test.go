package nastraffic

import (
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nastraffic/snmptest"
)

// startAgent runs an snmptest agent for one test.
func startAgent(t *testing.T, community string) *snmptest.Agent {
	t.Helper()
	a, err := snmptest.Listen("127.0.0.1:0", community)
	if err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}
