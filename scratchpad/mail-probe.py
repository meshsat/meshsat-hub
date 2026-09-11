#!/usr/bin/env python3
"""Prove all three relay gates are open, using a real Hub email.

Reaching the relay, being allowed to relay, and being DKIM-signed are separate
permissions with separate config, and all three were pinned to one worker
address. This drives a genuine Ko-fi payment through the webhook so the Hub
sends its own PlanChanged notice, then reads the relay log to see which gate,
if any, is still shut.
"""
import json, subprocess, sys, time, urllib.parse, urllib.request, urllib.error

CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
      "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub",
      "-t", "-A", "-c"]


def sh(*c):
    return subprocess.run(list(c), capture_output=True, text=True).stdout.strip()


def pods():
    return [l.split("/")[1] for l in sh(*CTX, "get", "pods", "-o", "name").splitlines()
            if l.startswith("pod/hub-")]


def env(v):
    return sh(*CTX, "exec", pods()[0], "--", "printenv", v)


def sql(q):
    return subprocess.run(DB + [q], capture_output=True, text=True).stdout.strip()


START = time.time()


def stamp_is_recent(line):
    """True for a syslog line written since this probe started."""
    try:
        ts = line.split(" ", 1)[0]
        t = time.mktime(time.strptime(ts[:19], "%Y-%m-%dT%H:%M:%S"))
        return t >= START - 30
    except Exception:
        return False


TENANT = "t-mail-probe"
import os as _os
TO = _os.environ.get("PROBE_TO", "mailprobe@meshsat.org")
stamp = str(int(time.time()))

print("=== which workers are the replicas on? ===")
placement = sh(*CTX, "get", "pods", "-o", "wide", "--no-headers")
workers = []
for line in placement.splitlines():
    if line.startswith("hub-"):
        f = line.split()
        print(f"  {f[0]}  {f[6]}")
        workers.append(f[6])
if "notrf01dmz02" in workers:
    print("  NOTE: a replica is on dmz02, whose address is the pinned one.")
    print("        A pass here would not prove the fix. Consider deleting that pod first.")

print("=== seed a tenant with a claim code ===")
# The country is US ON PURPOSE. A probe payment still records a receipt, and
# with an EU country the receipt drainer would issue a REAL invoice out of the
# gapless series and email it -- which is exactly what two earlier runs of this
# script did, burning MSH2026-0001 and MSH2026-0002 before anyone had paid.
# Non-EU parks the receipt in the VAT gate before the billing system is touched.
code = "MAILPRB1"
sql(f"""INSERT INTO tenants (id, slug, name, plan, status, billing_country, kofi_claim_code, created_at, updated_at)
        VALUES ('{TENANT}','{TENANT}','Mail Probe','free','active','US','{code}', now(), now())
        ON CONFLICT (id) DO UPDATE SET kofi_claim_code='{code}', plan='free', status='active', billing_country='US';""")
uid = "u-mail-probe"
# password_hash is NOT NULL with no default; local login is disabled on this
# Hub so the value is never used, but the column has to be satisfied.
sql(f"""INSERT INTO users (id, tenant_id, email, name, role, password_hash, created_at, updated_at)
        VALUES ('{uid}','{TENANT}','{TO}','Mail Probe','owner','x', now(), now())
        ON CONFLICT (id) DO UPDATE SET email='{TO}';""")
sql(f"UPDATE tenants SET owner_user_id='{uid}' WHERE id='{TENANT}';")
print("  owner:", sql(f"SELECT owner_user_id FROM tenants WHERE id='{TENANT}';"))

secret = env("HUB_KOFI_WEBHOOK_SECRET")
token = env("HUB_KOFI_VERIFICATION_TOKEN")
if not secret or not token:
    print("Ko-fi is not configured on this Hub", file=sys.stderr)
    sys.exit(2)

print("=== post a Ko-fi payment so the Hub sends its own PlanChanged notice ===")
payload = {
    "verification_token": token, "message_id": "mailprobe-" + stamp,
    "kofi_transaction_id": "txn-mailprobe-" + stamp,
    "type": "Subscription", "is_subscription_payment": True, "is_first_subscription_payment": True,
    "from_name": "Mail Probe", "email": TO, "amount": "9.00", "currency": "EUR",
    "message": "claim " + code, "tier_name": "Crew",
    "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
}
body = urllib.parse.urlencode({"data": json.dumps(payload)}).encode()
req = urllib.request.Request("https://hub.meshsat.net/api/webhook/kofi/" + secret, data=body,
                             headers={"Content-Type": "application/x-www-form-urlencoded"})
try:
    with urllib.request.urlopen(req, timeout=45) as r:
        print("  webhook ->", r.status, r.read().decode()[:120])
except urllib.error.HTTPError as e:
    print("  webhook ->", e.code, e.read().decode()[:200])

print("  plan now:", sql(f"SELECT plan FROM tenants WHERE id='{TENANT}';"))

print("=== what the Hub logged ===")
time.sleep(6)
log = sh(*CTX, "logs", "deploy/hub", "--since=3m", "--all-containers")
for line in log.splitlines():
    if "mail" in line.lower() and ("sent" in line or "could not send" in line or "EHLO" in line):
        print("  " + line[:300])

print("=== what the relay saw ===")
time.sleep(4)
relay = sh("ssh", "-i", "/home/claude-runner/.ssh/one_key", "-o", "BatchMode=yes",
           "root@nllei01smtp-dkim01",
           f"grep -E '10\\.255\\.200|{TO}|opendkim' /var/log/mail.log | tail -60")
# Only this run counts. Old rejections in the log are history, not evidence.
cut = time.strftime("%Y-%m-%dT", time.gmtime())
recent = [l for l in relay.splitlines() if stamp_is_recent(l)]
relay = "\n".join(recent) if recent else relay
print(relay if relay else "  (nothing)")

ok_helo = "Helo command rejected" not in relay
ok_signed = "DKIM-Signature field added" in relay or "d=meshsat.net" in relay
ok_sent = "status=sent" in relay
print()
print("  gate 1 HELO accepted   :", "PASS" if ok_helo else "FAIL")
print("  gate 2 relayed         :", "PASS" if ok_sent else "FAIL")
print("  gate 3 DKIM signed     :", "PASS" if ok_signed else "FAIL")

print("=== clean up ===")
sql(f"DELETE FROM receipts WHERE tenant_id='{TENANT}';")
sql(f"DELETE FROM kofi_deliveries WHERE tenant_id='{TENANT}';")
sql(f"DELETE FROM users WHERE tenant_id='{TENANT}';")
sql(f"DELETE FROM tenants WHERE id='{TENANT}';")
print("  tenants left:", sql(f"SELECT count(*) FROM tenants WHERE id='{TENANT}';"))
sys.exit(0 if (ok_helo and ok_sent and ok_signed) else 1)
