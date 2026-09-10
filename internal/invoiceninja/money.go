package invoiceninja

import (
	"errors"
	"strconv"
	"strings"
)

// Money in this package is always an integer count of minor units -- cents for
// EUR -- and never a float. A float64 cannot hold 0.07 exactly, and a rounding
// error in a VAT line is a wrong document rather than a wrong pixel.
//
// The two-decimal assumption is deliberate and narrow: the company this talks
// to invoices in EUR, and IssueReceipt refuses anything else before it gets
// here. A zero-decimal currency (JPY) or a three-decimal one (KWD) would need
// the exponent to come from the currency, not from a constant.
const minorUnits = 100

// amount renders minor units as an exact JSON decimal: 900 becomes 9.00, with
// the digits written rather than computed, so no float ever touches the wire.
type amount int64

func (a amount) MarshalJSON() ([]byte, error) { return []byte(a.String()), nil }

func (a amount) String() string {
	v, sign := int64(a), ""
	if v < 0 {
		v, sign = -v, "-"
	}
	return sign + strconv.FormatInt(v/minorUnits, 10) + "." +
		strings.TrimPrefix(strconv.FormatInt(minorUnits+v%minorUnits, 10), "1")
}

// ErrBadAmount is returned for a payload amount this code will not invoice:
// one that is not a plain decimal, or one that is not positive. Both are
// terminal -- retrying cannot turn a refund or a typo into an invoiceable sum,
// so the receipts drainer parks the row for a person rather than looping.
var ErrBadAmount = errors.New("invoiceninja: amount is not a positive decimal number")

// ParseAmount turns a payment provider's decimal string ("9.00", "29", "1,50")
// into minor units without going through a float.
//
// Providers send money as text and the obvious strconv.ParseFloat is what
// introduces the error this package exists to avoid, so the digits are read
// directly. More than two decimal places is refused rather than rounded: a
// payment this code does not understand should stop and wait for a person.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrBadAmount
	}
	// Some locales send a decimal comma. Only one separator may be present:
	// "1,234.50" is a thousands separator this refuses to guess at.
	if strings.Count(s, ",") == 1 && !strings.Contains(s, ".") {
		s = strings.Replace(s, ",", ".", 1)
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || strings.ContainsAny(s, ",+- ") {
		return 0, ErrBadAmount
	}
	if hasFrac {
		switch len(frac) {
		case 0:
			return 0, ErrBadAmount
		case 1:
			frac += "0"
		case 2:
		default:
			return 0, ErrBadAmount
		}
	} else {
		frac = "00"
	}
	w, err := strconv.ParseInt(whole, 10, 63)
	if err != nil {
		return 0, ErrBadAmount
	}
	f, err := strconv.ParseInt(frac, 10, 32)
	if err != nil {
		return 0, ErrBadAmount
	}
	total := w*minorUnits + f
	if neg {
		total = -total
	}
	return total, nil
}
