package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

// TR-069 control free of HTTP — FR-CPE-003 | MDS §4.19.
//
// Shared by the API routes in cpe_rpc.go and the console's CPE screen, so
// the validation, queue priority and audit line exist once. Every action
// queues rather than performs: see cpe_rpc.go on why.

// CPE actions an operator can queue.
const (
	CPEActionReboot      = "reboot"
	CPEActionFirmware    = "firmware"
	CPEActionParameters  = "parameters"
	CPEActionReprovision = "reprovision"
)

// Errors QueueCPEAction reports. ErrCPEInvalid wraps an operator-facing
// sentence.
var (
	ErrCPEUnavailable = errors.New("api: TR-069 ACS not configured")
	ErrCPENotFound    = errors.New("api: no device with that serial number")
	ErrCPEInvalid     = errors.New("api: invalid CPE action")
)

func cpeInvalid(msg string) error { return fmt.Errorf("%w: %s", ErrCPEInvalid, msg) }

// CPEInvalidReason returns the sentence inside an ErrCPEInvalid, or "".
func CPEInvalidReason(err error) string {
	if !errors.Is(err, ErrCPEInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrCPEInvalid.Error()+": ")
}

// CPEDevice loads a device's ACS view by serial.
func (h *Handler) CPEDevice(ctx context.Context, serial string) (*tr069.Device, error) {
	if h.cpeControl == nil {
		return nil, ErrCPEUnavailable
	}
	if serial == "" {
		return nil, cpeInvalid("serial is required")
	}
	device, err := h.cpeControl.GetDeviceBySerialACS(ctx, serial)
	if err != nil {
		return nil, fmt.Errorf("api: device lookup: %w", err)
	}
	if device == nil {
		return nil, ErrCPENotFound
	}
	return device, nil
}

// CPETasks lists a device's queued and past RPCs, optionally by status.
func (h *Handler) CPETasks(ctx context.Context, deviceID int, status *string) ([]tr069.Task, error) {
	if h.cpeControl == nil {
		return nil, ErrCPEUnavailable
	}
	return h.cpeControl.ListTasks(ctx, deviceID, status)
}

// QueueCPEAction queues one action for a device and writes the audit line.
// arg is the firmware URL for "firmware" and the parameter names for
// "parameters" (a slice, or one comma-separated string from a form).
// It returns the queued task, or nil for "reprovision", which flags the
// device instead of queueing an RPC.
func (h *Handler) QueueCPEAction(ctx context.Context, device *tr069.Device, action string, names []string, url string) (*tr069.Task, error) {
	if h.cpeControl == nil {
		return nil, ErrCPEUnavailable
	}
	switch action {
	case CPEActionReboot:
		// Priority 10: a reboot a technician is waiting on should overtake
		// routine provisioning already sitting in the queue.
		return h.enqueueCPE(ctx, device, tr069.RPCReboot, nil, 10, "cpe.reboot")

	case CPEActionFirmware:
		// A Download with no URL, or one the device cannot fetch, can leave
		// a router half-upgraded — so the obvious mistakes are refused here
		// rather than on the hardware.
		url = strings.TrimSpace(url)
		if url == "" {
			return nil, cpeInvalid("url is required")
		}
		if !hasHTTPScheme(url) {
			return nil, cpeInvalid("url must be an http:// or https:// address the device can reach")
		}
		return h.enqueueCPE(ctx, device, tr069.RPCDownload, map[string]string{"url": url}, 20, "cpe.firmware")

	case CPEActionParameters:
		var clean []string
		for _, n := range names {
			for _, part := range strings.Split(n, ",") {
				if p := strings.TrimSpace(part); p != "" {
					clean = append(clean, p)
				}
			}
		}
		if len(clean) == 0 {
			return nil, cpeInvalid("at least one parameter name is required")
		}
		params := make(map[string]string, len(clean))
		for i, n := range clean {
			params[strconv.Itoa(i)] = n
		}
		return h.enqueueCPE(ctx, device, tr069.RPCGetParameterValues, params, 50, "cpe.get_parameters")

	case CPEActionReprovision:
		// Marks the device as needing configuration rather than queueing the
		// RPC directly: the ACS rebuilds the parameter set from the
		// subscriber's current plan when the device next Informs, so a plan
		// change between now and then is reflected rather than baked in at
		// button-press time.
		if err := h.cpeControl.SetProvisioningState(ctx, device.ID, tr069.StateNeedsReprovision, ""); err != nil {
			return nil, fmt.Errorf("api: flag device for reprovisioning: %w", err)
		}
		middleware.Audit(ctx, "cpe.reprovision", device.SerialNumber, nil)
		return nil, nil

	default:
		return nil, cpeInvalid("unknown action " + action)
	}
}

func (h *Handler) enqueueCPE(ctx context.Context, device *tr069.Device, rpcType string,
	params map[string]string, priority int, auditAction string,
) (*tr069.Task, error) {
	task, err := h.cpeControl.EnqueueTask(ctx, device.ID, rpcType, params, priority,
		middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("api: queue %s: %w", rpcType, err)
	}
	middleware.Audit(ctx, auditAction, device.SerialNumber, map[string]any{
		"task_id": task.ID, "rpc_type": rpcType,
	})
	return task, nil
}
