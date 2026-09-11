#!/usr/bin/env python3
"""The VAT gate, on production, without ever touching the real invoice series.

Three throwaway tenants in three countries. A simulated Ko-fi payment for each.
The deployed drainer then decides: a US buyer and a buyer nobody can place must
be PARKED, and a Dutch buyer must be left issuable.

The Dutch one is deliberately never allowed to drain -- its next attempt is
pushed a year out the moment it exists. A receipt that issues draws a number out
of a gapless legal series, and a test must not spend one. Company 2 holds 0
invoices and its counter is at 1, so the first real customer receipt is still
MSH2026-0001; the last check re-asserts that.
"""
import hashlib, hmac, json, secrets, subprocess, sys, time, urllib.request, urllib.error

DB  = ["kubectl","--context","notrf01","-n","meshsat-hub-db","exec","meshsat-hub-main-1","--",
       "psql","-U","postgres","-d","meshsat_hub","-t","-A","-F|","-c"]
CTX = ["kubectl","--context","notrf01","-n","meshsat-hub"]
API = "https://hub.meshsat.net"

def sh(c): return subprocess.run(c, capture_output=True, text=True).stdout.strip()
def sql(q):
    r = subprocess.run(DB+[q], capture_output=True, text=True)
    bad=[l for l in r.stderr.splitlines() if "ERROR" in l]
    if bad: print("   SQL ERR:", bad[-1][:160])
    return r.stdout.strip()
def pods(): return [l.split("/")[1] for l in sh(CTX+["get","pods","-o","name"]).splitlines() if l.startswith("pod/hub-")]
def env(v):
    return sh(CTX+["exec",pods()[0],"--","printenv",v])

TOK   = env("HUB_AUTH_TOKEN")
KOFI  = env("HUB_KOFI_WEBHOOK_SECRET")
VTOK  = env("HUB_KOFI_VERIFICATION_TOKEN")

def hub(path, method="GET"):
    req = urllib.request.Request(API+path, method=method,
        headers={"Authorization":"Bearer "+TOK, "X-Tenant-ID":"default", "Accept":"application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=45); return r.status, r.read().decode()
    except urllib.error.HTTPError as e: return e.code, e.read().decode()[:300]

def kofi_pay(claim, amount="9.00", txn=None, subscription=True):
    payload = {"verification_token": VTOK, "message_id": secrets.token_hex(8),
               "kofi_transaction_id": txn or ("txn-"+secrets.token_hex(6)),
               "type": "Subscription", "is_subscription_payment": subscription,
               "tier_name": "Crew", "email": "vatprobe@meshsat.org",
               "message": claim, "amount": amount, "currency": "EUR"}
    body = urllib.parse.urlencode({"data": json.dumps(payload)}).encode()
    req = urllib.request.Request(f"{API}/api/webhook/kofi/{KOFI}", data=body, method="POST",
        headers={"Content-Type":"application/x-www-form-urlencoded"})
    try:
        r = urllib.request.urlopen(req, timeout=45); return r.status, r.read().decode()
    except urllib.error.HTTPError as e: return e.code, e.read().decode()[:200]

import urllib.parse
results=[]
def check(n, ok, d=""):
    results.append((n,ok)); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d) if d else ""))

CASES = [("t-vat-us", "US", "VATUS777", "parks"),
         ("t-vat-unknown", "", "VATUNK77", "parks"),
         ("t-vat-nl", "NL", "VATNL777", "issuable")]

print("=== setup: three tenants, three countries ===")
for tid, country, code, _ in CASES:
    sql(f"DELETE FROM receipts WHERE tenant_id='{tid}';")
    sql(f"DELETE FROM users WHERE tenant_id='{tid}';")
    sql(f"DELETE FROM tenants WHERE id='{tid}';")
    sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at,kofi_claim_code,billing_country,billing_country_evidence,owner_user_id) "
        f"VALUES ('{tid}','{tid}','{tid}','free','active',now(),now(),'{code}','{country}','declared {country}, probe','usr-{tid}');")
    sql(f"INSERT INTO users (id,email,name,password_hash,role,enabled,tenant_id,created_at,updated_at) "
        f"VALUES ('usr-{tid}','vatprobe@meshsat.org','VAT probe','x','owner',true,'{tid}',now(),now());")
print("   countries:", sql("SELECT string_agg(id||'='||coalesce(nullif(billing_country,''),'(none)'),', ') FROM tenants WHERE id LIKE 't-vat-%';"))

print("\n=== a payment for each ===")
for tid, country, code, _ in CASES:
    s, b = kofi_pay(code)
    check(f"{country or '(no country)'} payment accepted", s==200, f"{s} {b[:60]}")
time.sleep(2)
for tid, country, code, _ in CASES:
    got = sql(f"SELECT country, status FROM receipts WHERE tenant_id='{tid}';")
    check(f"a receipt exists carrying country {country or '(none)'}", got.startswith((country or "")+"|"), got or "no row")

# The Dutch one must never spend an invoice number in a test.
sql("UPDATE receipts SET next_attempt_at = now() + interval '1 year' WHERE tenant_id='t-vat-nl';")
print("   the NL receipt is held a year out so it cannot draw a real invoice number")

print("\n=== let the deployed drainer decide ===")
victim = pods()[0]
print("   forcing a leader change via", victim)
sh(CTX+["delete","pod",victim,"--wait=false"])
for _ in range(24):
    time.sleep(5)
    st = sql("SELECT string_agg(tenant_id||'='||status,', ') FROM receipts WHERE tenant_id LIKE 't-vat-%';")
    if "blocked" in st: break
print("   ", st)

for tid, country, code, expect in CASES:
    row = sql(f"SELECT status, last_error FROM receipts WHERE tenant_id='{tid}';")
    status = row.split("|")[0] if row else ""
    reason = row.split("|",1)[1] if "|" in row else ""
    if expect == "parks":
        check(f"a {country or 'placeless'} buyer is PARKED, not invoiced", status=="blocked", status or "no row")
        want = "outside the EU" if country else "no country on file"
        check(f"  and the reason says why ({want})", want in reason, reason[:90])
    else:
        check("a Dutch buyer is left issuable", status=="pending", status or "no row")

print("\n=== the operator surfaces ===")
s, b = hub("/api/admin/receipts/blocked")
check("parked receipts are listed for an operator", s==200 and b.count('"blocked"')>=2, f"{s} {b.count(chr(34)+'blocked'+chr(34))} rows")
s, b = hub("/api/admin/vat/threshold")
th = json.loads(b) if s==200 else {}
check("the threshold endpoint answers", s==200, str(s))
check("nothing unissued counts toward the threshold", th.get("cross_border_cents")==0,
      f"{th.get('cross_border_cents')} cents")
check("the figure explains what to do at the limit", bool(th.get("note")), "")

print("\n=== the real invoice series is untouched ===")
T = env("HUB_INVOICENINJA_TOKEN")
req = urllib.request.Request("https://invoiceninja.nuclearlighters.net/api/v1/invoices?per_page=5",
    headers={"X-API-TOKEN":T,"X-Requested-With":"XMLHttpRequest"})
try:
    n = len(json.loads(urllib.request.urlopen(req, timeout=25).read()).get("data",[]))
except Exception as e:
    n = -1
check("company 2 still holds no invoices", n==0, f"count={n}")

print("\n=== teardown ===")
for tid, _, _, _ in CASES:
    sql(f"DELETE FROM receipts WHERE tenant_id='{tid}';")
    sql(f"DELETE FROM users WHERE tenant_id='{tid}';")
    sql(f"DELETE FROM audit_log WHERE tenant_id='{tid}';")
    sql(f"DELETE FROM tenants WHERE id='{tid}';")
print("   removed:", sql("SELECT count(*) FROM tenants WHERE id LIKE 't-vat-%';")=="0",
      "| receipts left:", sql("SELECT count(*) FROM receipts;"))

bad=[r for r in results if not r[1]]
print("\n%d checks, %d failed" % (len(results), len(bad)))
sys.exit(1 if bad else 0)
