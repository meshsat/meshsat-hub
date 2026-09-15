#!/usr/bin/env python3
"""Phase D1c + D2: the free-tier ceiling and the SOS invariant, on production.

Drives a throwaway free-plan tenant through the real API with a real owner API
key: four devices allowed, the fifth refused 402 -- and then, while over the
cap, a genuine authenticated MO still gets through and still persists. That
last one is the invariant that outranks the feature and it is shown on the live
system, not only in a unit test.

Safe to run against production: the tenant is thrown away at the end, and no
route rules exist anywhere in this deployment, so no MO can reach a real phone.
"""
import base64, hashlib, json, secrets, subprocess, sys, time, urllib.request, urllib.error

DB = ["kubectl","--context","notrf01","-n","meshsat-hub-db","exec","meshsat-hub-main-1","--",
      "psql","-U","postgres","-d","meshsat_hub","-t","-A","-F|","-c"]
API = "https://hub.meshsat.net"
T   = "t-quota-probe"
IMEI_OVER = "300000000055555"

def sql(q):
    r = subprocess.run(DB+[q], capture_output=True, text=True)
    err = [l for l in r.stderr.splitlines() if "ERROR" in l]
    if err: print("   SQL ERR:", err[-1][:170])
    return r.stdout.strip()

def call(method, path, key, body=None, ctype="application/json"):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": ctype}
    if key: h["Authorization"] = "Bearer " + key
    req = urllib.request.Request(API+path, data=data, method=method, headers=h)
    try:
        r = urllib.request.urlopen(req, timeout=45); return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

results=[]
def check(n, ok, d=""):
    results.append((n,ok)); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d) if d else ""))

print("=== setup: a free-plan tenant with a real owner API key ===")
for stmt in (f"DELETE FROM devices WHERE tenant_id='{T}';",
             f"DELETE FROM api_keys WHERE tenant_id='{T}';",
             f"DELETE FROM credentials WHERE tenant_id='{T}';",
             f"DELETE FROM messages WHERE tenant_id='{T}';",
             f"DELETE FROM audit_log WHERE tenant_id='{T}';",
             f"DELETE FROM tenants WHERE id='{T}';"):
    sql(stmt)
sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
    f"VALUES ('{T}','quota-probe','Quota probe','free','active',now(),now());")
plain = "meshsat_" + secrets.token_hex(32)
h = hashlib.sha256(plain.encode()).hexdigest()
# expires_at is Go's zero time: the column default is the epoch, which the
# middleware reads as expired. The product's own insert always supplies it.
sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
    f"VALUES ('{secrets.token_hex(8)}','{h}','{plain[:16]}','owner','quota probe','','0001-01-01 00:00:00+00','{T}',now());")
print("   tenant on plan:", sql(f"SELECT plan FROM tenants WHERE id='{T}';"))

s,b = call("GET","/api/tenant/usage",plain)
u = json.loads(b) if s==200 else {}
check("an owner API key authenticates and carries its tenant", s==200, f"{s} {b[:120]}")
check("the tenant reads its own usage, on the free plan", u.get("plan")=="free", str(u.get("plan")))
check("the free plan ceiling is 4", u.get("limit")==4, "limit="+str(u.get("limit")))
check("a claim code is minted on first read", bool(u.get("claim_code")), str(u.get("claim_code")))
code1 = u.get("claim_code")
s,b = call("GET","/api/tenant/usage",plain)
check("minting is idempotent on a second read", json.loads(b).get("claim_code")==code1, "stable")

print("\n=== the ceiling ===")
for i in range(1,5):
    s,b = call("POST","/api/devices",plain,{"imei":f"3000000000{i:05d}","label":f"probe-{i}","type":"rockblock"})
    check(f"device {i} of 4 is accepted", s in (200,201), f"{s} {b[:90]}")
s,b = call("POST","/api/devices",plain,{"imei":"300000000099999","label":"probe-5","type":"rockblock"})
check("the 5th device is refused 402 Payment Required", s==402, f"{s} {b[:150]}")
check("the refusal is written for a person", "plan covers" in b and "move up a plan" in b, b[:150])
check("the refused device was NOT registered", sql(f"SELECT count(*) FROM devices WHERE imei='300000000099999';")=="0")

s,b = call("GET","/api/tenant/usage",plain)
u = json.loads(b)
check("usage reports 4 of 4 used, 0 remaining", u.get("used")==4 and u.get("remaining")==0,
      json.dumps({k:u.get(k) for k in ("used","limit","remaining")}))

print("\n=== D2: the SOS invariant, over the cap, on production ===")
# Push the tenant over its cap the way a downgrade would -- the ceiling gates
# registration, so going over it is only ever reached from the other side.
sql(f"INSERT INTO devices (imei,tenant_id,label,type,created_at,updated_at) "
    f"VALUES ('{IMEI_OVER}','{T}','over-cap','rockblock',now(),now()) ON CONFLICT DO NOTHING;")
s,b = call("GET","/api/tenant/usage",plain)
u = json.loads(b)
check("the tenant is now OVER its cap", u.get("used")==5 and u.get("over_limit") is True,
      json.dumps({k:u.get(k) for k in ("used","limit","over_limit")}))

# A real inbound path: the tenant's own Cloudloop webhook, authenticated by the
# per-tenant secret in the URL exactly as Ground Control's would be.
s,b = call("PUT","/api/tenant/integrations/cloudloop",plain,
           {"values":{"api_key":"probe-unused","api_url":"https://api.cloudloop.com"}})
tok = json.loads(b).get("reveal",{}).get("webhook_token","") if s==200 else ""
check("the tenant gets its own webhook URL", bool(tok), f"{s} {'token issued' if tok else b[:120]}")

now = time.gmtime()
mo = {"id": "probe-"+secrets.token_hex(8),
      "receivedAt": {"year":now.tm_year,"month":now.tm_mon,"day":now.tm_mday,
                     "hour":now.tm_hour,"minute":now.tm_min,"second":now.tm_sec},
      "identity": {"accountId":"probe","thingId":"probe-thing",
                   "hardware":{"imei":IMEI_OVER}},
      "message": base64.b64encode(b"SOS quota probe").decode()}
s,b = call("POST", f"/api/webhook/cloudloop/{tok}", None, mo)
check("an MO for an over-cap tenant is still accepted", s==200, f"{s} {b[:120]}")
time.sleep(3)
stored = sql(f"SELECT count(*) FROM messages WHERE device_imei='{IMEI_OVER}' AND direction='mo';")
check("the message from the over-cap device persisted", (stored or "0") != "0", "rows="+str(stored))
check("the webhook did not answer 402 or refuse the tenant", s not in (402,403), "http="+str(s))

s,b = call("GET","/api/devices",plain)
try:
    d = json.loads(b); lst = d.get("devices", d) if isinstance(d,dict) else d
    n = len(lst)
except Exception:
    n = -1
check("every registered device is still listed while over the cap", n>=5, f"{s} devices={n}")

s,b = call("POST","/api/devices",plain,{"imei":"300000000088888","label":"probe-6","type":"rockblock"})
check("and registering ANOTHER one is still refused", s==402, f"{s} {b[:90]}")

print("\n=== teardown ===")
for stmt in (f"DELETE FROM messages WHERE tenant_id='{T}';",
             f"DELETE FROM positions WHERE tenant_id='{T}';",
             f"DELETE FROM devices WHERE tenant_id='{T}';",
             f"DELETE FROM api_keys WHERE tenant_id='{T}';",
             f"DELETE FROM credentials WHERE tenant_id='{T}';",
             f"DELETE FROM audit_log WHERE tenant_id='{T}';",
             f"DELETE FROM tenants WHERE id='{T}';"):
    sql(stmt)
left = sql(f"SELECT count(*) FROM tenants WHERE id='{T}';")
print("   removed:", left=="0", "| stray devices:", sql(f"SELECT count(*) FROM devices WHERE imei LIKE '3000000000%';"))

bad=[r for r in results if not r[1]]
print("\n%d checks, %d failed" % (len(results), len(bad)))
sys.exit(1 if bad else 0)
