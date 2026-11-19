package billing

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Product catalogue and per-line invoicing — FR-BIL-022 | migration 058.
//
// An ISP sells more than bandwidth: installation, router rental, a static
// IP, add-ons. Each is a distinct supply under GST with its own HSN/SAC
// code, so each has to appear as its own line on the tax invoice rather
// than being folded into the plan's taxable value.
//
// That changes how an invoice's tax is computed. Before this, an invoice
// was one amount and the tax was computed on it. Now the tax is computed
// per line and the header is the sum of the lines — see ComposeInvoice for
// why that ordering, and not the other, is the GST-correct one.
//
// Products are attached to a subscriber (subscriber_products). A recurring
// attachment is billed on every renewal until staff end it; a one-time
// attachment is billed on the next invoice and then marked charged, in the
// same transaction as the invoice, so it can never be billed twice.

// Product kinds.
const (
	// ProductOneTime is charged on the next invoice and then done with.
	ProductOneTime = "one_time"
	// ProductRecurring is charged on every renewal until it is ended.
	ProductRecurring = "recurring"
)

// Attachment statuses.
const (
	// AttachmentActive: a recurring charge still being billed, or a
	// one-time charge not yet invoiced.
	AttachmentActive = "active"
	// AttachmentCharged: a one-time charge that has been invoiced.
	AttachmentCharged = "charged"
	// AttachmentEnded: a recurring charge staff have stopped.
	AttachmentEnded = "ended"
)

// Invoice line kinds.
const (
	// LineKindPlan is the subscriber's own plan for the cycle.
	LineKindPlan = "plan"
	// LineKindProduct is a catalogue item.
	LineKindProduct = "product"
	// LineKindOneOff is an ad-hoc charge, such as a converted proforma,
	// which has no catalogue entry behind it.
	LineKindOneOff = "one_off"
)

// Limits on a catalogue entry, mirrored by migration 058's CHECKs so that
// neither code nor the database is the only thing holding the line.
const (
	ProductMaxName        = 120
	ProductMaxDescription = 500
	ProductMaxQuantity    = 999
	// LineMaxDescription matches invoice_lines.chk_invoice_line_description.
	LineMaxDescription = 200
)

// ProductMaxPrice bounds a catalogue price, as ProformaMaxBase bounds a
// quotation: well above any plausible ISP charge, low enough that a
// slipped extra zero is caught rather than billed.
var ProductMaxPrice = decimal.NewFromInt(1_000_000)

// PlanHSNSAC is the SAC code for internet access, used for the plan line.
// 998432 — "Internet telecommunication services" — which is what the
// invoice footer has always printed.
const PlanHSNSAC = "998432"

var (
	// ErrProductInvalid wraps an operator-facing sentence.
	ErrProductInvalid = errors.New("billing: invalid product")
	// ErrProductNotFound: no such catalogue entry.
	ErrProductNotFound = errors.New("billing: product not found")
	// ErrProductInactive: the product exists but has been deactivated, so
	// it cannot be attached to anyone new.
	ErrProductInactive = errors.New("billing: product is no longer offered")
	// ErrProductCodeTaken: another product already uses that code.
	ErrProductCodeTaken = errors.New("billing: a product with that code already exists")
	// ErrProductInUse: the product is attached to subscribers, so it
	// cannot be deleted — deactivate it instead.
	ErrProductInUse = errors.New("billing: product is attached to subscribers")
	// ErrAttachmentNotFound: no such attachment.
	ErrAttachmentNotFound = errors.New("billing: subscriber product not found")
	// ErrAttachmentNotActive: already charged or already ended.
	ErrAttachmentNotActive = errors.New("billing: that charge is no longer active")
	// ErrNoInvoiceLines: an invoice must charge at least one thing.
	ErrNoInvoiceLines = errors.New("billing: an invoice needs at least one line")
)

// ProductInvalidReason returns the sentence inside an ErrProductInvalid,
// or "".
func ProductInvalidReason(err error) string {
	if !errors.Is(err, ErrProductInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrProductInvalid.Error()+": ")
}

var (
	productCodeRe = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{1,31}$`)
	hsnSACRe      = regexp.MustCompile(`^[0-9]{4,8}$`)
)

// Product is one catalogue entry.
type Product struct {
	ID          int
	Code        string
	Name        string
	Description string
	Kind        string
	// Price is GST-exclusive, exactly like plans.price.
	Price     decimal.Decimal
	HSNSAC    string
	Active    bool
	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt *time.Time
	// Attached is how many subscribers currently hold this product, when
	// the query asked for it. Tells an operator what deactivating costs.
	Attached int
}

// Recurring reports whether this product is billed every cycle.
func (p Product) Recurring() bool { return p.Kind == ProductRecurring }

// KindLabel is the operator-facing name for the kind.
func (p Product) KindLabel() string {
	if p.Recurring() {
		return "Recurring"
	}
	return "One-time"
}

// Normalise trims and validates a product, returning the cleaned copy.
//
// The code is upper-cased rather than rejected for case: an operator
// typing "install" means INSTALL, and migration 058's CHECK requires the
// stored value to already be upper-case and trimmed.
func (p Product) Normalise() (Product, error) {
	p.Code = strings.ToUpper(strings.TrimSpace(p.Code))
	p.Name = strings.Join(strings.Fields(p.Name), " ")
	p.Description = strings.TrimSpace(p.Description)
	p.HSNSAC = strings.TrimSpace(p.HSNSAC)

	switch {
	case p.Code == "":
		return p, fmt.Errorf("%w: give the product a short code, for example INSTALL", ErrProductInvalid)
	case !productCodeRe.MatchString(p.Code):
		return p, fmt.Errorf("%w: the code must be 2 to 32 characters, letters, digits and hyphens only, starting with a letter or digit", ErrProductInvalid)
	case p.Name == "":
		return p, fmt.Errorf("%w: give the product a name", ErrProductInvalid)
	case len([]rune(p.Name)) > ProductMaxName:
		return p, fmt.Errorf("%w: keep the name to %d characters", ErrProductInvalid, ProductMaxName)
	case len([]rune(p.Description)) > ProductMaxDescription:
		return p, fmt.Errorf("%w: keep the description to %d characters", ErrProductInvalid, ProductMaxDescription)
	case p.Kind != ProductOneTime && p.Kind != ProductRecurring:
		return p, fmt.Errorf("%w: say whether the charge is one-time or recurring", ErrProductInvalid)
	case !p.Price.IsPositive():
		return p, fmt.Errorf("%w: the price must be more than zero; to waive a charge, do not attach it", ErrProductInvalid)
	case !p.Price.Equal(p.Price.Round(2)):
		return p, fmt.Errorf("%w: the price can have at most two decimal places", ErrProductInvalid)
	case p.Price.GreaterThan(ProductMaxPrice):
		return p, fmt.Errorf("%w: the price is over ₹%s; check it", ErrProductInvalid, ProductMaxPrice.StringFixed(0))
	case p.HSNSAC == "":
		return p, fmt.Errorf("%w: give the HSN or SAC code this charge is taxed under, for example %s for internet services", ErrProductInvalid, PlanHSNSAC)
	case !hsnSACRe.MatchString(p.HSNSAC):
		return p, fmt.Errorf("%w: an HSN or SAC code is 4 to 8 digits", ErrProductInvalid)
	}
	return p, nil
}

// SubscriberProduct is one product attached to one subscriber.
type SubscriberProduct struct {
	ID           int
	SubscriberID int
	ProductID    int
	// Denormalised from the catalogue for display and for building invoice
	// lines without a second lookup.
	ProductCode string
	ProductName string
	Kind        string
	HSNSAC      string
	Quantity    int
	// UnitPrice is what this subscriber pays, snapshotted when the product
	// was attached. Deliberately not read back from the catalogue: see
	// migration 058.
	UnitPrice  decimal.Decimal
	Status     string
	AttachedBy string
	AttachedAt time.Time
	EndedAt    *time.Time
	EndedBy    string
	EndReason  string
	InvoiceID  *int
	ChargedAt  *time.Time
	// Username of the subscriber, when the query asked for it (the
	// cross-subscriber list does).
	Username string
}

// Base is the taxable value of this attachment: price × quantity.
func (sp SubscriberProduct) Base() decimal.Decimal {
	return sp.UnitPrice.Mul(decimal.NewFromInt(int64(sp.Quantity))).Round(2)
}

// Recurring reports whether this attachment is billed every cycle.
func (sp SubscriberProduct) Recurring() bool { return sp.Kind == ProductRecurring }

// Active reports whether this attachment still has billing ahead of it.
func (sp SubscriberProduct) Active() bool { return sp.Status == AttachmentActive }

// InvoiceNumber is the document number of the invoice that billed this
// charge, or "" when it has not been billed. A method rather than a field
// so the console template and the API's JSON derive it the same way, from
// the one id the row actually stores.
func (sp SubscriberProduct) InvoiceNumber() string {
	if sp.InvoiceID == nil {
		return ""
	}
	return InvoiceNumber(*sp.InvoiceID)
}

// StatusLabel is the operator-facing status.
func (sp SubscriberProduct) StatusLabel() string {
	switch sp.Status {
	case AttachmentCharged:
		return "Charged"
	case AttachmentEnded:
		return "Ended"
	default:
		if sp.Recurring() {
			return "Active"
		}
		return "Awaiting billing"
	}
}

// AttachRequest is staff attaching a catalogue product to a subscriber.
type AttachRequest struct {
	SubscriberID int
	ProductID    int
	Quantity     int
	// UnitPrice overrides the catalogue price for this subscriber. Zero
	// means "use the catalogue price", which is the ordinary case.
	UnitPrice decimal.Decimal
}

// Normalise validates an attach request, defaulting the quantity to 1.
//
// The price override is validated here but cannot be defaulted here: the
// catalogue price is not known to this layer. The store applies it.
func (r AttachRequest) Normalise() (AttachRequest, error) {
	if r.Quantity == 0 {
		r.Quantity = 1
	}
	switch {
	case r.SubscriberID <= 0:
		return r, fmt.Errorf("%w: no subscriber given", ErrProductInvalid)
	case r.ProductID <= 0:
		return r, fmt.Errorf("%w: choose a product", ErrProductInvalid)
	case r.Quantity < 1 || r.Quantity > ProductMaxQuantity:
		return r, fmt.Errorf("%w: the quantity must be between 1 and %d", ErrProductInvalid, ProductMaxQuantity)
	case r.UnitPrice.IsNegative():
		return r, fmt.Errorf("%w: a price override cannot be negative", ErrProductInvalid)
	case !r.UnitPrice.Equal(r.UnitPrice.Round(2)):
		return r, fmt.Errorf("%w: the price can have at most two decimal places", ErrProductInvalid)
	case r.UnitPrice.GreaterThan(ProductMaxPrice):
		return r, fmt.Errorf("%w: the price is over ₹%s; check it", ErrProductInvalid, ProductMaxPrice.StringFixed(0))
	}
	return r, nil
}

// ProductFilter narrows a catalogue listing.
type ProductFilter struct {
	Kind string // "" for any
	// ActiveOnly hides deactivated products, as the attach form does.
	ActiveOnly bool
	Limit      int
}

// ── Invoice lines ───────────────────────────────────────────────────────────

// LineCharge is one thing to be billed, before tax.
type LineCharge struct {
	Kind        string
	Description string
	HSNSAC      string
	// ProductID is set for LineKindProduct and nil otherwise, matching
	// invoice_lines.chk_invoice_line_product.
	ProductID *int
	// AttachmentID is the subscriber_products row this charge comes from,
	// so a one-time charge can be marked charged in the same transaction
	// as the invoice. Nil for the plan line and for ad-hoc charges.
	AttachmentID *int
	// OneTime marks an attachment that must not be billed again.
	OneTime   bool
	Quantity  int
	UnitPrice decimal.Decimal
}

// InvoiceLine is one priced, taxed line of an invoice.
type InvoiceLine struct {
	ID          int
	InvoiceID   int
	LineNo      int
	Kind        string
	ProductID   *int
	Description string
	HSNSAC      string
	Quantity    int
	UnitPrice   decimal.Decimal
	BaseAmount  decimal.Decimal
	CGSTAmount  decimal.Decimal
	SGSTAmount  decimal.Decimal
	IGSTAmount  decimal.Decimal
	TotalAmount decimal.Decimal

	// AttachmentID and OneTime are carried through from the LineCharge for
	// the store, which has to mark one-time attachments charged alongside
	// the insert. Not persisted on the line itself — subscriber_products
	// holds that link, in the direction that keeps one attachment to one
	// invoice.
	AttachmentID *int
	OneTime      bool
}

// Tax is the GST on this line.
func (l InvoiceLine) Tax() decimal.Decimal {
	return l.CGSTAmount.Add(l.SGSTAmount).Add(l.IGSTAmount)
}

// PlanCharge is the plan line for a renewal cycle.
func PlanCharge(planName string, price decimal.Decimal) LineCharge {
	desc := "Internet Plan — " + planName
	if r := []rune(desc); len(r) > LineMaxDescription {
		desc = string(r[:LineMaxDescription])
	}
	return LineCharge{
		Kind: LineKindPlan, Description: desc, HSNSAC: PlanHSNSAC,
		Quantity: 1, UnitPrice: price,
	}
}

// ProductCharge is the line for one attached catalogue product.
func ProductCharge(sp SubscriberProduct) LineCharge {
	productID, attachmentID := sp.ProductID, sp.ID
	desc := sp.ProductName
	if r := []rune(desc); len(r) > LineMaxDescription {
		desc = string(r[:LineMaxDescription])
	}
	return LineCharge{
		Kind: LineKindProduct, Description: desc, HSNSAC: sp.HSNSAC,
		ProductID: &productID, AttachmentID: &attachmentID,
		OneTime:  !sp.Recurring(),
		Quantity: sp.Quantity, UnitPrice: sp.UnitPrice,
	}
}

// ComposeInvoice taxes each charge in its own right and returns the lines
// together with the invoice header, whose totals are the sums of those
// lines.
//
// Tax is computed per line and then summed — not computed once on the
// summed base. The two differ by up to a paisa per line, and this is the
// order GST requires: each line is a separate supply under its own HSN/SAC
// code, and the tax shown against it has to be the tax on that supply.
// Taxing the total instead would leave the per-line figures unable to
// explain the header, which is the one thing a tax invoice must do.
//
// The consequence is that the header can no longer be re-derived by
// taxing one amount, so migration 058's deferred trigger checks the
// equality in the database as well.
//
// An invoice carrying only a plan line comes out exactly as
// CalculateGstInvoiceFrom would have computed it, which is what keeps
// every invoice raised before this feature comparable with the ones after.
//
// FR: FR-BIL-022, FR-BIL-001, FR-BIL-002
func ComposeInvoice(charges []LineCharge, subscriberState, homeState string, rate GstRate) ([]InvoiceLine, Invoice, error) {
	if len(charges) == 0 {
		return nil, Invoice{}, ErrNoInvoiceLines
	}

	lines := make([]InvoiceLine, 0, len(charges))
	hdr := Invoice{
		BaseAmount:  decimal.Zero,
		CgstAmount:  decimal.Zero,
		SgstAmount:  decimal.Zero,
		IgstAmount:  decimal.Zero,
		TotalAmount: decimal.Zero,
		GstRateID:   rate.ID,
	}

	for i, c := range charges {
		if c.Quantity < 1 || c.Quantity > ProductMaxQuantity {
			return nil, Invoice{}, fmt.Errorf("%w: line %d has quantity %d", ErrProductInvalid, i+1, c.Quantity)
		}
		if !c.UnitPrice.IsPositive() {
			return nil, Invoice{}, fmt.Errorf("%w: line %d (%s) is priced at ₹%s", ErrProductInvalid, i+1, c.Description, c.UnitPrice.StringFixed(2))
		}
		if strings.TrimSpace(c.Description) == "" {
			return nil, Invoice{}, fmt.Errorf("%w: line %d has no description", ErrProductInvalid, i+1)
		}
		if !hsnSACRe.MatchString(c.HSNSAC) {
			return nil, Invoice{}, fmt.Errorf("%w: line %d (%s) has no valid HSN/SAC code", ErrProductInvalid, i+1, c.Description)
		}

		base := c.UnitPrice.Mul(decimal.NewFromInt(int64(c.Quantity))).Round(2)
		taxed := CalculateGstInvoiceFrom(base, subscriberState, homeState, rate)

		lines = append(lines, InvoiceLine{
			LineNo: i + 1, Kind: c.Kind, ProductID: c.ProductID,
			Description: strings.TrimSpace(c.Description), HSNSAC: c.HSNSAC,
			Quantity: c.Quantity, UnitPrice: c.UnitPrice,
			BaseAmount: base,
			CGSTAmount: taxed.CgstAmount, SGSTAmount: taxed.SgstAmount,
			IGSTAmount: taxed.IgstAmount, TotalAmount: taxed.TotalAmount,
			AttachmentID: c.AttachmentID, OneTime: c.OneTime,
		})

		hdr.BaseAmount = hdr.BaseAmount.Add(base)
		hdr.CgstAmount = hdr.CgstAmount.Add(taxed.CgstAmount)
		hdr.SgstAmount = hdr.SgstAmount.Add(taxed.SgstAmount)
		hdr.IgstAmount = hdr.IgstAmount.Add(taxed.IgstAmount)
		hdr.TotalAmount = hdr.TotalAmount.Add(taxed.TotalAmount)
	}

	return lines, hdr, nil
}
