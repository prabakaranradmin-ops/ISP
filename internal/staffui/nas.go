package staffui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/rs/zerolog/log"
)

// NASStore registers and lists RADIUS NAS devices (MikroTik, Cisco, Huawei,
// ZTE, Juniper, wireless controllers) — the console's counterpart to
// internal/api/nas.go, which had a complete secure API and no UI: a client
// demo could not show "add your router" without curl.
//
// Satisfied by *db.NASStore.
type NASStore interface {
	ListNASDeviceSummaries(ctx context.Context) ([]nas.DeviceSummary, error)
	CreateNASDevice(ctx context.Context, d nas.NewNASDevice) (*nas.DeviceSummary, error)
	UpdateNASDevice(ctx context.Context, id int, u nas.NASDeviceUpdate) (*nas.DeviceSummary, error)
}

// defaultControlPort mirrors internal/api/nas.go's constant of the same name
// and purpose — MikroTik's CoA/PoD listener; RFC 5176 specifies 3799, but
// this is the value most deployments here actually need.
const defaultControlPort = 1700

type nasData struct {
	Devices []nas.DeviceSummary
	Vendors []string
}

// NAS lists registered devices and hosts the add-device form.
func (h *Handler) NAS(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "nas")
	if !ok {
		return
	}
	h.renderNAS(w, r, s, "", "")
}

// renderNAS is shared by the screen itself and by the create/update
// handlers, matching the renderCatalogue pattern: a validation failure comes
// back on the same page with the existing inventory still visible.
func (h *Handler) renderNAS(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Routers", "nas")
	d.Message, d.Error = message, errMsg

	if h.nas == nil {
		d.Error = "NAS management is not configured on this deployment."
		h.render(w, "nas", d)
		return
	}

	devices, err := h.nas.ListNASDeviceSummaries(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: list nas devices failed")
		d.Error = "Could not load registered devices."
		h.render(w, "nas", d)
		return
	}

	d.Data = nasData{Devices: devices, Vendors: nas.Vendors()}
	h.render(w, "nas", d)
}

// CreateNASDeviceForm registers a NAS from the console.
func (h *Handler) CreateNASDeviceForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "nas")
	if !ok {
		return
	}
	if h.nas == nil || h.secretEncryptor == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "NAS management is not configured.")
		return
	}

	ip := strings.TrimSpace(r.PostFormValue("ip"))
	vendor := strings.TrimSpace(r.PostFormValue("vendor"))
	description := strings.TrimSpace(r.PostFormValue("description"))
	secret := r.PostFormValue("radius_secret")
	allowMAB := r.PostFormValue("allow_mab") == "on"

	coaPort, podPort, err := nasPortsFromForm(r)
	if err != nil {
		h.renderNAS(w, r, s, "", err.Error())
		return
	}
	if err := validateNASForm(ip, vendor, secret); err != nil {
		h.renderNAS(w, r, s, "", err.Error())
		return
	}

	encrypted, err := h.secretEncryptor.Encrypt(secret)
	if err != nil {
		log.Error().Err(err).Msg("staffui: encrypt nas secret failed")
		h.renderNAS(w, r, s, "", "Could not encrypt the RADIUS secret.")
		return
	}

	created, err := h.nas.CreateNASDevice(r.Context(), nas.NewNASDevice{
		IP:              ip,
		Vendor:          vendor,
		Description:     description,
		SecretEncrypted: encrypted,
		KeyVersion:      h.secretEncryptor.ActiveVersion(),
		CoAPort:         coaPort,
		PoDPort:         podPort,
		AllowMAB:        allowMAB,
	})
	if err != nil {
		log.Error().Err(err).Str("ip", ip).Msg("staffui: create nas device failed")
		h.renderNAS(w, r, s, "", "Could not register that device — check the IP is not already registered.")
		return
	}
	h.renderNAS(w, r, s, fmt.Sprintf(
		"%s device %s registered. RADIUS daemons pick it up within 60 seconds.",
		created.Vendor, created.IP), "")
}

// UpdateNASDeviceForm edits a registered device's settings and, optionally,
// rotates its shared secret. Mirrors internal/api/nas.go's UpdateNASDevice
// semantics: an omitted (blank) secret leaves the existing one untouched.
func (h *Handler) UpdateNASDeviceForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "nas")
	if !ok {
		return
	}
	if h.nas == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "NAS management is not configured.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderNAS(w, r, s, "", "Invalid device id.")
		return
	}

	vendor := strings.TrimSpace(r.PostFormValue("vendor"))
	if !nas.KnownVendor(nas.Vendor(vendor)) {
		h.renderNAS(w, r, s, "", "Vendor must be one of: "+strings.Join(nas.Vendors(), ", ")+".")
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))
	allowMAB := r.PostFormValue("allow_mab") == "on"
	coaPort, podPort, err := nasPortsFromForm(r)
	if err != nil {
		h.renderNAS(w, r, s, "", err.Error())
		return
	}

	update := nas.NASDeviceUpdate{
		Vendor:      &vendor,
		Description: &description,
		CoAPort:     &coaPort,
		PoDPort:     &podPort,
		AllowMAB:    &allowMAB,
	}

	if secret := r.PostFormValue("radius_secret"); secret != "" {
		if len(secret) < 16 {
			h.renderNAS(w, r, s, "", "RADIUS secret must be at least 16 characters.")
			return
		}
		if h.secretEncryptor == nil {
			h.renderNAS(w, r, s, "", "No encryption key is configured; the secret cannot be rotated.")
			return
		}
		encrypted, err := h.secretEncryptor.Encrypt(secret)
		if err != nil {
			log.Error().Err(err).Msg("staffui: encrypt nas secret failed")
			h.renderNAS(w, r, s, "", "Could not encrypt the RADIUS secret.")
			return
		}
		version := h.secretEncryptor.ActiveVersion()
		update.SecretEncrypted = &encrypted
		update.KeyVersion = &version
	}

	updated, err := h.nas.UpdateNASDevice(r.Context(), id, update)
	if err != nil {
		log.Error().Err(err).Int("id", id).Msg("staffui: update nas device failed")
		h.renderNAS(w, r, s, "", "Could not save that device.")
		return
	}
	if updated == nil {
		h.renderNAS(w, r, s, "", "No device with that id.")
		return
	}
	h.renderNAS(w, r, s, fmt.Sprintf("Device %s updated.", updated.IP), "")
}

func nasPortsFromForm(r *http.Request) (coaPort, podPort int, err error) {
	coaPort, err = nasPortOrDefault(r.PostFormValue("coa_port"))
	if err != nil {
		return 0, 0, fmt.Errorf("CoA port must be between 1 and 65535.")
	}
	podPort, err = nasPortOrDefault(r.PostFormValue("pod_port"))
	if err != nil {
		return 0, 0, fmt.Errorf("PoD port must be between 1 and 65535.")
	}
	return coaPort, podPort, nil
}

func nasPortOrDefault(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultControlPort, nil
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("out of range")
	}
	return p, nil
}

// validateNASForm mirrors internal/api/nas.go's validateNASRequest so the
// console rejects exactly what the API would, for the same reason (RFC 5080
// §2.3's 16-character minimum for a RADIUS shared secret).
func validateNASForm(ip, vendor, secret string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("IP must be a valid address, for example 192.0.2.10.")
	}
	if !nas.KnownVendor(nas.Vendor(vendor)) {
		return fmt.Errorf("Vendor must be one of: %s.", strings.Join(nas.Vendors(), ", "))
	}
	if len(secret) < 16 {
		return fmt.Errorf("RADIUS secret must be at least 16 characters.")
	}
	return nil
}
