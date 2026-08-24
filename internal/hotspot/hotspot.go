// Package hotspot implements the captive portal — the walled-garden pages a
// walk-up user sees before they have any network access, and the endpoints
// that turn a voucher code or a subscriber login into a hotspot_grants row.
//
// FR: FR-HSP-001 | migration 034 | MDS §4.23.
//
// The grant is the whole product of this package. It does not put anyone
// online by itself: the NAS still authenticates the MAC over RADIUS, and
// db.HotspotStore.AuthorizeMAC (FR-HSP-002) is what finds the grant and
// answers Access-Accept. That indirection is deliberate — the captive portal
// runs on the public side of the network and must never be able to admit a
// session on its own say-so.
package hotspot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// ── Voucher codes ───────────────────────────────────────────────────────────

// codeAlphabet is Crockford-style base32: no 0/O or 1/I/L, because these codes
// are printed on paper and typed by someone standing at a counter. A voucher
// that fails because the user read a zero as an O is indistinguishable, to
// them, from one that was never valid.
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	// codeSecretLen is the number of random characters in a code. 12 characters
	// over a 30-symbol alphabet is ~58 bits. The first group is stored in the
	// clear as code_prefix so an operator can identify a voucher in a listing
	// without holding the code itself, which leaves ~39 bits of residual secret
	// against someone who has seen that listing — brute-forceable in principle,
	// which is exactly why redemption is rate limited (see AttemptLimiter) and
	// why a longer code was not the answer: nobody types 24 characters
	// correctly.
	codeSecretLen = 12
	codeGroupLen  = 4
	codeLabel     = "HS"
)

// GeneratedVoucher is one freshly minted code. Plaintext exists only in this
// struct and in the HTTP response that returns it; storage keeps the hash.
type GeneratedVoucher struct {
	Plaintext string
	Prefix    string
	Hash      string
}

// GenerateCode mints one voucher code as HS-XXXX-XXXX-XXXX.
func GenerateCode() (*GeneratedVoucher, error) {
	chars := make([]byte, codeSecretLen)
	limit := big.NewInt(int64(len(codeAlphabet)))
	for i := range chars {
		// crypto/rand.Int rather than a modulo of a random byte: 256 is not a
		// multiple of 30, so the naive version would make the first six symbols
		// of the alphabet measurably likelier than the rest.
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, fmt.Errorf("hotspot: generate voucher code: %w", err)
		}
		chars[i] = codeAlphabet[n.Int64()]
	}

	var b strings.Builder
	b.WriteString(codeLabel)
	for i := 0; i < codeSecretLen; i += codeGroupLen {
		b.WriteByte('-')
		b.Write(chars[i : i+codeGroupLen])
	}
	plaintext := b.String()

	return &GeneratedVoucher{
		Plaintext: plaintext,
		Prefix:    codeLabel + "-" + string(chars[:codeGroupLen]),
		Hash:      HashCode(plaintext),
	}, nil
}

// HashCode returns the stored representation of a voucher code.
//
// SHA-256, matching partner.HashKey and for the same reason: this is CSPRNG
// output, not a human-chosen password, so a work factor buys nothing. It also
// has to be deterministic and unsalted — redemption looks the voucher up by
// code_hash under a unique index, and a per-row salt would turn that lookup
// into a full-table scan comparing every candidate.
func HashCode(plaintext string) string {
	sum := sha256.Sum256([]byte(NormaliseCode(plaintext)))
	return hex.EncodeToString(sum[:])
}

// NormaliseCode canonicalises a typed code before hashing.
//
// Dashes and spaces are cosmetic grouping, and case is not meaningful in the
// alphabet above, so "hs xxxx xxxx xxxx" and "HS-XXXX-XXXX-XXXX" must hash the
// same. Without this the printed grouping becomes load-bearing and a user who
// omits the dashes is told their valid voucher is invalid.
func NormaliseCode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range strings.ToUpper(strings.TrimSpace(s)) {
		if c == '-' || c == ' ' {
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ── Domain types ────────────────────────────────────────────────────────────

// NewVoucher is one voucher to persist.
//
// A struct rather than positional arguments: the fields include two adjacent
// string pairs (CodeHash/CodePrefix, BatchRef/CreatedBy) that a positional call
// would silently accept transposed, and transposing the first pair would store
// a usable code in the clear.
type NewVoucher struct {
	CodeHash        string
	CodePrefix      string
	PlanID          int
	FranchiseID     *int
	DurationMinutes int
	// DataCapBytes is the volume allowance. 0 means unlimited.
	//
	// Enforced by QuotaScanner (migration 035), not by the FUP scanner: that one
	// finds over-quota sessions by joining subscribers, and a voucher grant has
	// no subscriber row by design (chk_grant_has_exactly_one_source, migration
	// 034). Voucher sessions are therefore metered on the grant itself by RADIUS
	// accounting and disconnected here when the cap is reached.
	DataCapBytes int64
	// ExpiresAt is the shelf life of the *unredeemed* code, distinct from
	// DurationMinutes, which is how long the session lasts once claimed. A
	// printed batch that never goes stale is a stack of free service sitting in
	// a drawer indefinitely.
	ExpiresAt *time.Time
	BatchRef  string
	CreatedBy string
	// SaleAmount is what this voucher is sold for — a decimal string, "0" for
	// a free voucher. Deliberately independent of the referenced plan's own
	// price (migration 044's own reasoning): a voucher's bundle (duration,
	// data cap) is its own product, not "one month of the plan" at the
	// plan's monthly rate, so the price a reseller actually charges for it
	// has to be named here rather than derived. Read at redemption to credit
	// FranchiseID's commission (voucher_commissions) when both this and
	// FranchiseID are set.
	SaleAmount string
}

// Voucher is a stored voucher as an operator sees it. The code itself is
// absent by construction — only CodePrefix is recoverable after generation.
type Voucher struct {
	ID              int        `json:"id"`
	CodePrefix      string     `json:"code_prefix"`
	PlanID          int        `json:"plan_id"`
	FranchiseID     *int       `json:"franchise_id,omitempty"`
	DurationMinutes int        `json:"duration_minutes"`
	DataCapBytes    int64      `json:"data_cap_bytes"`
	Status          string     `json:"status"`
	RedeemedByMAC   string     `json:"redeemed_by_mac,omitempty"`
	RedeemedAt      *time.Time `json:"redeemed_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
	BatchRef        string     `json:"batch_ref,omitempty"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	SaleAmount      string     `json:"sale_amount"`
}

// VoucherCommission is one voucher redemption's credited reseller
// commission — the settlement record CRD-EXP-010 asks for, held in its own
// table (voucher_commissions, migration 044) rather than lco_ledger; see
// that migration's own comment on why.
type VoucherCommission struct {
	ID                int       `json:"id"`
	FranchiseID       int       `json:"franchise_id"`
	VoucherID         int       `json:"voucher_id"`
	SaleAmount        string    `json:"sale_amount"`
	CommissionRatePct string    `json:"commission_rate_pct"`
	CommissionAmount  string    `json:"commission_amount"`
	CreatedAt         time.Time `json:"created_at"`
}

// VoucherCommissionSummary aggregates a franchise's voucher-sourced earnings
// — the counterpart to revenue.FranchisePnL, which only covers subscription
// recharges via lco_ledger.
type VoucherCommissionSummary struct {
	FranchiseID     int    `json:"franchise_id"`
	VoucherCount    int    `json:"voucher_count"`
	TotalSales      string `json:"total_sales"`
	TotalCommission string `json:"total_commission"`
}

// VoucherFilter narrows a voucher listing.
type VoucherFilter struct {
	BatchRef string
	Status   string
	Limit    int
}

// ── Store interfaces ────────────────────────────────────────────────────────

// GrantStore is the persistence surface the captive portal itself needs — the
// two ways a walk-up user becomes authorised, and nothing else. Voucher
// creation and device registration are staff operations and live behind
// api.HotspotQuerier instead, so a public-facing handler has no route to them
// even by mistake.
//
// Satisfied by *db.HotspotStore.
type GrantStore interface {
	// RedeemVoucher claims a voucher for a MAC and opens a grant atomically,
	// returning 0 when the claim does not land for any reason.
	RedeemVoucher(ctx context.Context, codeHash, mac string, nasID *int) (int64, error)
	// GrantForSubscriber opens a grant for an authenticated subscriber,
	// returning 0 when their status forbids it.
	GrantForSubscriber(ctx context.Context, mac string, subscriberID int, nasID *int, minutes int) (int64, error)
}

// AttemptLimiter bounds how often one client may guess. Allow reports whether
// the attempt may proceed.
//
// The guarantee against actually guessing a code is the size of the code space
// (see codeSecretLen); this bounds the abuse and makes an attempt surge visible.
// See clientKey in portal.go for why the two are not the same thing.
type AttemptLimiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}
