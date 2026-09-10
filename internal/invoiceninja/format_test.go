package invoiceninja

import "testing"

// The golden table is not hand-written. It was produced by running
// Number::formatMoney's own arithmetic inside the live 5.13.31 container
// against the real `currencies` and `countries` rows, for every combination
// below -- so a disagreement here is a disagreement with the document the
// customer is holding, not with somebody's reading of the docs.
//
// Regenerate with scratchpad/fmt-oracle.php if the instance is ever upgraded.
var goldenMoney = []struct {
	cents     int64
	code, iso string
	want      string
}{
	{900, "EUR", "NL", "€9,00"},
	{950, "EUR", "NL", "€9,50"},
	{123456, "EUR", "NL", "€1.234,56"},
	{123456789, "EUR", "NL", "€1.234.567,89"},
	{-900, "EUR", "NL", "-€9,00"},
	{-123450, "EUR", "NL", "-€1.234,50"},
	{0, "EUR", "NL", "€0,00"},
	{900, "EUR", "DE", "9,00 €"},
	{950, "EUR", "DE", "9,50 €"},
	{123456, "EUR", "DE", "1.234,56 €"},
	{123456789, "EUR", "DE", "1.234.567,89 €"},
	{-900, "EUR", "DE", "-9,00 €"},
	{-123450, "EUR", "DE", "-1.234,50 €"},
	{0, "EUR", "DE", "0,00 €"},
	{900, "EUR", "IE", "€9.00"},
	{950, "EUR", "IE", "€9.50"},
	{123456, "EUR", "IE", "€1,234.56"},
	{123456789, "EUR", "IE", "€1,234,567.89"},
	{-900, "EUR", "IE", "-€9.00"},
	{-123450, "EUR", "IE", "-€1,234.50"},
	{0, "EUR", "IE", "€0.00"},
	{900, "EUR", "FR", "9,00 €"},
	{950, "EUR", "FR", "9,50 €"},
	{123456, "EUR", "FR", "1 234,56 €"},
	{123456789, "EUR", "FR", "1 234 567,89 €"},
	{-900, "EUR", "FR", "-9,00 €"},
	{-123450, "EUR", "FR", "-1 234,50 €"},
	{0, "EUR", "FR", "0,00 €"},
	{900, "EUR", "MT", "€9.00"},
	{950, "EUR", "MT", "€9.50"},
	{123456, "EUR", "MT", "€1,234.56"},
	{123456789, "EUR", "MT", "€1,234,567.89"},
	{-900, "EUR", "MT", "-€9.00"},
	{-123450, "EUR", "MT", "-€1,234.50"},
	{0, "EUR", "MT", "€0.00"},
	{900, "EUR", "BE", "€9,00"},
	{950, "EUR", "BE", "€9,50"},
	{123456, "EUR", "BE", "€1.234,56"},
	{123456789, "EUR", "BE", "€1.234.567,89"},
	{-900, "EUR", "BE", "-€9,00"},
	{-123450, "EUR", "BE", "-€1.234,50"},
	{0, "EUR", "BE", "€0,00"},
	{900, "EUR", "EE", "9,00 €"},
	{950, "EUR", "EE", "9,50 €"},
	{123456, "EUR", "EE", "1 234,56 €"},
	{123456789, "EUR", "EE", "1 234 567,89 €"},
	{-900, "EUR", "EE", "-9,00 €"},
	{-123450, "EUR", "EE", "-1 234,50 €"},
	{0, "EUR", "EE", "0,00 €"},
	{900, "EUR", "ES", "9,00 €"},
	{950, "EUR", "ES", "9,50 €"},
	{123456, "EUR", "ES", "1.234,56 €"},
	{123456789, "EUR", "ES", "1.234.567,89 €"},
	{-900, "EUR", "ES", "-9,00 €"},
	{-123450, "EUR", "ES", "-1.234,50 €"},
	{0, "EUR", "ES", "0,00 €"},
	{900, "EUR", "IT", "9,00 €"},
	{950, "EUR", "IT", "9,50 €"},
	{123456, "EUR", "IT", "1.234,56 €"},
	{123456789, "EUR", "IT", "1.234.567,89 €"},
	{-900, "EUR", "IT", "-9,00 €"},
	{-123450, "EUR", "IT", "-1.234,50 €"},
	{0, "EUR", "IT", "0,00 €"},
	{900, "EUR", "AT", "9,00 €"},
	{950, "EUR", "AT", "9,50 €"},
	{123456, "EUR", "AT", "1.234,56 €"},
	{123456789, "EUR", "AT", "1.234.567,89 €"},
	{-900, "EUR", "AT", "-9,00 €"},
	{-123450, "EUR", "AT", "-1.234,50 €"},
	{0, "EUR", "AT", "0,00 €"},
	{900, "EUR", "LU", "€9,00"},
	{950, "EUR", "LU", "€9,50"},
	{123456, "EUR", "LU", "€1.234,56"},
	{123456789, "EUR", "LU", "€1.234.567,89"},
	{-900, "EUR", "LU", "-€9,00"},
	{-123450, "EUR", "LU", "-€1.234,50"},
	{0, "EUR", "LU", "€0,00"},
	{900, "EUR", "CY", "€9,00"},
	{950, "EUR", "CY", "€9,50"},
	{123456, "EUR", "CY", "€1.234,56"},
	{123456789, "EUR", "CY", "€1.234.567,89"},
	{-900, "EUR", "CY", "-€9,00"},
	{-123450, "EUR", "CY", "-€1.234,50"},
	{0, "EUR", "CY", "€0,00"},
	{900, "EUR", "LV", "€9,00"},
	{950, "EUR", "LV", "€9,50"},
	{123456, "EUR", "LV", "€1.234,56"},
	{123456789, "EUR", "LV", "€1.234.567,89"},
	{-900, "EUR", "LV", "-€9,00"},
	{-123450, "EUR", "LV", "-€1.234,50"},
	{0, "EUR", "LV", "€0,00"},
	{900, "EUR", "ZZ", "€9,00"},
	{950, "EUR", "ZZ", "€9,50"},
	{123456, "EUR", "ZZ", "€1.234,56"},
	{123456789, "EUR", "ZZ", "€1.234.567,89"},
	{-900, "EUR", "ZZ", "-€9,00"},
	{-123450, "EUR", "ZZ", "-€1.234,50"},
	{0, "EUR", "ZZ", "€0,00"},
	{900, "USD", "US", "$9.00"},
	{950, "USD", "US", "$9.50"},
	{123456, "USD", "US", "$1,234.56"},
	{123456789, "USD", "US", "$1,234,567.89"},
	{-900, "USD", "US", "-$9.00"},
	{-123450, "USD", "US", "-$1,234.50"},
	{0, "USD", "US", "$0.00"},
	{900, "GBP", "GB", "£9.00"},
	{950, "GBP", "GB", "£9.50"},
	{123456, "GBP", "GB", "£1,234.56"},
	{123456789, "GBP", "GB", "£1,234,567.89"},
	{-900, "GBP", "GB", "-£9.00"},
	{-123450, "GBP", "GB", "-£1,234.50"},
	{0, "GBP", "GB", "£0.00"},
	{900, "CHF", "CH", "CHF9.00"},
	{950, "CHF", "CH", "CHF9.50"},
	{123456, "CHF", "CH", "CHF1'234.56"},
	{123456789, "CHF", "CH", "CHF1'234'567.89"},
	{-900, "CHF", "CH", "-CHF9.00"},
	{-123450, "CHF", "CH", "-CHF1'234.50"},
	{0, "CHF", "CH", "CHF0.00"},
	{900, "SEK", "SE", "9,00 kr"},
	{950, "SEK", "SE", "9,50 kr"},
	{123456, "SEK", "SE", "1.234,56 kr"},
	{123456789, "SEK", "SE", "1.234.567,89 kr"},
	{-900, "SEK", "SE", "-9,00 kr"},
	{-123450, "SEK", "SE", "-1.234,50 kr"},
	{0, "SEK", "SE", "0,00 kr"},
	{900, "PLN", "PL", "9,00 zł"},
	{950, "PLN", "PL", "9,50 zł"},
	{123456, "PLN", "PL", "1 234,56 zł"},
	{123456789, "PLN", "PL", "1 234 567,89 zł"},
	{-900, "PLN", "PL", "-9,00 zł"},
	{-123450, "PLN", "PL", "-1 234,50 zł"},
	{0, "PLN", "PL", "0,00 zł"},
	{900, "DKK", "DK", "9,00 kr"},
	{950, "DKK", "DK", "9,50 kr"},
	{123456, "DKK", "DK", "1.234,56 kr"},
	{123456789, "DKK", "DK", "1.234.567,89 kr"},
	{-900, "DKK", "DK", "-9,00 kr"},
	{-123450, "DKK", "DK", "-1.234,50 kr"},
	{0, "DKK", "DK", "0,00 kr"},
}

func TestFormatMoneyMatchesTheBillingSystem(t *testing.T) {
	for _, tc := range goldenMoney {
		if got := FormatMoney(tc.cents, tc.code, tc.iso); got != tc.want {
			t.Errorf("FormatMoney(%d, %q, %q) = %q, the document says %q",
				tc.cents, tc.code, tc.iso, got, tc.want)
		}
	}
}

// The two shapes that matter most, called out by name so a regression in
// either is readable in the test output rather than one row of 140.
func TestTheDutchAndGermanRenderingsAreDifferentOnPurpose(t *testing.T) {
	if got := FormatMoney(900, "EUR", "NL"); got != "€9,00" {
		t.Errorf("a Dutch buyer's nine euro = %q, want the symbol first", got)
	}
	if got := FormatMoney(900, "EUR", "DE"); got != "9,00 €" {
		t.Errorf("a German buyer's nine euro = %q, want the symbol last", got)
	}
	if got := FormatMoney(900, "EUR", "IE"); got != "€9.00" {
		t.Errorf("an Irish buyer's nine euro = %q, want a full stop", got)
	}
}

// A country's swap flag only ever turns the symbol around; it never turns it
// back. Belgium's is false and it must not undo anything.
func TestACountryNeverUnsetsTheCurrencysSwap(t *testing.T) {
	if got := FormatMoney(900, "EUR", "BE"); got != "€9,00" {
		t.Errorf("Belgium = %q", got)
	}
	// An unknown country leaves the currency's own values alone.
	if got := FormatMoney(900, "EUR", "ZZ"); got != "€9,00" {
		t.Errorf("unknown country = %q", got)
	}
	if got := FormatMoney(900, "EUR", ""); got != "€9,00" {
		t.Errorf("no country = %q", got)
	}
}

// Anything this cannot match exactly falls back to a form nobody can misread.
// Quietly inventing a symbol, or rounding a 0-decimal currency stored in
// hundredths, would put a wrong figure in front of a customer.
func TestUnmatchableCurrenciesFallBackToThePlainForm(t *testing.T) {
	for _, tc := range []struct {
		cents      int64
		code, want string
	}{
		{900, "XYZ", "9.00 XYZ"},     // not a currency this knows
		{50000, "JPY", "500.00 JPY"}, // known, but its minor unit is not a hundredth
		{-900, "XYZ", "-9.00 XYZ"},
	} {
		if got := FormatMoney(tc.cents, tc.code, "NL"); got != tc.want {
			t.Errorf("FormatMoney(%d, %q) = %q, want %q", tc.cents, tc.code, got, tc.want)
		}
	}
}

func TestFormatMoneyIsCaseAndSpaceInsensitive(t *testing.T) {
	if got := FormatMoney(900, " eur ", " nl "); got != "€9,00" {
		t.Errorf("got %q; a currency or country with stray case or space still has to render", got)
	}
}
