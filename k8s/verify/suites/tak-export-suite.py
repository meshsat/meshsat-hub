#!/usr/bin/env python3
"""A TAK message a kit exports is consumed once, by the leader, for its own tenant.

MESHSAT-1458. Kits and apps publish the Cursor-on-Target they build on their own
bridge topic and the Hub forwards it to the tenant's TAK servers. Unit tests hold
the rules; this holds the three things only production can show:

  - the Hub is really subscribed to the tenant-shaped export topic on the real
    broker, and the message reaches the consumer;
  - "is this bridge registered to this tenant" is answered correctly by POSTGRES.
    The unit tests run on SQLite, and a no-row answer that Postgres words
    differently would turn every unknown bridge into a store error;
  - with two replicas up, exactly ONE of them (the lease holder) takes the
    message. Both subscribe; a replica that is not the leader must do nothing.

It reads meshsat_hub_takhosted_cot_total on BOTH pods before and after each
publish and compares the difference.

SAFETY. The probe tenant has no TAK server, so an accepted event is counted as
"nowhere" and written to nothing. It has no device, no route, no provider
account and no geofence; the export topic does not reach the routing engine.
The publisher uses the Hub's own broker user, as position-dedup-suite does.
Nothing leaves the cluster, and everything is deleted at the end.
"""
import datetime
import json
import subprocess
import sys
import time

CTX = ["--context", "notrf01"]
NS = CTX + ["-n", "meshsat-hub"]
DBNS = CTX + ["-n", "meshsat-hub-db"]
DB = ["kubectl"] + DBNS + ["exec", "meshsat-hub-main-1", "--",
                           "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-c"]
T = "t-tak-probe"          # the tenant the probe bridge belongs to
T2 = "t-tak-probe-b"       # a second tenant, to own a bridge the first does not
KIT = "tak-probe-kit"      # registered to T
OTHER = "tak-probe-other"  # registered to T2
GHOST = "tak-probe-ghost"  # registered to nobody
PUB = "tak-probe-pub"
METRIC = "meshsat_hub_takhosted_cot_total"

results = []


def check(ok, n, d=""):
    results.append((n, ok))
    print(("  PASS  " if ok else "  FAIL  ") + n + (("   " + d) if d else ""))


def sql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    for line in r.stderr.splitlines():
        if "ERROR" in line:
            print("   SQL ERR:", line[:170])
    return r.stdout.strip()


def hub_pods():
    out = subprocess.run(["kubectl"] + NS + ["get", "pods", "-l", "app.kubernetes.io/name=hub",
                                             "--no-headers", "-o", "custom-columns=N:.metadata.name,R:.status.containerStatuses[0].ready"],
                         capture_output=True, text=True).stdout
    return [line.split()[0] for line in out.splitlines() if line.split()[-1:] == ["true"]]


def counts(pod):
    """One pod's counter, as {result: value}. The pod's own token, never printed."""
    r = subprocess.run(["kubectl"] + NS + ["exec", pod, "-c", "hub", "--", "sh", "-c",
                                           'wget -qO- --header "Authorization: Bearer $HUB_METRICS_TOKEN" '
                                           "http://localhost:6070/metrics"],
                       capture_output=True, text=True)
    out = {}
    for line in r.stdout.splitlines():
        if not line.startswith(METRIC + "{") or 'source="client"' not in line:
            continue
        labels, value = line.rsplit(" ", 1)
        result = labels.split('result="', 1)[1].split('"', 1)[0]
        out[result] = float(value)
    return out


def snapshot(pods):
    return {p: counts(p) for p in pods}


def moved(before, after, result):
    """How far `result` moved on each pod, as a list in pod order."""
    return [int(round(after[p].get(result, 0) - before[p].get(result, 0))) for p in sorted(before)]


def cot(uid):
    now = datetime.datetime.now(datetime.timezone.utc)
    ts = now.strftime("%Y-%m-%dT%H:%M:%SZ")
    stale = (now + datetime.timedelta(minutes=2)).strftime("%Y-%m-%dT%H:%M:%SZ")
    return ('<event version="2.0" uid="%s" type="a-f-G-U-C" how="m-g" time="%s" start="%s" stale="%s">'
            '<point lat="52.1" lon="4.3" hae="0" ce="10" le="10"/>'
            '<detail><contact callsign="TAK-PROBE"/></detail></event>') % (uid, ts, ts, stale)


def publish(tenant, bridge, body):
    topic = "meshsat/%s/bridge/%s/tak/cot/out" % (tenant, bridge)
    r = subprocess.run(
        ["kubectl"] + NS + ["exec", PUB, "--", "mosquitto_pub",
                            "-h", "nats", "-p", "1883", "-q", "1",
                            "-u", "meshsat", "-P", PW,
                            "-t", topic, "-m", body],
        capture_output=True, text=True)
    if r.returncode != 0:
        print("   PUBLISH FAILED:", (r.stderr or r.stdout)[:200])
        return False
    return True


def purge():
    for b in (KIT, OTHER, GHOST):
        sql(f"DELETE FROM bridges WHERE bridge_id='{b}';")
    for t in (T, T2):
        sql(f"DELETE FROM audit_log WHERE tenant_id='{t}';")
        sql(f"DELETE FROM tenants WHERE id='{t}';")
    subprocess.run(["kubectl"] + NS + ["delete", "pod", PUB, "--ignore-not-found", "--wait=false"],
                   capture_output=True, text=True)


print("=== preconditions ===")
pods = hub_pods()
check(len(pods) >= 2, "two hub replicas are running (or the leader check proves nothing)", "%d ready" % len(pods))
if len(pods) < 2:
    sys.exit(1)
first = snapshot(pods)
check(all("forwarded" in c and "unregistered" in c for c in first.values()),
      "the counter is exported on every replica, with its series materialised",
      "; ".join("%s: %d series" % (p, len(c)) for p, c in sorted(first.items())))
if not all(first.values()):
    print("   the deployed Hub does not have the consumer; nothing further can be checked")
    sys.exit(1)

PW = subprocess.run(
    ["kubectl"] + NS + ["get", "secret", "nats-secrets", "-o", "jsonpath={.data.NATS_MQTT_PASSWORD}"],
    capture_output=True, text=True).stdout
PW = subprocess.run(["base64", "-d"], input=PW, capture_output=True, text=True).stdout.strip()
check(bool(PW), "broker credentials read from the cluster")

print()
print("=== setup: two throwaway tenants, one bridge each, no TAK server ===")
purge()
for t, slug in ((T, "tak-probe"), (T2, "tak-probe-b")):
    sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
        f"VALUES ('{t}','{slug}','TAK probe','free','active',now(),now());")
sql(f"INSERT INTO bridges (bridge_id,tenant_id,label) VALUES ('{KIT}','{T}','tak probe');")
sql(f"INSERT INTO bridges (bridge_id,tenant_id,label) VALUES ('{OTHER}','{T2}','tak probe b');")
check(sql(f"SELECT count(*) FROM bridges WHERE bridge_id IN ('{KIT}','{OTHER}');") == "2",
      "both probe bridges are registered")

# The publisher pod: the hub-verify label for the broker's network policy
# (MESHSAT-1205) and the restricted profile for Kyverno (MESHSAT-1204). See
# position-dedup-suite.py, which this copies.
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
subprocess.run(["kubectl"] + NS + ["wait", "--for=condition=Ready", f"pod/{PUB}", "--timeout=120s"],
               capture_output=True, text=True)

try:
    stamp = str(int(time.time()))
    good = cot("TAK-PROBE-" + stamp)

    print()
    print("1. ONE export from a registered bridge is taken ONCE, by ONE replica")
    before = snapshot(pods)
    check(publish(T, KIT, good), "published one event on the probe bridge's export topic")
    time.sleep(6)
    after = snapshot(pods)
    took = moved(before, after, "nowhere")
    check(sorted(took) == [0] * (len(pods) - 1) + [1],
          "exactly one replica accepted it (no TAK server, so it is counted as 'nowhere')",
          "per replica: %s%s" % (took, "  -- BOTH REPLICAS TOOK IT" if sum(took) > 1 else
                                 "  -- NOBODY TOOK IT: is the Hub subscribed?" if sum(took) == 0 else ""))
    check(sum(moved(before, after, "unregistered")) == 0 and sum(moved(before, after, "error")) == 0,
          "and it was not refused: Postgres answered that the bridge is registered")
    check(sum(moved(before, after, "forwarded")) == 0,
          "nothing was written to any TAK server (the probe tenant has none)")

    print()
    print("2. the SAME event again is a redelivery, not a second event")
    before = snapshot(pods)
    check(publish(T, KIT, good), "published the identical payload again")
    time.sleep(6)
    after = snapshot(pods)
    check(sum(moved(before, after, "duplicate")) == 1 and sum(moved(before, after, "nowhere")) == 0,
          "counted once as a duplicate and not taken again",
          "duplicate %s, nowhere %s" % (moved(before, after, "duplicate"), moved(before, after, "nowhere")))

    print()
    print("3. a bridge NOBODY registered is refused, and that is a 'no', not a store error")
    before = snapshot(pods)
    check(publish(T, GHOST, cot("TAK-PROBE-GHOST-" + stamp)), "published as a bridge that does not exist")
    time.sleep(6)
    after = snapshot(pods)
    check(sum(moved(before, after, "unregistered")) == 1,
          "counted as unregistered", "unregistered %s" % moved(before, after, "unregistered"))
    check(sum(moved(before, after, "error")) == 0,
          "and not as a store error (Postgres's no-row is read as 'not registered')",
          "error %s" % moved(before, after, "error"))
    check(sum(moved(before, after, "nowhere")) + sum(moved(before, after, "forwarded")) == 0,
          "and it went nowhere")

    print()
    print("4. ANOTHER tenant's bridge named under this tenant is refused, never adopted")
    before = snapshot(pods)
    check(publish(T, OTHER, cot("TAK-PROBE-OTHER-" + stamp)),
          "published under the probe tenant, naming a bridge the second tenant owns")
    time.sleep(6)
    after = snapshot(pods)
    check(sum(moved(before, after, "unregistered")) == 1,
          "counted as unregistered: the tenant is the topic's, and the bridge is not in it",
          "unregistered %s" % moved(before, after, "unregistered"))
    check(sum(moved(before, after, "nowhere")) + sum(moved(before, after, "forwarded")) == 0,
          "and it was filed under neither tenant")

    print()
    print("5. a payload that is not exactly one event is refused whole")
    before = snapshot(pods)
    two = cot("TAK-PROBE-TWO-A-" + stamp) + cot("TAK-PROBE-TWO-B-" + stamp)
    check(publish(T, KIT, two), "published two events in one payload")
    time.sleep(6)
    after = snapshot(pods)
    check(sum(moved(before, after, "structure")) == 1,
          "counted as refused for its structure", "structure %s" % moved(before, after, "structure"))
    check(sum(moved(before, after, "nowhere")) + sum(moved(before, after, "forwarded")) == 0,
          "and neither half was taken")
finally:
    print()
    print("=== purge ===")
    purge()
    left = sql(f"SELECT count(*) FROM bridges WHERE bridge_id IN ('{KIT}','{OTHER}','{GHOST}');")
    print("   probe bridges remaining:", left)

bad = [n for n, ok in results if not ok]
print()
print("%d/%d passed" % (len(results) - len(bad), len(results)))
sys.exit(1 if bad else 0)
