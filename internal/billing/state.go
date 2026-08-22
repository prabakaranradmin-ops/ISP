package billing

import "strings"

// Indian GST state codes — FR-BIL-006 | CRD §1.3.
//
// Every GSTIN begins with the two-digit code of the state that issued it,
// and GSTR-1 reports supplies by that code. This file is the single place
// those codes are written down, so a state cannot be spelled one way when
// tax is calculated and another when it is filed.
//
// It exists because registered_state was free text validated only as
// non-empty, and CalculateGstInvoice compared it to the literal "TN". A
// Tamil Nadu subscriber recorded as "Tamil Nadu" rather than "TN" was
// therefore charged IGST instead of CGST+SGST. The invoice total is
// identical either way (18% either as 9+9 or as 18), so nothing looked
// wrong - but IGST accrues entirely to the centre while CGST/SGST splits
// with the state, which makes it the wrong tax filed under the wrong head.
//
// The canonical stored form remains the two-letter code the column has
// always held; the numeric GST code is derived here for filing rather than
// stored, so existing rows keep their meaning.

// State is one entry in the GST state registry.
type State struct {
	// Code is the two-letter form stored in subscribers.registered_state.
	Code string
	// GSTCode is the two-digit numeric code GSTIN and GSTR-1 use.
	GSTCode string
	Name    string
}

// gstStates is the statutory list. Ordered by GST code for reviewability
// against the published schedule rather than alphabetically.
//
// Two historical notes, because both are live traps when reconciling older
// data:
//   - 28 was Andhra Pradesh before the 2014 bifurcation. New registrations
//     use 37 for Andhra Pradesh and 36 for Telangana; 28 is retained here
//     so a pre-bifurcation record still resolves rather than failing
//     validation, but it is not offered as a current choice.
//   - 25 (Daman and Diu) merged into 26 in 2020, which is now Dadra and
//     Nagar Haveli and Daman and Diu. 25 is retained for the same reason.
var gstStates = []State{
	{"JK", "01", "Jammu and Kashmir"},
	{"HP", "02", "Himachal Pradesh"},
	{"PB", "03", "Punjab"},
	{"CH", "04", "Chandigarh"},
	{"UK", "05", "Uttarakhand"},
	{"HR", "06", "Haryana"},
	{"DL", "07", "Delhi"},
	{"RJ", "08", "Rajasthan"},
	{"UP", "09", "Uttar Pradesh"},
	{"BR", "10", "Bihar"},
	{"SK", "11", "Sikkim"},
	{"AR", "12", "Arunachal Pradesh"},
	{"NL", "13", "Nagaland"},
	{"MN", "14", "Manipur"},
	{"MZ", "15", "Mizoram"},
	{"TR", "16", "Tripura"},
	{"ML", "17", "Meghalaya"},
	{"AS", "18", "Assam"},
	{"WB", "19", "West Bengal"},
	{"JH", "20", "Jharkhand"},
	{"OD", "21", "Odisha"},
	{"CG", "22", "Chhattisgarh"},
	{"MP", "23", "Madhya Pradesh"},
	{"GJ", "24", "Gujarat"},
	{"DD", "25", "Daman and Diu"}, // merged into 26 in 2020; see note above
	{"DN", "26", "Dadra and Nagar Haveli and Daman and Diu"},
	{"MH", "27", "Maharashtra"},
	{"KA", "29", "Karnataka"},
	{"GA", "30", "Goa"},
	{"LD", "31", "Lakshadweep"},
	{"KL", "32", "Kerala"},
	{"TN", "33", "Tamil Nadu"},
	{"PY", "34", "Puducherry"},
	{"AN", "35", "Andaman and Nicobar Islands"},
	{"TS", "36", "Telangana"},
	{"AP", "37", "Andhra Pradesh"},
	{"LA", "38", "Ladakh"},
	{"OT", "97", "Other Territory"},
}

// legacyGSTCodes are codes that still appear in historical records but are
// not issued for new registrations. Resolvable, never suggested.
var legacyGSTCodes = map[string]string{
	"28": "AP", // pre-bifurcation Andhra Pradesh
}

// Lookup tables, built once. Names are matched case-insensitively and with
// spaces removed, so "Tamil Nadu", "tamilnadu" and "TAMIL NADU" all resolve
// to the same state - the point of this package is that a human typing a
// state name cannot change which tax is charged.
var (
	byCode    = map[string]State{}
	byGSTCode = map[string]State{}
	byName    = map[string]State{}
)

func init() {
	for _, s := range gstStates {
		byCode[s.Code] = s
		byGSTCode[s.GSTCode] = s
		byName[normaliseKey(s.Name)] = s
	}
	for legacy, code := range legacyGSTCodes {
		if s, ok := byCode[code]; ok {
			byGSTCode[legacy] = s
		}
	}
}

func normaliseKey(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

// NormaliseState resolves any reasonable spelling of an Indian state to its
// canonical two-letter code, reporting whether it was recognised.
//
// Accepts the two-letter code, the numeric GST code, or the full name, in
// any case and with or without internal spaces. Rejects everything else
// rather than guessing: a state that cannot be resolved must fail loudly at
// the point of entry, because the alternative is the silent mis-taxation
// this whole file exists to prevent.
func NormaliseState(input string) (string, bool) {
	key := normaliseKey(input)
	if key == "" {
		return "", false
	}
	if s, ok := byCode[key]; ok {
		return s.Code, true
	}
	if s, ok := byGSTCode[key]; ok {
		return s.Code, true
	}
	if s, ok := byName[key]; ok {
		return s.Code, true
	}
	return "", false
}

// GSTCodeFor returns the two-digit filing code for a canonical state code.
func GSTCodeFor(stateCode string) (string, bool) {
	s, ok := byCode[strings.ToUpper(strings.TrimSpace(stateCode))]
	if !ok {
		return "", false
	}
	return s.GSTCode, true
}

// StateName returns the full name for a canonical state code.
func StateName(stateCode string) (string, bool) {
	s, ok := byCode[strings.ToUpper(strings.TrimSpace(stateCode))]
	if !ok {
		return "", false
	}
	return s.Name, true
}

// States returns the registry, for a form that offers a choice rather than
// a free-text box.
func States() []State {
	out := make([]State, len(gstStates))
	copy(out, gstStates)
	return out
}
