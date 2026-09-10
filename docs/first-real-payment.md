# The one hop only a real payment can prove

Everything else about the paid path is verified against production with simulated
deliveries: matching, idempotency, tier mapping, renewal stacking, the receipt, the
plan-changed email, the lapse, the warning. Two things those cannot prove, because
both live on Ko-fi's side:

1. **The verification token in `HUB_KOFI_VERIFICATION_TOKEN` is the one Ko-fi
   actually sends.** Ours is compared constant-time against the token in the
   payload. If somebody pasted the wrong value, every genuine payment gets a 401
   and the customer is charged and not upgraded.
2. **A genuine Ko-fi body parses.** No test in the repository has ever fed the
   handler a real payload — the fixtures marshal our own Go struct, so the JSON
   tags are asserted against themselves.

One payment settles both. It costs EUR 9 and it is refundable.

## Before you start

Have the Hub open at Settings, signed in as the tenant you want to upgrade. You
need its **claim code** from the usage panel — eight characters, no I/O/0/1.

## The payment

1. Go to <https://ko-fi.com/X2S326G23T> and take the **Crew** membership, EUR 9.
2. In the message box, put the claim code **and nothing else that looks like one**.
   The matcher reads the first code-shaped token in the message.
3. Pay with the address you want on the receipt. If it differs from the tenant
   owner's address, the claim code is what matches — that is the point of it.

## What to check, in order

Give it about a minute; the receipt is drained by a background job, not by the
webhook.

| # | Check | Where | Expected |
|---|---|---|---|
| 1 | The webhook was accepted | `kubectl --context notrf01 -n meshsat-hub logs deploy/hub \| grep kofi` | `kofi: plan granted`, with the tenant id. **A `verification token mismatch` line here is defect 1 above** — the token is wrong, fix it in OpenBao and ask Ko-fi to resend. |
| 2 | The plan changed | Settings → usage panel | `crew`, 24 devices and bridges, and a paid-to date about 32 days out |
| 3 | The customer was told | the paying address's inbox | "Your MeshSat Hub plan is now crew", from `MeshSat Hub <billing@meshsat.net>`, naming the exact moment it runs to |
| 4 | The receipt was issued | same inbox, may be a minute later | An Invoice Ninja PDF, series `MSH2026-…`, EUR 9.00 gross with BTW 21 broken out as 7.44 + 1.56 |
| 5 | Nothing was double-applied | `GET /api/admin/payments/unmatched` | empty — a payment that landed here means the claim code did not match |
| 6 | The document is in the books | Invoice Ninja, company 2 | one invoice, marked paid, no gap in the number series |

## If the receipt does not arrive

It is queued, not lost. `GET /api/admin/receipts/blocked` lists anything parked for
a person, with the reason. Fix the cause and `POST /api/admin/receipts/{id}/requeue`.
A receipt is never dropped: money was taken, so the document is owed.

## Afterwards

Keep the payload. Copy the `data` field from the webhook delivery in Ko-fi's
settings page into `internal/kofi/testdata/` as the golden fixture, so the parser
is finally tested against something Ko-fi wrote rather than against itself.

Cancel the membership on Ko-fi if you do not want it to renew — cancelling sends no
webhook, which is by design: the plan simply runs to the date it is paid to.
