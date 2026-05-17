# Design — SMS + Email as Routing Destinations

## Goal

The dispatch.Registry from spec/006 already declares `sms` and `email` destination types. The existing internal/sms/ + internal/email/ gateways (shipped in v1.2) have ready-to-use `Send(...)` methods. This spec closes the wiring gap so routes with these destination types actually fan out.

## Handler bindings

Two new Handler implementations:

```
internal/routing/handlers/sms.go
  func (h *SMSHandler) Dispatch(ctx, route, msg) error {
    addr := resolveDestination(route, msg, "sms")
    if addr == "" {
      counter no-address{sms}++
      return ErrNoAddress
    }
    return h.gateway.Send(ctx, addr, msg.Text)
  }

internal/routing/handlers/email.go
  func (h *EmailHandler) Dispatch(ctx, route, msg) error {
    addr := resolveDestination(route, msg, "email")
    if addr == "" {
      counter no-address{email}++
      return ErrNoAddress
    }
    subject := fmt.Sprintf("MeshSat: %s from %s", msg.SourceType, msg.DeviceIMEI)
    body := msg.Text
    if pgpKey := h.keystore.Lookup(addr); pgpKey != nil {
      body = pgp.Encrypt(body, pgpKey)
    }
    return h.gateway.Send(ctx, addr, subject, body)
  }
```

## Destination address resolution (REQ-802 / REQ-803 / REQ-805 / REQ-806)

```
resolveDestination(route, msg, kind) string {
  if route.destination_address != "" { return route.destination_address }
  return msg.SourceDevice.ContactList.Lookup(kind)  // device-side address book
}
```

The override-via-`destination_address` lets operators set up "all SOS from this device goes to my pager at +1-555-0100" rules without touching the device's onboard contacts.

## Schema delta (REQ-804 / REQ-811)

`route.json` from spec/006 gains an optional `destination_address` field. OpenAPI 3.0 supports `format: tel` for phone numbers and `format: email` for email addresses; we use string with regex pattern instead since `format` is advisory.

```json
"destination_address": {
  "type": "string",
  "maxLength": 200,
  "description": "Optional override of the device-contact-list lookup for sms/email destinations"
}
```

## PGP encryption for email (REQ-809)

The v1.2 email gateway already includes PGP integration (`internal/email/pgp.go`). The new handler asks the gateway's keystore "do you have a key for this recipient?" and encrypts before send if yes. Plaintext otherwise.

## Cross-spec dependency

- spec/006 (routing engine core) MUST be merged first — provides dispatch.Registry.
- v1.2 SMS gateway (internal/sms/, MESHSAT-174) already shipped — no new gateway code.
- v1.2 PGP email gateway (internal/email/, MESHSAT-186) already shipped — no new gateway code.

## Out of scope

- SMS-rate-limit-per-route (rely on existing per-device rate limiter).
- Email digest mode (batched delivery) — deferred.
- Phone-number portability (Twilio number routing decisions) — gateway concern, not routing.
