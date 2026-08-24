package staffui

import (
	"context"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/rs/zerolog/log"
)

// NetworkHealthStore backs the Network Map screen (CRD-EXP-008) — a
// cross-device, live-session view of the registered NAS estate. Not a
// hand-drawn topology tool: every device already exists (Routers screen)
// and every session already exists (the health panel); this is the one
// place that shows them together instead of one device or one subscriber
// at a time.
//
// Redefined per package rather than reusing NASStore's own shape, matching
// every other store interface in this file. Satisfied by *db.NASStore, the
// same instance already passed as HandlerDeps.NAS.
type NetworkHealthStore interface {
	GetNetworkHealth(ctx context.Context) ([]nas.NetworkHealthRow, error)
}

type networkData struct {
	Devices     []nas.NetworkHealthRow
	TotalOnline int
}

// NetworkMap shows every registered NAS device with its current active-
// session count. Same reach as Routers: owner and NOC engineer, the two
// roles who care about the network estate as a whole rather than one
// subscriber's connection.
func (h *Handler) NetworkMap(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "network")
	if !ok {
		return
	}
	d := h.page(s, "Network Map", "network")

	if h.networkHealth == nil {
		d.Error = "Network map is not configured on this deployment."
		h.render(w, "network", d)
		return
	}

	devices, err := h.networkHealth.GetNetworkHealth(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: network health failed")
		d.Error = "Could not load network status."
		h.render(w, "network", d)
		return
	}

	total := 0
	for _, dev := range devices {
		total += dev.ActiveSessions
	}

	d.Data = networkData{Devices: devices, TotalOnline: total}
	h.render(w, "network", d)
}
