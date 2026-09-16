# The stand flow (TTC 2026)

What the Hub does when a visitor at a stand texts us, and the knobs an operator has. The kit and
radio side lives in the Bridge repo; everything here is `meshsat-hub`.

Issues: MESHSAT-1175 (the flow), MESHSAT-1181 (mesh presence).

## What a visitor experiences

They text a keyword to the platform number. The Hub answers with a short menu, asks for consent
once, offers the meshes, takes one message, and relays it to a kit over SMS. The kit puts it on its
Meshtastic mesh, someone answers on a T-Deck, and the reply comes back to the same phone.

Two bearers carry the same flow: **SMS**, which is the one the stand runs on, and **WhatsApp**,
which is built but must never be load-bearing — Meta restricted the WABA twice, and the flow has to
survive that being switched off with a config change.

## The rules that shape it

**Every visitor-facing message is ONE SMS segment.** Multi-segment messages from the platform's
non-geographic `+3197…` number are not reliably delivered — measured 24/24 delivered at one segment
against 2/3 at two, with carrier error 30008. One character outside GSM-7 (a curly apostrophe, an em
dash) collapses the limit from 160 to 70, so the string looks short and fails anyway.
`internal/booth/sms_length_test.go` measures eighteen paths and is the guard. It walks the state
machine, so a message the Service sends on a path no menu choice reaches has to be named in the test
explicitly.

**The visitor picks an index, never an address.** A typed "1" resolves against the options for the
current state, which are built from a configured allowlist of kits. Nothing a visitor types can
become a destination.

**One conversation per kit.** The person on the T-Deck replies in plain text and will not retype a
reference, so the return leg refuses to guess between two open conversations. Serialising means that
refusal essentially never happens. Two kits still serve two visitors at once, and a visitor is never
blocked by their own open conversation.

**Correlation lives in Postgres, not in memory.** Both Hub replicas process every message, so a mesh
reply routinely lands on the pod that did not send the original.

## Spend

The quotas count **messages put on a bearer**, not relays — a visitor can walk the whole menu
without relaying and still be sent four billed messages. `booth_sends` is one row per message, so a
window is a `WHERE` and the count cannot drift from what was actually sent.

| limit | default | why |
|---|---|---|
| per visitor | 30 / 24 h | a full visitor run is about seven messages |
| whole stand | 250 / 24 h | sized against the account balance, not a round number |

At NL rates 250/day over two days is roughly USD 57. When the ceiling is reached the visitor is told
once — claimed per sender per day, because the telling costs a message too — and then it goes quiet.

## Is anything listening?

Two different questions, and the visitor is told about them differently:

- **Is the kit online?** Whether the Pi is reachable over cellular. A kit that is offline is refused
  with "nothing would arrive".
- **Is anything on its mesh?** `mesh_nodes` records nodes the Hub has HEARD behind each bridge, fed
  from `mo/decoded` — the one payload carrying a `bridge_id`.

**A row is proof; no row proves nothing.** Presence is only observable when a node transmits, so at
the start of a show day the table is legitimately empty while every mesh is fine. Therefore a live
mesh is *marked*, a quiet one is offered plainly and still relays, and a failed presence lookup
answers *available*. Nothing hides a destination on silence.

A visitor who picks a quiet mesh is warned before spending the message. If nobody answers, the
sweeper closes the relay after its window and tells them, naming the reference — so silence becomes
an explanation rather than a mystery.

## Delivery receipts

Twilio accepting a message means it was accepted, not received. The Hub attaches a `StatusCallback`
to every outbound message and `failed`/`undelivered` land at Warn with the carrier's error code.

This goes on each message, not on the phone number: a Twilio number has no SMS status field — its
`status_callback` is the voice one — so there is nothing to configure in the console.

## Configuration

| variable | default | notes |
|---|---|---|
| `HUB_BOOTH_ENABLED` | `false` | must never switch itself on: it intercepts inbound messages |
| `HUB_BOOTH_KITS` | — | the destination allowlist, `<bridge_id>:<label>[:<mesh_dest>]` comma separated |
| `HUB_BOOTH_SMS_KEYWORD` | — | opens a conversation on SMS; empty disables the SMS bearer |
| `HUB_BOOTH_MESH_WINDOW` | `30m` | how recently a node must have been heard to call a mesh live; `0` disables the check |
| `HUB_WHATSAPP_ENABLED` | `false` | turning it off must not disturb SMS |
| `HUB_BOOTH_CONTENT_*` | — | Twilio Content SIDs for the WhatsApp interactive messages |

SMS is a **shared** bearer: kit OOB replies and satellite traffic arrive on the same number, so the
booth claims a message only from somebody already in a conversation, or one whose entire text is the
keyword. Everything else falls through to the normal pipeline untouched.

## Checking it at the stand

```sql
-- who has been heard, and how long ago
select bridge_id, node_id, now() - last_heard as ago from mesh_nodes order by last_heard desc;

-- today's spend against the ceiling
select count(*) from booth_sends where created_at > now() - interval '24 hours';

-- conversations still open
select ref, sender, bridge_id, expires_at from booth_relays where closed_at is null;
```

A relay that was answered has a `closed_at`. One that expired unanswered is closed by the sweeper,
which runs on both replicas and claims the right to speak so only one of them does.
