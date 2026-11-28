package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/crm"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/pkg/validate"
	"github.com/rs/zerolog/log"
)

// Lead pipeline operations, free of HTTP — FR-CRM-001..003 | MDS §4.16.
//
// The API handlers in crm.go and the operations console both call these, so
// validation, the converted/lost guards, the atomic claim, KYC capture, CPE
// issue, metrics and the audit line exist once. Franchise scoping stays with
// each caller: the API derives it from the token, and the console's Leads
// screen is ISP-wide by section gate.

// Errors the lead operations report. ErrLeadInvalid wraps a sentence fit to
// show the operator as-is.
var (
	ErrLeadsUnavailable    = errors.New("api: lead store not configured")
	ErrLeadInvalid         = errors.New("api: invalid lead")
	ErrLeadNotFound        = errors.New("api: lead not found")
	ErrLeadNotEntitled     = errors.New("api: not entitled to this lead")
	ErrLeadDuplicateLogin  = errors.New("api: CAF number or username already exists")
	ErrLeadConversionStore = errors.New("api: lead conversion failed")
)

func leadInvalid(msg string) error { return fmt.Errorf("%w: %s", ErrLeadInvalid, msg) }

// LeadInvalidReason returns the operator-facing sentence inside an
// ErrLeadInvalid, or "" for any other error.
func LeadInvalidReason(err error) string {
	if !errors.Is(err, ErrLeadInvalid) {
		return ""
	}
	msg := err.Error()
	prefix := ErrLeadInvalid.Error() + ": "
	if len(msg) > len(prefix) {
		return msg[len(prefix):]
	}
	return msg
}

// NewLead files a prospect. franchiseScope, when non-nil, overrides
// l.FranchiseID: a franchise-scoped caller's leads always land in their own
// franchise, whatever they asked for.
func (h *Handler) NewLead(ctx context.Context, l crm.Lead, franchiseScope *int) (*crm.Lead, error) {
	if h.leads == nil {
		return nil, ErrLeadsUnavailable
	}
	if l.FullName == "" {
		return nil, leadInvalid("full_name is required")
	}
	// Validated here as well as by chk_leads_mobile_e164: a lead's number
	// becomes a subscriber's number on conversion, so catching a malformed
	// one now beats rejecting it at the worst possible moment.
	if !validate.E164(l.MobileNumber) {
		return nil, leadInvalid("mobile_number must be E.164 format (e.g. +919876543210)")
	}
	if !crm.ValidSource(l.Source) {
		return nil, leadInvalid("source must be one of walk_in, referral, website, campaign, franchise, other")
	}
	if franchiseScope != nil {
		l.FranchiseID = franchiseScope
	}
	created, err := h.leads.CreateLead(ctx, l)
	if err != nil {
		return nil, fmt.Errorf("api: create lead: %w", err)
	}
	crm.LeadsTotal.WithLabelValues(created.Status).Inc()
	middleware.Audit(ctx, "lead.create", strconv.Itoa(created.ID), map[string]any{"source": created.Source})
	return created, nil
}

// leadInScope loads a lead and checks the caller may act on it. scopeKnown
// false means the caller's franchise binding could not be determined, which
// is refused rather than treated as ISP-wide.
func (h *Handler) leadInScope(ctx context.Context, id int, scope *int) (*crm.Lead, error) {
	lead, err := h.leads.GetLead(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("api: lead lookup: %w", err)
	}
	if lead == nil {
		return nil, ErrLeadNotFound
	}
	if scope != nil && (lead.FranchiseID == nil || *lead.FranchiseID != *scope) {
		return nil, ErrLeadNotEntitled
	}
	return lead, nil
}

// MoveLead changes a lead's stage, owner, notes or lost reason — pipeline
// movement short of conversion. nil fields are left unchanged.
func (h *Handler) MoveLead(ctx context.Context, id int, status, assignedTo, notes, lostReason *string, scope *int) (*crm.Lead, error) {
	if h.leads == nil {
		return nil, ErrLeadsUnavailable
	}
	// "converted" is deliberately not reachable here: reaching it also
	// requires creating a subscriber and setting converted_subscriber_id,
	// which only ConvertLeadToSubscriber does. Allowing it would produce a
	// converted lead pointing at nobody — a row
	// chk_lead_converted_has_subscriber refuses anyway, but as an opaque
	// failure rather than this readable refusal.
	if status != nil && !crm.ValidStatus(*status) {
		return nil, leadInvalid("status must be one of new, contacted, qualified, lost (use /convert to convert)")
	}
	if _, err := h.leadInScope(ctx, id, scope); err != nil {
		return nil, err
	}
	updated, err := h.leads.UpdateLead(ctx, id, status, assignedTo, notes, lostReason)
	if err != nil {
		return nil, fmt.Errorf("api: update lead: %w", err)
	}
	if updated == nil {
		return nil, ErrLeadNotFound
	}
	if status != nil {
		crm.LeadsTotal.WithLabelValues(*status).Inc()
	}
	middleware.Audit(ctx, "lead.update", strconv.Itoa(id), map[string]any{"status": status})
	return updated, nil
}

// ConvertLeadInput is what a subscriber needs that a lead does not. Name,
// mobile and email are carried over from the lead rather than accepted, so
// the subscriber provably came from that prospect (FR-CRM-002).
type ConvertLeadInput struct {
	CAFNumber       string
	Username        string
	Password        string
	PlanID          int
	RegisteredState string
	Aadhaar         string
	PAN             string
	// CPESerial optionally issues a device in the same onboarding moment
	// (FR-INV-002). A failure to issue never fails the conversion.
	CPESerial string
}

// ConvertLeadResult reports all three outcomes of the handoff, including a
// subscriber created whose device could not be issued.
type ConvertLeadResult struct {
	Subscriber *SubscriberRecord
	Lead       *crm.Lead
	CPEIssued  string
	CPEWarning string
}

// ConvertLeadToSubscriber claims the lead and creates the subscriber
// atomically, then optionally issues a CPE. The subscriber starts
// pending_payment, as every new subscriber does.
//
// Errors: crm.ErrAlreadyConverted (including losing a concurrent race),
// crm.ErrLeadLost, ErrLeadInvalid, ErrLeadNotFound, ErrLeadNotEntitled,
// ErrLeadDuplicateLogin, ErrLeadsUnavailable, or a wrapped store failure.
func (h *Handler) ConvertLeadToSubscriber(ctx context.Context, id int, in ConvertLeadInput, scope *int) (*ConvertLeadResult, error) {
	if h.leads == nil {
		return nil, ErrLeadsUnavailable
	}
	lead, err := h.leadInScope(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	if lead.Status == crm.StatusConverted {
		crm.ConversionConflictsTotal.Inc()
		return nil, crm.ErrAlreadyConverted
	}
	if lead.Status == crm.StatusLost {
		return nil, crm.ErrLeadLost
	}

	subReq := CreateSubscriberRequest{
		CAFNumber:       in.CAFNumber,
		Username:        in.Username,
		Password:        in.Password,
		MobileNumber:    lead.MobileNumber,
		Email:           lead.Email,
		PlanID:          in.PlanID,
		RegisteredState: in.RegisteredState,
	}
	if err := validateCreateSubscriber(subReq); err != nil {
		return nil, leadInvalid(err.Error())
	}
	hash, err := hashSubscriberPassword(in.Password)
	if err != nil {
		return nil, fmt.Errorf("api: password hash: %w", err)
	}
	rec := subscriberRecordFrom(subReq)
	rec.FranchiseID = lead.FranchiseID // the partner who sourced the lead keeps the subscriber

	convertedLead, created, err := h.leads.ConvertLead(ctx, id, rec, hash)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrLeadDuplicateLogin
		}
		return nil, fmt.Errorf("%w: %v", ErrLeadConversionStore, err)
	}
	if convertedLead == nil {
		// Lost the claim: another caller converted this lead between the
		// read above and the transaction. Exactly one subscriber exists,
		// and it is not ours.
		crm.ConversionConflictsTotal.Inc()
		return nil, crm.ErrAlreadyConverted
	}

	h.persistKYC(ctx, created.ID, in.Aadhaar, in.PAN)

	res := &ConvertLeadResult{Subscriber: created, Lead: convertedLead}
	if in.CPESerial != "" {
		// Deliberately after the conversion committed, and never fatal to
		// it: the subscriber exists and can be billed whether or not the
		// warehouse could supply a router (MDS §4.16).
		res.CPEIssued, res.CPEWarning = h.issueCPEForConversion(ctx, in.CPESerial, created.ID)
	}

	crm.LeadsTotal.WithLabelValues(crm.StatusConverted).Inc()
	middleware.Audit(ctx, "lead.convert", strconv.Itoa(id), map[string]any{
		"subscriber_id": created.ID, "cpe_serial": res.CPEIssued,
	})
	return res, nil
}

// issueCPEForConversion issues a device as part of onboarding, returning
// (serial, "") on success and ("", warning) on any failure — never an error,
// because no CPE problem may undo a completed conversion.
func (h *Handler) issueCPEForConversion(ctx context.Context, serial string, subscriberID int) (issued, warning string) {
	if h.inventory == nil {
		return "", "inventory is not configured; issue the device separately"
	}
	device, err := h.inventory.IssueDevice(ctx, serial, subscriberID)
	if err != nil {
		log.Error().Err(err).Str("serial", serial).Int("subscriber_id", subscriberID).
			Msg("api: CPE issuance failed during lead conversion")
		return "", "device could not be issued; the subscriber was created and the device can be issued separately"
	}
	if device == nil {
		return "", "device " + serial + " is not in stock; the subscriber was created and a device can be issued separately"
	}
	// Counted by inventory.Metered, which wraps the store.
	return device.SerialNumber, ""
}

// LeadList returns leads, optionally by stage and source, within scope.
func (h *Handler) LeadList(ctx context.Context, status, source *string, scope *int) ([]crm.Lead, error) {
	if h.leads == nil {
		return nil, ErrLeadsUnavailable
	}
	return h.leads.ListLeads(ctx, status, source, scope)
}

// LeadFunnelReport returns the FR-CRM-003 funnel within scope.
func (h *Handler) LeadFunnelReport(ctx context.Context, scope *int) (*crm.FunnelReport, error) {
	if h.leads == nil {
		return nil, ErrLeadsUnavailable
	}
	return h.leads.GetFunnel(ctx, scope)
}

// LeadByID returns one lead within scope.
func (h *Handler) LeadByID(ctx context.Context, id int, scope *int) (*crm.Lead, error) {
	if h.leads == nil {
		return nil, ErrLeadsUnavailable
	}
	return h.leadInScope(ctx, id, scope)
}
