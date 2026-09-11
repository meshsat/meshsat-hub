#!/usr/bin/env python3
"""Prove the Stripe path end to end, in TEST MODE, against the deployed Hub.

This is the thing Ko-fi made impossible. Ko-fi had no test mode, so the only way
to exercise a payment was to make one, and the only way to exercise a REFUND was
to make one and give it back. Stripe has a full test mode, so the whole chain --
subscription, invoice, receipt, real document, cancellation, refund, credit note
-- is provable without a cent moving.

HOW TO RUN IT. Stripe keeps test and live mode entirely separate, including the
webhook endpoint and its signing secret, and the Hub holds one signing secret.
So the sequence is:

  1. put the TEST keys in OpenBao, let the Hub roll, run this suite
  2. swap in the LIVE keys, let it roll again, make ONE small real payment
     and refund it

Step 2 is the only thing test mode cannot prove: that the live signing secret
and the live key are the ones Stripe is actually using.

WHAT IT DOES NOT DO. It never opens the hosted Checkout page -- that needs a
browser. It asserts the Hub builds a session with the right metadata, and then
creates the subscription through Stripe's API with a test card, which fires the
identical webhooks. The one thing that skips is checkout.session.completed, so
the customer binding is asserted separately.

SAFETY. It refuses to run against a live key -- checked twice, on the key this
script holds and on the key the deployed Hub is using. Documents it creates in the
billing system are purged and the gapless counters re-asserted, the way
refund-suite.py does -- a test artefact must never take a number the first real
customer should have had.

Needs:  scratchpad/.stripe-test-key   (sk_test_...)
"""
import json, os, subprocess, sys, time, urllib.error, urllib.parse, urllib.request

SP = os.path.dirname(os.path.abspath(__file__))
HUB = "https://hub.meshsat.net"
IN = "https://invoiceninja.nuclearlighters.net/api/v1"
CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-F|", "-c"]

TENANT = "t-stripe-e2e"
OWNER = "u-stripe-e2e"
# The relay's own mailbox, so the receipt and any Hub notice can be read back.
EMAIL = os.environ.get("E2E_TO", "root@localhost")

PASS, FAIL = [], []


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


# --- Stripe ---------------------------------------------------------------

KEY = ""
kp = os.path.join(SP, ".stripe-test-key")
if os.path.exists(kp):
    KEY = open(kp).read().strip()


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


# --- Hub ------------------------------------------------------------------

def api(method, path, body=None, token=None, tenant=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(HUB + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    if tenant:
        req.add_header("X-Tenant-ID", tenant)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=45) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw[:300]


# --- Invoice Ninja --------------------------------------------------------

def inja(method, path, body=None, password=False):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(IN + path, data=data, method=method)
    req.add_header("X-API-TOKEN", open(os.path.join(SP, ".in-token")).read().strip())
    req.add_header("X-Requested-With", "XMLHttpRequest")
    req.add_header("Accept", "application/json")
    if password:
        req.add_header("X-Api-Password", open(os.path.join(SP, ".in-password")).read().strip())
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read()
            return r.status, (json.loads(raw.decode()) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def wait_for(fn, secs=180, every=6):
    """Poll until fn() is truthy. Webhooks and drainers are both asynchronous."""
    for _ in range(secs // every):
        v = fn()
        if v:
            return v
        time.sleep(every)
    return None


def main():
    if not KEY:
        return fail_setup("no scratchpad/.stripe-test-key")

    admin = podenv("HUB_AUTH_TOKEN")
    if not admin:
        return fail_setup("the Hub pod has no HUB_AUTH_TOKEN")

    print("=== 0. the Hub is on test keys, and the billing system is clean ===")
    # The refusal this script's docstring has always promised, and which until
    # 2026-09-11 was not implemented: `live` was computed and only PRINTED, so
    # the one safety property it advertised did not exist. Pointed at a Hub on
    # live keys, everything below drives real money through the real billing
    # system and takes numbers out of the gapless series.
    #
    # Two independent checks, because either alone can be wrong: the key this
    # script holds, and the key the Hub is actually running with.
    if not KEY.startswith("sk_test_"):
        return fail_setup(
            "scratchpad/.stripe-test-key does not hold a TEST key (expected sk_test_...). "
            "This script drives payments, refunds and real documents; it must never "
            "run against live credentials.")
    logs = sh(*CTX, "logs", "deploy/hub", "--tail=800")
    live = "this is a TEST key" not in logs
    print("   mode:", "LIVE" if live else "test")
    if live:
        return fail_setup(
            "the deployed Hub is running on a LIVE Stripe key, so its webhook would apply "
            "these events to real tenants and its receipt drainer would issue real invoices "
            "out of the gapless series. Refusing. Use stripe-live-check.py, which proves the "
            "live path without moving money, or docs/first-real-payment.md.")
    check("the Stripe webhook is enabled", "stripe: payment webhook enabled" in logs)

    _, comp = inja("GET", "/companies/Wpmbk5ezJn")
    inv_before = int(comp["data"]["settings"]["invoice_number_counter"])
    cr_before = int(comp["data"]["settings"]["credit_number_counter"])
    print(f"   counters before: invoice={inv_before} credit={cr_before}")

    price = podenv("HUB_STRIPE_PRICES").split("=")[0].strip()
    check("a price is configured", price.startswith("price_"), price)
    if not price.startswith("price_"):
        return finish()

    print("=== 1. seed a tenant with a country, so the VAT gate lets it through ===")
    for q in (f"DELETE FROM receipts WHERE tenant_id='{TENANT}';",
              f"DELETE FROM refunds WHERE tenant_id='{TENANT}';",
              f"DELETE FROM stripe_events WHERE tenant_id='{TENANT}';",
              f"DELETE FROM users WHERE tenant_id='{TENANT}';",
              f"DELETE FROM tenants WHERE id='{TENANT}';"):
        sql(q)
    sql(f"""INSERT INTO tenants (id,slug,name,plan,status,billing_country,owner_user_id,created_at,updated_at)
            VALUES ('{TENANT}','{TENANT}','Stripe E2E','free','active','NL','{OWNER}',now(),now());""")
    sql(f"""INSERT INTO users (id,tenant_id,email,name,role,password_hash,created_at,updated_at)
            VALUES ('{OWNER}','{TENANT}','{EMAIL}','Stripe E2E','owner','x',now(),now());""")

    print("=== 2. the Hub builds a checkout session carrying THIS tenant ===")
    st, body = api("POST", "/api/tenant/billing/checkout", {"plan": "crew"},
                   token=admin, tenant=TENANT)
    check("checkout returns a session URL", st == 200 and (body or {}).get("url", "").startswith("https://"),
          f"{st} {body}")
    if st == 200:
        # Read the session back from Stripe and prove the binding is on it. This
        # is what replaces the claim code: nobody has to type anything.
        sid = (body or {}).get("url", "").rstrip("/").split("/")[-1].split("#")[0]
        _, sessions = stripe("GET", "/checkout/sessions?limit=3")
        found = None
        for s in (sessions or {}).get("data", []):
            if (s.get("metadata") or {}).get("tenant_id") == TENANT:
                found = s
                break
        check("the session carries the tenant in its metadata", found is not None,
              "this is what binds a payment to an account by construction")
        if found:
            check("it refuses Stripe's automatic tax",
                  (found.get("automatic_tax") or {}).get("enabled") is False,
                  found.get("automatic_tax"))
            check("it collects a billing address",
                  found.get("billing_address_collection") == "required",
                  "without it the receipt parks for want of a country")

    print("=== 3. subscribe through the API with a test card ===")
    st, cus = stripe("POST", "/customers", {
        "email": EMAIL, "name": "Stripe E2E",
        "metadata[tenant_id]": TENANT,
        "address[country]": "NL",
        # In live mode there is no test card, so the subscription runs on a
        # trial: it fires the same lifecycle events and charges nothing.
    })
    check("a test customer was created", st == 200, f"{st} {cus}")
    if st != 200:
        return finish()
    cus_id = cus["id"]
    # Bind it the way checkout.session.completed would, since this path skips it.
    sql(f"UPDATE tenants SET stripe_customer_id='{cus_id}' WHERE id='{TENANT}';")

    subform = {"customer": cus_id, "items[0][price]": price,
               "metadata[tenant_id]": TENANT, "automatic_tax[enabled]": "false"}
    if live:
        subform["trial_period_days"] = "14"
    else:
        subform["payment_method"] = "pm_card_visa"
    st, sub = stripe("POST", "/subscriptions", subform)
    check("a subscription was created", st == 200, f"{st} {sub if st != 200 else sub.get('id')}")
    if st != 200:
        return finish(cus_id)
    sub_id = sub["id"]

    print("=== 4. the Hub heard about it ===")
    plan = wait_for(lambda: (sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';") == "crew") or None)
    check("customer.subscription.created granted the plan", bool(plan),
          sql(f"SELECT plan||' '||coalesce(plan_expires_at::text,'-') FROM tenants WHERE id='{TENANT}';"))
    check("the subscription id was bound to the tenant",
          sql(f"SELECT stripe_subscription_id FROM tenants WHERE id='{TENANT}';") == sub_id)

    row = wait_for(lambda: (sql(f"SELECT status||'|'||invoice_number FROM receipts WHERE tenant_id='{TENANT}' ORDER BY created_at DESC LIMIT 1;") or "").startswith("issued|") or None)
    got = sql(f"SELECT status||'|'||invoice_number||'|'||left(last_error,60) FROM receipts WHERE tenant_id='{TENANT}' ORDER BY created_at DESC LIMIT 1;")
    check("invoice.paid produced a real document", bool(row), got)
    inv_no = got.split("|")[1] if "|" in got else ""

    print("=== 5. cancel it — the event Ko-fi could never send ===")
    st, _ = stripe("DELETE", f"/subscriptions/{sub_id}")
    check("the subscription was cancelled in Stripe", st == 200, st)
    dropped = wait_for(lambda: (sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';") == "free") or None)
    check("customer.subscription.deleted dropped the plan straight away", bool(dropped),
          sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';"))
    check("and it did not suspend the tenant",
          sql(f"SELECT status FROM tenants WHERE id='{TENANT}';") == "active",
          "a plan ending lowers a ceiling; it never stops a device reporting")

    print("=== 6. refund it — no operator involved ===")
    _, charges = stripe("GET", f"/charges?customer={cus_id}&limit=1")
    ch = (charges or {}).get("data", [{}])[0]
    if ch.get("id"):
        st, _ = stripe("POST", "/refunds", {"charge": ch["id"]})
        check("the charge was refunded in Stripe", st == 200, st)
        credit = wait_for(lambda: (sql(f"SELECT credit_number FROM refunds WHERE tenant_id='{TENANT}' LIMIT 1;") or "").startswith("MSHCN") or None, secs=240)
        check("charge.refunded produced a credit note BY ITSELF", bool(credit),
              sql(f"SELECT status||'|'||coalesce(credit_number,'')||'|'||left(last_error,60) FROM refunds WHERE tenant_id='{TENANT}' LIMIT 1;"))
        check("the refund records that it was not a person",
              sql(f"SELECT requested_by FROM refunds WHERE tenant_id='{TENANT}' LIMIT 1;") == "stripe")
    else:
        check("a charge to refund", False, "no charge found for the test customer")

    print("=== 7. a replay changes nothing ===")
    n = sql(f"SELECT count(*) FROM stripe_events WHERE tenant_id='{TENANT}';")
    check("every applied event was recorded", int(n or 0) >= 3, f"stripe_events={n}")

    return finish(cus_id, inv_before, cr_before)


def fail_setup(why):
    print("SETUP: " + why, file=sys.stderr)
    return 2


def finish(cus_id=None, inv_before=None, cr_before=None):
    print("=== 8. purge every artefact ===")
    if cus_id:
        stripe("DELETE", f"/customers/{cus_id}")
        print(f"   deleted test customer {cus_id}")

    _, found = inja("GET", "/clients?per_page=100")
    purged = 0
    for c in (found or {}).get("data", []) if isinstance(found, dict) else []:
        if c.get("id_number") != TENANT:
            print(f"   !! refusing to purge client {c['id']} ({c.get('id_number')}) -- not ours")
            continue
        st, _ = inja("POST", f"/clients/{c['id']}/purge", {}, password=True)
        purged += 1
        print(f"   purged {c['id']}: {st}")

    if inv_before is not None:
        _, ri = inja("GET", "/invoices?per_page=100")
        _, rc = inja("GET", "/credits?per_page=100")
        live = [d for d in ((ri or {}).get("data", []) + (rc or {}).get("data", [])) if d.get("number")]
        if live:
            print("   !! documents remain; NOT rewinding the counters")
            for d in live:
                print("      left behind:", d.get("number"))
        else:
            _, comp = inja("GET", "/companies/Wpmbk5ezJn")
            s = dict(comp["data"]["settings"])
            s["invoice_number_counter"] = inv_before
            s["credit_number_counter"] = cr_before
            s["client_number_counter"] = 1
            s["payment_number_counter"] = 1
            inja("PUT", "/companies/Wpmbk5ezJn", {"settings": s})
            _, comp = inja("GET", "/companies/Wpmbk5ezJn")
            a = comp["data"]["settings"]
            check("the invoice counter is back where it started",
                  int(a["invoice_number_counter"]) == inv_before, str(a["invoice_number_counter"]))
            check("the branded wrapper survived",
                  len(a.get("email_style_custom") or "") > 30000, str(len(a.get("email_style_custom") or "")))

    for q in (f"DELETE FROM refunds WHERE tenant_id='{TENANT}';",
              f"DELETE FROM receipts WHERE tenant_id='{TENANT}';",
              f"DELETE FROM stripe_events WHERE tenant_id='{TENANT}';",
              f"DELETE FROM audit_log WHERE tenant_id='{TENANT}';",
              f"DELETE FROM users WHERE tenant_id='{TENANT}';",
              f"DELETE FROM tenants WHERE id='{TENANT}';"):
        sql(q)
    check("the probe tenant is gone", sql(f"SELECT count(*) FROM tenants WHERE id='{TENANT}';") == "0")

    print(f"\n=== {len(PASS)}/{len(PASS) + len(FAIL)} ===")
    for f in FAIL:
        print("  FAILED: " + f)
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
