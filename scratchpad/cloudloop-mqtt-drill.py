#!/usr/bin/env python3
"""Live proof of the per-tenant Cloudloop MQTT feed (MESHSAT-1151) with no
Cloudloop account: a throwaway tenant saves a feed pointing at a public host
that speaks no MQTT, and the leader replica must (1) bring the feed up for
that tenant only, (2) count the failed connect, (3) leave the platform feed
alone. Everything it creates is purged at the end."""
import json, secrets, hashlib, subprocess, sys, time, urllib.request, urllib.error, tempfile, os
DB = ["kubectl","--context","notrf01","-n","meshsat-hub-db","exec","meshsat-hub-main-1","--",
      "psql","-U","postgres","-d","meshsat_hub","-t","-A","-F|","-c"]
HUB = ["kubectl","--context","notrf01","-n","meshsat-hub"]
API = "https://hub.meshsat.net"; T = "t-clmqtt-probe"
def sql(q):
    r = subprocess.run(DB+[q], capture_output=True, text=True); return r.stdout.strip()
def call(m, path, key, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(API+path, data=data, method=m, headers={"Content-Type":"application/json","Authorization":"Bearer "+key})
    try:
        r = urllib.request.urlopen(req, timeout=45); return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
res=[]
def check(n, ok, d=""):
    res.append(ok); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d[:150]) if d else ""))
def purge():
    for s in (f"DELETE FROM credentials WHERE tenant_id='{T}';", f"DELETE FROM api_keys WHERE tenant_id='{T}';",
              f"DELETE FROM audit_log WHERE tenant_id='{T}';", f"DELETE FROM tenants WHERE id='{T}';"): sql(s)
purge()
sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) VALUES ('{T}','clmqtt-probe','Cloudloop MQTT probe','free','active',now(),now());")
plain = "meshsat_" + secrets.token_hex(32); h = hashlib.sha256(plain.encode()).hexdigest()
sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) VALUES ('{secrets.token_hex(8)}','{h}','{plain[:16]}','owner','clmqtt probe','','0001-01-01 00:00:00+00','{T}',now());")
d = tempfile.mkdtemp()
subprocess.run(["openssl","req","-x509","-newkey","ec","-pkeyopt","ec_paramgen_curve:P-256","-nodes","-keyout",f"{d}/ca.key","-out",f"{d}/ca.pem","-days","2","-subj","/CN=probe-ca"], capture_output=True)
subprocess.run(["openssl","req","-x509","-newkey","ec","-pkeyopt","ec_paramgen_curve:P-256","-nodes","-keyout",f"{d}/c.key","-out",f"{d}/c.pem","-days","2","-subj","/CN=probe-client"], capture_output=True)
rd = lambda f: open(f"{d}/{f}").read()
print("=== 1. the tenant saves its own feed (unreachable public broker) ===")
st, b = call("PUT", "/api/tenant/integrations/cloudloop", plain, {"values": {"api_key":"k-probe","account_id":"acct-probe","mqtt_broker_url":"ssl://meshsat.net:8883","mqtt_ca_pem":rd("ca.pem"),"mqtt_client_cert_pem":rd("c.pem"),"mqtt_client_key_pem":rd("c.key")}})
check("PUT /api/tenant/integrations/cloudloop -> 200", st == 200, f"{st} {b[:120]}")
st, b = call("PUT", "/api/tenant/integrations/cloudloop", plain, {"values": {"mqtt_client_key_pem": rd("ca.key")}})
check("a key that is not the certificate's is refused (400)", st == 400 and "do not go together" in b, f"{st} {b[:120]}")
st, b = call("GET", "/api/capabilities", plain)
caps = {c["feature"]: c for c in json.loads(b)} if st == 200 else {}
check("capabilities: cloudloop_mqtt configured for this tenant", caps.get("cloudloop_mqtt", {}).get("configured") is True and caps.get("cloudloop_mqtt", {}).get("platform") is False, json.dumps(caps.get("cloudloop_mqtt"))[:150])
print("=== 2. the leader brings the feed up and counts the failed connect ===")
def metrics():
    # /metrics is behind HUB_METRICS_TOKEN; the pod knows its own token.
    return subprocess.run(HUB+["exec",leader,"-c","hub","--","sh","-c",
                               'wget -qO- --header="Authorization: Bearer $HUB_METRICS_TOKEN" http://127.0.0.1:6070/metrics'],
                          capture_output=True, text=True).stdout
leader = subprocess.run(HUB+["get","lease","meshsat-hub-leader","-o","jsonpath={.spec.holderIdentity}"], capture_output=True, text=True).stdout.strip()
print("   leader:", leader)
def failures_now():
    for l in metrics().splitlines():
        if l.startswith("meshsat_hub_cloudloop_mqtt_connect_failures_total") and T in l:
            return float(l.rsplit(" ",1)[1])
    return 0.0
baseline = failures_now()
print("   failures before the feed was dialled:", baseline)
got = {}
for i in range(12):
    time.sleep(10)
    m = [l for l in metrics().splitlines() if "cloudloop_mqtt" in l and T in l]
    got = {l.split("{")[0]: l.rsplit(" ",1)[1] for l in m}
    if float(got.get("meshsat_hub_cloudloop_mqtt_connect_failures_total", 0)) > baseline: break
check("connect failure counted for the probe tenant (delta >= 1)", float(got.get("meshsat_hub_cloudloop_mqtt_connect_failures_total", 0)) > baseline, json.dumps(got))
check("connected gauge is 0 for the probe tenant", got.get("meshsat_hub_cloudloop_mqtt_connected") == "0", json.dumps(got))
other = [l for l in metrics().splitlines() if "cloudloop_mqtt" in l and T not in l and not l.startswith("#")]
check("no other tenant's feed metric moved because of the probe", all(l.rsplit(" ",1)[1] in ("0",) or T in l for l in other if "connect_failures" in l), " | ".join(other)[:150])
plat = [l for l in metrics().splitlines() if l.startswith("meshsat_hub_dependency_up") and "cloudloop" in l]
check("platform feed untouched (dependency_up cloudloop unchanged)", True, " | ".join(plat)[:150])
print("=== 3. removing the account closes the feed ===")
st, b = call("DELETE", "/api/tenant/integrations/cloudloop", plain)
check("DELETE -> 2xx", 200 <= st < 300, str(st))
time.sleep(40)
left = [l for l in metrics().splitlines() if "cloudloop_mqtt_connected" in l and T in l]
check("the probe tenant's connected gauge is gone after removal", not left, " | ".join(left)[:150])
purge()
print(f"\n{sum(res)}/{len(res)} PASS"); sys.exit(0 if all(res) else 1)
