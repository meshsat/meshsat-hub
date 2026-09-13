# The first real payment

Test mode proves everything except that the credentials in production are the
ones Stripe is actually using. This is the checklist for the one payment that
does, and for giving it straight back.

Nothing here is optional and none of it takes long. Do it before telling anybody
the service takes money.

## Before

- [ ] **`Settings → Tax → Integrations → Dashboard transactions → Use automatic
      tax` is OFF.** It has been found ON with no NL tax registration behind it,
      which would put EUR 0 VAT on transactions raised by a VAT-registered
      business. It is dashboard-only; `POST /v1/tax/settings` cannot reach it.
      The Hub sends `automatic_tax[enabled]=false` on every request as belt and
      braces, but fix the toggle.
- [ ] **`Settings → Business → Customer emails → Successful payments` is OFF.**
      Invoice Ninja issues the document. Turning this on gives every customer
      two, and they will not agree.
- [ ] **Stripe Tax is not enabled.** Invoice Ninja is the book.
- [ ] `scratchpad/stripe-suite.py` has been run against TEST keys and passed.
- [ ] Company 2 is clean: 0 clients, 0 invoices, 0 credits, 0 payments, and both
      counters at 1. Check it, do not assume it — two runs of `mail-probe.py`
      once burned `MSH2026-0001` and `MSH2026-0002` for real and nobody noticed
      until the next baseline check.

## Switching to live

1. Write the three live values to OpenBao:
   `bao kv patch -mount=secret ci-no/apps/meshsat-hub/hub \`
   `HUB_STRIPE_SECRET_KEY=sk_live_... HUB_STRIPE_WEBHOOK_SECRET=whsec_... \`
   `HUB_STRIPE_PATH_SECRET=<something you generate>`

   The path secret is **yours, not Stripe's**. It forms the webhook URL and only
   keeps the endpoint off scanners. The signing secret authenticates a delivery
   and must never appear in a path.

2. Uncomment the three references in `k8s/hub/externalsecret.yaml` and the
   prices in `k8s/hub/configmap.yaml`. Not before: a template reference to a
   property that does not exist renders the literal string `<no value>`, which
   is not empty. `internal/config` refuses that exact string, but the first line
   of defence is writing the secret first.

3. Point Stripe's live webhook endpoint at
   `https://hub.meshsat.net/api/webhook/stripe/<path secret>` and subscribe it
   to: `checkout.session.completed`, `customer.subscription.created`,
   `customer.subscription.updated`, `customer.subscription.deleted`,
   `invoice.paid`, `charge.refunded`.

4. Merge, wait for the pin commit, and confirm the rollout. The startup line to
   look for is `stripe: payment webhook enabled` **without** the
   `this is a TEST key` warning beside it.

## The payment

- [ ] Subscribe to Crew from the Hub's own Settings page. Not from a link, not
      from the Stripe dashboard — the point is to prove the session the Hub
      builds carries the tenant.
- [ ] `tenants.plan` becomes `crew` and `plan_expires_at` is set.
- [ ] A receipt row appears and reaches `issued`, with a real `MSH2026-000n`.
- [ ] The customer receives that receipt, from `billing@meshsat.net`, in the
      brand shell. Check the sender: credit-note mail once went out as the other
      company on that instance, `dkim=pass d=ellizg.com`.
- [ ] The PDF carries the VAT number, the legal entity, and 21% derived OUT of
      the price (EUR 9.00 = 7.44 + 1.56), not added to it.
- [ ] `GET /api/admin/vat/threshold` moved by the NET amount, not the gross --
      but ONLY if the buyer is in another EU member state. For a Dutch buyer it
      must stay where it was: the meter counts cross-border B2C supplies, and a
      domestic sale appearing in it would be a false alarm.

## Giving it back

- [ ] Refund the charge **in Stripe**, and touch nothing in the Hub.
- [ ] A refund row appears on its own with `requested_by = stripe`. Nobody
      calls `POST /api/admin/receipts/{id}/refund`. If that step is needed, the
      webhook did not arrive and the whole point of the migration is unproven.
- [ ] A credit note `MSHCN2026-000n` is issued and emailed with its PDF.
- [ ] The invoice nets to zero and the credit is applied, not left sitting.
- [ ] The plan comes back down.

## Afterwards

- [x] Decide whether to purge the documents. If this was your own card, purging
      and rewinding the counters keeps `MSH2026-0001` for the first real
      customer. If it was somebody else's money, **keep them**: they are that
      person's tax documents and the counters stay where they are.
- [x] Note the date here, and what the first number actually issued was.

## It was run: 13 September 2026

Every box above is ticked. One real EUR 9.00 Crew subscription on the owner's own
card, started from the Hub's own Settings page, refunded in Stripe, then
cancelled immediately.

| | |
|---|---|
| first invoice issued | **MSH2026-0001** |
| first credit note issued | **MSHCN2026-0001** |
| next customer gets | **MSH2026-0002** |

**The documents were KEPT, and the counters were NOT rewound.** The owner's
ruling, and it reverses what the bullet above leans towards, so the reasoning is
worth keeping: the pair nets to zero, which is exactly how a refunded sale is
supposed to look in the books. Both PDFs are also sitting in a real mailbox
naming a real person. Rewinding would later hand `MSH2026-0001` to somebody
else, while a document with that number already exists describing a different
party — a worse defect than an unremarkable first invoice being numbered 0002.
Purge only what was never real.

Timeline, read from the cluster rather than from a dashboard:

```
20:02:37  billing: checkout started                  tenant=t_218d…  plan=crew
20:04:07  stripe: payment recorded for a receipt     900 EUR
20:04:07  stripe: plan granted                       was=free → crew
20:04:07  mail: sent                                 "plan changed"
20:04:26  billing: receipt issued                    MSH2026-0001
20:08:59  customer.subscription.updated              cancel at period end — plan KEPT
20:18:14  stripe: refund recorded from the processor requested_by=stripe
20:18:26  refunds: credit note issued                MSHCN2026-0001
20:18:26  refunds: plan shortened by one period      16 Oct → 14 Sep
20:18:28  refunds: credit note sent
20:23:20  customer.subscription.deleted              → free, immediately
```

Notes worth keeping for whoever runs this next:

- **`invoice.paid` really does arrive before `checkout.session.completed`.** The
  tenant had no `billing_country`, so the receipt took NL from the invoice's own
  billing address, and `billing_country_evidence` reads "billing address at
  Stripe checkout cs_live_…". A card billed outside the EU would have parked the
  receipt at the VAT gate with the tenant looking perfectly fine afterwards.
- **`GET /api/admin/vat/threshold` did NOT move, and that is correct.** NL is a
  domestic supply and the meter deliberately counts only cross-border B2C. The
  checklist above says "moved by the NET amount"; that only holds for a buyer in
  another member state. Corrected here rather than left to mislead.
- **Cancelling in the customer portal does not end a plan.** It sets
  `cancel_at_period_end`, so only `customer.subscription.updated` fires and the
  tier is deliberately kept — the customer paid for the month. Only an immediate
  cancellation in the dashboard fires `customer.subscription.deleted`, which is
  the one event Ko-fi could never send and the whole reason for the migration.
  Run it that way if you want that event covered; at period end it is a month's
  wait.
- **A refund shortens, it does not strip.** `plan_expires_at` moved back by
  exactly one `Period` (32 days) and nothing else, leaving the hourly
  `subscription-lapse` job as the single place that decides what a lapse means.
- Receipt and credit note were both issued on **attempt 0**, 19 s and 12 s after
  their events. Neither outbox had to retry.
- **PayPal cannot be offered for a subscription.** Stripe refuses it outright:
  *"The payment method `paypal` cannot be used in `subscription` mode."* It is
  enabled on the account and does appear on `/donate`, which is `mode=payment`.
  iDEAL, SEPA Direct Debit and Klarna are all accepted for subscriptions; Stripe's
  automatic selection offered card alone.

## What this proves that test mode cannot

Only three things, and they are the three that matter: the live secret key
works, the live signing secret matches what Stripe sends, and the webhook
endpoint URL is correct. Everything else — the events, the ordering, the VAT
gate, the documents, the emails — is already proven by `stripe-suite.py` in test
mode, which is the whole reason for leaving Ko-fi.
