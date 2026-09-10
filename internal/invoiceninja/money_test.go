package invoiceninja

import "testing"

// TestAmountIsExact is the reason this type exists: 9.00 EUR at 21% inclusive
// VAT is 7.44 + 1.56, and that only reconciles if the gross reaches the
// billing system as the digits the customer was charged.
func TestAmountIsExact(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{900, "9.00"},
		{2900, "29.00"},
		{7, "0.07"},
		{100, "1.00"},
		{99, "0.99"},
		{123456, "1234.56"},
		{0, "0.00"},
		{-450, "-4.50"},
	}
	for _, c := range cases {
		if got := amount(c.cents).String(); got != c.want {
			t.Errorf("amount(%d) = %q, want %q", c.cents, got, c.want)
		}
		b, err := amount(c.cents).MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%d): %v", c.cents, err)
		}
		if string(b) != c.want {
			t.Errorf("amount(%d) JSON = %s, want %s (unquoted number)", c.cents, b, c.want)
		}
	}
}

func TestParseAmount(t *testing.T) {
	ok := map[string]int64{
		"9.00":   900,
		"9":      900,
		"9.0":    900,
		"29.00":  2900,
		"0.07":   7,
		"1,50":   150, // decimal comma
		" 9.00 ": 900,
		"-4.50":  -450,
		"+3.00":  300,
	}
	for in, want := range ok {
		got, err := ParseAmount(in)
		if err != nil {
			t.Errorf("ParseAmount(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseAmount(%q) = %d, want %d", in, got, want)
		}
	}

	// Refused rather than guessed at. A thousands separator or a third decimal
	// place means this code does not understand the payload, and a payment it
	// does not understand should wait for a person.
	for _, in := range []string{"", "abc", "1,234.50", "9.001", "9.", ".5", "9 00", "1.2.3"} {
		if v, err := ParseAmount(in); err == nil {
			t.Errorf("ParseAmount(%q) accepted, got %d; want a refusal", in, v)
		}
	}
}
