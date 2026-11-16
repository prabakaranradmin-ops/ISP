package staffui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
	"github.com/rs/zerolog/log"
)

// CPEController is TR-069 control as the API runs it (FR-CPE-003).
// Satisfied by *api.Handler, so a console action queues the same RPC at the
// same priority with the same audit line as the API route.
type CPEController interface {
	CPEDevice(ctx context.Context, serial string) (*tr069.Device, error)
	CPETasks(ctx context.Context, deviceID int, status *string) ([]tr069.Task, error)
	QueueCPEAction(ctx context.Context, device *tr069.Device, action string, names []string, url string) (*tr069.Task, error)
}

// canActOnCPE mirrors the API's nocOrTech gate on the control routes.
func canActOnCPE(role string) bool {
	return role == "noc_engineer" || role == "technician" || role == "isp_owner"
}

type cpeData struct {
	// List view.
	Show    string
	Query   string
	Devices []CPEDeviceRow

	// Device view.
	Device   *tr069.Device
	Username string
	Silent   bool
	Tasks    []tr069.Task
	CanAct   bool
}

// CPEList lists managed CPE.
func (h *Handler) CPEList(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "cpe")
	if !ok {
		return
	}
	d := h.page(s, "CPE devices", "cpe")
	if h.console == nil {
		d.Error = "CPE management is not configured on this deployment."
		h.render(w, "cpe", d)
		return
	}
	q := r.URL.Query()
	cd := cpeData{Show: q.Get("show"), Query: strings.TrimSpace(q.Get("q"))}
	if cd.Show == "" {
		cd.Show = "managed"
	}
	if !inStrings(CPEViews, cd.Show) {
		d.Error = "Unknown list " + cd.Show + "; showing managed devices."
		cd.Show = "managed"
	}
	devices, err := h.console.ListACSDevices(r.Context(), cd.Show, cd.Query, 500)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list CPE failed")
		d.Error = "Could not load devices."
	}
	cd.Devices = devices
	d.Data = cd
	h.render(w, "cpe", d)
}

// CPEDetail shows one device, its queued and past actions, and the action
// forms for roles that may use them.
func (h *Handler) CPEDetail(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "cpe")
	if !ok {
		return
	}
	msg := ""
	if r.URL.Query().Get("queued") != "" {
		msg = "Queued. It runs the next time the device checks in."
	}
	h.renderCPEDetail(w, r, s, msg, "")
}

func (h *Handler) renderCPEDetail(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	if h.cpe == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "CPE management is not configured on this deployment.")
		return
	}
	device, err := h.cpe.CPEDevice(r.Context(), r.PathValue("serial"))
	switch {
	case errors.Is(err, api.ErrCPENotFound), api.CPEInvalidReason(err) != "":
		h.renderError(w, r, s, http.StatusNotFound, "No device with that serial number.")
		return
	case errors.Is(err, api.ErrCPEUnavailable):
		h.renderError(w, r, s, http.StatusServiceUnavailable, "The TR-069 ACS is not configured on this deployment.")
		return
	case err != nil:
		log.Error().Err(err).Msg("staffui: CPE lookup failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not load that device.")
		return
	}
	d := h.page(s, "CPE "+device.SerialNumber, "cpe")
	d.Message, d.Error = message, errMsg
	cd := cpeData{
		Device: device,
		CanAct: canActOnCPE(s.Role),
		Silent: device.LastInformAt != nil && time.Since(*device.LastInformAt) > CPESilentAfter,
	}
	if device.SubscriberID != nil && h.subscribers != nil {
		if sub, err := h.subscribers.GetSubscriberByID(r.Context(), *device.SubscriberID); err == nil && sub != nil {
			cd.Username = sub.Username
		}
	}
	if tasks, err := h.cpe.CPETasks(r.Context(), device.ID, nil); err == nil {
		cd.Tasks = tasks
	} else {
		log.Error().Err(err).Msg("staffui: CPE tasks failed")
	}
	d.Data = cd
	h.render(w, "cpe", d)
}

// CPEActionForm handles POST /staff/cpe/{serial}/action.
func (h *Handler) CPEActionForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "cpe")
	if !ok {
		return
	}
	if !canActOnCPE(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Your role cannot queue actions on customer equipment.")
		return
	}
	if h.cpe == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "CPE management is not configured on this deployment.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	device, err := h.cpe.CPEDevice(ctx, r.PathValue("serial"))
	if err != nil {
		h.renderError(w, r, s, http.StatusNotFound, "No device with that serial number.")
		return
	}
	action, arg := r.PostFormValue("action"), r.PostFormValue("arg")
	var names []string
	if action == api.CPEActionParameters {
		names = []string{arg}
	}
	if _, err := h.cpe.QueueCPEAction(ctx, device, action, names, arg); err != nil {
		if reason := api.CPEInvalidReason(err); reason != "" {
			h.renderCPEDetail(w, r, s, "", "Not queued: "+reason+".")
			return
		}
		log.Error().Err(err).Str("serial", device.SerialNumber).Str("action", action).Msg("staffui: CPE action failed")
		h.renderCPEDetail(w, r, s, "", "Could not queue that action. Try again in a moment.")
		return
	}
	http.Redirect(w, r, "/staff/cpe/"+urlPathSegment(device.SerialNumber)+"?queued=1", http.StatusSeeOther) //nolint:gosec // fixed prefix, escaped segment
}

// urlPathSegment escapes a serial for use as one path segment, so a serial
// containing / or ? cannot change where the redirect goes.
func urlPathSegment(s string) string {
	return strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23", " ", "%20").Replace(s)
}
