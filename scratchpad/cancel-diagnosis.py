#!/usr/bin/env python3
"""Why does customer.subscription.deleted not drop the plan?

Two candidates, and they need telling apart:
  (a) the event never arrives -- one of the three published edges silent-drops
      every request, so about a third of Stripe's deliveries hit a black hole;
  (b) the Hub receives it and does not apply it -- a real defect in onSubscription.

So: run the lifecycle, wait, and if the plan has not moved, fetch the very event
Stripe generated and redeliver it BY HAND through an edge known to be alive. If
the hand delivery drops the plan, it was (a). If it does not, it is (b).
No money moves: the subscription is a trial throughout.
"""
import base64, hashlib, hmac, http.client, json, socket, ssl, subprocess, sys, time
import urllib.error, urllib.parse, urllib.request

CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-F|", "-c"]
TENANT, OWNER = "t-cancel-dx", "u-cancel-dx"


def sh(*a):
    return subprocess.run(list(a), capture_output=True, text=True).stdout.strip()


def sql(q):
    return subprocess.run(DB + [q], capture_output=True, text=True).stdout.strip()


def podenv(v):
    p = [l.split("/")[1] for l in sh(*CTX, "get", "pods", "-o", "name").splitlines()
         if l.startswith("pod/hub-")]
    return sh(*CTX, "exec", p[0], "--", "printenv", v) if p else ""


def pinned(ip):
    class Conn(http.client.HTTPSConnection):
        def connect(self):
            s = socket.create_connection((ip, self.port), self.timeout)
            self.sock = (self._context or ssl.create_default_context()).wrap_socket(
                s, server_hostname=self.host)
    return Conn


def live_edge():
    for ip in sorted({a[4][0] for a in socket.getaddrinfo("hub.meshsat.net", 443, socket.AF_INET)}):
        try:
            c = pinned(ip)("hub.meshsat.net", 443, timeout=15)
            c.request("GET", "/healthz")
            if c.getresponse().status == 200:
                return ip
        except Exception:
            pass
    sys.exit("no live edge")


def stripe(method, path, form=None, key=None):
    data = urllib.parse.urlencode(form, doseq=True).encode() if form is not None else None
    r = urllib.request.Request("https://api.stripe.com/v1" + path, data=data, method=method)
    r.add_header("Authorization", "Basic " + base64.b64encode((key + ":").encode()).decode())
    if data:
        r.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode() or "{}")


def plan():
    return sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';")


def wait_plan(want, secs=100):
    for _ in range(secs // 5):
        if plan() == want:
            return True
        time.sleep(5)
    return False


def purge():
    for q in (f"DELETE FROM stripe_events WHERE tenant_id='{TENANT}';",
              f"DELETE FROM audit_log WHERE tenant_id='{TENANT}';",
              f"DELETE FROM receipts WHERE tenant_id='{TENANT}';",
              f"DELETE FROM users WHERE tenant_id='{TENANT}';",
              f"DELETE FROM tenants WHERE id='{TENANT}';"):
        sql(q)


def main():
    sk, wh, path = podenv("HUB_STRIPE_SECRET_KEY"), podenv("HUB_STRIPE_WEBHOOK_SECRET"), podenv("HUB_STRIPE_PATH_SECRET")
    price = (podenv("HUB_STRIPE_PRICES") or "").split("=")[0].strip()
    if not (sk and wh and path and price.startswith("price_")):
        sys.exit("missing Stripe configuration on the pod")
    edge = live_edge()
    print(f"live edge  : {edge}")

    purge()
    sql(f"""INSERT INTO tenants (id,slug,name,plan,status,billing_country,owner_user_id,created_at,updated_at)
            VALUES ('{TENANT}','{TENANT}','Cancel diagnosis','free','active','NL','{OWNER}',now(),now());""")
    sql(f"""INSERT INTO users (id,tenant_id,email,name,role,password_hash,created_at,updated_at)
            VALUES ('{OWNER}','{TENANT}','billing+dx@meshsat.net','Cancel dx','owner','x',now(),now());""")

    st, cus = stripe("POST", "/customers", {
        "email": "billing+dx@meshsat.net", "name": "Cancel diagnosis",
        "metadata[tenant_id]": TENANT, "address[country]": "NL"}, key=sk)
    cus_id = cus["id"]
    sql(f"UPDATE tenants SET stripe_customer_id='{cus_id}' WHERE id='{TENANT}';")
    st, sub = stripe("POST", "/subscriptions", {
        "customer": cus_id, "items[0][price]": price, "metadata[tenant_id]": TENANT,
        "automatic_tax[enabled]": "false", "trial_period_days": "14",
        "payment_behavior": "allow_incomplete"}, key=sk)
    sub_id = sub["id"]
    t_created = int(time.time()) - 20
    print(f"subscription: {sub_id} {sub.get('status')}")
    granted = wait_plan("crew")
    print(f"granted    : {granted}  plan={plan()}")

    def evidence(kind, want, since, subid):
        """Ask Stripe whether IT still has the delivery pending, then hand-deliver."""
        st, evs = stripe("GET", f"/events?type={kind}&created[gte]={since}&limit=20", key=sk)
        mine = [e for e in evs.get("data", []) if e["data"]["object"].get("id") == subid]
        if not mine:
            print(f"  {kind}: Stripe generated NO such event")
            return None
        ev = mine[0]
        print(f"  {kind}: {ev['id']}  pending_webhooks={ev.get('pending_webhooks')}"
              f"  (>0 means Stripe has a delivery it could not complete)")
        body = json.dumps(ev, separators=(",", ":")).encode()
        ts = int(time.time())
        sig = hmac.new(wh.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
        c = pinned(edge)("hub.meshsat.net", 443, timeout=60)
        c.request("POST", f"/api/webhook/stripe/{path}", body=body, headers={
            "Content-Type": "application/json", "Stripe-Signature": f"t={ts},v1={sig}",
            "Host": "hub.meshsat.net"})
        r = c.getresponse()
        print(f"  hand-delivered through {edge}: HTTP {r.status} {r.read().decode().strip()[:80]!r}")
        ok = wait_plan(want, 60)
        print(f"  -> plan={plan()}  applied={ok}")
        return ok

    if not granted:
        print("--- the grant did not arrive; is it Stripe's delivery or the Hub? ---")
        a = evidence("customer.subscription.created", "crew", t_created, sub_id)
        print("VERDICT (created) : " + ("DELIVERY -- the Hub applies the genuine event when it reaches it."
                                        if a else "THE HUB -- it had the event and did not apply it."))

    t0 = int(time.time()) - 5
    st, _ = stripe("DELETE", f"/subscriptions/{sub_id}", key=sk)
    print(f"cancelled  : HTTP {st}")
    if plan() == "crew":
        dropped = wait_plan("free")
        print(f"auto-drop  : {dropped}  plan={plan()}")
        if not dropped:
            print("--- the cancellation did not arrive; is it Stripe's delivery or the Hub? ---")
            b = evidence("customer.subscription.deleted", "free", t0, sub_id)
            print("VERDICT (deleted) : " + ("DELIVERY -- the Hub applies the genuine event when it reaches it."
                                            if b else "THE HUB -- it had the event and did not apply it."))
    else:
        print("auto-drop  : n/a, the plan was never granted")

    stripe("DELETE", f"/customers/{cus_id}", key=sk)
    purge()
    print("cleaned up")
    return 0


if __name__ == "__main__":
    sys.exit(main())
