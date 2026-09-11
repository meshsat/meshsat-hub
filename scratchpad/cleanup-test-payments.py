#!/usr/bin/env python3
"""Purge the two test payments and give MSH2026-0001 back to the first customer.

The owner's own EUR 1 donation and EUR 9 Crew subscription proved the whole
chain end to end. Both are refunded, both carry credit notes, and both ledgers
net to zero -- so the money is settled and only the paperwork is left.

Purging the CLIENT takes its invoices, payments and credits with it; deletes
are otherwise soft in this billing system. The counters are only rewound if
NOTHING is left using them, because rewinding past a real document would hand
a second customer a number already issued out of a gapless series.

The `default` tenant ROW is never deleted -- it is the platform tenant. Only
its receipts and refunds go.
"""
import hashlib, json, os, subprocess, sys, urllib.error, urllib.request

SP = os.path.dirname(os.path.abspath(__file__))
IN = "https://invoiceninja.nuclearlighters.net/api/v1"
COMPANY = "Wpmbk5ezJn"
TENANT = "default"
IN_TOKEN = open(os.path.join(SP, ".in-token")).read().strip()
IN_PW = open(os.path.join(SP, ".in-password")).read().strip()

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(("  PASS  " if ok else "  FAIL  ") + name + (f"  -- {detail}" if detail != "" else ""))


def inja(method, path, body=None, password=False):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(IN + path, data=data, method=method)
    req.add_header("X-API-TOKEN", IN_TOKEN)
    req.add_header("X-Requested-With", "XMLHttpRequest")
    req.add_header("Accept", "application/json")
    if password:
        req.add_header("X-Api-Password", IN_PW)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            raw = r.read()
            return r.status, (json.loads(raw.decode()) if raw.strip() else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-c"]


def psql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    bad = [l for l in r.stderr.splitlines() if "ERROR" in l]
    if bad:
        print("   SQL ERR:", bad[-1][:150])
    return r.stdout.strip()


def main():
    print("=== 0. what the company looks like before ===")
    st, comp = inja("GET", f"/companies/{COMPANY}")
    if st != 200:
        sys.exit(f"cannot read the company: {st} {comp}")
    before = comp["data"]["settings"]
    shell_before = hashlib.sha256((before.get("email_style_custom") or "").encode()).hexdigest()[:16]
    print(f"  invoice counter {before.get('invoice_number_counter')} | "
          f"credit counter {before.get('credit_number_counter')} | shell {shell_before}")

    print("=== 1. everything is settled before anything is deleted ===")
    _, invs = inja("GET", "/invoices?per_page=100")
    _, crs = inja("GET", "/credits?per_page=100")
    inv_rows = (invs or {}).get("data", [])
    cr_rows = (crs or {}).get("data", [])
    for d in inv_rows + cr_rows:
        print(f"    {d.get('number')}  amount {d.get('amount')}  balance {d.get('balance')}")
    check("every invoice is settled", all(float(i.get("balance") or 0) == 0 for i in inv_rows), len(inv_rows))
    check("every credit note is applied", all(float(c.get("balance") or 0) == 0 for c in cr_rows), len(cr_rows))
    if FAIL:
        sys.exit("refusing to purge while something is unsettled")

    print("=== 2. purge the client (takes its documents with it) ===")
    _, found = inja("GET", "/clients?per_page=50")
    purged = 0
    for c in (found or {}).get("data", []):
        if c.get("id_number") != TENANT:
            print(f"  !! refusing to purge client {c['id']} (id_number {c.get('id_number')!r})")
            continue
        st, _ = inja("POST", f"/clients/{c['id']}/purge", {}, password=True)
        print(f"  purged client {c['id']}: HTTP {st}")
        purged += st == 200
    check("the test client was purged", purged >= 1, purged)

    print("=== 3. rewind the counters, but only if nothing is left ===")
    _, ri = inja("GET", "/invoices?per_page=100")
    _, rc = inja("GET", "/credits?per_page=100")
    live = [d.get("number") for d in (ri or {}).get("data", []) + (rc or {}).get("data", []) if d.get("number")]
    if live:
        print("  !! documents remain; NOT rewinding:", live)
        check("counters left alone because documents remain", True)
    else:
        s = dict(before)
        s["invoice_number_counter"] = 1
        s["credit_number_counter"] = 1
        s["client_number_counter"] = 1
        s["payment_number_counter"] = 1
        st, _ = inja("PUT", f"/companies/{COMPANY}", {"settings": s})
        print(f"  counters rewound: HTTP {st}")

    print("=== 4. the company is clean and its branding survived ===")
    _, comp = inja("GET", f"/companies/{COMPANY}")
    after = comp["data"]["settings"]
    if not live:
        check("invoice counter is back to 1", int(after["invoice_number_counter"]) == 1,
              after["invoice_number_counter"])
        check("credit counter is back to 1", int(after["credit_number_counter"]) == 1,
              after["credit_number_counter"])
    for res in ("clients", "invoices", "credits", "payments"):
        _, d = inja("GET", f"/{res}?per_page=100")
        n = len(d.get("data", [])) if isinstance(d, dict) else -1
        check(f"no {res} left on the company", n == 0, n)
    shell_after = hashlib.sha256((after.get("email_style_custom") or "").encode()).hexdigest()[:16]
    check("the branded email wrapper is byte-identical", shell_after == shell_before,
          f"{shell_before} -> {shell_after}")
    check("the payment template survived", bool(after.get("email_template_payment")))
    check("the credit-note template survived", bool(after.get("email_template_credit")))

    print("=== 5. the Hub's own rows ===")
    psql(f"DELETE FROM refunds WHERE tenant_id='{TENANT}';")
    psql(f"DELETE FROM receipts WHERE tenant_id='{TENANT}';")
    check("no receipts left", psql(f"SELECT count(*) FROM receipts WHERE tenant_id='{TENANT}';") == "0")
    check("no refunds left", psql(f"SELECT count(*) FROM refunds WHERE tenant_id='{TENANT}';") == "0")
    # The platform tenant row itself is NEVER deleted.
    check("the platform tenant still exists",
          psql(f"SELECT count(*) FROM tenants WHERE id='{TENANT}';") == "1")

    print(f"\n=== {len(PASS)}/{len(PASS) + len(FAIL)} ===")
    for f in FAIL:
        print("  FAILED: " + f)
    return 0 if not FAIL else 1


if __name__ == "__main__":
    sys.exit(main())
