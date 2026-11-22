package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/nastraffic"
)

// Interface traffic in the console — FR-NET-006.

type fakeTraffic struct {
	routers []nastraffic.RouterStatus
	ifaces  map[int][]nastraffic.InterfaceStatus
	points  []nastraffic.Point
	setErr  error

	seriesFor  int
	setCalls   int
	setEnabled bool
	setCt      *string
	setPort    int
}

func (f *fakeTraffic) ListTrafficRouters(context.Context) ([]nastraffic.RouterStatus, error) {
	return f.routers, nil
}
func (f *fakeTraffic) ListNASInterfaces(_ context.Context, id int) ([]nastraffic.InterfaceStatus, error) {
	return f.ifaces[id], nil
}
func (f *fakeTraffic) TrafficSeries(_ context.Context, id int, _, _ time.Time, _ time.Duration) ([]nastraffic.Point, error) {
	f.seriesFor = id
	return f.points, nil
}
func (f *fakeTraffic) NASInterface(context.Context, int) (int, string, error) { return 0, "", nil }
func (f *fakeTraffic) SetNASSNMP(_ context.Context, _ int, enabled bool, ct *string, port int) error {
	f.setCalls++
	f.setEnabled, f.setCt, f.setPort = enabled, ct, port
	return f.setErr
}

type fakeEncryptor struct{}

func (fakeEncryptor) Encrypt(p string) (string, error) { return "enc:" + p, nil }
func (fakeEncryptor) ActiveVersion() string            { return "v1" }

func i64(v int64) *int64 { return &v }

// serveTraffic is servePR for /staff/network/traffic/{id}/...: servePR takes
// the id from the third path segment, which here is the word "traffic". The
// real mux binds {id} from the route pattern; this does the same.
func serveTraffic(h *Handler, role, method, target string, form url.Values, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if method == "GET" {
		r = httptest.NewRequest("GET", target, nil)
	} else {
		r = httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) > 3 {
		r.SetPathValue("id", parts[3])
	}
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}
func bp(v bool) *bool { return &v }

func trafficFixture() *fakeTraffic {
	ok := time.Now().Add(-2 * time.Minute)
	return &fakeTraffic{
		routers: []nastraffic.RouterStatus{
			{NASID: 1, IP: "10.0.0.1", Vendor: "mikrotik", Description: "Core", Enabled: true,
				HasCommunity: true, Port: 161, LastPolledAt: &ok, LastOKAt: &ok, Interfaces: 2},
			{NASID: 2, IP: "10.0.0.2", Vendor: "cisco"},
		},
		ifaces: map[int][]nastraffic.InterfaceStatus{
			1: {
				{ID: 11, Name: "ether1", Alias: "Uplink to ISP", Index: 1, SpeedMbps: i64(1000), OperUp: bp(true),
					HasHC: true, LatestAt: &ok, LatestInBps: 412_000_000, LatestOutBps: 38_500_000},
				{ID: 12, Name: "ether2", Index: 2, SpeedMbps: i64(1000), OperUp: bp(false), HasHC: true},
			},
			2: {{ID: 21, Name: "fa0/0", Index: 1, HasHC: false}},
		},
	}
}

// Same reach as the Network Map it hangs off.
func TestFR_NET_006_TrafficIsReachedOnlyByNetworkRoles(t *testing.T) {
	h := &Handler{traffic: trafficFixture()}
	for _, role := range []string{"isp_owner", "noc_engineer"} {
		if w := servePR(h, role, "GET", "/staff/network/traffic", nil, h.Traffic); w.Code != http.StatusOK {
			t.Errorf("%s got %d", role, w.Code)
		}
	}
	for _, role := range []string{"billing_admin", "csr", "technician"} {
		if w := servePR(h, role, "GET", "/staff/network/traffic", nil, h.Traffic); w.Code != http.StatusForbidden {
			t.Errorf("%s got %d, want 403", role, w.Code)
		}
	}
}

func TestFR_NET_006_TheScreensRenderWithRealData(t *testing.T) {
	f := trafficFixture()
	base := time.Now().Truncate(5 * time.Minute).Add(-2 * time.Hour)
	for i := 0; i < 12; i++ {
		f.points = append(f.points, nastraffic.Point{At: base.Add(time.Duration(i) * 5 * time.Minute),
			InAvgBps: int64(100+i) * 1_000_000, OutAvgBps: 10_000_000, InPeakBps: 900_000_000, OutPeakBps: 20_000_000})
	}
	h := &Handler{traffic: f, secretEncryptor: fakeEncryptor{}}

	list := servePR(h, "noc_engineer", "GET", "/staff/network/traffic", nil, h.Traffic)
	if !strings.Contains(list.Body.String(), "/staff/network/traffic/1") ||
		!strings.Contains(list.Body.String(), "polling") {
		t.Errorf("the router list is missing its link or status")
	}

	w := serveTraffic(h, "noc_engineer", "GET", "/staff/network/traffic/1?range=24h", nil, h.TrafficRouter)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "<no value>") {
		t.Error("a template field name is wrong — the page rendered <no value>")
	}
	for _, want := range []string{
		`class="line in"`, `class="line out"`, "data-points=", "Uplink to ISP",
		"412 Mbit/s", "1 Gbit/s", "peak 900 Mbit/s", "Show as a table", `aria-current="page"`,
		`action="/staff/network/traffic/1/snmp"`, "Stored — leave blank to keep it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the router page is missing %q", want)
		}
	}
	// With no interface asked for, the busiest graphable one is shown.
	if f.seriesFor != 11 {
		t.Errorf("graphed interface %d, want the busiest (11)", f.seriesFor)
	}
	// The community is never rendered back.
	if strings.Contains(body, "enc:") {
		t.Error("a stored community reached the page")
	}
}

// An interface id from another router is ignored rather than graphed under
// this router's heading.
func TestFR_NET_006_AnotherRoutersInterfaceIsNotGraphedHere(t *testing.T) {
	f := trafficFixture()
	h := &Handler{traffic: f}
	serveTraffic(h, "isp_owner", "GET", "/staff/network/traffic/1?if=21", nil, h.TrafficRouter)
	if f.seriesFor == 21 {
		t.Error("router 2's interface was graphed on router 1's page")
	}
}

// An interface without 64-bit counters says so instead of offering a graph.
func TestFR_NET_006_AnInterfaceWithoutCountersIsNotOfferedAGraph(t *testing.T) {
	h := &Handler{traffic: trafficFixture()}
	w := serveTraffic(h, "isp_owner", "GET", "/staff/network/traffic/2", nil, h.TrafficRouter)
	body := w.Body.String()
	if !strings.Contains(body, "No 64-bit counters") || strings.Contains(body, `class="line in"`) {
		t.Error("an interface with no 64-bit counters was offered a graph")
	}
	if !strings.Contains(body, "Monitoring is off") {
		t.Error("a router with monitoring off does not say so")
	}
}

func TestFR_NET_006_TheSettingsForm(t *testing.T) {
	post := func(f *fakeTraffic, enc bool, form url.Values) (*fakeTraffic, string) {
		h := &Handler{traffic: f}
		if enc {
			h.secretEncryptor = fakeEncryptor{}
		}
		w := serveTraffic(h, "noc_engineer", "POST", "/staff/network/traffic/1/snmp", form, h.SaveTrafficSettingsForm)
		loc, _ := url.Parse(w.Header().Get("Location"))
		return f, loc.Query().Get("err")
	}

	t.Run("a community is encrypted before it is stored", func(t *testing.T) {
		f, problem := post(&fakeTraffic{}, true, url.Values{"snmp_enabled": {"on"}, "snmp_community": {"public"}, "snmp_port": {"1161"}})
		if problem != "" || f.setCt == nil || *f.setCt != "enc:public" || !f.setEnabled || f.setPort != 1161 {
			t.Errorf("stored %v %v %d, problem %q", f.setCt, f.setEnabled, f.setPort, problem)
		}
	})
	t.Run("a blank community keeps the stored one", func(t *testing.T) {
		f, _ := post(&fakeTraffic{}, true, url.Values{"snmp_enabled": {"on"}})
		if f.setCalls != 1 || f.setCt != nil {
			t.Errorf("a blank community was sent as %v", f.setCt)
		}
	})
	t.Run("enabling with no community anywhere says what to do", func(t *testing.T) {
		_, problem := post(&fakeTraffic{setErr: nastraffic.ErrSNMPNeedsCommunity}, true, url.Values{"snmp_enabled": {"on"}})
		if !strings.Contains(problem, "community") {
			t.Errorf("got %q", problem)
		}
	})
	t.Run("a bad port is refused before the store", func(t *testing.T) {
		f, problem := post(&fakeTraffic{}, true, url.Values{"snmp_port": {"99999"}})
		if f.setCalls != 0 || !strings.Contains(problem, "1 to 65535") {
			t.Errorf("calls %d, problem %q", f.setCalls, problem)
		}
	})
	t.Run("with no encryption key a community cannot be stored", func(t *testing.T) {
		f, problem := post(&fakeTraffic{}, false, url.Values{"snmp_community": {"public"}})
		if f.setCalls != 0 || !strings.Contains(problem, "encryption key") {
			t.Errorf("calls %d, problem %q", f.setCalls, problem)
		}
	})
	t.Run("a store failure is not reported as success", func(t *testing.T) {
		_, problem := post(&fakeTraffic{setErr: errors.New("boom")}, true, url.Values{})
		if problem == "" {
			t.Error("a failed save redirected without an error")
		}
	})
}

// ── The chart ────────────────────────────────────────────────────────────────

// A missing bucket breaks the line rather than bridging it: a straight line
// across an outage reads as steady traffic through it.
func TestFR_NET_006_AGapInTheDataBreaksTheLine(t *testing.T) {
	r := nastraffic.LookupRange("24h")
	from := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return from.Add(time.Duration(m) * time.Minute) }
	pts := []nastraffic.Point{
		{At: at(0), InAvgBps: 1e6}, {At: at(5), InAvgBps: 2e6},
		// 10:00–12:00 unmeasured
		{At: at(130), InAvgBps: 3e6}, {At: at(135), InAvgBps: 4e6},
	}
	v := buildChart(pts, r, from, time.UTC)
	if n := strings.Count(v.InPath, "M"); n != 2 {
		t.Errorf("the path has %d move-to segments, want 2 — the gap was bridged: %s", n, v.InPath)
	}
	if !strings.Contains(v.Coverage, "4 of 288") {
		t.Errorf("coverage reads %q", v.Coverage)
	}
}

func TestFR_NET_006_TheAxisReadsInRoundNumbers(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 1: 1, 7: 10, 412_000_000: 500_000_000, 2_400_000: 2_500_000, 1_000_000_000: 1_000_000_000, 1_300_000: 2_000_000} {
		if got := niceCeiling(in); got != want {
			t.Errorf("niceCeiling(%d) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int64]string{0: "0 bit/s", 950: "950 bit/s", 1_500: "1.5 kbit/s", 412_000_000: "412 Mbit/s",
		38_500_000: "38.5 Mbit/s", 1_000_000_000: "1 Gbit/s", 9_870_000_000: "9.87 Gbit/s"} {
		if got := FormatBps(in); got != want {
			t.Errorf("FormatBps(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFR_NET_006_AnEmptyPeriodStillDrawsItsAxes(t *testing.T) {
	r := nastraffic.LookupRange("7d")
	v := buildChart(nil, r, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), time.UTC)
	if !v.Empty || len(v.YTicks) != 1 || len(v.XTicks) < 6 || v.InPath != "" {
		t.Errorf("empty chart: empty=%v yticks=%d xticks=%d path=%q", v.Empty, len(v.YTicks), len(v.XTicks), v.InPath)
	}
}
