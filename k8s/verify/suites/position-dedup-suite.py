#!/usr/bin/env python3
"""One published position produces exactly one row, with two replicas running.

MESHSAT-1120's acceptance criterion says this must be "proven on production by
publishing once and counting rows, not by reading code", and that is the whole
point: both replicas subscribe to meshsat/+/position with a plain Subscribe, so
both receive every message and both insert. Reading the handler cannot tell you
whether the second insert actually collapses -- that depends on the id being
derived from the message and on the INSERT being a silent no-op in the dialect
production actually runs, which is Postgres, not the SQLite the tests use.

It also pins the two things a digest id can get WRONG, in opposite directions:

  - collapsing too little  -> the duplicate survives and the bug is back
  - collapsing too MUCH    -> two devices parked at the same coordinates become
                              one row and one of them vanishes from the map

SAFETY. Critical rule 14: a test MO message can text a real phone, because
routing carries wildcard 'Relay MO -> SMS' rules. This publishes to
meshsat/{id}/position, which does NOT reach the routing engine. Belt and braces
on top of that: source is "probe" rather than iridium/globalstar, so the APRS-IS
injector returns before it would ever transmit; the throwaway tenant has no
geofence, so nothing is evaluated; it has no dead man's switch, so no check-in
matters; and it has no provider account of any kind, so no carrier is reachable.
Nothing leaves the cluster. Everything is deleted at the end.
"""
import json
import subprocess
import sys
import time

CTX = ["--context", "notrf01"]
NS = CTX + ["-n", "meshsat-hub"]
DBNS = CTX + ["-n", "meshsat-hub-db"]
DB = ["kubectl"] + DBNS + ["exec", "meshsat-hub-main-1", "--",
                           "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-c"]
T = "t-dup-probe"
DEV_A = "dup-probe-a"
DEV_B = "dup-probe-b"
PUB = "dup-probe-pub"


def sql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    for line in r.stderr.splitlines():
        if "ERROR" in line:
            print("   SQL ERR:", line[:170])
    return r.stdout.strip()


def rows(dev):
    out = sql(f"SELECT count(*) FROM positions WHERE device_imei='{dev}';")
    try:
        return int(out.splitlines()[0])
    except Exception:
        return -1


results = []


def check(ok, n, d=""):
    results.append((n, ok))
    print(("  PASS  " if ok else "  FAIL  ") + n + (("   " + d) if d else ""))


def purge():
    for dev in (DEV_A, DEV_B):
        sql(f"DELETE FROM positions WHERE device_imei='{dev}';")
        sql(f"DELETE FROM devices WHERE imei='{dev}';")
    sql(f"DELETE FROM audit_log WHERE tenant_id='{T}';")
    sql(f"DELETE FROM tenants WHERE id='{T}';")
    subprocess.run(["kubectl"] + NS + ["delete", "pod", PUB, "--ignore-not-found", "--wait=false"],
                   capture_output=True, text=True)


def publish(dev, payload):
    """Publish one retained=false position through the real broker."""
    topic = f"meshsat/{dev}/position"
    body = json.dumps(payload)
    r = subprocess.run(
        ["kubectl"] + NS + ["exec", PUB, "--", "mosquitto_pub",
                            "-h", "nats", "-p", "1883",
                            "-u", "meshsat", "-P", PW,
                            "-t", topic, "-m", body],
        capture_output=True, text=True)
    if r.returncode != 0:
        print("   PUBLISH FAILED:", (r.stderr or r.stdout)[:200])
        return False
    return True


print("=== preconditions ===")
pods = subprocess.run(
    ["kubectl"] + NS + ["get", "pods", "-l", "app.kubernetes.io/name=hub", "--no-headers"],
    capture_output=True, text=True).stdout.strip().splitlines()
ready = [p for p in pods if " 1/1 " in p or "\t1/1\t" in p or " 1/1" in p]
check(len(ready) >= 2, "two hub replicas are running (or this proves nothing)",
      "%d ready" % len(ready))
if len(ready) < 2:
    sys.exit(1)

PW = subprocess.run(
    ["kubectl"] + NS + ["get", "secret", "nats-secrets", "-o",
                        "jsonpath={.data.NATS_MQTT_PASSWORD}"],
    capture_output=True, text=True).stdout
PW = subprocess.run(["base64", "-d"], input=PW, capture_output=True, text=True).stdout.strip()
check(bool(PW), "broker credentials read from the cluster")

print()
print("=== setup: a throwaway tenant and two devices that own nothing ===")
purge()
sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
    f"VALUES ('{T}','dup-probe','Dup probe','free','active',now(),now());")
for dev in (DEV_A, DEV_B):
    sql(f"INSERT INTO devices (imei,label,type,tenant_id,created_at,updated_at) "
        f"VALUES ('{dev}','dup probe','probe','{T}',now(),now());")

# A publisher pod, because nothing in the cluster ships an MQTT client.
#
# It MUST carry the hub-verify label (MESHSAT-1205). Since nats-confine went in,
# nats:1883 admits only named sources, and hub-verify is one of them -- but this
# pod is not the CronJob pod, it is a throwaway `kubectl run`, and an unlabelled
# throwaway is nobody as far as the policy is concerned. The symptom was
# "PUBLISH FAILED: Error: Bad file descriptor" on the first nightly after the
# policy landed; the two nightlies before it passed because they predated it.
#
# And it MUST satisfy the restricted Pod Security profile (MESHSAT-1204): Kyverno
# ENFORCES it in this namespace, so a bare `kubectl run` -- root, no seccomp, all
# capabilities -- is refused at admission. The mosquitto image ships a
# mosquitto user (1883:1883); nothing here needs more than that.
PUB_OVERRIDES = json.dumps({"spec": {
    "securityContext": {"runAsNonRoot": True, "runAsUser": 1883, "runAsGroup": 1883,
                        "seccompProfile": {"type": "RuntimeDefault"}},
    "containers": [{"name": PUB, "image": "docker.io/eclipse-mosquitto",
                    "command": ["sleep", "900"],
                    "securityContext": {"allowPrivilegeEscalation": False,
                                        "capabilities": {"drop": ["ALL"]}}}]}})
subprocess.run(["kubectl"] + NS + ["run", PUB, "--image=docker.io/eclipse-mosquitto",
                                   "--labels=app.kubernetes.io/name=hub-verify",
                                   "--restart=Never", f"--overrides={PUB_OVERRIDES}",
                                   "--command", "--", "sleep", "900"],
               capture_output=True, text=True)
subprocess.run(["kubectl"] + NS + ["wait", "--for=condition=Ready", f"pod/{PUB}",
                                   "--timeout=120s"], capture_output=True, text=True)

try:
    base = {"lat": 52.1, "lon": 4.3, "source": "probe", "timestamp": "2026-09-14T12:00:00Z"}

    print()
    print("1. ONE publish -> ONE row (the acceptance criterion)")
    check(publish(DEV_A, base), "published one position")
    time.sleep(6)
    n = rows(DEV_A)
    check(n == 1, "exactly one row after one publish, with both replicas up",
          "got %d row(s)%s" % (n, " -- BOTH REPLICAS INSERTED" if n > 1 else ""))

    print()
    print("2. the SAME message again is harmless (a broker replay must not double it)")
    check(publish(DEV_A, base), "published the identical payload again")
    time.sleep(6)
    n = rows(DEV_A)
    check(n == 1, "still exactly one row", "got %d" % n)

    print()
    print("3. it does not collapse too much -- a genuine second report is kept")
    moved = dict(base, lat=52.2, timestamp="2026-09-14T12:05:00Z")
    check(publish(DEV_A, moved), "published a different position for the same device")
    time.sleep(6)
    n = rows(DEV_A)
    check(n == 2, "now two rows", "got %d" % n)

    print()
    print("4. two DIFFERENT devices at IDENTICAL coordinates stay two rows")
    # This is the mutation that survived the first sweep on MESHSAT-1120: it is
    # the reason the TOPIC is in the digest. Without it, two kits parked side by
    # side collapse into one row and one of them disappears from the map.
    same = dict(base, lat=60.0, lon=10.0, timestamp="2026-09-14T13:00:00Z")
    ok = publish(DEV_A, same) and publish(DEV_B, same)
    check(ok, "published byte-identical payloads for two devices")
    time.sleep(6)
    na, nb = rows(DEV_A), rows(DEV_B)
    check(na == 3 and nb == 1,
          "each device kept its own row", "device A=%d (want 3) device B=%d (want 1)" % (na, nb))

    print()
    print("5. the row id is derived from the message, not from a clock")
    ids = sql(f"SELECT id FROM positions WHERE device_imei='{DEV_A}' ORDER BY id;")
    clocky = [i for i in ids.splitlines() if i.startswith("pos-1") and i[4:].isdigit()]
    check(not clocky, "no id is a nanosecond timestamp", "clock-derived: %s" % clocky[:2])
    check(all(i.startswith("pos-") for i in ids.splitlines() if i),
          "ids still say they are positions", ids.replace("\n", " ")[:90])
finally:
    print()
    print("=== purge ===")
    purge()
    left = rows(DEV_A) + rows(DEV_B)
    print("   position rows remaining:", left)

bad = [n for n, ok in results if not ok]
print()
print("%d/%d passed" % (len(results) - len(bad), len(results)))
sys.exit(1 if bad else 0)
