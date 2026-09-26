package vat

import (
	"regexp"
	"strings"
)

// Alpha2 turns what a person declared as their country into an ISO 3166-1
// alpha-2 code, or reports that it could not.
//
// The enrolment form asks "Country" in a free-text field, so what arrives is
// whatever a person types: "USA", "United States", "Germany", "de", or, from a
// dropdown, "Netherlands (NL)". tenants.billing_country is VARCHAR(2), and until
// MESHSAT-1365 the sign-in path wrote the declared text into it after nothing
// more than ToUpper(TrimSpace()). The first outside customers who tried to sign
// in had typed "USA" and every attempt failed on the column length (SQLSTATE
// 22001); they concluded their accounts were broken and enrolled again.
//
// A country that is not recognised answers ("", false). The caller keeps the
// raw text in the evidence column and leaves the code empty, which parks that
// tenant's receipts for a person (MESHSAT-1016): a document is never issued on
// a guess, and a sign-in is never refused over a spelling.
func Alpha2(declared string) (string, bool) {
	s := strings.TrimSpace(declared)
	if s == "" {
		return "", false
	}
	// "Netherlands (NL)": the shape a dropdown submits. The code in brackets
	// wins over the name in front of it, so the name may be translated or
	// abbreviated freely.
	if m := bracketedCode.FindStringSubmatch(s); m != nil {
		if code, ok := byAlpha2[strings.ToUpper(m[1])]; ok {
			return code, true
		}
	}
	up := strings.ToUpper(s)
	if len(up) == 2 {
		if code, ok := byAlpha2[up]; ok {
			return code, true
		}
	}
	if len(up) == 3 {
		if code, ok := byAlpha3[up]; ok {
			return code, true
		}
	}
	// A name written in a script fold cannot reduce (Greek, Cyrillic) folds to
	// nothing, and nothing must never match.
	if key := fold(s); key != "" {
		if code, ok := byName[key]; ok {
			return code, true
		}
	}
	return "", false
}

// IsAlpha2 reports whether s is exactly an assigned ISO 3166-1 alpha-2 code,
// case-insensitively. It is the check a store applies before writing
// billing_country: the column is two characters wide and a longer value is an
// error that must name itself, not SQLSTATE 22001 at the bottom of a sign-in.
func IsAlpha2(s string) bool {
	if len(s) != 2 {
		return false
	}
	_, ok := byAlpha2[strings.ToUpper(s)]
	return ok
}

var bracketedCode = regexp.MustCompile(`\(\s*([A-Za-z]{2})\s*\)\s*$`)

// fold reduces a name to lower-case letters and digits so that "United States
// of America", "united-states-of-america" and "U.S.A." meet at one key. A
// leading "the" is dropped ("The Netherlands", "The Gambia") and a handful of
// accented letters that appear in ISO short names are folded to ASCII.
func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'à', 'á', 'â', 'ã', 'ä', 'å':
			r = 'a'
		case 'ç':
			r = 'c'
		case 'è', 'é', 'ê', 'ë':
			r = 'e'
		case 'ì', 'í', 'î', 'ï':
			r = 'i'
		case 'ñ':
			r = 'n'
		case 'ò', 'ó', 'ô', 'õ', 'ö', 'ø':
			r = 'o'
		case 'ù', 'ú', 'û', 'ü':
			r = 'u'
		case 'ß':
			b.WriteString("ss")
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if strings.HasPrefix(out, "the") && len(out) > 3 {
		out = out[3:]
	}
	return out
}

// country is one ISO 3166-1 entry: the alpha-2 code, the alpha-3 code, the
// ISO short name, and the other names people write for it.
type country struct {
	a2, a3 string
	names  []string
}

var (
	byAlpha2 = map[string]string{}
	byAlpha3 = map[string]string{}
	byName   = map[string]string{}
)

func init() {
	for _, c := range countries {
		byAlpha2[c.a2] = c.a2
		byAlpha3[c.a3] = c.a2
		for _, n := range c.names {
			if key := fold(n); key != "" {
				byName[key] = c.a2
			}
		}
	}
	// Greece's VAT prefix, which turns up in payment data and which
	// internal/vat already accepts alongside GR.
	byAlpha2["EL"] = "GR"
}

// countries is the ISO 3166-1 list (249 assigned codes) with the names people
// actually type. The first name is the ISO short name.
var countries = []country{
	{"AF", "AFG", []string{"Afghanistan"}},
	{"AX", "ALA", []string{"Åland Islands", "Aland Islands", "Aland"}},
	{"AL", "ALB", []string{"Albania"}},
	{"DZ", "DZA", []string{"Algeria"}},
	{"AS", "ASM", []string{"American Samoa"}},
	{"AD", "AND", []string{"Andorra"}},
	{"AO", "AGO", []string{"Angola"}},
	{"AI", "AIA", []string{"Anguilla"}},
	{"AQ", "ATA", []string{"Antarctica"}},
	{"AG", "ATG", []string{"Antigua and Barbuda", "Antigua & Barbuda", "Antigua"}},
	{"AR", "ARG", []string{"Argentina"}},
	{"AM", "ARM", []string{"Armenia"}},
	{"AW", "ABW", []string{"Aruba"}},
	{"AU", "AUS", []string{"Australia"}},
	{"AT", "AUT", []string{"Austria", "Österreich", "Oesterreich"}},
	{"AZ", "AZE", []string{"Azerbaijan"}},
	{"BS", "BHS", []string{"Bahamas", "The Bahamas"}},
	{"BH", "BHR", []string{"Bahrain"}},
	{"BD", "BGD", []string{"Bangladesh"}},
	{"BB", "BRB", []string{"Barbados"}},
	{"BY", "BLR", []string{"Belarus"}},
	{"BE", "BEL", []string{"Belgium", "België", "Belgie", "Belgique"}},
	{"BZ", "BLZ", []string{"Belize"}},
	{"BJ", "BEN", []string{"Benin"}},
	{"BM", "BMU", []string{"Bermuda"}},
	{"BT", "BTN", []string{"Bhutan"}},
	{"BO", "BOL", []string{"Bolivia (Plurinational State of)", "Bolivia"}},
	{"BQ", "BES", []string{"Bonaire, Sint Eustatius and Saba", "Caribbean Netherlands", "Bonaire"}},
	{"BA", "BIH", []string{"Bosnia and Herzegovina", "Bosnia & Herzegovina", "Bosnia"}},
	{"BW", "BWA", []string{"Botswana"}},
	{"BV", "BVT", []string{"Bouvet Island"}},
	{"BR", "BRA", []string{"Brazil", "Brasil"}},
	{"IO", "IOT", []string{"British Indian Ocean Territory"}},
	{"BN", "BRN", []string{"Brunei Darussalam", "Brunei"}},
	{"BG", "BGR", []string{"Bulgaria"}},
	{"BF", "BFA", []string{"Burkina Faso"}},
	{"BI", "BDI", []string{"Burundi"}},
	{"CV", "CPV", []string{"Cabo Verde", "Cape Verde"}},
	{"KH", "KHM", []string{"Cambodia"}},
	{"CM", "CMR", []string{"Cameroon"}},
	{"CA", "CAN", []string{"Canada"}},
	{"KY", "CYM", []string{"Cayman Islands"}},
	{"CF", "CAF", []string{"Central African Republic"}},
	{"TD", "TCD", []string{"Chad"}},
	{"CL", "CHL", []string{"Chile"}},
	{"CN", "CHN", []string{"China", "People's Republic of China", "PRC", "Mainland China"}},
	{"CX", "CXR", []string{"Christmas Island"}},
	{"CC", "CCK", []string{"Cocos (Keeling) Islands", "Cocos Islands", "Keeling Islands"}},
	{"CO", "COL", []string{"Colombia"}},
	{"KM", "COM", []string{"Comoros"}},
	{"CG", "COG", []string{"Congo", "Republic of the Congo", "Congo-Brazzaville", "Congo Brazzaville"}},
	{"CD", "COD", []string{"Congo, Democratic Republic of the", "Democratic Republic of the Congo", "DR Congo", "DRC", "Congo-Kinshasa", "Congo Kinshasa"}},
	{"CK", "COK", []string{"Cook Islands"}},
	{"CR", "CRI", []string{"Costa Rica"}},
	{"CI", "CIV", []string{"Côte d'Ivoire", "Cote d'Ivoire", "Ivory Coast"}},
	{"HR", "HRV", []string{"Croatia", "Hrvatska"}},
	{"CU", "CUB", []string{"Cuba"}},
	{"CW", "CUW", []string{"Curaçao", "Curacao"}},
	{"CY", "CYP", []string{"Cyprus"}},
	{"CZ", "CZE", []string{"Czechia", "Czech Republic", "Česko", "Cesko"}},
	{"DK", "DNK", []string{"Denmark", "Danmark"}},
	{"DJ", "DJI", []string{"Djibouti"}},
	{"DM", "DMA", []string{"Dominica"}},
	{"DO", "DOM", []string{"Dominican Republic"}},
	{"EC", "ECU", []string{"Ecuador"}},
	{"EG", "EGY", []string{"Egypt"}},
	{"SV", "SLV", []string{"El Salvador"}},
	{"GQ", "GNQ", []string{"Equatorial Guinea"}},
	{"ER", "ERI", []string{"Eritrea"}},
	{"EE", "EST", []string{"Estonia", "Eesti"}},
	{"SZ", "SWZ", []string{"Eswatini", "Swaziland"}},
	{"ET", "ETH", []string{"Ethiopia"}},
	{"FK", "FLK", []string{"Falkland Islands (Malvinas)", "Falkland Islands", "Falklands", "Malvinas"}},
	{"FO", "FRO", []string{"Faroe Islands", "Faroes", "Føroyar"}},
	{"FJ", "FJI", []string{"Fiji"}},
	{"FI", "FIN", []string{"Finland", "Suomi"}},
	{"FR", "FRA", []string{"France"}},
	{"GF", "GUF", []string{"French Guiana"}},
	{"PF", "PYF", []string{"French Polynesia"}},
	{"TF", "ATF", []string{"French Southern Territories"}},
	{"GA", "GAB", []string{"Gabon"}},
	{"GM", "GMB", []string{"Gambia", "The Gambia"}},
	{"GE", "GEO", []string{"Georgia"}},
	{"DE", "DEU", []string{"Germany", "Deutschland", "Federal Republic of Germany"}},
	{"GH", "GHA", []string{"Ghana"}},
	{"GI", "GIB", []string{"Gibraltar"}},
	{"GR", "GRC", []string{"Greece", "Hellas"}},
	{"GL", "GRL", []string{"Greenland"}},
	{"GD", "GRD", []string{"Grenada"}},
	{"GP", "GLP", []string{"Guadeloupe"}},
	{"GU", "GUM", []string{"Guam"}},
	{"GT", "GTM", []string{"Guatemala"}},
	{"GG", "GGY", []string{"Guernsey"}},
	{"GN", "GIN", []string{"Guinea"}},
	{"GW", "GNB", []string{"Guinea-Bissau"}},
	{"GY", "GUY", []string{"Guyana"}},
	{"HT", "HTI", []string{"Haiti"}},
	{"HM", "HMD", []string{"Heard Island and McDonald Islands"}},
	{"VA", "VAT", []string{"Holy See", "Vatican City", "Vatican", "Vatican City State"}},
	{"HN", "HND", []string{"Honduras"}},
	{"HK", "HKG", []string{"Hong Kong", "Hong Kong SAR"}},
	{"HU", "HUN", []string{"Hungary", "Magyarország", "Magyarorszag"}},
	{"IS", "ISL", []string{"Iceland", "Ísland"}},
	{"IN", "IND", []string{"India"}},
	{"ID", "IDN", []string{"Indonesia"}},
	{"IR", "IRN", []string{"Iran (Islamic Republic of)", "Iran"}},
	{"IQ", "IRQ", []string{"Iraq"}},
	{"IE", "IRL", []string{"Ireland", "Republic of Ireland", "Éire", "Eire"}},
	{"IM", "IMN", []string{"Isle of Man"}},
	{"IL", "ISR", []string{"Israel"}},
	{"IT", "ITA", []string{"Italy", "Italia"}},
	{"JM", "JAM", []string{"Jamaica"}},
	{"JP", "JPN", []string{"Japan"}},
	{"JE", "JEY", []string{"Jersey"}},
	{"JO", "JOR", []string{"Jordan"}},
	{"KZ", "KAZ", []string{"Kazakhstan"}},
	{"KE", "KEN", []string{"Kenya"}},
	{"KI", "KIR", []string{"Kiribati"}},
	{"KP", "PRK", []string{"Korea (Democratic People's Republic of)", "North Korea", "DPRK"}},
	{"KR", "KOR", []string{"Korea, Republic of", "South Korea", "Republic of Korea", "Korea"}},
	{"KW", "KWT", []string{"Kuwait"}},
	{"KG", "KGZ", []string{"Kyrgyzstan"}},
	{"LA", "LAO", []string{"Lao People's Democratic Republic", "Laos"}},
	{"LV", "LVA", []string{"Latvia", "Latvija"}},
	{"LB", "LBN", []string{"Lebanon"}},
	{"LS", "LSO", []string{"Lesotho"}},
	{"LR", "LBR", []string{"Liberia"}},
	{"LY", "LBY", []string{"Libya"}},
	{"LI", "LIE", []string{"Liechtenstein"}},
	{"LT", "LTU", []string{"Lithuania", "Lietuva"}},
	{"LU", "LUX", []string{"Luxembourg", "Luxemburg"}},
	{"MO", "MAC", []string{"Macao", "Macau"}},
	{"MG", "MDG", []string{"Madagascar"}},
	{"MW", "MWI", []string{"Malawi"}},
	{"MY", "MYS", []string{"Malaysia"}},
	{"MV", "MDV", []string{"Maldives"}},
	{"ML", "MLI", []string{"Mali"}},
	{"MT", "MLT", []string{"Malta"}},
	{"MH", "MHL", []string{"Marshall Islands"}},
	{"MQ", "MTQ", []string{"Martinique"}},
	{"MR", "MRT", []string{"Mauritania"}},
	{"MU", "MUS", []string{"Mauritius"}},
	{"YT", "MYT", []string{"Mayotte"}},
	{"MX", "MEX", []string{"Mexico", "México"}},
	{"FM", "FSM", []string{"Micronesia (Federated States of)", "Micronesia", "Federated States of Micronesia"}},
	{"MD", "MDA", []string{"Moldova, Republic of", "Moldova", "Republic of Moldova"}},
	{"MC", "MCO", []string{"Monaco"}},
	{"MN", "MNG", []string{"Mongolia"}},
	{"ME", "MNE", []string{"Montenegro"}},
	{"MS", "MSR", []string{"Montserrat"}},
	{"MA", "MAR", []string{"Morocco"}},
	{"MZ", "MOZ", []string{"Mozambique"}},
	{"MM", "MMR", []string{"Myanmar", "Burma"}},
	{"NA", "NAM", []string{"Namibia"}},
	{"NR", "NRU", []string{"Nauru"}},
	{"NP", "NPL", []string{"Nepal"}},
	{"NL", "NLD", []string{"Netherlands", "The Netherlands", "Netherlands (Kingdom of the)", "Kingdom of the Netherlands", "Holland", "Nederland"}},
	{"NC", "NCL", []string{"New Caledonia"}},
	{"NZ", "NZL", []string{"New Zealand", "Aotearoa"}},
	{"NI", "NIC", []string{"Nicaragua"}},
	{"NE", "NER", []string{"Niger"}},
	{"NG", "NGA", []string{"Nigeria"}},
	{"NU", "NIU", []string{"Niue"}},
	{"NF", "NFK", []string{"Norfolk Island"}},
	{"MK", "MKD", []string{"North Macedonia", "Macedonia", "Republic of North Macedonia"}},
	{"MP", "MNP", []string{"Northern Mariana Islands"}},
	{"NO", "NOR", []string{"Norway", "Norge", "Noreg"}},
	{"OM", "OMN", []string{"Oman"}},
	{"PK", "PAK", []string{"Pakistan"}},
	{"PW", "PLW", []string{"Palau"}},
	{"PS", "PSE", []string{"Palestine, State of", "Palestine", "State of Palestine"}},
	{"PA", "PAN", []string{"Panama"}},
	{"PG", "PNG", []string{"Papua New Guinea"}},
	{"PY", "PRY", []string{"Paraguay"}},
	{"PE", "PER", []string{"Peru"}},
	{"PH", "PHL", []string{"Philippines", "The Philippines"}},
	{"PN", "PCN", []string{"Pitcairn", "Pitcairn Islands"}},
	{"PL", "POL", []string{"Poland", "Polska"}},
	{"PT", "PRT", []string{"Portugal"}},
	{"PR", "PRI", []string{"Puerto Rico"}},
	{"QA", "QAT", []string{"Qatar"}},
	{"RE", "REU", []string{"Réunion", "Reunion"}},
	{"RO", "ROU", []string{"Romania", "România"}},
	{"RU", "RUS", []string{"Russian Federation", "Russia"}},
	{"RW", "RWA", []string{"Rwanda"}},
	{"BL", "BLM", []string{"Saint Barthélemy", "Saint Barthelemy", "St Barthelemy", "St. Barthélemy"}},
	{"SH", "SHN", []string{"Saint Helena, Ascension and Tristan da Cunha", "Saint Helena", "St Helena"}},
	{"KN", "KNA", []string{"Saint Kitts and Nevis", "St Kitts and Nevis", "St. Kitts and Nevis"}},
	{"LC", "LCA", []string{"Saint Lucia", "St Lucia", "St. Lucia"}},
	{"MF", "MAF", []string{"Saint Martin (French part)", "Saint Martin", "St Martin"}},
	{"PM", "SPM", []string{"Saint Pierre and Miquelon", "St Pierre and Miquelon"}},
	{"VC", "VCT", []string{"Saint Vincent and the Grenadines", "St Vincent and the Grenadines", "St. Vincent and the Grenadines"}},
	{"WS", "WSM", []string{"Samoa"}},
	{"SM", "SMR", []string{"San Marino"}},
	{"ST", "STP", []string{"Sao Tome and Principe", "São Tomé and Príncipe", "Sao Tome"}},
	{"SA", "SAU", []string{"Saudi Arabia", "KSA"}},
	{"SN", "SEN", []string{"Senegal"}},
	{"RS", "SRB", []string{"Serbia", "Srbija"}},
	{"SC", "SYC", []string{"Seychelles"}},
	{"SL", "SLE", []string{"Sierra Leone"}},
	{"SG", "SGP", []string{"Singapore"}},
	{"SX", "SXM", []string{"Sint Maarten (Dutch part)", "Sint Maarten"}},
	{"SK", "SVK", []string{"Slovakia", "Slovak Republic", "Slovensko"}},
	{"SI", "SVN", []string{"Slovenia", "Slovenija"}},
	{"SB", "SLB", []string{"Solomon Islands"}},
	{"SO", "SOM", []string{"Somalia"}},
	{"ZA", "ZAF", []string{"South Africa", "RSA"}},
	{"GS", "SGS", []string{"South Georgia and the South Sandwich Islands", "South Georgia"}},
	{"SS", "SSD", []string{"South Sudan"}},
	{"ES", "ESP", []string{"Spain", "España", "Espana"}},
	{"LK", "LKA", []string{"Sri Lanka"}},
	{"SD", "SDN", []string{"Sudan"}},
	{"SR", "SUR", []string{"Suriname", "Surinam"}},
	{"SJ", "SJM", []string{"Svalbard and Jan Mayen", "Svalbard"}},
	{"SE", "SWE", []string{"Sweden", "Sverige"}},
	{"CH", "CHE", []string{"Switzerland", "Schweiz", "Suisse", "Svizzera", "Swiss Confederation"}},
	{"SY", "SYR", []string{"Syrian Arab Republic", "Syria"}},
	{"TW", "TWN", []string{"Taiwan, Province of China", "Taiwan", "Republic of China"}},
	{"TJ", "TJK", []string{"Tajikistan"}},
	{"TZ", "TZA", []string{"Tanzania, United Republic of", "Tanzania", "United Republic of Tanzania"}},
	{"TH", "THA", []string{"Thailand"}},
	{"TL", "TLS", []string{"Timor-Leste", "East Timor"}},
	{"TG", "TGO", []string{"Togo"}},
	{"TK", "TKL", []string{"Tokelau"}},
	{"TO", "TON", []string{"Tonga"}},
	{"TT", "TTO", []string{"Trinidad and Tobago", "Trinidad & Tobago", "Trinidad"}},
	{"TN", "TUN", []string{"Tunisia"}},
	{"TR", "TUR", []string{"Türkiye", "Turkiye", "Turkey"}},
	{"TM", "TKM", []string{"Turkmenistan"}},
	{"TC", "TCA", []string{"Turks and Caicos Islands", "Turks and Caicos"}},
	{"TV", "TUV", []string{"Tuvalu"}},
	{"UG", "UGA", []string{"Uganda"}},
	{"UA", "UKR", []string{"Ukraine"}},
	{"AE", "ARE", []string{"United Arab Emirates", "UAE", "Emirates"}},
	{"GB", "GBR", []string{"United Kingdom", "United Kingdom of Great Britain and Northern Ireland", "UK", "U.K.", "Great Britain", "Britain", "England", "Scotland", "Wales", "Northern Ireland"}},
	{"US", "USA", []string{"United States", "United States of America", "USA", "U.S.A.", "U.S.", "US of A", "America"}},
	{"UM", "UMI", []string{"United States Minor Outlying Islands"}},
	{"UY", "URY", []string{"Uruguay"}},
	{"UZ", "UZB", []string{"Uzbekistan"}},
	{"VU", "VUT", []string{"Vanuatu"}},
	{"VE", "VEN", []string{"Venezuela (Bolivarian Republic of)", "Venezuela"}},
	{"VN", "VNM", []string{"Viet Nam", "Vietnam"}},
	{"VG", "VGB", []string{"Virgin Islands (British)", "British Virgin Islands", "BVI"}},
	{"VI", "VIR", []string{"Virgin Islands (U.S.)", "US Virgin Islands", "U.S. Virgin Islands", "United States Virgin Islands"}},
	{"WF", "WLF", []string{"Wallis and Futuna"}},
	{"EH", "ESH", []string{"Western Sahara"}},
	{"YE", "YEM", []string{"Yemen"}},
	{"ZM", "ZMB", []string{"Zambia"}},
	{"ZW", "ZWE", []string{"Zimbabwe"}},
}
