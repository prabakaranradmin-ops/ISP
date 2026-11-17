package billing

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Proforma invoices — FR-BIL-018 | migration 056.
//
// A proforma is a quotation: what a charge will cost, with GST estimated at
// today's rate, for a customer who needs a document to raise payment
// against. It is not a tax invoice. It has its own number series (PF-...)
// so it never takes a number from the INV- series, which GST requires to
// run without gaps, and it never appears in GSTR-1.
//
// Converting one issues the real tax invoice for a one-off charge: the
// wallet is debited, the tax part posted to GST Payable exactly as a
// renewal's is, and an invoice carrying the proforma's description is
// written — all in one transaction. The wallet has to cover it, which is
// to say the customer pays first and the tax invoice follows the payment.
//
// Plan prepayments are not converted: recording the payment is enough,
// and each renewal raises its own tax invoice as it is consumed.
// Converting a plan quote as well would charge the customer twice.

// Proforma statuses.
const (
	ProformaOpen      = "open"
	ProformaConverted = "converted"
	ProformaCancelled = "cancelled"
)

// Limits on what a proforma can say.
const (
	ProformaMaxDescription = 200
	ProformaMaxValidDays   = 90
	ProformaDefaultValid   = 15
)

// ProformaMaxBase bounds a single quotation. Well above any plausible
// one-off charge for an ISP customer, and low enough that a slipped extra
// zero is caught rather than quoted.
var ProformaMaxBase = decimal.NewFromInt(1_000_000)

var (
	// ErrProformaInvalid wraps an operator-facing sentence.
	ErrProformaInvalid = errors.New("billing: invalid proforma")
	// ErrProformaNotOpen: already converted or cancelled.
	ErrProformaNotOpen = errors.New("billing: proforma is no longer open")
	// ErrProformaExpired: past its valid-until date; the quoted price no
	// longer stands, so a new proforma is needed.
	ErrProformaExpired = errors.New("billing: proforma has expired")
	// ErrProformaNotFound: no such proforma.
	ErrProformaNotFound = errors.New("billing: proforma not found")
)

// ProformaInvalidReason returns the sentence inside an ErrProformaInvalid,
// or "".
func ProformaInvalidReason(err error) string {
	if !errors.Is(err, ErrProformaInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrProformaInvalid.Error()+": ")
}

// ProformaRequest is what staff ask for.
type ProformaRequest struct {
	SubscriberID int
	Description  string
	Base         decimal.Decimal // taxable value, before GST
	ValidDays    int             // 0 means ProformaDefaultValid
}

// Normalise trims the request and validates it, returning the cleaned copy.
func (r ProformaRequest) Normalise() (ProformaRequest, error) {
	r.Description = strings.Join(strings.Fields(r.Description), " ")
	if r.ValidDays == 0 {
		r.ValidDays = ProformaDefaultValid
	}
	switch {
	case r.Description == "":
		return r, fmt.Errorf("%w: describe what is being quoted, for example \"Installation and router\"", ErrProformaInvalid)
	case len([]rune(r.Description)) > ProformaMaxDescription:
		return r, fmt.Errorf("%w: keep the description to %d characters", ErrProformaInvalid, ProformaMaxDescription)
	case !r.Base.IsPositive():
		return r, fmt.Errorf("%w: the amount must be more than zero", ErrProformaInvalid)
	case !r.Base.Equal(r.Base.Round(2)):
		return r, fmt.Errorf("%w: the amount can have at most two decimal places", ErrProformaInvalid)
	case r.Base.GreaterThan(ProformaMaxBase):
		return r, fmt.Errorf("%w: the amount is over ₹%s; check it, or split the quotation", ErrProformaInvalid, ProformaMaxBase.StringFixed(0))
	case r.ValidDays < 1 || r.ValidDays > ProformaMaxValidDays:
		return r, fmt.Errorf("%w: a proforma can stay valid for 1 to %d days", ErrProformaInvalid, ProformaMaxValidDays)
	}
	return r, nil
}

// ProformaValidUntil is the last day a proforma issued at `at` stands.
func ProformaValidUntil(at time.Time, validDays int) time.Time {
	y, m, d := at.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, at.Location()).AddDate(0, 0, validDays)
}

// ProformaExpired reports whether a proforma valid until validUntil (a
// date) has lapsed at now: it stands for the whole of its last day.
func ProformaExpired(validUntil, now time.Time) bool {
	y, m, d := validUntil.Date()
	end := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	return !now.Before(end)
}

// ProformaNumber is the document number printed on a proforma.
func ProformaNumber(id int) string { return fmt.Sprintf("PF-%06d", id) }

// Proforma is one proforma as stored.
type Proforma struct {
	ID              int
	SubscriberID    int
	Username        string
	MobileNumber    string
	RegisteredState string
	Description     string
	Base            decimal.Decimal
	CGST            decimal.Decimal
	SGST            decimal.Decimal
	IGST            decimal.Decimal
	Total           decimal.Decimal
	CGSTRate        decimal.Decimal
	SGSTRate        decimal.Decimal
	IGSTRate        decimal.Decimal
	ValidUntil      time.Time
	Status          string
	CreatedBy       string
	CreatedAt       time.Time
	InvoiceID       *int
	ConvertedAt     *time.Time
	ConvertedBy     string
	CancelledAt     *time.Time
	CancelledBy     string
	CancelReason    string
}

// Number is the proforma's document number.
func (p Proforma) Number() string { return ProformaNumber(p.ID) }

// Tax is the GST estimated on the proforma.
func (p Proforma) Tax() decimal.Decimal { return p.CGST.Add(p.SGST).Add(p.IGST) }

// ConvertedProforma is the result of converting: the tax invoice written
// and the wallet balance it left.
type ConvertedProforma struct {
	ProformaID     int
	InvoiceID      int
	Invoice        Invoice
	WalletLedgerID int
	NewBalance     decimal.Decimal
}

// ProformaFilter narrows a proforma list.
type ProformaFilter struct {
	Status       string // "" for any
	SubscriberID int    // 0 for any
	Limit        int
}
