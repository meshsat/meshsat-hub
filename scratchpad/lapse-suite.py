#!/usr/bin/env python3
"""Phase D5: prove the DEPLOYED lapse singleton, not just the arithmetic.

Backdates a throwaway tenant, forces a leader change so the job's immediate
Once() pass runs, and checks what it did and did not touch.
"""
import subprocess, sys, time

CTX = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB  = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec",
       "meshsat-hub-main-1", "--", "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]

def sh(c): return subprocess.run(c, capture_output=True, text=True).stdout.strip()
def sql(q): return sh(DB + [q])
def pods(): return [l.split("/")[1] for l in sh(CTX+["get","pods","-o","name"]).splitlines() if l.startswith("pod/hub-")]

def leader():
    out = sh(CTX+["get","lease","-o","jsonpath={range .items[*]}{.metadata.name}={.spec.holderIdentity}{\"\\n\"}{end}"])
    for line in out.splitlines():
        if "hub" in line.lower():
            return line
    return out

T_LAPSE = "t-lapse-probe"
T_WARN  = "t-warn-probe"
results = []
def check(n, ok, d=""):
    results.append((n, ok)); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d) if d else ""))

print("=== setup: two throwaway tenants ===")
for t in (T_LAPSE, T_WARN):
    sql(f"DELETE FROM tenants WHERE id='{t}';")
# one already expired, one expiring inside the 3-day warning window
for t, plan, when, code, imei in ((T_LAPSE,'crew',"now() - interval '2 days'",'LAPSE777','300000000091111'),
                                  (T_WARN, 'fleet',"now() + interval '2 days'",'WARN7777','300000000092222')):
    sql(f"DELETE FROM users WHERE tenant_id='{t}';")
    sql(f"DELETE FROM devices WHERE tenant_id='{t}';")
    # An owner with a real address: without one there is nowhere to send a
    # warning, so warn() correctly does nothing and records nothing.
    sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at,plan_expires_at,kofi_claim_code,kofi_payer_email,owner_user_id) "
        f"VALUES ('{t}','{t}','{t}','{plan}','active',now(),now(),{when},'{code}','billing@meshsat.net','usr-{t}');")
    sql(f"INSERT INTO users (id,email,name,password_hash,role,enabled,tenant_id,created_at,updated_at) "
        f"VALUES ('usr-{t}','billing@meshsat.net','Lapse Probe','x','owner',true,'{t}',now(),now());")
    sql(f"INSERT INTO devices (imei,tenant_id,label,type,created_at,updated_at) "
        f"VALUES ('{imei}','{t}','probe','rockblock',now(),now()) ON CONFLICT DO NOTHING;")
before = {t: sql(f"SELECT plan, coalesce(plan_expires_at::text,'-') FROM tenants WHERE id='{t}';") for t in (T_LAPSE,T_WARN)}
print("   before:", before)
print("   leader:", leader())

print("\n=== force a leader change so the job's immediate pass runs ===")
ps = pods()
hold = leader()
victim = None
for p in ps:
    if p in hold:
        victim = p
if not victim:
    victim = ps[0]
print("   deleting", victim)
sh(CTX+["delete","pod",victim,"--wait=false"])
for i in range(40):
    time.sleep(6)
    now = leader()
    if victim not in now:
        print("   new leader after %ds: %s" % ((i+1)*6, now)); break
time.sleep(20)  # let Once() finish

print("\n=== what the deployed job did ===")
after_lapse = sql(f"SELECT plan, coalesce(plan_expires_at::text,'-') FROM tenants WHERE id='{T_LAPSE}';")
check("an expired paid plan dropped to free", after_lapse.split("|")[0] == "free", after_lapse)
check("the stale expiry was cleared", after_lapse.split("|")[1] == "-", after_lapse)
after_warn = sql(f"SELECT plan, coalesce(lapse_warned_at::text,'-') FROM tenants WHERE id='{T_WARN}';")
check("a plan not yet expired was NOT lapsed", after_warn.split("|")[0] == "fleet", after_warn)
check("a warning was recorded for the one expiring soon", after_warn.split("|")[1] != "-", after_warn)

dev = sql(f"SELECT count(*) FROM devices WHERE tenant_id='{T_LAPSE}';")
check("the lapsed tenant keeps every registered device", dev == "1", "devices=" + dev)
aud = sql(f"SELECT count(*) FROM audit_log WHERE tenant_id='{T_LAPSE}' AND action='subscription_lapsed';")
check("the lapse wrote an audit line", int(aud or 0) >= 1, "audit rows=" + str(aud))

print("\n=== teardown ===")
for t in (T_LAPSE, T_WARN):
    sql(f"DELETE FROM devices WHERE tenant_id='{t}';")
    sql(f"DELETE FROM users WHERE tenant_id='{t}';")
    sql(f"DELETE FROM audit_log WHERE tenant_id='{t}';")
    sql(f"DELETE FROM tenants WHERE id='{t}';")
print("   removed:", sql(f"SELECT count(*) FROM tenants WHERE id IN ('{T_LAPSE}','{T_WARN}');") == "0")

bad = [r for r in results if not r[1]]
print("\n%d checks, %d failed" % (len(results), len(bad)))
sys.exit(1 if bad else 0)
