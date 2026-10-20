package directory

import (
	"fmt"
	"strconv"
	"strings"
)

// Canonical returns the stored form of an address value of kind k, or
// ErrInvalidAddressValue. TETRA takes an SSI (1-16777214, leading zeros
// dropped), a 15- or 16-digit TSI, or "tetra:<ssi>", and is stored as
// "tetra:<ssi>"; CELL takes digits (an optional leading +, at most 20) or
// "cell:<digits>" and is stored as "cell:<digits>": the forms the Bridge's
// ledger uses, so one number written two ways is one address. MQTT is
// "bridge:<id>" or "device:<id>", the principals the Hub routes to; the id
// is not empty and holds no MQTT wildcard or separator. Every other kind
// keeps its value as given, trimmed. The rules are the Bridge's
// (internal/tetra ParseAddress, internal/gateway ParseCellAddress).
// [MESHSAT-1584 C7]
func (k AddressKind) Canonical(value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%w: empty %s address", ErrInvalidAddressValue, k)
	}
	switch k {
	case KindTETRA:
		ssi := strings.TrimSpace(strings.TrimPrefix(v, "tetra:"))
		if ssi == "" || len(ssi) > 17 || !allDigits(ssi) {
			return "", fmt.Errorf("%w: TETRA %q is not an SSI, a TSI or tetra:<ssi>", ErrInvalidAddressValue, value)
		}
		if len(ssi) <= 8 {
			n, _ := strconv.Atoi(ssi)
			if n < 1 || n > 16777214 {
				return "", fmt.Errorf("%w: TETRA SSI %q is outside 1-16777214", ErrInvalidAddressValue, value)
			}
			ssi = strconv.Itoa(n)
		}
		return "tetra:" + ssi, nil
	case KindCell:
		msisdn := v
		if len(msisdn) >= 5 && strings.EqualFold(msisdn[:5], "cell:") {
			msisdn = strings.TrimSpace(msisdn[5:])
		}
		digits := strings.TrimPrefix(msisdn, "+")
		if msisdn == "" || len(msisdn) > 20 || digits == "" || !allDigits(digits) {
			return "", fmt.Errorf("%w: CELL %q is not digits or cell:<digits>", ErrInvalidAddressValue, value)
		}
		return "cell:" + msisdn, nil
	case KindMQTT:
		for _, prefix := range []string{"bridge:", "device:"} {
			if id, ok := strings.CutPrefix(v, prefix); ok {
				if id == "" || strings.ContainsAny(id, "/+#") || strings.ContainsFunc(id, isSpace) {
					return "", fmt.Errorf("%w: MQTT %q has no usable id", ErrInvalidAddressValue, value)
				}
				return v, nil
			}
		}
		return "", fmt.Errorf("%w: MQTT %q is not bridge:<id> or device:<id>", ErrInvalidAddressValue, value)
	}
	return v, nil
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// GroupKind is what a group is for. A TEAM is what a team send addresses
// (C8); the kinds are the Bridge's.
type GroupKind string

const (
	GroupTeam     GroupKind = "TEAM"
	GroupRole     GroupKind = "ROLE"
	GroupList     GroupKind = "LIST"
	GroupMLSGroup GroupKind = "MLS_GROUP"
)

// Valid reports whether g is one of the four group kinds.
func (g GroupKind) Valid() bool {
	switch g {
	case GroupTeam, GroupRole, GroupList, GroupMLSGroup:
		return true
	}
	return false
}

// Policy scopes, as the Bridge reads them.
const (
	ScopeContact    = "contact"
	ScopeGroup      = "group"
	ScopePrecedence = "precedence"
	ScopeDefault    = "default"
)

// Precedences are the STANAG 4406 levels a precedence-scoped policy names.
var Precedences = []string{"Override", "Flash", "Immediate", "Priority", "Routine", "Deferred"}

// Valid reports whether s is one of the five dispatch strategies.
func (s Strategy) Valid() bool {
	switch s {
	case StrategyPrimaryOnly, StrategyAnyReachable, StrategyOrderedFallback, StrategyHeMBBonded, StrategyAllBearers:
		return true
	}
	return false
}

// Check returns ErrInvalidPolicy, with the reason, for a policy outside
// the model: an unknown scope, a contact or group scope without an id, an
// unknown precedence, a default scope with an id, an unknown strategy or
// bearer kind, or a negative retry setting. Whether a Bridge implements a
// valid policy is the Bridge's to say per row (its pushed-policy check);
// the Hub keeps its own model whole.
func (p *DispatchPolicy) Check() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidPolicy, fmt.Sprintf(format, a...))
	}
	switch p.ScopeType {
	case ScopeContact, ScopeGroup:
		if p.ScopeID == "" {
			return bad("a %s scope names its %s id", p.ScopeType, p.ScopeType)
		}
	case ScopePrecedence:
		known := false
		for _, x := range Precedences {
			known = known || x == p.ScopeID
		}
		if !known {
			return bad("unknown precedence %q", p.ScopeID)
		}
	case ScopeDefault:
		if p.ScopeID != "" {
			return bad("the default scope has no scope id")
		}
	default:
		return bad("unknown scope %q", p.ScopeType)
	}
	if !p.Strategy.Valid() {
		return bad("unknown strategy %q", p.Strategy)
	}
	for _, list := range [][]AddressKind{p.Preferred, p.Fallback} {
		for _, k := range list {
			if !k.Valid() {
				return bad("unknown bearer kind %q", k)
			}
		}
	}
	if p.MaxRetries < 0 || p.RetryDelay < 0 {
		return bad("negative retry settings")
	}
	return nil
}
