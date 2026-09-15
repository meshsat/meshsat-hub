#!/usr/bin/env python3
"""The real proof of the per-tenant Cloudloop MQTT feed (MESHSAT-1151): the
platform's own Cloudloop certificate, saved on the TEST TENANT's account
through the tenant API, must bring up a second, tenant-scoped connection to
Cloudloop's real broker beside the platform feed, under its own client id.
The test tenant owns no devices, so any MO that arrives on its feed is
refused as another tenant's device and processed by the platform feed only.
Everything created here is removed at the end. Secrets stay in memory."""
import json, secrets, hashlib, subprocess, sys, time, urllib.request, urllib.error
HUB = ["kubectl","--context","notrf01","-n","meshsat-hub"]
DB = ["kubectl","--context","notrf01","-n","meshsat-hub-db","exec","meshsat-hub-main-1","-c","postgres","--",
      "psql","-U","postgres","-d","meshsat_hub","-t","-A","-F|","-c"]
API = "https://hub.meshsat.net"; T = "t_218d449edba9e7ed"
def sh(cmd): return subprocess.run(cmd, capture_output=True, text=True).stdout
def sql(q): return sh(DB+[q]).strip()
def call(m, path, key, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API+path, data=data, method=m, headers={"Content-Type":"application/json","Authorization":"Bearer "+key})
    try:
        r = urllib.request.urlopen(req, timeout=45); return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
res=[]
def check(n, ok, d=""):
    res.append(bool(ok)); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d[:150]) if d else ""))
pod = sh(HUB+["get","pods","-l","app.kubernetes.io/name=hub","-o","name"]).split()[0]
env = lambda v: sh(HUB+["exec",pod,"-c","hub","--","sh","-c",f"printenv {v}"]).strip()
cat = lambda p: sh(HUB+["exec",pod,"-c","hub","--","cat",p])
broker, account = env("HUB_CLOUDLOOP_MQTT_BROKER"), env("HUB_CLOUDLOOP_ACCOUNT_ID")
ca, cert, key = cat(env("HUB_CLOUDLOOP_MQTT_CA_CERT")), cat(env("HUB_CLOUDLOOP_MQTT_CERT")), cat(env("HUB_CLOUDLOOP_MQTT_KEY"))
check("platform certificate material read from the pod (not shown)", all(x.startswith("-----BEGIN") for x in (ca,cert,key)) and broker and account, f"broker={broker}")
leader = sh(HUB+["get","lease","meshsat-hub-leader","-o","jsonpath={.spec.holderIdentity}"]).strip()
def metrics():
    return sh(HUB+["exec",leader,"-c","hub","--","sh","-c",'wget -qO- --header="Authorization: Bearer $HUB_METRICS_TOKEN" http://127.0.0.1:6070/metrics'])
def gauge():
    for l in metrics().splitlines():
        if l.startswith("meshsat_hub_cloudloop_mqtt_connected") and T in l: return l.rsplit(" ",1)[1]
    return None
assert sql(f"select count(*) from credentials where tenant_id='{T}' and provider='cloudloop'") == "0", "the test tenant already has a Cloudloop account; not touching it"
plain = "meshsat_" + secrets.token_hex(32); h = hashlib.sha256(plain.encode()).hexdigest(); kid = secrets.token_hex(8)
sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) VALUES ('{kid}','{h}','{plain[:16]}','owner','cloudloop mqtt live drill','','0001-01-01 00:00:00+00','{T}',now());")
t0 = time.time()
try:
    print("=== 1. the tenant saves the feed through its own API ===")
    st, b = call("PUT", "/api/tenant/integrations/cloudloop", plain, {"values": {"api_key":"drill-not-used","account_id":account,"mqtt_broker_url":broker,"mqtt_ca_pem":ca,"mqtt_client_cert_pem":cert,"mqtt_client_key_pem":key}})
    check("PUT /api/tenant/integrations/cloudloop -> 200", st == 200, str(st))
    print("=== 2. the leader connects the tenant's feed to Cloudloop's real broker ===")
    g = None
    for i in range(18):
        time.sleep(10); g = gauge()
        if g == "1": break
    check("connected gauge is 1 for the test tenant", g == "1", f"gauge={g} after {int(time.time()-t0)}s")
    log = sh(HUB+["logs",leader,"-c","hub","--since=5m"])
    sub = [l for l in log.splitlines() if "a tenant's feed is subscribed" in l and T in l]
    check("leader logged the subscription under the tenant's own client id", bool(sub), (json.loads(sub[-1]).get("client_id","") + " " + json.loads(sub[-1]).get("topic","")) if sub else "no such line")
    lost = [l for l in log.splitlines() if '"cloudloop mqtt: connection lost"' in l and time.time()-t0 < 600]
    check("the platform feed was not evicted (no 'connection lost' since the drill began)", not [l for l in lost if l[8:27] >= time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t0))], str(len(lost)))
    fails = [l for l in metrics().splitlines() if "cloudloop_mqtt_connect_failures_total" in l and T in l]
    check("no connect failure counted for the tenant", not fails or fails[0].endswith(" 0"), " ".join(fails)[:120])
finally:
    print("=== 3. cleanup: the account is removed, the feed closes, the key goes ===")
    st, _ = call("DELETE", "/api/tenant/integrations/cloudloop", plain)
    check("DELETE -> 2xx", 200 <= st < 300, str(st))
    for i in range(9):
        time.sleep(10)
        if gauge() is None: break
    check("the tenant's connected gauge is gone", gauge() is None)
    log = sh(HUB+["logs",leader,"-c","hub","--since=3m"])
    check("leader logged the feed closing", any("closing a tenant's feed" in l and T in l for l in log.splitlines()))
    sql(f"DELETE FROM api_keys WHERE id='{kid}';")
    check("drill key removed; tenant has no Cloudloop row", sql(f"select count(*) from api_keys where id='{kid}'")=="0" and sql(f"select count(*) from credentials where tenant_id='{T}' and provider='cloudloop'")=="0")
print(f"\n{sum(res)}/{len(res)} PASS"); sys.exit(0 if all(res) else 1)
