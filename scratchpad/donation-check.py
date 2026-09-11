#!/usr/bin/env python3
"""Check what a real donation actually produced, after somebody has paid one.

This asserts rather than drives: press the Support MeshSat button on
meshsat.net, pay, then run this. It finds the most recent donation receipt and
checks the whole chain behind it.

The thing under test is the tax treatment, which is the part that changed. A
donation buys nothing, so there is no counter-performance, so it is outside the
scope of BTW (internal/vat.ForDonation, and the Belastingdienst's own rule on
vrijwillige bijdragen). That has three consequences this checks:

  * the document carries NO VAT line -- not 21%, not 0% of a taxable supply,
    but no tax at all;
  * it was never PARKED for the giver's country, however far outside the EU
    they are, because place of supply is a question about a supply;
  * it does NOT move the EUR 10 000 cross-border threshold meter.

And the thing that must not have happened: no plan granted, to anybody.

Nothing is purged here. A donation is real income and its document is the
giver's record; deciding whether to keep it is a separate call from whether it
was produced correctly.
"""
import json, os, subprocess, sys, urllib.error, urllib.request

SP = os.path.dirname(os.path.abspath(__file__))
HUB = "https://hub.meshsat.net"
IN = "https://invoiceninja.nuclearlighters.net/api/v1"
IN_TOKEN = open(os.path.join(SP, ".in-token")).read().strip()

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("  PASS  " if ok else "  FAIL  ") + name + (("  -- " + str(detail)) if detail else ""))


def sh(*a, **kw):
    return subprocess.run(a, capture_output=True, text=True, **kw).stdout.strip()


CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-F|", "-c"]


def psql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    bad = [l for l in r.stderr.splitlines() if "ERROR" in l]
    if bad:
        print("   SQL ERR:", bad[-1][:160])
    return r.stdout.strip()


def podenv(v):
    pods = [l.split("/")[1] for l in sh(*CTX, "get", "pods", "-o", "name").splitlines()
            if l.startswith("pod/hub-")]
    return sh(*CTX, "exec", pods[0], "--", "printenv", v) if pods else ""


def api(method, path, token, tenant=None):
    req = urllib.request.Request(HUB + path, method=method)
    req.add_header("Authorization", "Bearer " + token)
    if tenant:
        req.add_header("X-Tenant-ID", tenant)
    try:
        with urllib.request.urlopen(req, timeout=45) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def inja(path):
    req = urllib.request.Request(IN + path)
    req.add_header("X-API-TOKEN", IN_TOKEN)
    req.add_header("X-Requested-With", "XMLHttpRequest")
    req.add_header("Accept", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read()
            return r.status, (raw if raw[:4] == b"%PDF" else json.loads(raw.decode()))
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:400]


def main():
    admin = podenv("HUB_AUTH_TOKEN")
    if not admin:
        print("the Hub pod has no HUB_AUTH_TOKEN", file=sys.stderr)
        return 2

    print("=== 1. the donation was recorded ===")
    row = psql(
        "SELECT id, tenant_id, plan, country, amount_cents, currency, status, "
        "coalesce(invoice_number,''), coalesce(last_error,''), email, name "
        "FROM receipts WHERE plan = 'donation' ORDER BY paid_at DESC LIMIT 1;")
    if not row:
        print("  no donation receipt exists yet -- make one first", file=sys.stderr)
        return 2
    (rid, tenant, plan, country, cents, cur, status, invno, err, email, name) = row.split("|")
    cents = int(cents)
    print(f"  receipt {rid}  tenant={tenant}  {int(cents)/100:.2f} {cur}  country={country or '(none)'}")

    check("it is recorded as a donation, not a subscription", plan == "donation", plan)
    check("it hangs off a tenant", tenant != "", tenant)

    print("=== 2. it was NOT parked ===")
    check("the receipt reached issued", status == "issued", f"status={status} err={err[:110]}")
    check("a real document number was taken", invno.startswith("MSH"), invno)
    if status != "issued":
        print("\n  A donation must never park for its country: place of supply is a")
        print("  question about a supply, and a gift is not one. If this parked with")
        print("  a country reason, internal/vat.ForDonation is not being consulted.")

    print("=== 3. the document carries NO VAT ===")
    st, inv = inja(f"/invoices?number={invno}&include=client")
    data = (inv or {}).get("data") if isinstance(inv, dict) else None
    if not data:
        check("the invoice is readable in the billing system", False, f"HTTP {st}")
    else:
        d = data[0]
        items = d.get("line_items") or []
        check("the invoice has exactly one line", len(items) == 1, len(items))
        if items:
            it = items[0]
            check("tax_name1 is empty", (it.get("tax_name1") or "") == "",
                  repr(it.get("tax_name1")))
            check("tax_rate1 is zero", float(it.get("tax_rate1") or 0) == 0.0,
                  it.get("tax_rate1"))
        check("the total is the whole gift, nothing derived out of it",
              abs(float(d.get("amount") or 0) * 100 - cents) < 1,
              f'{d.get("amount")} vs {cents/100:.2f}')
        check("no tax was carried on the invoice at all",
              float(d.get("total_taxes") or 0) == 0.0, d.get("total_taxes"))
        cl = (d.get("client") or {})
        check("the giver is on the document, not the platform",
              (cl.get("name") or "") != "" or (cl.get("contacts") or [{}])[0].get("email", "") != "",
              cl.get("name"))

    print("=== 4. the threshold meter did not move ===")
    st, th = api("GET", "/api/admin/vat/threshold", admin, "default")
    if st == 200 and isinstance(th, dict):
        by = th.get("by_country") or {}
        inband = by.get(country, 0) if country else 0
        check("this giver's country contributes nothing to the meter",
              inband == 0, f"{country}={inband}")
        print(f"    cross-border total: {th.get('cross_border_cents')} of {th.get('threshold_cents')}")
    else:
        check("the threshold endpoint answered", False, f"HTTP {st}")

    print("=== 5. nothing was granted ===")
    if tenant:
        plan_now = psql(f"SELECT plan FROM tenants WHERE id='{tenant}';")
        check("the tenant it hangs off did not gain a tier",
              plan_now in ("", "free", "default", "beta"), plan_now)
    paid = psql("SELECT count(*) FROM tenants WHERE plan IN ('crew','fleet') "
                "AND stripe_subscription_id = '';")
    check("no tier exists without a subscription behind it", paid == "0", paid)

    print(f"\n=== {len(PASS)}/{len(PASS) + len(FAIL)} ===")
    for f in FAIL:
        print("  FAILED: " + f)
    print("\nNothing was purged. A donation is real income and the document is the")
    print("giver's record; whether to keep it is a separate decision from whether")
    print("it was produced correctly.")
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
