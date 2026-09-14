package webhook

import (
	"fmt"

	"github.com/meshsat/meshsat-hub/internal/netguard"
)

// ErrUnsafeTarget is returned for a webhook URL that points back into
// infrastructure rather than out at a customer's endpoint.
//
// The check itself moved to internal/netguard in MESHSAT-1121, unchanged: the
// same primitive was needed for per-tenant provider URLs (APRS-IS, Apprise,
// ntfy, the email gateway, wg-easy, hawkBit), and two copies of an SSRF guard is
// one copy that gets fixed. This wrapper keeps the name and the error the rest
// of the package already uses.
var ErrUnsafeTarget = netguard.ErrUnsafe

// ValidateTarget refuses an outbound webhook URL that would make the Hub fetch
// something on its own side of the network.
//
// Checked at registration AND at dispatch, deliberately. A name that resolves
// publicly when it is registered can resolve to 127.0.0.1 an hour later, and a
// check that runs only at registration does not see that.
func ValidateTarget(raw string) error {
	if err := netguard.ValidatePublicURL(raw); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}
