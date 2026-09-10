package stripe

import (
	"log/slog"
	"strings"

	"github.com/meshsat/meshsat-hub/internal/plans"
)

// Which plan a price buys.
//
// The mapping is configuration, because re-pricing a tier must not be a build.
// What is NOT configuration is the ceiling on what any price may buy: custom and
// beta are unlimited and operator-set, and a price that resolved to either would
// sell an uncapped fleet for whatever that price happens to be. internal/kofi
// learned this the same way -- plans.Known() is not the guard, because that
// table holds custom and beta too.

// tierMap resolves a Stripe price id to a plan name.
type tierMap map[string]string

// sellable is the closed set a price may resolve to. Adding to it is a
// commercial decision, not a configuration one.
var sellable = map[string]bool{
	plans.Crew:  true,
	plans.Fleet: true,
}

// SetPrices configures the price id to plan mapping. Entries naming a plan that
// is not sellable are dropped with an error rather than silently honoured: a
// typo in a ConfigMap should cost a log line, never an unlimited plan.
func (h *Handler) SetPrices(m map[string]string) {
	h.prices = tierMap{}
	for price, plan := range m {
		p := plans.Normalise(plan)
		if !sellable[p] {
			slog.Error("stripe: refusing a price that does not name a sellable tier; "+
				"custom and beta are operator-set and unlimited",
				"price", price, "plan", plan)
			continue
		}
		h.prices[strings.TrimSpace(price)] = p
	}
}

// planFor resolves a price id. An unknown price is NOT guessed at: it returns
// empty, and the caller records the payment for a person rather than granting
// something arbitrary. Ko-fi's equivalent defaulted to Crew on an unrecognised
// tier name, which is a guess about money.
func (h *Handler) planFor(priceID string) string {
	if h.prices == nil {
		return ""
	}
	return h.prices[strings.TrimSpace(priceID)]
}
