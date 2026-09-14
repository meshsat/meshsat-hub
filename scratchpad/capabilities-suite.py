#!/usr/bin/env python3
"""GET /api/capabilities on production, with a real tenant (MESHSAT-1121).

Why this exists rather than a curl: an unauthenticated probe proves NOTHING
about this route. The Hub's auth middleware runs BEFORE chi's routing, so a
missing path and a present one both answer 401, and with a token that gets past
auth but has no tenant they both answer 403. I checked registration that way
first and the negative control returned exactly the same code, which is how I
know the check was worthless. The only probe that distinguishes them is an
authorised one that gets a 200 and a body.

It follows quota-suite.py's pattern: a throwaway tenant and a real owner API
key written straight into Postgres, the real public API, and a purge at the end.

Safe against production. It creates no device, sends no message, spends no
airtime and pages nobody; the only provider account it writes is an ntfy URL on
its own throwaway tenant, and nothing is ever published to it.
"""
import hashlib
import json
import secrets
import subprocess
import sys
import urllib.error
import urllib.request

DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]
API = "https://hub.meshsat.net"
T = "t-caps-probe"

# Every feature the table claims to cover. A drift here means the UI is asking
# about something the API stopped answering.
WANT = {"notifications", "email", "ota", "wireguard", "aprs", "tak"}


def sql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    for line in r.stderr.splitlines():
        if "ERROR" in line:
            print("   SQL ERR:", line[:170])
    return r.stdout.strip()


def call(method, path, key, body=None):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json"}
    if key:
        h["Authorization"] = "Bearer " + key
    req = urllib.request.Request(API + path, data=data, method=method, headers=h)
    try:
        r = urllib.request.urlopen(req, timeout=45)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


results = []


def check(ok, n, d=""):
    results.append((n, ok))
    print(("  PASS  " if ok else "  FAIL  ") + n + (("   " + d) if d else ""))


def purge():
    for stmt in (f"DELETE FROM credentials WHERE tenant_id='{T}';",
                 f"DELETE FROM api_keys WHERE tenant_id='{T}';",
                 f"DELETE FROM audit_log WHERE tenant_id='{T}';",
                 f"DELETE FROM tenants WHERE id='{T}';"):
        sql(stmt)


print("=== setup: a throwaway tenant with a real owner API key ===")
purge()
sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
    f"VALUES ('{T}','caps-probe','Capabilities probe','free','active',now(),now());")
plain = "meshsat_" + secrets.token_hex(32)
kh = hashlib.sha256(plain.encode()).hexdigest()
# expires_at must be Go's zero time: the column default is the epoch, which the
# middleware reads as already expired (quota-suite.py found this the hard way).
sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
    f"VALUES ('{secrets.token_hex(8)}','{kh}','{plain[:16]}','owner','caps probe','',"
    f"'0001-01-01 00:00:00+00','{T}',now());")

try:
    print("1. the route is registered and answers a real tenant")
    s, b = call("GET", "/api/capabilities", plain)
    check(s == 200, "GET /api/capabilities -> 200", "got %d" % s)
    if s != 200:
        print("   body:", b[:300])
        raise SystemExit(1)
    caps = {c["feature"]: c for c in json.loads(b)}
    check(set(caps) == WANT, "every feature is reported",
          "missing=%s extra=%s" % (sorted(WANT - set(caps)), sorted(set(caps) - WANT)))

    print("2. a brand-new tenant is told what will NOT happen")
    unconfigured = [f for f, c in caps.items() if not c["configured"]]
    check(len(unconfigured) == len(WANT),
          "a tenant with no provider account has nothing configured",
          "configured: %s" % [f for f, c in caps.items() if c["configured"]])
    silent = [f for f in unconfigured if not (caps[f].get("reason") or "").strip()]
    check(not silent, "each one carries a sentence for the customer", "silent: %s" % silent)

    print("3. THE LEAK CHECK, on production")
    # The platform's OWN apprise, hawkBit, wg-easy and APRS accounts are
    # configured from the environment and live in this very process. If any of
    # them resolved here, this tenant would be told the feature works and would
    # then be served by the OPERATOR's relay on the operator's credentials.
    leaked = [f for f, c in caps.items() if c["configured"] or c.get("platform")]
    check(not leaked,
          "no platform account resolves for a tenant that has none of its own",
          "LEAKED: %s" % leaked)

    print("4. a tenant's own account flips exactly one feature")
    # ntfy.sh is real and public, so it satisfies the SSRF guard on save. Nothing
    # is ever published to it -- this only stores a provider account.
    s, b = call("PUT", "/api/tenant/integrations/ntfy", plain, {"values": {"url": "https://ntfy.sh"}})
    check(s == 200, "the tenant sets its own ntfy account", "got %d %s" % (s, b[:120]))
    s, b = call("GET", "/api/capabilities", plain)
    after = {c["feature"]: c for c in json.loads(b)} if s == 200 else {}
    check(after.get("notifications", {}).get("configured") is True,
          "notifications is now configured")
    check(after.get("notifications", {}).get("platform") is False,
          "and it is the TENANT's account, not the platform's")
    check(not (after.get("notifications", {}).get("reason") or ""),
          "a configured feature carries no leftover warning")
    others = [f for f, c in after.items() if f != "notifications" and c["configured"]]
    check(not others, "and nothing else changed", "also configured: %s" % others)

    print("5. both replicas serve it (kubectl logs/exec only ever picks one)")
    pods = subprocess.run(
        ["kubectl", "--context", "notrf01", "-n", "meshsat-hub", "get", "pods",
         "-l", "app.kubernetes.io/name=hub", "-o",
         "jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}"],
        capture_output=True, text=True).stdout.split()
    check(len(pods) >= 2, "found both replicas", " ".join(pods))
    for p in pods:
        out = subprocess.run(
            ["kubectl", "--context", "notrf01", "-n", "meshsat-hub", "exec", p, "-c", "hub", "--",
             "sh", "-c",
             "wget -qO- --header='Authorization: Bearer %s' "
             "http://127.0.0.1:6070/api/capabilities 2>/dev/null" % plain],
            capture_output=True, text=True).stdout
        try:
            got = {c["feature"] for c in json.loads(out)}
        except Exception:
            got = set()
        check(got == WANT, "%s serves the full set" % p, "got %d feature(s)" % len(got))
finally:
    print()
    print("=== purge ===")
    purge()
    left = sql(f"SELECT count(*) FROM tenants WHERE id='{T}';")
    print("   tenant rows remaining:", left or "?")

bad = [n for n, ok in results if not ok]
print()
print("%d/%d passed" % (len(results) - len(bad), len(results)))
sys.exit(1 if bad else 0)
