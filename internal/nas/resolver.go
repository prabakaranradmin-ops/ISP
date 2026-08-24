package nas

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// DeviceRow is one row from the nas_devices table, as read from Postgres.
type DeviceRow struct {
	ID                    int
	IP                    string
	Vendor                string
	RadiusSecretEncrypted string
	CoAPort               int
	PoDPort               int
	AllowMAB              bool
}

// DeviceSummary is a NAS as an operator sees it.
//
// Deliberately not DeviceRow: that type carries RadiusSecretEncrypted because
// the resolver needs it, and a management API must never return the secret in
// any form. Two types rather than one with an omitted field, so the API cannot
// start serving the ciphertext by someone adding a JSON tag.
type DeviceSummary struct {
	ID          int       `json:"id"`
	IP          string    `json:"ip"`
	Vendor      string    `json:"vendor"`
	Description string    `json:"description,omitempty"`
	CoAPort     int       `json:"coa_port"`
	PoDPort     int       `json:"pod_port"`
	AllowMAB    bool      `json:"allow_mab"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NetworkHealthRow is one registered NAS device's live standing (CRD-EXP-008)
// — a cross-device view neither the per-subscriber health panel nor the
// Routers screen's per-device list gives today: how many sessions are
// actually active on it right now, read from the same
// subscriber_session_history rows the health panel already trusts.
type NetworkHealthRow struct {
	ID             int    `json:"id"`
	IP             string `json:"ip"`
	Vendor         string `json:"vendor"`
	Description    string `json:"description,omitempty"`
	ActiveSessions int    `json:"active_sessions"`
}

// NewNASDevice is a NAS to register.
//
// The secret arrives already encrypted. Neither this package nor the store
// below it ever handles a plaintext RADIUS secret: encryption happens once, at
// the API edge that received it, so there is no layer where a plaintext secret
// could be logged or persisted by mistake.
//
// Declared here rather than in internal/db because internal/api needs it too,
// and internal/db already imports internal/api — the reverse import would be a
// cycle.
type NewNASDevice struct {
	IP              string
	Vendor          string
	Description     string
	SecretEncrypted string
	KeyVersion      string
	CoAPort         int
	PoDPort         int
	AllowMAB        bool
}

// NASDeviceUpdate carries the fields a partial update may change. Nil means
// "leave alone", so toggling AllowMAB does not require resubmitting the secret.
type NASDeviceUpdate struct {
	Vendor          *string
	Description     *string
	CoAPort         *int
	PoDPort         *int
	AllowMAB        *bool
	SecretEncrypted *string
	KeyVersion      *string
}

// PlanProfileRow is one row from the plan_nas_profiles table.
type PlanProfileRow struct {
	PlanID      int
	Vendor      string
	ProfileName string
}

// DeviceStore lists registered NAS devices and plan-to-profile mappings.
// Satisfied by *db.NASStore.
type DeviceStore interface {
	ListNASDevices(ctx context.Context) ([]DeviceRow, error)
	ListPlanNASProfiles(ctx context.Context) ([]PlanProfileRow, error)
}

func planVendorKey(planID int, vendor Vendor) string {
	return fmt.Sprintf("%d:%s", planID, vendor)
}

const refreshInterval = 60 * time.Second

// Resolver is an in-memory cache of nas_devices, refreshed periodically.
// The registered NAS count is small (an operational inventory — dozens to
// low hundreds — not per-subscriber scale) and changes rarely, so a full
// in-memory map beats a per-packet DB or Redis round trip on the RADIUS hot
// path (NFR-PERF-001: 15ms p99).
//
// Resolver implements layeh.com/radius's SecretSource directly, so it is a
// drop-in replacement for radius.StaticSecretSource(secret).
type Resolver struct {
	mu           sync.RWMutex
	byIP         map[string]Device
	planProfiles map[string]string // planVendorKey -> profile name

	store          DeviceStore
	keyStore       crypto.KeyStore
	defaultSecret  []byte
	defaultCoAPort int
	defaultPoDPort int
}

// NewResolver constructs a Resolver. defaultSecret/defaultPort are used for
// any NAS IP with no nas_devices row — this reproduces today's actual
// behavior exactly (one global secret, MikroTik VSA, one CoA/PoD port), so
// an upgraded deployment needs zero new rows to keep working (MDS §4.11
// rollout note).
func NewResolver(store DeviceStore, keyStore crypto.KeyStore, defaultSecret []byte, defaultPort int) *Resolver {
	return &Resolver{
		byIP:           make(map[string]Device),
		planProfiles:   make(map[string]string),
		store:          store,
		keyStore:       keyStore,
		defaultSecret:  defaultSecret,
		defaultCoAPort: defaultPort,
		defaultPoDPort: defaultPort,
	}
}

// Run refreshes the device cache every refreshInterval until ctx is
// cancelled. Callers that need the cache warm before serving traffic
// should call Refresh once themselves first — Run's first refresh only
// happens after the first tick.
func (r *Resolver) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Refresh(ctx); err != nil {
				log.Warn().Err(err).Msg("nas: device cache refresh failed, keeping previous cache")
			}
		}
	}
}

// Refresh reloads the device cache from the store. A device whose secret
// fails to decrypt is skipped (logged, not fatal) rather than aborting the
// whole refresh — one bad row should not take every other registered NAS
// back to the fallback default.
func (r *Resolver) Refresh(ctx context.Context) error {
	rows, err := r.store.ListNASDevices(ctx)
	if err != nil {
		return fmt.Errorf("nas: list devices: %w", err)
	}
	profileRows, err := r.store.ListPlanNASProfiles(ctx)
	if err != nil {
		return fmt.Errorf("nas: list plan profiles: %w", err)
	}

	next := make(map[string]Device, len(rows))
	for _, row := range rows {
		secret, err := crypto.Decrypt(row.RadiusSecretEncrypted, r.keyStore)
		if err != nil {
			log.Error().Err(err).Str("nas_ip", row.IP).
				Msg("nas: decrypt secret failed, skipping device this refresh")
			continue
		}

		coaPort, podPort := row.CoAPort, row.PoDPort
		if coaPort == 0 {
			coaPort = r.defaultCoAPort
		}
		if podPort == 0 {
			podPort = r.defaultPoDPort
		}

		next[row.IP] = Device{
			ID:      row.ID,
			IP:      row.IP,
			Vendor:  Vendor(row.Vendor),
			Secret:  []byte(secret),
			CoAPort: coaPort,
			PoDPort: podPort,
			// Only a registered NAS can carry this. The fallback Device below
			// leaves it at its false zero value, so an unregistered NAS never
			// gets MAB (FR-HSP-002).
			AllowMAB: row.AllowMAB,
		}
	}

	nextProfiles := make(map[string]string, len(profileRows))
	for _, row := range profileRows {
		nextProfiles[planVendorKey(row.PlanID, Vendor(row.Vendor))] = row.ProfileName
	}

	r.mu.Lock()
	r.byIP = next
	r.planProfiles = nextProfiles
	r.mu.Unlock()
	return nil
}

// ResolveProfile returns the pre-provisioned QoS profile/role name planID
// maps to for vendor, or "" if no plan_nas_profiles row exists — the caller
// (a policy-reference AttributeBuilder) treats an empty name as a build
// error, incrementing nas_attribute_build_errors_total rather than sending
// nothing silently.
func (r *Resolver) ResolveProfile(planID int, vendor Vendor) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.planProfiles[planVendorKey(planID, vendor)]
}

// Resolve returns the registered Device for ip, or the MikroTik/global-
// secret fallback if ip has no nas_devices row (MDS §4.11 rollout note).
func (r *Resolver) Resolve(ip string) Device {
	r.mu.RLock()
	d, ok := r.byIP[ip]
	r.mu.RUnlock()
	if ok {
		return d
	}

	nasUnclassifiedTotal.WithLabelValues(ip).Inc()
	return Device{
		IP:      ip,
		Vendor:  VendorMikrotik,
		Secret:  r.defaultSecret,
		CoAPort: r.defaultCoAPort,
		PoDPort: r.defaultPoDPort,
	}
}

// RADIUSSecret implements layeh.com/radius's SecretSource, letting Resolver
// substitute directly for radius.StaticSecretSource(secret) in
// radius.PacketServer.
func (r *Resolver) RADIUSSecret(_ context.Context, remoteAddr net.Addr) ([]byte, error) {
	return r.ResolveAddr(remoteAddr).Secret, nil
}

// ResolveAddr is Resolve for callers that only have a net.Addr (a RADIUS
// request's RemoteAddr), such as internal/radius's Access-Accept builder.
// An address that cannot be parsed resolves to the same fallback Resolve
// gives an unregistered IP, rather than an error the caller would have to
// handle separately — a malformed remote address should never be able to
// prevent an Accept response.
func (r *Resolver) ResolveAddr(addr net.Addr) Device {
	ip, err := hostFromAddr(addr)
	if err != nil {
		nasUnclassifiedTotal.WithLabelValues("unparseable").Inc()
		return Device{
			Vendor:  VendorMikrotik,
			Secret:  r.defaultSecret,
			CoAPort: r.defaultCoAPort,
			PoDPort: r.defaultPoDPort,
		}
	}
	return r.Resolve(ip)
}

func hostFromAddr(addr net.Addr) (string, error) {
	if udpAddr, ok := addr.(*net.UDPAddr); ok {
		return udpAddr.IP.String(), nil
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", fmt.Errorf("nas: parse remote addr %q: %w", addr.String(), err)
	}
	return host, nil
}
