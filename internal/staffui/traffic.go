package staffui

import (
	"context"
	"errors"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/nastraffic"
	"github.com/rs/zerolog/log"
)

// NAS interface traffic — FR-NET-006.
//
// Under the Network section rather than a section of its own: it is the
// Network Map's detail view, for the same owner and NOC staff, and the map
// links each router to it.

// TrafficStore backs the traffic screens. Satisfied by *db.NASStore.
type TrafficStore interface {
	ListTrafficRouters(ctx context.Context) ([]nastraffic.RouterStatus, error)
	ListNASInterfaces(ctx context.Context, nasID int) ([]nastraffic.InterfaceStatus, error)
	TrafficSeries(ctx context.Context, interfaceID int, from, to time.Time, bucket time.Duration) ([]nastraffic.Point, error)
	NASInterface(ctx context.Context, interfaceID int) (nasID int, name string, err error)
	SetNASSNMP(ctx context.Context, nasID int, enabled bool, communityEncrypted *string, port int) error
}

type trafficListData struct {
	Routers []nastraffic.RouterStatus
	Now     time.Time
}

type trafficRouterData struct {
	Router     nastraffic.RouterStatus
	Interfaces []nastraffic.InterfaceStatus
	Selected   *nastraffic.InterfaceStatus
	Ranges     []nastraffic.Range
	Range      nastraffic.Range
	Chart      chartView
	Now        time.Time
	// CanEncrypt is false when no encryption key is configured, in which
	// case a community cannot be stored and the settings form says why.
	CanEncrypt bool
	// PollerNote says where polling runs, so an operator who enables a router
	// and sees nothing knows which service to look at.
	PollerNote string
}

// Traffic handles GET /staff/network/traffic — every router's polling state.
func (h *Handler) Traffic(w http.ResponseWriter, r *http.Request) {
	s, ok := h.trafficGate(w, r)
	if !ok {
		return
	}
	routers, err := h.traffic.ListTrafficRouters(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list traffic routers failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not read the routers.")
		return
	}
	d := h.page(s, "Interface traffic", "network")
	d.Data = trafficListData{Routers: routers, Now: time.Now()}
	h.render(w, "traffic", d)
}

// TrafficRouter handles GET /staff/network/traffic/{id}?range=24h&if=<id>.
func (h *Handler) TrafficRouter(w http.ResponseWriter, r *http.Request) {
	s, ok := h.trafficGate(w, r)
	if !ok {
		return
	}
	nasID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || nasID <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid router id.")
		return
	}
	routers, err := h.traffic.ListTrafficRouters(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list traffic routers failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not read the router.")
		return
	}
	var router *nastraffic.RouterStatus
	for i := range routers {
		if routers[i].NASID == nasID {
			router = &routers[i]
		}
	}
	if router == nil {
		h.renderError(w, r, s, http.StatusNotFound, "No router with that id.")
		return
	}
	ifaces, err := h.traffic.ListNASInterfaces(r.Context(), nasID)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list NAS interfaces failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not read the router's interfaces.")
		return
	}

	q := r.URL.Query()
	rng := nastraffic.LookupRange(q.Get("range"))
	d := h.page(s, "Traffic — "+router.IP, "network")
	data := trafficRouterData{
		Router: *router, Interfaces: ifaces, Ranges: nastraffic.Ranges, Range: rng,
		Now: time.Now(), CanEncrypt: h.secretEncryptor != nil,
		PollerNote: "Polling runs in the RADIUS service (radiusd) every 5 minutes; a newly enabled router shows its interfaces after the next poll.",
	}

	// The interface to graph: the one asked for, if it belongs to this
	// router; otherwise the busiest, which the list is ordered by. An id
	// from another router is ignored rather than graphed, so a hand-edited
	// URL cannot show one router's traffic under another's heading.
	want := atoiOrZero(q.Get("if"))
	for i := range ifaces {
		if ifaces[i].ID == want {
			data.Selected = &ifaces[i]
		}
	}
	if data.Selected == nil {
		for i := range ifaces {
			if ifaces[i].HasHC {
				data.Selected = &ifaces[i]
				break
			}
		}
	}
	if data.Selected != nil {
		to := time.Now().Truncate(rng.Bucket).Add(rng.Bucket)
		from := to.Add(-rng.Span)
		pts, err := h.traffic.TrafficSeries(r.Context(), data.Selected.ID, from, to, rng.Bucket)
		if err != nil {
			log.Error().Err(err).Msg("staffui: traffic series failed")
			d.Error = "Could not read this interface's traffic."
		}
		data.Chart = buildChart(pts, rng, from, time.Local)
	}

	if code := q.Get("m"); code == "saved" {
		d.Message = "Monitoring settings saved."
	}
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}
	d.Data = data
	h.render(w, "traffic_router", d)
}

// SaveTrafficSettingsForm handles POST /staff/network/traffic/{id}/snmp.
//
// The community is encrypted here, the same way the Routers screen encrypts
// a RADIUS secret, and left unchanged when the field is blank — it is never
// rendered back into the form, so a blank field has to mean "keep it".
func (h *Handler) SaveTrafficSettingsForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.trafficGate(w, r)
	if !ok {
		return
	}
	nasID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || nasID <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid router id.")
		return
	}
	back := "/staff/network/traffic/" + strconv.Itoa(nasID)
	fail := func(msg string) {
		http.Redirect(w, r, back+"?err="+neturl.QueryEscape(msg), http.StatusSeeOther)
	}

	enabled := r.PostFormValue("snmp_enabled") == "on"
	port := 161
	if raw := strings.TrimSpace(r.PostFormValue("snmp_port")); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			fail("The SNMP port must be a number from 1 to 65535.")
			return
		}
		port = p
	}

	var encrypted *string
	if community := strings.TrimSpace(r.PostFormValue("snmp_community")); community != "" {
		if h.secretEncryptor == nil {
			fail("No encryption key is configured on this deployment, so an SNMP community cannot be stored.")
			return
		}
		if len(community) > 255 {
			fail("That community string is longer than any router accepts.")
			return
		}
		ct, err := h.secretEncryptor.Encrypt(community)
		if err != nil {
			log.Error().Err(err).Msg("staffui: encrypt SNMP community failed")
			fail("The community could not be encrypted. Nothing was changed.")
			return
		}
		encrypted = &ct
	}

	if err := h.traffic.SetNASSNMP(r.Context(), nasID, enabled, encrypted, port); err != nil {
		if errors.Is(err, nastraffic.ErrSNMPNeedsCommunity) {
			fail("Enter the router's SNMP community to turn on traffic monitoring.")
			return
		}
		log.Error().Err(err).Int("nas_id", nasID).Msg("staffui: save SNMP settings failed")
		fail("The settings could not be saved. Nothing was changed.")
		return
	}
	http.Redirect(w, r, back+"?m=saved", http.StatusSeeOther)
}

func (h *Handler) trafficGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "network")
	if !ok {
		return s, false
	}
	if h.traffic == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Interface traffic is not configured on this deployment.")
		return s, false
	}
	return s, true
}
