package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

// NOC-facing TR-069 control — FR-CPE-003 | MDS §4.19.
//
// Every endpoint here *queues* an RPC rather than performing one. CWMP is
// CPE-initiated and, behind CGNAT, a Connection Request frequently cannot
// reach the device — so the honest contract is "this will run when the
// device next checks in", and the API says so by answering 202 with the
// queued task rather than 200 with a result.

// CPEControlQuerier is the persistence surface NOC control needs.
// Satisfied by *db.TR069Store.
type CPEControlQuerier interface {
	GetDeviceBySerialACS(ctx context.Context, serial string) (*tr069.Device, error)
	EnqueueTask(ctx context.Context, deviceID int, rpcType string, params map[string]string, priority int, createdBy string) (*tr069.Task, error)
	ListTasks(ctx context.Context, deviceID int, status *string) ([]tr069.Task, error)
	SetProvisioningState(ctx context.Context, deviceID int, state, lastFault string) error
}

// resolveDevice looks a device up by serial, writing the error response
// itself and returning nil when it cannot be found.
func (h *Handler) resolveDevice(w http.ResponseWriter, r *http.Request) *tr069.Device {
	device, err := h.CPEDevice(r.Context(), r.PathValue("serial"))
	switch {
	case err == nil:
		return device
	case CPEInvalidReason(err) != "":
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", CPEInvalidReason(err))
	case errors.Is(err, ErrCPEUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "TR-069 ACS not configured")
	case errors.Is(err, ErrCPENotFound):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no device with that serial number")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "device lookup failed")
	}
	return nil
}

// queueAction is the shared tail of every control endpoint. 202, not 200:
// nothing has happened to the device yet, and saying otherwise would have a
// technician standing next to a router that has not rebooted wondering why.
func (h *Handler) queueAction(w http.ResponseWriter, r *http.Request, device *tr069.Device, action string, names []string, url string) {
	task, err := h.QueueCPEAction(r.Context(), device, action, names, url)
	if reason := CPEInvalidReason(err); reason != "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", reason)
		return
	}
	if err != nil {
		msg := "could not queue the RPC"
		if action == CPEActionReprovision {
			msg = "could not flag the device for reprovisioning"
		}
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", msg)
		return
	}
	if action == CPEActionReprovision {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"serial_number":  device.SerialNumber,
			"state":          tr069.StateNeedsReprovision,
			"delivered_when": "the device opens its next CWMP session",
			"last_inform_at": device.LastInformAt,
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task":           task,
		"delivery":       "queued",
		"delivered_when": "the device opens its next CWMP session (periodic Inform, or sooner on reboot)",
		"last_inform_at": device.LastInformAt,
	})
}

// RebootCPE handles POST /api/v1/cpe/devices/{serial}/reboot.
func (h *Handler) RebootCPE(w http.ResponseWriter, r *http.Request) {
	if device := h.resolveDevice(w, r); device != nil {
		h.queueAction(w, r, device, CPEActionReboot, nil, "")
	}
}

type firmwareRequest struct {
	URL string `json:"url"`
}

// UpgradeCPEFirmware handles POST /api/v1/cpe/devices/{serial}/firmware.
func (h *Handler) UpgradeCPEFirmware(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}
	var req firmwareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	h.queueAction(w, r, device, CPEActionFirmware, nil, req.URL)
}

type getParamsRequest struct {
	Names []string `json:"names"`
}

// GetCPEParameters handles POST /api/v1/cpe/devices/{serial}/parameters —
// queueing a diagnostic read.
func (h *Handler) GetCPEParameters(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}
	var req getParamsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	h.queueAction(w, r, device, CPEActionParameters, req.Names, "")
}

// ReprovisionCPE handles POST /api/v1/cpe/devices/{serial}/reprovision. See
// QueueCPEAction on why this flags the device rather than queueing an RPC.
func (h *Handler) ReprovisionCPE(w http.ResponseWriter, r *http.Request) {
	if device := h.resolveDevice(w, r); device != nil {
		h.queueAction(w, r, device, CPEActionReprovision, nil, "")
	}
}

// ListCPETasks handles GET /api/v1/cpe/devices/{serial}/tasks?status=.
func (h *Handler) ListCPETasks(w http.ResponseWriter, r *http.Request) {
	device := h.resolveDevice(w, r)
	if device == nil {
		return
	}

	var status *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}

	tasks, err := h.cpeControl.ListTasks(r.Context(), device.ID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list CPE tasks failed")
		return
	}
	if tasks == nil {
		tasks = []tr069.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": device, "tasks": tasks})
}

// hasHTTPScheme reports whether a URL starts with an http(s) scheme.
func hasHTTPScheme(u string) bool {
	return len(u) > 7 && (u[:7] == "http://" || (len(u) > 8 && u[:8] == "https://"))
}
