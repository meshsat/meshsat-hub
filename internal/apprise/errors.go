package apprise

import "errors"

// errNoAppriseAccount is returned when a tenant has no Apprise server set. It is
// an error and not a silent success on purpose: a notification nobody can
// deliver should be visible in the logs and to the caller.
var errNoAppriseAccount = errors.New("apprise: no notification relay configured for this tenant (Integrations page)")
