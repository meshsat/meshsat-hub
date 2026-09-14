# Apprise — the platform's own notification relay

Deployed for MESHSAT-1121. `HUB_APPRISE_ENABLED` was unset and no Apprise server
existed anywhere in the estate, so every notification URL a customer saved on the
Notifications page was accepted with HTTP 200 and delivered NOWHERE. That silence
is the defect this whole issue started from.

**This is the PLATFORM's relay, serving the default tenant.** Every other tenant
points at their own on the Integrations page. See `internal/apprise/pool.go`.

## What it actually is

Apprise is a fan-out: it takes a notification URL (`slack://`, `mailto://`,
`tgram://`, `discord://` — 90-odd backends) and delivers to it. The TARGETS were
always per tenant and per device (`store.NotificationPref`); only this relay was
missing.

Stateless, so no volume and no `retain` question. Losing the pod loses nothing.

## Why no persistence and two replicas would still be wrong

It holds no state, but it is also not on any critical path worth the complexity:
a notification is best-effort and the escalation engine already reports a failed
send. One replica, restarted on failure.
