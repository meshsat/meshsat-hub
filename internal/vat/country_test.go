package vat

import "testing"

// What people typed on the enrolment form, and what the billing column can
// hold. The first two rows are the two real customers MESHSAT-1365 locked out.
func TestAlpha2RecognisesWhatPeopleType(t *testing.T) {
	cases := map[string]string{
		"USA":                        "US",
		"United States":              "US",
		"United States of America":   "US",
		"U.S.A.":                     "US",
		"us":                         "US",
		"Germany":                    "DE",
		"de":                         "DE",
		"DEU":                        "DE",
		"Deutschland":                "DE",
		"Netherlands":                "NL",
		"The Netherlands":            "NL",
		"Holland":                    "NL",
		"Netherlands (NL)":           "NL",
		"Nederland (nl)":             "NL",
		"  nl ":                      "NL",
		"UK":                         "GB",
		"United Kingdom":             "GB",
		"England":                    "GB",
		"Greece":                     "GR",
		"EL":                         "GR",
		"Czech Republic":             "CZ",
		"Czechia":                    "CZ",
		"South Korea":                "KR",
		"Ivory Coast":                "CI",
		"Côte d'Ivoire":              "CI",
		"Cote d'Ivoire":              "CI",
		"Türkiye":                    "TR",
		"Turkey":                     "TR",
		"Russia":                     "RU",
		"Vietnam":                    "VN",
		"Norway":                     "NO",
		"Switzerland":                "CH",
		"Bosnia & Herzegovina":       "BA",
		"Trinidad and Tobago":        "TT",
		"united-states-of-america":   "US",
		"Congo, Democratic Republic": "",
	}
	for in, want := range cases {
		got, ok := Alpha2(in)
		if want == "" {
			if ok || got != "" {
				t.Errorf("Alpha2(%q) = %q, %v; want unrecognised", in, got, ok)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("Alpha2(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// An unrecognised country is not a guess and not a refusal: it is ("", false),
// which parks the receipt for a person and lets the sign-in through.
func TestAlpha2UnrecognisedIsEmptyNotAnError(t *testing.T) {
	for _, in := range []string{"", "   ", "Mars", "Europe", "XX", "ZZZ", "(ZZ)", "somewhere (XY)", "Ελλάδα"} {
		if got, ok := Alpha2(in); ok || got != "" {
			t.Errorf("Alpha2(%q) = %q, %v; want \"\", false", in, got, ok)
		}
	}
}

func TestIsAlpha2(t *testing.T) {
	for in, want := range map[string]bool{"US": true, "us": true, "NL": true, "EL": true, "": false, "USA": false, "U": false, "XX": false, "1A": false} {
		if got := IsAlpha2(in); got != want {
			t.Errorf("IsAlpha2(%q) = %v, want %v", in, got, want)
		}
	}
}

// Every table entry is a distinct assigned alpha-2, a distinct alpha-3, and
// resolves through every name it lists; the table itself is what the guard
// trusts.
func TestCountryTableIsConsistent(t *testing.T) {
	if n := len(countries); n != 249 {
		t.Fatalf("table has %d entries, ISO 3166-1 assigns 249", n)
	}
	seen2, seen3 := map[string]bool{}, map[string]bool{}
	for _, c := range countries {
		if len(c.a2) != 2 || len(c.a3) != 3 || len(c.names) == 0 {
			t.Errorf("malformed entry %+v", c)
		}
		if seen2[c.a2] || seen3[c.a3] {
			t.Errorf("duplicate code in %+v", c)
		}
		seen2[c.a2], seen3[c.a3] = true, true
		for _, n := range c.names {
			if got, ok := Alpha2(n); !ok || got != c.a2 {
				t.Errorf("Alpha2(%q) = %q, %v; want %s", n, got, ok, c.a2)
			}
		}
		if got, ok := Alpha2(c.a3); !ok || got != c.a2 {
			t.Errorf("Alpha2(%q) = %q, %v; want %s", c.a3, got, ok, c.a2)
		}
	}
}
