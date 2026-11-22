// Package vendors implements the supplier master that inventory and
// procurement share — FR-INV-004 | migration 071.
//
// Before this, three columns held a vendor as free text (cpe_device_types,
// cpe_purchases, purchase_orders), so the same supplier spelled two ways was
// two suppliers to every report and a GSTIN had nowhere to live. One row per
// supplier, referenced by id, with the contact details and registration
// number that a purchase invoice actually needs.
//
// Its own package rather than a file in internal/inventory or
// internal/procurement: both of those import it, and putting it in either one
// would make the other depend on a package it has nothing else to do with.
package vendors

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Field limits, in characters. Each matches its column in migration 071, and
// is counted in runes rather than bytes for the reason FR-GRP-001 found the
// hard way: VARCHAR(n) counts characters, len(string) counts bytes, and a
// Tamil or Devanagari vendor name is three bytes a character — so a
// byte-counted limit refuses names a third of the length the column accepts.
const (
	MaxName    = 200
	MaxContact = 200
	MaxPhone   = 30
	MaxEmail   = 255
)

var (
	// ErrNameRequired is returned for a vendor with no name. A supplier that
	// cannot be named cannot be paid.
	ErrNameRequired = errors.New("vendors: a vendor name is required")
	// ErrNameTooLong and the three limit errors below name the field, because
	// one "too long" on a form with five fields tells the operator nothing.
	ErrNameTooLong    = errors.New("vendors: the vendor name is too long")
	ErrContactTooLong = errors.New("vendors: the contact person's name is too long")
	ErrPhoneTooLong   = errors.New("vendors: the phone number is too long")
	ErrEmailTooLong   = errors.New("vendors: the email address is too long")

	// ErrGSTINFormat is returned for a GSTIN that is not 15 characters in the
	// shape the GST registry issues.
	ErrGSTINFormat = errors.New("vendors: a GSTIN is 15 characters: 2 digits, 5 letters, 4 digits, a letter, a letter or digit, Z, then a letter or digit")
	// ErrGSTINChecksum is returned for a well-shaped GSTIN whose last
	// character does not match the checksum of the other fourteen — which
	// means a character was mistyped or transposed. Separate from
	// ErrGSTINFormat so the message can say which of the two went wrong:
	// "that is not a GSTIN" and "that GSTIN has a typo in it" send the
	// operator to different places.
	ErrGSTINChecksum = errors.New("vendors: that GSTIN's check digit does not match the rest of it, so a character is mistyped")

	// ErrNameTaken is returned when another active vendor already holds this
	// name, ignoring case and runs of whitespace.
	ErrNameTaken = errors.New("vendors: another active vendor already has that name")
	// ErrGSTINTaken is returned when another active vendor already holds this
	// GSTIN. A GSTIN is one legal entity, so this is always a duplicate —
	// either of the vendor, or of somebody's typing.
	ErrGSTINTaken = errors.New("vendors: another active vendor is already registered under that GSTIN")
	// ErrNotFound is returned when no vendor has the given id.
	ErrNotFound = errors.New("vendors: no such vendor")
	// ErrRequired is returned when a purchase, device type or purchase order
	// is written without naming a vendor. Distinct from ErrNotFound: nothing
	// was chosen, as against something was chosen and does not exist, and the
	// first is a form the operator has not finished filling in.
	ErrRequired = errors.New("vendors: a vendor must be chosen from the master")
	// ErrInactive is returned when a retired vendor is named on a new
	// purchase or order. Existing records keep pointing at it; nothing new
	// may.
	ErrInactive = errors.New("vendors: that vendor has been retired and cannot be used for new purchases")
)

// gstinFormat mirrors chk_vendor_gstin_format in migration 071, which in turn
// mirrors chk_subscriber_gstin_format from migration 038 — deliberately
// identical, because a GSTIN does not change shape depending on whether its
// holder is a customer or a supplier.
var gstinFormat = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$`)

// whitespaceRun matches the internal runs of whitespace Normalise collapses,
// matching uq_vendors_name_active's own regexp_replace. The two have to agree:
// if Go stored "Anna  Nagar" while the index keyed it as "anna nagar", the
// duplicate the index exists to refuse would be stored under a name no search
// would find.
var whitespaceRun = regexp.MustCompile(`\s+`)

// Vendor is one supplier.
//
// Contact, GSTIN and address are all optional, and that is the shape the
// requirement needs rather than a shortcut: a vendor is created the moment
// somebody needs to record a purchase from them, which is reliably before
// anybody has dug their registration number out of an invoice. Demanding a
// GSTIN up front would mean either a blocked purchase or an invented GSTIN,
// and an invented one is worse than none at all — it would pass the format
// check, sit in the master looking authoritative, and be claimed as input tax
// credit against a registration that does not exist.
type Vendor struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	ContactPerson string `json:"contact_person,omitempty"`
	Phone         string `json:"phone,omitempty"`
	Email         string `json:"email,omitempty"`
	GSTIN         string `json:"gstin,omitempty"`
	Address       string `json:"address,omitempty"`
	Notes         string `json:"notes,omitempty"`
	Active        bool   `json:"active"`
}

// Usage is how much of the system is pointing at one vendor, for the master
// list. Shown because "retire this vendor" is a different decision depending
// on whether it has one stale device type behind it or four years of
// purchases, and because a vendor with no usage at all is the one a duplicate
// cleanup should merge away.
type Usage struct {
	DeviceTypes    int `json:"device_types"`
	Purchases      int `json:"purchases"`
	PurchaseOrders int `json:"purchase_orders"`
	// SpendStr is the summed cost of this vendor's CPE purchases plus the
	// amount of every purchase order that reached 'received', as a fixed-2dp
	// string — money never leaves this codebase as a float (FR-BIL-002).
	//
	// Only received orders count: a requested or rejected order is not money
	// that went anywhere, and including it would make the master's spend
	// column disagree with the ledger for every order still awaiting a
	// decision.
	SpendStr string `json:"spend"`
}

// VendorWithUsage is a master-list row.
type VendorWithUsage struct {
	Vendor
	Usage Usage `json:"usage"`
}

// Referenced reports whether anything points at this vendor, which is the
// test for whether retiring it would hide history.
func (u Usage) Referenced() bool {
	return u.DeviceTypes > 0 || u.Purchases > 0 || u.PurchaseOrders > 0
}

// Normalise trims every field and collapses internal runs of whitespace in
// the name, so "Anna  Nagar Cables" and "Anna Nagar Cables" are stored — and
// therefore refused as duplicates — as one name. Trimming alone is not
// enough: uq_vendors_name_active collapses whitespace too, and a name stored
// uncollapsed would be keyed differently from how it reads.
//
// The GSTIN is upper-cased because the registry issues it in upper case and
// the format check requires it, so an operator pasting a lower-case one from
// an email is doing nothing wrong.
func (v *Vendor) Normalise() {
	v.Name = whitespaceRun.ReplaceAllString(strings.TrimSpace(v.Name), " ")
	v.ContactPerson = whitespaceRun.ReplaceAllString(strings.TrimSpace(v.ContactPerson), " ")
	v.Phone = strings.TrimSpace(v.Phone)
	v.Email = strings.TrimSpace(v.Email)
	v.GSTIN = strings.ToUpper(strings.TrimSpace(v.GSTIN))
	v.Address = strings.TrimSpace(v.Address)
	v.Notes = strings.TrimSpace(v.Notes)
}

// Validate reports the first thing wrong with a normalised vendor. Call
// Normalise first; validating un-normalised input would measure lengths
// against whitespace the store is about to remove.
func (v Vendor) Validate() error {
	switch {
	case v.Name == "":
		return ErrNameRequired
	case utf8.RuneCountInString(v.Name) > MaxName:
		return ErrNameTooLong
	case utf8.RuneCountInString(v.ContactPerson) > MaxContact:
		return ErrContactTooLong
	case utf8.RuneCountInString(v.Phone) > MaxPhone:
		return ErrPhoneTooLong
	case utf8.RuneCountInString(v.Email) > MaxEmail:
		return ErrEmailTooLong
	}
	return ValidateGSTIN(v.GSTIN)
}

// ValidateGSTIN checks a GSTIN's shape and its check digit. An empty string
// is valid — a GSTIN is optional, see Vendor.
//
// Two layers because they catch different mistakes and only one of them can
// live in the schema. The format refuses something that is not a GSTIN at all
// (a PAN, a phone number, a truncated paste), and migration 071 enforces it
// too so nonsense is unstorable however it arrives. The checksum refuses a
// GSTIN with a character mistyped or two transposed — which the format cannot
// see, since a transposition is still fifteen characters in the right shape.
//
// Worth checking rather than trusting, because a wrong GSTIN on a purchase
// invoice is not a cosmetic error: input tax credit is matched against the
// supplier's registration, and a GSTIN that does not resolve means the credit
// is denied. The person entering it has the invoice in front of them, which
// is the only moment the mistake is cheap to fix.
func ValidateGSTIN(gstin string) error {
	if gstin == "" {
		return nil
	}
	if !gstinFormat.MatchString(gstin) {
		return ErrGSTINFormat
	}
	if gstin[14] != GSTINCheckDigit(gstin[:14]) {
		return ErrGSTINChecksum
	}
	return nil
}

// GSTINCheckDigit computes the fifteenth character of a GSTIN from the first
// fourteen, as the GST registry does.
//
// Each character is valued 0–9 for a digit and 10–35 for a letter, multiplied
// by a factor that alternates 1, 2 from the left; each product is folded by
// adding its quotient and remainder over 36; the check code is 36 minus the
// total modulo 36, taken modulo 36 again so a total that divides evenly gives
// '0' rather than a 36th character that does not exist.
//
// first14 must already have passed gstinFormat, which guarantees fourteen
// characters that are all digits or upper-case letters. TestGSTINCheckDigit
// pins the implementation against 27AAPFU0939F1ZV, the registry's own
// published example, so this is verified arithmetic rather than a transcribed
// algorithm nobody ran.
func GSTINCheckDigit(first14 string) byte {
	sum := 0
	for i := 0; i < len(first14); i++ {
		factor := 1
		if i%2 == 1 {
			factor = 2
		}
		product := gstinCodePoint(first14[i]) * factor
		sum += product/36 + product%36
	}
	code := (36 - sum%36) % 36
	if code < 10 {
		return byte('0' + code)
	}
	return byte('A' + code - 10)
}

// gstinCodePoint values one GSTIN character: 0–9 for digits, 10–35 for A–Z.
// Anything else returns 0, which only reachable for input that never passed
// gstinFormat — and 0 rather than a panic because a check digit that comes out
// wrong refuses the GSTIN, which is the safe outcome either way.
func gstinCodePoint(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	}
	return 0
}

// NormalisedKey returns the form uq_vendors_name_active keys a name under: it
// is what two names have to share to be the same vendor. Exported so a caller
// can tell a would-be duplicate apart from a genuinely new vendor without
// having to reproduce the index's expression.
func NormalisedKey(name string) string {
	return strings.ToLower(whitespaceRun.ReplaceAllString(strings.TrimSpace(name), " "))
}
