#!/usr/bin/env python3
"""A saved provider account must be in force on BOTH replicas at once.

MESHSAT-1127. Each replica caches resolved accounts for 60 seconds and
Invalidate used to be in-memory only, so the replica that did not serve the
write kept its old answer -- including a cached NEGATIVE, which is the damaging
direction: it behaves as though the tenant has no credentials, and ForTenant is
the resolution point for every client pool. No SMS sent, no notification
delivered, no position injected, no mail. Both replicas take traffic.

WHY THIS RUNS FIVE TIMES. The bug is a race, so ONE run proves nothing: the
capabilities suite scored 13/13 and then 11/13 with no code change between them,
and the 13/13 was load-balancer luck. Each run seeds the negative cache on both
pods, writes through the public edge so the PUT lands wherever the balancer
sends it, and reads both pods 1.5 seconds later -- a second, not the 60s TTL.

Safe against production: a throwaway tenant per run, purged at the end. It
writes one ntfy URL and publishes nothing to it. No device, no message, no
airtime, nobody paged.
"""

import hashlib
import json
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.request

NS = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub"]
DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-c"]
API = "https://hub.meshsat.net"
RUNS = 5


def sql(q):
    return subprocess.run(DB + [q], capture_output=True, text=True).stdout.strip()


def purge(t):
    for s in (f"DELETE FROM credentials WHERE tenant_id='{t}';",
              f"DELETE FROM api_keys WHERE tenant_id='{t}';",
              f"DELETE FROM audit_log WHERE tenant_id='{t}';",
              f"DELETE FROM tenants WHERE id='{t}';"):
        sql(s)


def pods():
    """Only replicas that are Running, Ready and not terminating: during a
    rollout the label also matches the pod on its way out, which answers '?'
    and turns a healthy roll into a false failure."""
    out = subprocess.run(NS + ["get", "pods", "-l", "app.kubernetes.io/name=hub", "-o", "json"],
                         capture_output=True, text=True).stdout
    try:
        items = json.loads(out)["items"]
    except (ValueError, KeyError):
        return []
    live = []
    for it in items:
        if it["status"].get("phase") != "Running" or it["metadata"].get("deletionTimestamp"):
            continue
        if all(c.get("ready") for c in it["status"].get("containerStatuses", [])):
            live.append(it["metadata"]["name"])
    return live


def configured_on(pod, key, feature="notifications"):
    """Ask ONE pod directly. Going through the edge would pick one at random,
    which is precisely what this test must not do."""
    out = subprocess.run(
        NS + ["exec", pod, "-c", "hub", "--", "sh", "-c",
              f"wget -qO- --header='Authorization: Bearer {key}' "
              f"http://127.0.0.1:6070/api/capabilities 2>/dev/null"],
        capture_output=True, text=True).stdout
    try:
        return {c["feature"]: c["configured"] for c in json.loads(out)}[feature]
    except Exception:
        return "?"


def main():
    ps = pods()
    if len(ps) < 2:
        print("FAIL  fewer than two replicas are running, so this proves nothing:", ps)
        return 1

    img = subprocess.run(
        NS + ["get", "deploy", "hub", "-o",
              "jsonpath={.spec.template.spec.containers[0].image}"],
        capture_output=True, text=True).stdout
    print("binary under test:", img.split("@sha256:")[-1][:12])
    print("replicas:", " ".join(ps))
    print()

    failures = 0
    run = 0
    retried = 0
    while run < RUNS:
        run += 1
        # Re-list every run: a Reloader or Argo roll during the suite swaps a pod
        # out, and asking the old one answers '?' about nothing.
        ps = pods()
        if len(ps) < 2:
            print(f"  run {run}: fewer than two ready replicas right now ({ps}); waiting for the roll")
            time.sleep(15); run -= 1; retried += 1
            if retried > 8:
                print("FAIL  the deployment never settled on two ready replicas"); return 1
            continue
        t = f"t-replica-{run}"
        purge(t)
        sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
            f"VALUES ('{t}','replica-{run}','Replica probe','free','active',now(),now());")
        key = "meshsat_" + secrets.token_hex(32)
        kh = hashlib.sha256(key.encode()).hexdigest()
        # expires_at must be Go's zero time; the column default is the epoch,
        # which the middleware reads as already expired.
        sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,"
            f"expires_at,tenant_id,created_at) VALUES ('{secrets.token_hex(8)}','{kh}',"
            f"'{key[:16]}','owner','replica probe','','0001-01-01 00:00:00+00','{t}',now());")

        # Seed the NEGATIVE cache on BOTH pods. Without this the replicas have
        # nothing cached and would agree by accident.
        for p in ps:
            configured_on(p, key)

        req = urllib.request.Request(
            API + "/api/tenant/integrations/ntfy",
            data=json.dumps({"values": {"url": "https://ntfy.sh"}}).encode(),
            method="PUT",
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
        try:
            code = urllib.request.urlopen(req, timeout=30).status
        except urllib.error.HTTPError as e:
            code = e.code
        if code != 200:
            print(f"  run {run}: FAIL  the write itself was refused ({code})")
            failures += 1
            purge(t)
            continue

        time.sleep(1.5)
        seen = {p: configured_on(p, key) for p in ps}
        purge(t)
        if all(v is True for v in seen.values()):
            print(f"  run {run}: PASS  both replicas agree 1.5s after the write")
            continue
        gone = [p for p, v in seen.items() if v == "?" and p not in pods()]
        if gone and retried < 8:
            # The pod left mid-run (a rollout), so this run proved nothing
            # either way; do it again against the pods that exist now.
            print(f"  run {run}: RETRY  {gone} left during the run (rollout); repeating")
            retried += 1; run -= 1
            continue
        print(f"  run {run}: FAIL  {seen}")
        failures += 1

    print()
    print(f"{RUNS - failures}/{RUNS} runs passed")
    if failures:
        print("A replica serving the stale answer will not send that tenant's SMS, "
              "deliver its notifications or inject its positions.")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
