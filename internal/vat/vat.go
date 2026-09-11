// Package vat decides how one payment is treated for VAT, from the buyer's
// country and nothing else.
//
// It is a rule, not a rate. The rate lives in configuration because re-pricing
// is an edit; this package answers the prior question of whether Dutch VAT
// applies at all, which configuration cannot answer.
//
// The rule, and why:
//
//   - The Netherlands is the seller's own country. Domestic supply, Dutch VAT.
//
//   - Another EU member state, consumer: the place of supply for an
//     electronically supplied service is where the customer is, so strictly the
//     customer's own rate applies. Article 59c lets a supplier established in
//     one member state charge its HOME rate while its cross-border B2C supplies
//     stay under EUR 10,000 a year across the whole EU. That threshold is the
//     entire legal basis for charging Dutch 21% to a German consumer, and it is
//     measured, not assumed -- see Threshold and internal/api's VAT endpoint.
//     If it is ever crossed, this function must start returning the customer's
//     rate and the business must register for OSS.
//
//   - Outside the EU: the place of supply is outside the EU, so no EU VAT
//     applies at all -- threshold or no threshold. Charging Dutch 21% to a
//     customer in the United States is simply wrong, and it is what this code
//     did for every buyer before this package existed. Park it for a person.
//
//   - No country on file: guessing is how somebody gets the wrong document.
//     Park it.
//
// What this package deliberately cannot see: territories inside a member state
// that are outside the EU VAT area -- the Canaries, Ceuta and Melilla, the
// French overseas departments, Aland, Busingen and Heligoland, Livigno, Mount
// Athos, Campione d'Italia. A country code cannot distinguish them. A customer
// in one of those is treated as an ordinary EU customer, which over-charges
// rather than under-charges, and is a known limit rather than an oversight.
package vat

import "strings"

// Decision is what to do about VAT for one payment.
type Decision struct {
	// Charge reports whether the receipt may be issued with the configured
	// Dutch rate. False means it must be parked for a person to look at.
	Charge bool
	// Reason is written onto a parked receipt and read by whoever opens the
	// blocked list, so it says what is wrong and what would settle it.
	Reason string
	// Basis records why VAT is charged the way it is. It goes in the log and
	// on the record, so the reasoning survives the person who made it.
	Basis string
	// OutsideScope reports that the payment carries NO VAT at all and the
	// document must still be issued.
	//
	// This is a third state, and conflating it with either of the other two
	// gets it wrong in a different direction. It is not Charge, because there
	// is no VAT to charge. It is not a parked decision -- Charge false with a
	// Reason -- because nothing is wrong and nobody needs to look: the
	// treatment is settled, not undecided.
	OutsideScope bool
}

// euVATArea is the EU-27 as ISO 3166-1 alpha-2. "EL" is accepted alongside "GR"
// because it is the VAT prefix Greece uses and it turns up in payment data.
var euVATArea = map[string]bool{
	"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true,
	"DK": true, "EE": true, "FI": true, "FR": true, "DE": true, "GR": true,
	"EL": true, "HU": true, "IE": true, "IT": true, "LV": true, "LT": true,
	"LU": true, "MT": true, "NL": true, "PL": true, "PT": true, "RO": true,
	"SK": true, "SI": true, "ES": true, "SE": true,
}

// Home is the seller's own country.
const Home = "NL"

// For decides the VAT treatment for a buyer in the given country, which is an
// ISO 3166-1 alpha-2 code or empty when it is not known.
func For(country string) Decision {
	c := strings.ToUpper(strings.TrimSpace(country))
	switch {
	case c == "":
		return Decision{
			Reason: "no country on file for this buyer, so the VAT treatment cannot be decided. " +
				"Set the tenant's billing country and requeue.",
		}
	case c == Home:
		return Decision{Charge: true, Basis: "domestic supply, Dutch VAT"}
	case euVATArea[c]:
		return Decision{Charge: true, Basis: "EU consumer, Dutch VAT under the Article 59c EUR 10 000 threshold"}
	default:
		return Decision{
			Reason: "buyer is outside the EU (" + c + "), so EU VAT does not apply and this receipt " +
				"cannot be issued at the Dutch rate. Issue it by hand, or decide the treatment first.",
		}
	}
}

// ForDonation decides the VAT treatment of a voluntary payment that buys
// nothing at all.
//
// The Belastingdienst's rule on vrijwillige bijdragen turns on counter-
// performance, and NOT on where the giver is:
//
//	"U berekent wel btw over vrijwillige bijdragen die u ontvangt als
//	 vergoeding."  -- their example is a museum with a voluntary entrance
//	 fee: there IS a supply, the visitor merely chooses the price.
//
//	"U berekent geen btw als u vrijwillige bijdragen ontvangt zonder dat er
//	 een overeenkomst is tussen u en de gever."  -- their example is a street
//	 musician.
//
// A MeshSat donation is the second kind. It grants no tier, no period and no
// feature, and that is enforced rather than promised: internal/stripe's
// onCheckout never writes a plan for a mode=payment session, and
// billing.DonationPlan carries no tier and no period. Remove that property and
// this function stops being true -- a donation that unlocked anything would be
// a sale at 21%.
//
// Two consequences, and the second is the one that is easy to miss:
//
//   - The document carries no VAT line. Charging 21% on a gift is charging tax
//     that is not due, and the customer's own books would then reclaim VAT that
//     was never owed.
//
//   - The giver's country is IRRELEVANT. Place of supply is a question about a
//     supply, and there is none, so a donation from outside the EU is NOT
//     parked the way a SALE outside the EU is. Parking it would hold a document
//     hostage to a decision that does not need making, and the money would sit
//     with no record for a person who cannot resolve it.
//
// It is still income. For an eenmanszaak this is winst uit onderneming and is
// declared as turnover; it simply carries no BTW. And it does not count toward
// the Article 59c threshold, because that measures cross-border B2C SUPPLIES --
// see CountsTowardThreshold and the store's CrossBorderSalesSince.
func ForDonation() Decision {
	return Decision{
		OutsideScope: true,
		Basis:        "voluntary contribution with no counter-performance, outside the scope of BTW",
	}
}

// InEU reports whether a country is in the EU VAT area.
func InEU(country string) bool {
	return euVATArea[strings.ToUpper(strings.TrimSpace(country))]
}

// CountsTowardThreshold reports whether a supply to this country counts toward
// the EUR 10 000 cross-border threshold. Domestic Dutch sales do NOT: the
// threshold is about supplies to OTHER member states, and counting our own
// would raise a false alarm on the busiest possible month.
func CountsTowardThreshold(country string) bool {
	c := strings.ToUpper(strings.TrimSpace(country))
	return c != Home && euVATArea[c]
}

// Threshold is the EU-wide annual limit, in euro cents, on cross-border B2C
// supplies of electronically supplied services below which a supplier may
// charge its home rate. Measured excluding VAT.
const Threshold = 10_000_00

// NetOfInclusive strips the VAT out of a gross amount charged at ratePercent.
//
// Every figure the Hub stores is the GROSS the customer paid, because the
// company derives VAT out of the price rather than adding it on. The EUR 10 000
// threshold, though, is measured on the TAXABLE amount — supplies excluding VAT
// — so counting the gross overstates the distance travelled by the rate itself:
// a EUR 9.00 sale is EUR 7.44 of supply and EUR 1.56 of somebody else's money.
//
// Left uncorrected this reports 21% more than was supplied. That errs towards
// alarming early rather than late, which is why it survived unnoticed, but the
// threshold is the basis for charging a flat Dutch rate at all and a meter that
// is knowingly wrong is not a meter.
//
// Rounds half away from zero, matching the billing system's own line rounding,
// and returns the gross unchanged for a rate that makes no sense rather than
// inventing a number.
func NetOfInclusive(grossCents int64, ratePercent float64) int64 {
	if ratePercent <= 0 || ratePercent >= 100 {
		return grossCents
	}
	// Integer arithmetic on hundredths of a percent, so a rate like 21 or 9 is
	// exact and no float rounding creeps into a tax figure.
	basis := int64(ratePercent*100 + 0.5) // 21% -> 2100
	num := grossCents * 10000
	den := 10000 + basis
	neg := num < 0
	if neg {
		num = -num
	}
	out := (num + den/2) / den
	if neg {
		return -out
	}
	return out
}
