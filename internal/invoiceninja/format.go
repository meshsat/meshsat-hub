package invoiceninja

import (
	"fmt"
	"strings"
)

// Rendering money the way the document does.
//
// A customer receives the billing system's receipt and the Hub's own notice
// about the same payment, minutes apart. The receipt said "EUR 9,00" and the
// Hub said "9.00 EUR" -- the same money, written two ways, which is the same
// class of mismatch as the two different email designs this file's neighbours
// were written to fix.
//
// So FormatMoney mirrors Invoice Ninja's Number::formatMoney, read out of the
// running 5.13.31 rather than guessed at. The rule, verbatim in its order:
//
//	thousand, decimal, precision, swap all start from the CURRENCY
//	country.thousand_separator overrides when it has any length
//	country.decimal_separator  overrides when it has any length
//	country.swap_currency_symbol forces swap ON -- it never turns it back off
//	show_currency_code && CHF -> "CHF 9,00"
//	show_currency_code        -> "9,00 EUR"
//	swap                      -> "9,00 €"      (symbol trimmed, one space)
//	otherwise                 -> "€9,00"       (negative: "-€9,00")
//
// The country is the CLIENT's, not ours, so a German customer's document reads
// "9,00 €" and an Irish one's "€9.00" for the same nine euro. Getting that from
// the country on the receipt is the whole reason this takes one.

type currency struct {
	symbol    string
	precision int
	thousand  string
	decimal   string
	swap      bool
}

type countryOverride struct {
	thousand string
	decimal  string
	swap     bool
}

// FormatMoney renders minor units as the billing system would render them on
// the document, given the currency and the buyer's ISO-3166-1 alpha-2 country.
//
// It falls back to an unambiguous "9.00 EUR" for anything it cannot match
// exactly: an unknown currency, or one whose minor unit is not a hundredth.
// Amounts are stored as hundredths whatever the currency (see ParseAmount), so
// rendering a 0-decimal currency through this rule would mean rounding, and
// quietly rounding money is worse than plainly not matching the house style.
func FormatMoney(cents int64, code, country string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	c, ok := currencies[code]
	if !ok || c.precision != 2 {
		return plainMoney(cents) + " " + code
	}

	thousand, decimal, swap := c.thousand, c.decimal, c.swap
	if o, ok := countryOverrides[strings.ToUpper(strings.TrimSpace(country))]; ok {
		if len(o.thousand) >= 1 {
			thousand = o.thousand
		}
		if len(o.decimal) >= 1 {
			decimal = o.decimal
		}
		// Only ever ON. A country whose flag is false leaves the currency's.
		if o.swap {
			swap = true
		}
	}

	neg := cents < 0
	if neg {
		cents = -cents
	}
	value := group(cents/100, thousand) + decimal + fmt.Sprintf("%02d", cents%100)

	symbol := strings.TrimSpace(c.symbol)
	if swap {
		if neg {
			return "-" + value + " " + symbol
		}
		return value + " " + symbol
	}
	// The minus goes ahead of the symbol, not between it and the digits.
	if neg {
		return "-" + symbol + value
	}
	return symbol + value
}

// group inserts the thousands separator, which may be empty.
func group(whole int64, sep string) string {
	s := fmt.Sprintf("%d", whole)
	if sep == "" || len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteString(sep)
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// plainMoney is the unambiguous form: no symbol, a full stop, no grouping. It
// is what an operator reads in a log line, an audit entry or an API error,
// where a euro sign and a locale's comma help nobody.
func plainMoney(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// currencies is Invoice Ninja's own currency table, for the currencies the
// payment processor can actually send. Generated from GET /api/v1/statics on
// the live instance; regenerate from there rather than editing by hand.
var currencies = map[string]currency{
	"AUD": {symbol: "$", precision: 2, thousand: ",", decimal: ".", swap: false},
	"BRL": {symbol: "R$", precision: 2, thousand: ".", decimal: ",", swap: false},
	"CAD": {symbol: "$", precision: 2, thousand: ",", decimal: ".", swap: false},
	"CHF": {symbol: "CHF", precision: 2, thousand: "'", decimal: ".", swap: false},
	"CZK": {symbol: "K\u010d", precision: 2, thousand: " ", decimal: ",", swap: true},
	"DKK": {symbol: "kr", precision: 2, thousand: ".", decimal: ",", swap: true},
	"EUR": {symbol: "\u20ac", precision: 2, thousand: ".", decimal: ",", swap: false},
	"GBP": {symbol: "\u00a3", precision: 2, thousand: ",", decimal: ".", swap: false},
	"HKD": {symbol: "", precision: 2, thousand: ",", decimal: ".", swap: false},
	"HUF": {symbol: "Ft", precision: 0, thousand: ".", decimal: ",", swap: true},
	"ILS": {symbol: "NIS ", precision: 2, thousand: ",", decimal: ".", swap: false},
	"INR": {symbol: "\u20b9", precision: 2, thousand: ",", decimal: ".", swap: false},
	"JPY": {symbol: "\u00a5", precision: 0, thousand: ",", decimal: ".", swap: false},
	"MXN": {symbol: "$", precision: 2, thousand: ",", decimal: ".", swap: false},
	"MYR": {symbol: "RM", precision: 2, thousand: ",", decimal: ".", swap: false},
	"NOK": {symbol: "kr", precision: 2, thousand: ".", decimal: ",", swap: true},
	"NZD": {symbol: "$", precision: 2, thousand: ",", decimal: ".", swap: false},
	"PHP": {symbol: "P ", precision: 2, thousand: ",", decimal: ".", swap: false},
	"PLN": {symbol: "z\u0142", precision: 2, thousand: " ", decimal: ",", swap: true},
	"RUB": {symbol: "", precision: 2, thousand: ",", decimal: ".", swap: false},
	"SEK": {symbol: "kr", precision: 2, thousand: ".", decimal: ",", swap: true},
	"SGD": {symbol: "", precision: 2, thousand: ",", decimal: ".", swap: false},
	"THB": {symbol: "\u0e3f", precision: 2, thousand: ",", decimal: ".", swap: false},
	"TWD": {symbol: "NT$", precision: 2, thousand: ",", decimal: ".", swap: false},
	"USD": {symbol: "$", precision: 2, thousand: ",", decimal: ".", swap: false},
	"ZAR": {symbol: "R", precision: 2, thousand: ",", decimal: ".", swap: false},
}

// countryOverrides carries the countries that override the currency's own
// separators or force the symbol to the right. Every other country leaves the
// currency's values alone, which is why only 26 of the 249 are here.
var countryOverrides = map[string]countryOverride{
	"AT": {thousand: "", decimal: "", swap: true},    // Austria
	"BG": {thousand: "", decimal: "", swap: true},    // Bulgaria
	"CA": {thousand: ",", decimal: ".", swap: false}, // Canada
	"CZ": {thousand: "", decimal: "", swap: true},    // Czech Republic
	"DE": {thousand: "", decimal: "", swap: true},    // Germany
	"EE": {thousand: " ", decimal: "", swap: true},   // Estonia
	"ES": {thousand: "", decimal: "", swap: true},    // Spain
	"FI": {thousand: "", decimal: "", swap: true},    // Finland
	"FR": {thousand: " ", decimal: "", swap: true},   // France
	"GR": {thousand: "", decimal: "", swap: true},    // Greece
	"HR": {thousand: "", decimal: "", swap: true},    // Croatia
	"HU": {thousand: "", decimal: "", swap: true},    // Hungary
	"IE": {thousand: ",", decimal: ".", swap: false}, // Ireland
	"IS": {thousand: "", decimal: "", swap: true},    // Iceland
	"IT": {thousand: "", decimal: "", swap: true},    // Italy
	"JP": {thousand: "", decimal: "", swap: true},    // Japan
	"LT": {thousand: "", decimal: "", swap: true},    // Lithuania
	"MT": {thousand: ",", decimal: ".", swap: false}, // Malta
	"PL": {thousand: "", decimal: "", swap: true},    // Poland
	"PT": {thousand: "", decimal: "", swap: true},    // Portugal
	"RO": {thousand: "", decimal: "", swap: true},    // Romania
	"SE": {thousand: "", decimal: "", swap: true},    // Sweden
	"SI": {thousand: "", decimal: "", swap: true},    // Slovenia
	"SK": {thousand: "", decimal: "", swap: true},    // Slovakia
	"SR": {thousand: "", decimal: "", swap: true},    // Suriname
	"US": {thousand: ",", decimal: ".", swap: false}, // United States
}
