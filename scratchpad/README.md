# scratchpad — the probes that prove the live paths

These are the scripts that exercise production directly: the payment chain, the
signup journey, VAT, quotas, the lapse job. They were written against the real
Hub because the things they prove cannot be proved any other way — a Go test
with an in-memory fake cannot tell you that the live Stripe signing secret is
the one Stripe uses, or that a receipt reached a real inbox.

They lived in a session temp directory until 2026-09-11, while this repo's
CLAUDE.md cited them by name as durable artefacts. Two of them are the only
evidence that parts of the billing chain work at all, so they are in git now.

**None of them contains a secret.** Every one reads its credentials at runtime
from the running pod (`kubectl exec ... printenv`) or from the environment. Keep
it that way.

## The rules that matter

**A probe that drives a payment must seed a NON-EU billing country.** The VAT
gate in `internal/vat` parks a receipt for a buyer outside the EU *before* the
billing system is touched, which is what stops a probe from issuing a real
invoice. Two runs of `mail-probe.py` with `billing_country='NL'` sailed through
that gate, took `MSH2026-0001` and `MSH2026-0002` out of the gapless series and
emailed them to nobody, leaving the counter at 3 — so the first real customer
would have been given 0003. `refund-suite.py` and `stripe-suite.py` are the
deliberate exceptions, because issuing the document is the thing they exist to
prove; both purge and re-assert the counters afterwards.

**Anything that issues a document must clean up after itself.** Purge the
Invoice Ninja client, rewind `invoice_number_counter` and `credit_number_counter`
to 1, and verify `email_style_custom` survived by sha256 — a purge that takes
the company's email shell with it is worse than the documents it removed.

**Clean up in Stripe before deleting the tenant row.** Delete the tenant first
and the Hub keeps receiving subscription events for an account that no longer
exists. It records those as unattributed, correctly — and on 2026-09-11 that
litter fired a tier-1 page minutes after the alert was deployed, three times,
for no money at all. A probe must never manufacture the alerts it exists to
help prove. Cancel in Stripe, wait, then purge.

**Test MO messages cost money.** Routing has wildcard `Relay MO -> SMS` rules,
so a test message can text a real phone. See rule 14 in CLAUDE.md.

## What each one is for

| script | what it proves | money |
|---|---|---|
| `stripe-live-check.py` | the live Stripe path end to end — the pinned path secret, the signing secret, checkout, the subscription lifecycle through the real webhook. Also probes all three public edges. | none: a Checkout session is free and a trialing subscription charges nothing |
| `stripe-suite.py` | the full chain in Stripe **test** mode: subscription, invoice, receipt, document, cancellation, refund, credit note | none (test key) |
| `refund-suite.py` | a real refund produces a real credit note, the ledger nets to zero, the plan moves back by exactly one period, and both emails arrive | **real** — purges afterwards |
| `donation-check.py` | a donation grants no tier and is outside the scope of BTW | **real** if driven |
| `journey-suite.py` | approval → first sign-in → tenant creation, through authentik's real OIDC round trip | none |
| `captcha-e2e.py` | the enrollment flow reaches the Turnstile stage — enrollment cannot be scripted past it, which is the CAPTCHA working | none |
| `enroll-suite.py` | the enrollment policies: disposable domains, terms, the pause switch | none |
| `vat-suite.py` | the country rules and the threshold meter | none |
| `quota-suite.py` | the device ceiling gates registration and **nothing else** — an SOS still gets through | none |
| `lapse-suite.py` | the lapse job warns and then ends, at the right moments | none |
| `cancel-diagnosis.py` | tells a lost webhook delivery apart from a Hub defect, using Stripe's own `pending_webhooks` | none |
| `replay-invoice.py` | redelivers one genuine event, signed as Stripe signs it | none |
| `cleanup-test-payments.py` | the purge-and-rewind used after any probe that issued a document | — |
| `mail-probe.py` | the transactional mail path end to end | ⚠ seed a non-EU country |
| `fmt-oracle.php` | regenerates the money-formatting golden test from the running Invoice Ninja's own arithmetic | none |

`kofi-suite.py` is deliberately not here: `internal/kofi` no longer exists.

## Running them

They expect a working `kubectl --context notrf01` and the Hub deployed. Most
read what they need from the pod:

```bash
python3 scratchpad/stripe-live-check.py
```

`stripe-suite.py` additionally wants a Stripe test key in
`scratchpad/.stripe-test-key`, which is gitignored and does not exist. Note its
docstring claims it refuses to run against a live key and **it does not** — the
check is computed and only printed. Implement that before pointing it anywhere.
