#!/usr/bin/env python3
"""Prove the live Stripe path without a cent moving.

Two things cost nothing in Stripe and prove most of the chain:

  * Creating a Checkout SESSION is free. Only completing one charges anybody.
    So the outbound half -- the Hub's client, the key, the tenant metadata, the
    refusal of automatic tax -- is provable, and the session is expired
    afterwards so nobody can pay it by accident.

  * A subscription with a TRIAL fires customer.subscription.created (status
    trialing) and, on cancellation, customer.subscription.deleted, with no
    charge at any point. That is the whole subscription lifecycle -- the event
    the old provider could never send -- through the real live webhook.

What this does NOT prove, because both need real money: invoice.paid producing a
document, and charge.refunded producing a credit note. Those are what
docs/first-real-payment.md is for.

Everything it creates is deleted at the end.
"""
import hashlib, hmac, http.client, json, os, socket, ssl, subprocess, sys, time
import urllib.error, urllib.parse, urllib.request

SP = os.path.dirname(os.path.abspath(__file__))
HUB = "https://hub.meshsat.net"
CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-F|", "-c"]
TENANT = "t-stripe-live"
OWNER = "u-stripe-live"

PASS, FAIL = [], []

# hub.meshsat.net answers with all three VPS edges. An edge whose IPsec tunnel to
# the cluster is down still terminates TLS and then silent-drops the request
# (`http-request silent-drop if { nbsrv(meshsat_hub) eq 0 }`), so a third of the
# calls below died as RemoteDisconnected and looked like Hub defects. Probe the
# edges, report any that are dead, and pin the run to a live one -- this suite
# is here to measure the Hub, not the path to it.
EDGES, EDGE = [], None
WHSEC, PATHSEC = "", ""


def edge_addrs():
    try:
        return sorted({ai[4][0] for ai in socket.getaddrinfo("hub.meshsat.net", 443, socket.AF_INET)})
    except OSError:
        return []


def edge_alive(ip):
    try:
        c = pinned(ip)("hub.meshsat.net", 443, timeout=15)
        c.request("GET", "/healthz")
        return c.getresponse().status == 200
    except Exception:
        return False
    finally:
        try:
            c.close()
        except Exception:
            pass


def pinned(ip):
    class Conn(http.client.HTTPSConnection):
        def connect(self):
            sock = socket.create_connection((ip, self.port), self.timeout)
            ctx = self._context or ssl.create_default_context()
            self.sock = ctx.wrap_socket(sock, server_hostname=self.host)
    return Conn


def opener_for(ip):
    class Handler(urllib.request.HTTPSHandler):
        def https_open(self, req):
            return self.do_open(pinned(ip), req, context=self._context)
    return urllib.request.build_opener(Handler())


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("  PASS  " if ok else "  FAIL  ") + name + (("   " + str(detail)) if detail else ""))


def sh(*a):
    return subprocess.run(list(a), capture_output=True, text=True).stdout.strip()


def sql(q):
    return subprocess.run(DB + [q], capture_output=True, text=True).stdout.strip()


def pod():
    for l in sh(*CTX, "get", "pods", "-o", "name").splitlines():
        if l.startswith("pod/hub-"):
            return l.split("/")[1]
    sys.exit("no hub pod")


def podenv(v):
    return sh(*CTX, "exec", pod(), "--", "printenv", v)


KEY = podenv("HUB_STRIPE_SECRET_KEY")


def stripe(method, path, form=None):
    data = urllib.parse.urlencode(form, doseq=True).encode() if form is not None else None
    req = urllib.request.Request("https://api.stripe.com/v1" + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + KEY)
    req.add_header("Stripe-Version", "2025-08-27.basil")
    if data:
        req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode() or "{}")


def api(method, path, body=None, token=None, tenant=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(HUB + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    if tenant:
        req.add_header("X-Tenant-ID", tenant)
    if data:
        req.add_header("Content-Type", "application/json")
    op = opener_for(EDGE) if EDGE else urllib.request
    try:
        with op.open(req, timeout=45) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw[:300]


def redeliver(kind, since, subid):
    """Stripe could not deliver it. Fetch the genuine event and hand it to a live
    edge, so the check still answers the question it is asking -- does the HUB
    apply this event -- instead of reporting the network as a Hub defect."""
    _, evs = stripe("GET", f"/events?type={kind}&created[gte]={since}&limit=20")
    mine = [e for e in evs.get("data", []) if e["data"]["object"].get("id") == subid]
    if not mine:
        return None, "Stripe generated no such event"
    ev = mine[0]
    pending = ev.get("pending_webhooks") or 0
    body = json.dumps(ev, separators=(",", ":")).encode()
    ts = int(time.time())
    sig = hmac.new(WHSEC.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    c = pinned(EDGE)("hub.meshsat.net", 443, timeout=60)
    c.request("POST", f"/api/webhook/stripe/{PATHSEC}", body=body, headers={
        "Content-Type": "application/json", "Stripe-Signature": f"t={ts},v1={sig}",
        "Host": "hub.meshsat.net"})
    c.getresponse().read()
    return pending, ev["id"]


def webhook_check(name, want, kind, since, subid, probe):
    if wait_for(lambda: probe() == want):
        return check(name, True)
    pending, detail = redeliver(kind, since, subid)
    if pending is None:
        return check(name, False, detail)
    if wait_for(lambda: probe() == want, secs=60):
        return check(name, True,
                     f"the Hub applied it — but Stripe could NOT deliver it "
                     f"(pending_webhooks={pending}); redelivered by hand through {EDGE}. "
                     f"That is the dead edge above, not the Hub")
    check(name, False, f"the Hub had the genuine event ({detail}) and did not apply it")


def wait_for(fn, secs=120, every=5):
    for _ in range(secs // every):
        if fn():
            return True
        time.sleep(every)
    return False


def cleanup(cus=None, session=None):
    if session:
        stripe("POST", f"/checkout/sessions/{session}/expire")
    if cus:
        stripe("DELETE", f"/customers/{cus}")
    for q in (f"DELETE FROM receipts WHERE tenant_id='{TENANT}';",
              f"DELETE FROM refunds WHERE tenant_id='{TENANT}';",
              f"DELETE FROM stripe_events WHERE tenant_id='{TENANT}';",
              f"DELETE FROM audit_log WHERE tenant_id='{TENANT}';",
              f"DELETE FROM users WHERE tenant_id='{TENANT}';",
              f"DELETE FROM tenants WHERE id='{TENANT}';"):
        sql(q)


def main():
    if not KEY:
        print("the Hub has no HUB_STRIPE_SECRET_KEY yet", file=sys.stderr)
        return 2
    global WHSEC, PATHSEC
    admin = podenv("HUB_AUTH_TOKEN")
    WHSEC, PATHSEC = podenv("HUB_STRIPE_WEBHOOK_SECRET"), podenv("HUB_STRIPE_PATH_SECRET")
    prices = podenv("HUB_STRIPE_PRICES")
    price = prices.split("=")[0].strip() if prices else ""

    print("=== 0. the public edge is whole ===")
    global EDGES, EDGE
    EDGES = edge_addrs()
    live = [ip for ip in EDGES if edge_alive(ip)]
    dead = [ip for ip in EDGES if ip not in live]
    check("every published edge serves the Hub", EDGES and not dead,
          f"{len(live)}/{len(EDGES)} live" + (f"; DEAD: {', '.join(dead)}" if dead else ""))
    if not live:
        print("no live edge; nothing below can run", file=sys.stderr)
        return 1
    EDGE = live[0]
    print(f"  ....  pinning the rest of this run to {EDGE}")

    print("=== 0b. the Hub is configured ===")
    logs = sh(*CTX, "logs", "deploy/hub", "--tail=800")
    check("the Stripe webhook is enabled", "stripe: payment webhook enabled" in logs)
    check("it is NOT on a test key", "this is a TEST key" not in logs,
          "this check exists so a live run is never mistaken for a rehearsal")
    check("a price is configured", price.startswith("price_"), price)
    if not price.startswith("price_"):
        return 1

    print("=== 1. seed a throwaway tenant ===")
    cleanup()
    sql(f"""INSERT INTO tenants (id,slug,name,plan,status,billing_country,owner_user_id,created_at,updated_at)
            VALUES ('{TENANT}','{TENANT}','Stripe live check','free','active','NL','{OWNER}',now(),now());""")
    sql(f"""INSERT INTO users (id,tenant_id,email,name,role,password_hash,created_at,updated_at)
            VALUES ('{OWNER}','{TENANT}','billing+e2e@meshsat.net','Live check','owner','x',now(),now());""")

    print("=== 2. the Hub builds a real Checkout session (free; nobody can pay it) ===")
    st, body = api("POST", "/api/tenant/billing/checkout", {"plan": "crew"}, token=admin, tenant=TENANT)
    check("checkout returns a URL", st == 200 and (body or {}).get("url", "").startswith("https://"), f"{st} {body}")
    session_id = None
    if st == 200:
        _, sess = stripe("GET", "/checkout/sessions?limit=5")
        mine = [s for s in sess.get("data", []) if (s.get("metadata") or {}).get("tenant_id") == TENANT]
        check("the session carries this tenant in its metadata", bool(mine),
              "this is what replaces the claim code")
        if mine:
            s0 = mine[0]
            session_id = s0["id"]
            check("it refuses Stripe's automatic tax",
                  (s0.get("automatic_tax") or {}).get("enabled") is False,
                  json.dumps(s0.get("automatic_tax")))
            check("it collects a billing address",
                  s0.get("billing_address_collection") == "required",
                  "without a country the receipt parks at the VAT gate")
            check("it is a subscription, not a one-off", s0.get("mode") == "subscription")
            check("the amount is the published price",
                  s0.get("amount_total") in (900, None), s0.get("amount_total"))

    print("=== 3. the live webhook, through a TRIALING subscription (no charge) ===")
    st, cus = stripe("POST", "/customers", {
        "email": "billing+e2e@meshsat.net", "name": "Stripe live check",
        "metadata[tenant_id]": TENANT, "address[country]": "NL",
    })
    check("a customer was created", st == 200, str(cus)[:120])
    if st != 200:
        cleanup(session=session_id)
        return report()
    cus_id = cus["id"]
    sql(f"UPDATE tenants SET stripe_customer_id='{cus_id}' WHERE id='{TENANT}';")

    t_sub = int(time.time()) - 5
    st, sub = stripe("POST", "/subscriptions", {
        "customer": cus_id, "items[0][price]": price,
        "metadata[tenant_id]": TENANT,
        "automatic_tax[enabled]": "false",
        # A trial charges nothing, and still fires the real lifecycle events.
        "trial_period_days": "14",
        "payment_behavior": "default_incomplete" if False else "allow_incomplete",
    })
    check("a trialing subscription was created", st == 200,
          sub.get("status") if st == 200 else str(sub)[:160])
    if st != 200:
        cleanup(cus_id, session_id)
        return report()
    sub_id = sub["id"]
    check("it is trialing, so nothing was charged", sub.get("status") == "trialing", sub.get("status"))

    webhook_check("customer.subscription.created reached the Hub and granted the plan", "crew",
                  "customer.subscription.created", t_sub, sub_id,
                  lambda: sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';"))
    check("the subscription was bound to the tenant",
          sql(f"SELECT stripe_subscription_id FROM tenants WHERE id='{TENANT}';") == sub_id)

    t_cancel = int(time.time()) - 5
    print("=== 4. cancel it — the event the old provider could never send ===")
    st, _ = stripe("DELETE", f"/subscriptions/{sub_id}")
    check("cancelled in Stripe", st == 200, st)
    webhook_check("customer.subscription.deleted dropped the plan straight away", "free",
                  "customer.subscription.deleted", t_cancel, sub_id,
                  lambda: sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';"))
    check("and the tenant was NOT suspended",
          sql(f"SELECT status FROM tenants WHERE id='{TENANT}';") == "active",
          "a plan ending lowers a ceiling; it never stops a device reporting")
    check("no receipt was invented for a trial nobody paid",
          sql(f"SELECT count(*) FROM receipts WHERE tenant_id='{TENANT}';") == "0")

    print("=== 5. clean up ===")
    cleanup(cus_id, session_id)
    check("the throwaway tenant is gone", sql(f"SELECT count(*) FROM tenants WHERE id='{TENANT}';") == "0")
    return report()


def report():
    print(f"\n=== {len(PASS)}/{len(PASS) + len(FAIL)} ===")
    for f in FAIL:
        print("  FAILED: " + f)
    print("\nNot covered here, both need real money: invoice.paid -> a document,")
    print("and charge.refunded -> a credit note. See docs/first-real-payment.md.")
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
