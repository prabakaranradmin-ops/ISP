package revenue

import (
	"strings"
	"testing"
)

// Named subscriber groups — FR-GRP-001.

func TestFR_GRP_001_NormaliseCollapsesWhitespaceInsideTheName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Anna Nagar", "Anna Nagar"},
		{"  Anna Nagar  ", "Anna Nagar"},
		// The one that matters: the unique index is on lower(btrim(name)),
		// which strips the ends and nothing else. Two live groups reading
		// identically to every operator is the mistake that index exists to
		// prevent, so the collapse has to happen before storage.
		{"Anna  Nagar", "Anna Nagar"},
		{"Anna\tNagar", "Anna Nagar"},
		{"Anna \n Nagar", "Anna Nagar"},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := SubscriberGroup{Name: tc.in}.Normalise().Name
		if got != tc.want {
			t.Errorf("Normalise(%q).Name = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Normalising twice changes nothing, so a stored name can be re-submitted
	// from an edit form without drifting.
	once := SubscriberGroup{Name: "Anna  Nagar", Description: " Feeder A "}.Normalise()
	if twice := once.Normalise(); twice != once {
		t.Errorf("Normalise is not idempotent: %+v then %+v", once, twice)
	}
}

func TestFR_GRP_001_ANamelessGroupIsRefused(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		g := SubscriberGroup{Name: name}.Normalise()
		if err := g.Validate(); err == nil {
			t.Errorf("a group named %q was accepted", name)
		}
	}
}

// The column is VARCHAR(100), which counts characters. A byte-length check
// would refuse a 40-character Tamil locality name the database accepts
// without complaint — and the localities groups get named after are exactly
// the text most likely not to be ASCII.
func TestFR_GRP_001_TheNameLimitIsCharactersNotBytes(t *testing.T) {
	// 40 Tamil characters: 120 bytes, well inside the column.
	tamil := strings.Repeat("அ", 40)
	if n := len(tamil); n <= 100 {
		t.Fatalf("the fixture is not long enough in bytes to prove anything (%d)", n)
	}
	if err := (SubscriberGroup{Name: tamil}).Validate(); err != nil {
		t.Errorf("a 40-character Tamil group name was refused: %v", err)
	}

	// 100 characters is the limit and 101 is over it, whatever the script.
	if err := (SubscriberGroup{Name: strings.Repeat("a", 100)}).Validate(); err != nil {
		t.Errorf("a 100-character name was refused: %v", err)
	}
	if err := (SubscriberGroup{Name: strings.Repeat("a", 101)}).Validate(); err == nil {
		t.Error("a 101-character name was accepted, so the insert would fail on the column width")
	}
	if err := (SubscriberGroup{Name: strings.Repeat("அ", 101)}).Validate(); err == nil {
		t.Error("a 101-character Tamil name was accepted")
	}
}

func TestFR_GRP_001_ADescriptionIsOptional(t *testing.T) {
	if err := (SubscriberGroup{Name: "Anna Nagar"}).Validate(); err != nil {
		t.Errorf("a group with no description was refused: %v", err)
	}
}
