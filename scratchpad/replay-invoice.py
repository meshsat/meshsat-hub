#!/usr/bin/env python3
"""Redeliver ONE genuine Stripe event that was lost to the dahlia field move.

EUR 9.00 was charged on 2026-09-11 and produced no VAT document: onInvoicePaid
could not resolve a tenant because the tenant_id had moved to
parent.subscription_details.metadata, which the Hub did not read. The event was
therefore never recorded in stripe_events -- `once` runs only after resolution
-- so replaying it is processed as new rather than swallowed as a duplicate.

This fetches the event from Stripe (so the body is Stripe's, not ours), signs
it with the endpoint's own secret exactly as Stripe does, and POSTs it to the
live webhook. It writes nothing directly: the Hub decides what to do with it,
which is the point -- this proves the deployed fix on the real payload.

Idempotent twice over: receipts.delivery_key is UNIQUE on the invoice id, and a
second run finds the event already in stripe_events.
"""
import hashlib, hmac, json, subprocess, sys, time, urllib.request, urllib.error

EVENT = sys.argv[1] if len(sys.argv) > 1 else "evt_1UEKmW4j5c6KcLizC95N8RtI"
HUB = "https://hub.meshsat.net"
CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]


def podenv(var):
    pods = [l.split("/")[1] for l in subprocess.run(
        CTX + ["get", "pods", "-o", "name"], capture_output=True, text=True
    ).stdout.splitlines() if l.startswith("pod/hub-")]
    if not pods:
        sys.exit("no hub pod")
    return subprocess.run(CTX + ["exec", pods[0], "--", "printenv", var],
                          capture_output=True, text=True).stdout.strip()


def main():
    sk = podenv("HUB_STRIPE_SECRET_KEY")
    wh = podenv("HUB_STRIPE_WEBHOOK_SECRET")
    path = podenv("HUB_STRIPE_PATH_SECRET")
    if not (sk and wh and path):
        sys.exit("missing one of the three Stripe secrets on the pod")

    # 1. The body comes from Stripe, not from us.
    req = urllib.request.Request("https://api.stripe.com/v1/events/" + EVENT)
    import base64
    req.add_header("Authorization", "Basic " + base64.b64encode((sk + ":").encode()).decode())
    with urllib.request.urlopen(req, timeout=60) as r:
        ev = json.loads(r.read().decode())

    obj = ev["data"]["object"]
    print(f"event      : {ev['id']}  ({ev['type']}, api {ev.get('api_version')})")
    print(f"invoice    : {obj.get('id')}  {obj.get('amount_paid')} {obj.get('currency')}")
    par = obj.get("parent", {}).get("subscription_details", {})
    print(f"tenant_id  : {par.get('metadata', {}).get('tenant_id')!r}  (the field that was not read)")

    body = json.dumps(ev, separators=(",", ":")).encode()
    ts = int(time.time())
    sig = hmac.new(wh.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()

    # 2. Deliver it exactly as Stripe would.
    post = urllib.request.Request(f"{HUB}/api/webhook/stripe/{path}", data=body, method="POST")
    post.add_header("Content-Type", "application/json")
    post.add_header("Stripe-Signature", f"t={ts},v1={sig}")
    try:
        with urllib.request.urlopen(post, timeout=60) as r:
            print(f"\ndelivered  : HTTP {r.status} {r.read().decode().strip()!r}")
    except urllib.error.HTTPError as e:
        print(f"\ndelivered  : HTTP {e.code} {e.read().decode()[:200]!r}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
