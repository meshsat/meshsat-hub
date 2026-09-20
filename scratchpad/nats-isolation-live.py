"""MESHSAT-1033 acceptance on production: what can a CUSTOMER bridge see and do on the broker?

Provisions two throwaway tenants through the real API, exactly as an approved customer would:
  - t-nats-probe:  one bridge with MQTT credentials and a Hub-issued certificate (the "attacker")
  - t-nats-victim: one registered device (the "victim")
then connects the probe bridge to wss://mqtt-hub.meshsat.net/mqtt with its own credentials and
certificate, subscribes to every topic family it should NOT reach, and counts what arrives.
Finally it publishes a position for the victim's device under its OWN namespace and reads
where the Hub filed it.

Nothing is printed of what arrives but topic family and count: the point is to prove a leak
without copying anybody's messages. Everything is deleted at the end.

  python3 scratchpad/nats-isolation-live.py --expect pre    # the defect, before the fix
  python3 scratchpad/nats-isolation-live.py --expect post   # after the fix is deployed
"""
import argparse, hashlib, json, os, secrets, ssl, subprocess, sys, tempfile, time, urllib.error, urllib.request

import paho.mqtt.client as mqtt

DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "-c", "postgres", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]
API = "https://hub.meshsat.net"
PROBE_T, VICTIM_T = "t-nats-probe", "t-nats-victim"
PROBE_BRIDGE, VICTIM_DEV = "nats-probe-kit", "nats-victim-dev"
WATCH_S = 75

results = []


def check(name, ok, detail=""):
    results.append((name, ok))
    print(("  PASS  " if ok else "  FAIL  ") + name + (("   " + detail) if detail else ""))


def sql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    err = [l for l in r.stderr.splitlines() if "ERROR" in l]
    if err:
        print("   SQL ERR:", err[-1][:170])
    return r.stdout.strip()


def call(method, path, key, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API + path, data=data, method=method,
                                 headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
    try:
        r = urllib.request.urlopen(req, timeout=45)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def tenant_with_key(tid, slug):
    sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) VALUES ('{tid}','{slug}','{slug}','beta','active',now(),now()) ON CONFLICT DO NOTHING;")
    plain = "meshsat_" + secrets.token_hex(32)
    sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
        f"VALUES ('{secrets.token_hex(8)}','{hashlib.sha256(plain.encode()).hexdigest()}','{plain[:16]}','owner','nats isolation probe','','0001-01-01 00:00:00+00','{tid}',now());")
    return plain


def cleanup(pkey=None, vkey=None):
    if pkey:
        call("DELETE", f"/api/bridges/{PROB