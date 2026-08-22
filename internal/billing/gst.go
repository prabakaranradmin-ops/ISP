// Package billing implements GST computation, wallet double-entry, dunning,
// and invoice generation for the ISP BSS/OSS platform.
//
// FR: FR-BIL-001..007 | DDS §5.4, §5.6 | DBD §6.2
package billing

import (
	"github.com/shopspring/decimal"
)

// GstRate holds the applicable tax rates for a billing period.
type GstRate struct {
	ID       int
	CgstRate decimal.Decimal
	SgstRate decimal.Decimal
	IgstRate decimal.Decimal
}

// Invoice represents a computed GST invoice ready for persistence.
type Invoice struct {
	SubscriberID int
	BaseAmount   decimal.Decimal
	CgstAmount   decimal.Decimal
	SgstAmount   decimal.Decimal
	IgstAmount   decimal.Decimal
	TotalAmount  decimal.Decimal
	GstRateID    int
	GbIncluded   int
	GbUsed       decimal.Decimal
}

// DefaultHomeState is the supplier's own state when none is configured.
//
// "TN" preserves the behaviour this function had when the comparison was
// written inline: an operator who has not set GST_HOME_STATE gets exactly
// what they got before. It is a default, not an assumption about where an
// ISP is - CalculateGstInvoiceFrom takes the home state explicitly.
const DefaultHomeState = "TN"

// CalculateGstInvoice applies intrastate (CGST+SGST) or interstate (IGST)
// tax rules against the default home state.
//
// Retained for callers that predate configurable supplier state; new code
// should use CalculateGstInvoiceFrom and pass the configured value.
//
// FR: FR-BIL-001, FR-BIL-002 | DDS §5.4
func CalculateGstInvoice(baseAmount decimal.Decimal, subscriberState string, rate GstRate) Invoice {
	return CalculateGstInvoiceFrom(baseAmount, subscriberState, DefaultHomeState, rate)
}

// CalculateGstInvoiceFrom applies intrastate (CGST+SGST) or interstate
// (IGST) tax rules based on the subscriber's state against the supplier's
// own. Uses banker's rounding (Round(2)) as required by FR-BIL-002.
//
// Both states are normalised before comparison rather than compared as
// given. The inline comparison this replaced tested subscriberState ==
// "TN" literally, so a Tamil Nadu subscriber recorded as "Tamil Nadu",
// "tn" or "33" fell through to the interstate branch and was charged IGST.
// The total is the same 18% either way, which is exactly why it went
// unnoticed - but IGST accrues wholly to the centre where CGST/SGST splits
// with the state, so the money was correct and its destination was not.
//
// An unrecognised state is treated as interstate, which is the safe
// direction: IGST is the correct head for a genuinely out-of-state
// supply, and the alternative would be claiming a state share for a state
// this system could not identify. Entry-point validation
// (billing.NormaliseState, applied when a subscriber is created) is what
// keeps unrecognised values from reaching here at all.
//
// FR: FR-BIL-001, FR-BIL-002 | DDS §5.4
func CalculateGstInvoiceFrom(baseAmount decimal.Decimal, subscriberState, homeState string, rate GstRate) Invoice {
	var cgst, sgst, igst decimal.Decimal

	subCode, subOK := NormaliseState(subscriberState)
	homeCode, homeOK := NormaliseState(homeState)

	if subOK && homeOK && subCode == homeCode {
		// Intrastate: split CGST + SGST
		cgst = baseAmount.Mul(rate.CgstRate).Div(decimal.NewFromInt(100)).Round(2)
		sgst = baseAmount.Mul(rate.SgstRate).Div(decimal.NewFromInt(100)).Round(2)
		igst = decimal.Zero
	} else {
		// Interstate: IGST only
		igst = baseAmount.Mul(rate.IgstRate).Div(decimal.NewFromInt(100)).Round(2)
		cgst = decimal.Zero
		sgst = decimal.Zero
	}

	total := baseAmount.Add(cgst).Add(sgst).Add(igst)
	return Invoice{
		BaseAmount:  baseAmount,
		CgstAmount:  cgst,
		SgstAmount:  sgst,
		IgstAmount:  igst,
		TotalAmount: total,
		GstRateID:   rate.ID,
	}
}
