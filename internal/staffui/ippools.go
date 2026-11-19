package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/rs/zerolog/log"
)

// IP pools and static addresses — FR-NET-005.
//
// IPPoolManager is satisfied by *api.Handler, so the console validates,
// audits and invalidates the auth cache exactly as the API does.
type IPPoolManager interface {
	CreatePool(ctx context.Context, in nas.Pool) (*nas.Pool, error)
	DeletePool(ctx context.Context, id int) error
	ListPools(ctx context.Context, f nas.PoolFilter) ([]nas.Pool, error)
	AddExclusion(ctx context.Context, poolID int, r nas.AddrRange, reason string) (*nas.PoolExclusion, error)
	RemoveExclusion(ctx context.Context, id int) error
	AllocateStaticIP(ctx context.Context, req nas.AllocateRequest) (*nas.StaticAssignment, error)
	ReleaseStaticIP(ctx context.Context, id int, reason string) error
	ListSubscriberStaticIPs(ctx context.Context, subscriberID int) ([]nas.StaticAssignment, error)
	ListPoolAssignments(ctx context.Context, poolID int) ([]nas.StaticAssignment, error)
}

// ipPoolMessages words each outcome carried back in the URL.
var ipPoolMessages = map[string]string{
	"created":   "IP pool added. Exclude the gateway and broadcast addresses before handing any out.",
	"deleted":   "IP pool deleted.",
	"excluded":  "Those addresses are now excluded and will not be handed out.",
	"included":  "Exclusion removed. Those addresses can be handed out again.",
	"allocated": "Static address assigned. The subscriber gets it on their next authentication.",
	"released":  "Static address released, and any charge for it ended.",

	"duplicate": "Not added: this router already has a pool with that name.",
	"inuse":     "Not deleted: addresses from this pool have been assigned. Release them and keep the pool — its history explains the addresses on past invoices.",
	"notstatic": "Addresses are only handed out from a static pool.",
	"exhausted": "No free address left in that pool. Widen it, remove an exclusion, or release an address.",
	"taken":     "That address is already assigned to someone.",
	"excl":      "That address is excluded from the pool.",
	"outside":   "That address is not inside the pool's range.",
	"already":   "That subscriber already holds a static address. Release it before assigning another.",
	"freed":     "That address has already been released.",
	"missing":   "No pool or address with that id.",
	"failed":    "That did not go through. Nothing was changed; try again.",
}

var ipPoolSuccess = map[string]bool{
	"created": true, "deleted": true, "excluded": true, "included": true,
	"allocated": true, "released": true,
}

type poolRow struct {
	nas.Pool
	Holders []nas.StaticAssignment
}

type ipPoolsData struct {
	Purpose   string
	Rows      []poolRow
	CanManage bool
	Chips     []ipPoolChip
	Purposes  []ipPoolChip
	Expanded  int
	Routers   []NASOption
}

type ipPoolChip struct{ Key, Label string }

var ipPoolChips = []ipPoolChip{
	{"", "All"}, {nas.PoolStatic, "Static"}, {nas.PoolDHCP, "Router DHCP"}, {nas.PoolCGNAT, "CGNAT"},
}

var ipPoolPurposes = []ipPoolChip{
	{nas.PoolStatic, "Static assignment (handed out here)"},
	{nas.PoolDHCP, "Router DHCP (recorded only)"},
	{nas.PoolCGNAT, "CGNAT (recorded only)"},
}

// NASOption is one router for the pool form's dropdown.
type NASOption struct {
	ID          int
	IP          string
	Description string
}

// Label reads "198.51.100.1 — Core router" for the dropdown.
func (o NASOption) Label() string {
	if o.Description == "" {
		return o.IP
	}
	return o.IP + " — " + o.Description
}

// canManagePools: network infrastructure, the same reach as the Routers
// screen. A pool describes the network's address space.
func canManagePools(role string) bool {
	return role == "isp_owner" || role == "noc_engineer"
}

// canAssignStaticIP: handing out an address is a network act and the
// charge that follows is a billing one, so both tiers do this — matching
// the API's nocOrBilling gate.
func canAssignStaticIP(role string) bool {
	return role == "isp_owner" || role == "noc_engineer" || role == "billing_admin"
}

func (h *Handler) ipPoolGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "ip-pools")
	if !ok {
		return s, false
	}
	if h.ipPools == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"IP pools are not configured on this deployment.")
		return s, false
	}
	return s, true
}

func (h *Handler) requirePoolManager(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.ipPoolGate(w, r)
	if !ok {
		return s, false
	}
	if !canManagePools(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only network engineers and the owner can change IP pools.")
		return s, false
	}
	return s, true
}

// IPPools handles GET /staff/ip-pools.
func (h *Handler) IPPools(w http.ResponseWriter, r *http.Request) {
	s, ok := h.ipPoolGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "IP pools", "ip-pools")
	q := r.URL.Query()

	pd := ipPoolsData{
		Purpose:   q.Get("purpose"),
		CanManage: canManagePools(s.Role),
		Chips:     ipPoolChips,
		Purposes:  ipPoolPurposes,
	}
	switch pd.Purpose {
	case nas.PoolStatic, nas.PoolDHCP, nas.PoolCGNAT:
	default:
		pd.Purpose = ""
	}
	if code := q.Get("p"); code != "" {
		if m, ok := ipPoolMessages[code]; ok {
			if ipPoolSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	if reason := strings.TrimSpace(q.Get("err")); reason != "" {
		if len([]rune(reason)) > 200 {
			reason = string([]rune(reason)[:200])
		}
		d.Error = "Not saved: " + reason + "."
	}
	if n, err := strconv.Atoi(q.Get("show")); err == nil && n > 0 {
		pd.Expanded = n
	}

	list, err := h.ipPools.ListPools(r.Context(), nas.PoolFilter{Purpose: pd.Purpose, Limit: 200})
	if err != nil {
		log.Error().Err(err).Msg("staffui: list IP pools failed")
		d.Error = "Could not load the IP pools."
	}
	for _, p := range list {
		row := poolRow{Pool: p}
		if p.ID == pd.Expanded {
			if holders, err := h.ipPools.ListPoolAssignments(r.Context(), p.ID); err == nil {
				row.Holders = holders
			} else {
				log.Error().Err(err).Int("pool_id", p.ID).Msg("staffui: list pool assignments failed")
				d.Error = "Could not load who holds addresses from that pool."
			}
		}
		pd.Rows = append(pd.Rows, row)
	}

	// The router dropdown, so a pool can be tied to one.
	if pd.CanManage && h.nas != nil {
		if devices, err := h.nas.ListNASDeviceSummaries(r.Context()); err == nil {
			for _, dev := range devices {
				pd.Routers = append(pd.Routers, NASOption{
					ID: dev.ID, IP: dev.IP, Description: dev.Description,
				})
			}
		} else {
			log.Warn().Err(err).Msg("staffui: router list for the pool form unavailable")
		}
	}

	d.Data = pd
	h.render(w, "ip_pools", d)
}

// CreatePoolForm handles POST /staff/ip-pools.
func (h *Handler) CreatePoolForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requirePoolManager(w, r)
	if !ok {
		return
	}
	rng, err := nas.ParseAddrRange(r.PostFormValue("first_address"), r.PostFormValue("last_address"))
	if err != nil {
		h.redirectPools(w, r, ipPoolErrCode(err))
		return
	}
	in := nas.Pool{
		Name: r.PostFormValue("name"), Purpose: r.PostFormValue("purpose"),
		Range: rng, Description: r.PostFormValue("description"),
	}
	if v := strings.TrimSpace(r.PostFormValue("nas_id")); v != "" && v != "0" {
		n, err := strconv.Atoi(v)
		if err != nil {
			h.redirectPools(w, r, "failed")
			return
		}
		in.NASID = &n
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.ipPools.CreatePool(ctx, in); err != nil {
		h.poolFailure(w, r, err, "create IP pool")
		return
	}
	h.redirectPools(w, r, "created")
}

// DeletePoolForm handles POST /staff/ip-pools/{id}/delete.
func (h *Handler) DeletePoolForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requirePoolManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid pool id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.ipPools.DeletePool(ctx, id); err != nil {
		h.poolFailure(w, r, err, "delete IP pool")
		return
	}
	h.redirectPools(w, r, "deleted")
}

// AddExclusionForm handles POST /staff/ip-pool-exclusions.
//
// The pool comes from the form rather than the path so the form works
// without JavaScript: a single action with a <select> needs no scripting
// to rewrite where it posts to.
func (h *Handler) AddExclusionForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requirePoolManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("pool_id")))
	if err != nil || id <= 0 {
		h.redirectPools(w, r, "invalid:choose the pool to exclude addresses from")
		return
	}
	rng, err := nas.ParseAddrRange(r.PostFormValue("first_address"), r.PostFormValue("last_address"))
	if err != nil {
		h.redirectPools(w, r, ipPoolErrCode(err))
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.ipPools.AddExclusion(ctx, id, rng, r.PostFormValue("reason")); err != nil {
		h.poolFailure(w, r, err, "add exclusion")
		return
	}
	h.redirectPools(w, r, "excluded")
}

// RemoveExclusionForm handles POST /staff/ip-pool-exclusions/{id}/delete.
func (h *Handler) RemoveExclusionForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requirePoolManager(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid exclusion id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.ipPools.RemoveExclusion(ctx, id); err != nil {
		h.poolFailure(w, r, err, "remove exclusion")
		return
	}
	h.redirectPools(w, r, "included")
}

// AllocateStaticIPForm handles POST /staff/subscribers/{id}/static-ip,
// posted from the subscriber's own page.
func (h *Handler) AllocateStaticIPForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canAssignStaticIP(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only network engineers, billing staff and the owner can assign a static address.")
		return
	}
	if h.ipPools == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"IP pools are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}

	poolID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("pool_id")))
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Address not assigned: choose a pool.")
		return
	}
	req := nas.AllocateRequest{PoolID: poolID, SubscriberID: id}
	if v := strings.TrimSpace(r.PostFormValue("address")); v != "" {
		addr, err := netip.ParseAddr(v)
		if err != nil {
			h.renderSubscriberDetail(w, r, s, id,
				"Address not assigned: enter an IP address, for example 203.0.113.4, or leave it blank for the next free one.")
			return
		}
		req.Address = addr.Unmap()
	}
	if v := strings.TrimSpace(r.PostFormValue("product_id")); v != "" && v != "0" {
		if req.ProductID, err = strconv.Atoi(v); err != nil {
			h.renderSubscriberDetail(w, r, s, id, "Address not assigned: that is not a valid charge.")
			return
		}
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	if _, err := h.ipPools.AllocateStaticIP(ctx, req); err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Address not assigned: "+ipPoolReason(err))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/staff/subscribers/%d?msg=%s", id,
		url.QueryEscape(ipPoolMessages["allocated"])), http.StatusSeeOther)
}

// ReleaseStaticIPForm handles POST /staff/static-ips/{id}/release.
func (h *Handler) ReleaseStaticIPForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canAssignStaticIP(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only network engineers, billing staff and the owner can release a static address.")
		return
	}
	if h.ipPools == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"IP pools are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid address id.")
		return
	}
	subscriberID, _ := strconv.Atoi(r.PostFormValue("subscriber_id"))

	ctx := middleware.WithSubject(r.Context(), s.Username)
	err = h.ipPools.ReleaseStaticIP(ctx, id, r.PostFormValue("reason"))

	if subscriberID <= 0 {
		h.redirectPools(w, r, releaseCode(err))
		return
	}
	if err != nil {
		h.renderSubscriberDetail(w, r, s, subscriberID, "Address not released: "+ipPoolReason(err))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/staff/subscribers/%d?msg=%s", subscriberID,
		url.QueryEscape(ipPoolMessages["released"])), http.StatusSeeOther)
}

func releaseCode(err error) string {
	if err == nil {
		return "released"
	}
	return ipPoolErrCode(err)
}

// ipPoolReason is the sentence to show an operator for a failure.
func ipPoolReason(err error) string {
	if code := ipPoolErrCode(err); code != "failed" {
		if reason, ok := strings.CutPrefix(code, "invalid:"); ok {
			return reason + "."
		}
		if m, ok := ipPoolMessages[code]; ok {
			return m
		}
	}
	log.Error().Err(err).Msg("staffui: IP pool action failed")
	return "it did not go through. Nothing was changed; try again."
}

// ipPoolErrCode maps a failure onto one of ipPoolMessages' keys.
func ipPoolErrCode(err error) string {
	switch {
	case errors.Is(err, nas.ErrPoolNameTaken):
		return "duplicate"
	case errors.Is(err, nas.ErrPoolInUse):
		return "inuse"
	case errors.Is(err, nas.ErrPoolNotStatic):
		return "notstatic"
	case errors.Is(err, nas.ErrPoolExhausted):
		return "exhausted"
	case errors.Is(err, nas.ErrAddressTaken):
		return "taken"
	case errors.Is(err, nas.ErrAddressExcluded):
		return "excl"
	case errors.Is(err, nas.ErrAddressOutsidePool):
		return "outside"
	case errors.Is(err, nas.ErrSubscriberHasAddress):
		return "already"
	case errors.Is(err, nas.ErrAssignmentNotActive):
		return "freed"
	case errors.Is(err, nas.ErrPoolNotFound), errors.Is(err, nas.ErrAssignmentNotFound):
		return "missing"
	case nas.PoolInvalidReason(err) != "":
		return "invalid:" + nas.PoolInvalidReason(err)
	default:
		return "failed"
	}
}

func (h *Handler) poolFailure(w http.ResponseWriter, r *http.Request, err error, what string) {
	code := ipPoolErrCode(err)
	if code == "failed" {
		log.Error().Err(err).Msgf("staffui: %s failed", what)
	}
	h.redirectPools(w, r, code)
}

// redirectPools goes back to the list with an outcome code.
func (h *Handler) redirectPools(w http.ResponseWriter, r *http.Request, code string) {
	q := url.Values{}
	if reason, ok := strings.CutPrefix(code, "invalid:"); ok {
		q.Set("err", reason)
	} else {
		q.Set("p", code)
	}
	if from := r.PostFormValue("from_purpose"); from != "" {
		q.Set("purpose", from)
	}
	http.Redirect(w, r, "/staff/ip-pools?"+q.Encode(), http.StatusSeeOther)
}

// staticIPProducts lists the recurring catalogue items an operator might
// bill a static address with, for the assign form's dropdown.
//
// Recurring only: a static address is a monthly charge, and offering a
// one-time product here would attach a charge that bills once and then
// looks like it stopped working.
func (h *Handler) staticIPProducts(ctx context.Context) []billing.Product {
	if h.products == nil {
		return nil
	}
	list, err := h.products.ListProducts(ctx, billing.ProductFilter{
		Kind: billing.ProductRecurring, ActiveOnly: true, Limit: 200,
	})
	if err != nil {
		log.Warn().Err(err).Msg("staffui: product list for the static-IP form unavailable")
		return nil
	}
	return list
}
