"""MESHSAT-613 acceptance on production: the Bridge relay client, inner TLS and all.

Provisions a throwaway tenant with two bridges (a kit and a phone), generates
their MQTT credentials AND issues their certificates through the real API
(the kit's must carry ServerAuth + a SAN, which is what 21f6289 added), then
runs the Bridge repo's RELAY_LIVE Go test: in one process the kit end serves a
chi router through hub.meshsat.net with mutual TLS inside the tunnel, and the
phone end dials back in through the Hub, verifies the kit's certificate under
the Hub CA with ServerName = kit id, and GETs /health. A client with no
certificate is refused by the kit's handshake. Run twice so the two ends cross
replicas; the Hub pods' logs are read for both. Everything is deleted at the end.

Run from the runner:  python3 scratchpad/relay-tunnel-live.py
"""
import hashlib, json, os, secrets, subprocess, sys, tempfile, time, urllib.error, urllib.request

DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]
API = "https://hub.meshsat.net"
T = "t-relay-tunnel"
KIT, PHONE = "relay-tunnel-kit", "relay-tunnel-phone"
BRIDGE_REPO = os.path.expanduser("~/gitlab/products/cubeos/meshsat")

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
    h = {"Content-Type": "application/json", "Authorization": "Bearer " + key}
    req = urllib.request.Request(API + path, data=data, method=method, headers=h)
    try:
        r = urllib.request.urlopen(req, timeout=45)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def cleanup():
    for stmt in (f"DELETE FROM bridges WHERE tenant_id='{T}';",
                 f"DELETE FROM api_keys WHERE tenant_id='{T}';",
                 f"DELETE FROM audit_log WHERE tenant_id='{T}';",
                 f"DELETE FROM tenants WHERE id='{T}';"):
        sql(stmt)


def cert_facts(pem_path):
    out = subprocess.run(["openssl", "x509", "-in", pem_path, "-noout", "-ext", "extendedKeyUsage,subjectAltName"],
                         capture_output=True, text=True).stdout
    return " ".join(out.split())


def main():
    print("=== setup: throwaway tenant, two bridges, credentials and certificates ===")
    cleanup()
    sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
        f"VALUES ('{T}','relay-tunnel','Relay tunnel probe','beta','active',now(),now());")
    plain = "meshsat_" + secrets.token_hex(32)
    h = hashlib.sha256(plain.encode()).hexdigest()
    sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
        f"VALUES ('{secrets.token_hex(8)}','{h}','{plain[:16]}','owner','relay tunnel probe','','0001-01-01 00:00:00+00','{T}',now());")
    tmp = tempfile.mkdtemp(prefix="relay-tunnel-")
    env = dict(os.environ, RELAY_LIVE="1", RELAY_HUB_API=API, RELAY_BRIDGE_ID=KIT, RELAY_CLIENT_ID=PHONE)
    ok_setup = True
    for bid, role in ((KIT, "BRIDGE"), (PHONE, "CLIENT")):
        s, b = call("POST", "/api/bridges", plain, {"bridge_id": bid, "label": "relay tunnel probe " + bid})
        check(f"bridge {bid} created", s in (200, 201), f"{s} {b[:80]}")
        s, b = call("POST", f"/api/bridges/{bid}/credentials", plain)
        pw = json.loads(b).get("password", "") if s == 200 else ""
        check(f"credentials generated for {bid}", bool(pw), f"{s}")
        env[f"RELAY_{role}_PASSWORD"] = pw
        s, b = call("POST", f"/api/bridges/{bid}/certificate", plain)
        cert = json.loads(b) if s in (200, 201) else {}
        check(f"certificate issued for {bid}", bool(cert.get("cert_pem")) and bool(cert.get("key_pem")) and bool(cert.get("ca_pem")), f"{s}")
        for k, name in (("cert_pem", "CERT"), ("key_pem", "KEY")):
            p = os.path.join(tmp, f"{bid}.{name.lower()}.pem")
            with open(p, "w") as f:
                f.write(cert.get(k, ""))
            os.chmod(p, 0o600)
            env[f"RELAY_{role}_{name}"] = p
        if bid == KIT:
            p = os.path.join(tmp, "ca.pem")
            with open(p, "w") as f:
                f.write(cert.get("ca_pem", ""))
            env["RELAY_CA"] = p
            facts = cert_facts(env["RELAY_BRIDGE_CERT"])
            check("the kit's certificate carries ServerAuth and a SAN equal to its id (21f6289 deployed)",
                  "TLS Web Server Authentication" in facts and f"DNS:{KIT}" in facts, facts[:120])
        ok_setup = ok_setup and bool(pw) and bool(cert.get("cert_pem"))
    if not ok_setup:
        cleanup()
        sys.exit(1)
    try:
        since = time.time()
        for n in (1, 2):
            print(f"=== run {n}: RELAY_LIVE go test in the Bridge repo ===")
            r = subprocess.run(["go", "test", "-count=1", "-run", "TestRelayLive$", "-v", "./internal/relayclient/"],
                               cwd=BRIDGE_REPO, env=env, capture_output=True, text=True, timeout=180)
            for l in r.stdout.splitlines():
                if "live_test.go" in l or "--- " in l or l.startswith("ok") or l.startswith("FAIL"):
                    print("   " + l.strip()[:200])
            check(f"[{n}] kit end served /health through the Hub with mutual TLS, no-cert client refused",
                  r.returncode == 0 and "--- PASS: TestRelayLive" in r.stdout, r.stderr.strip()[-160:])
            time.sleep(2)
        time.sleep(3)
        logs = subprocess.run(["kubectl", "--context", "notrf01", "-n", "meshsat-hub", "logs", "-l", "app.kubernetes.io/name=hub",
                               "--prefix", f"--since={int(time.time() - since) + 10}s", "--tail=3000"], capture_output=True, text=True).stdout
        pod = lambda l: l.split("]")[0].split("/")[1]
        serving = sorted({pod(l) for l in logs.splitlines() if "relay: bridge serving" in l and KIT in l})
        connected = sorted({pod(l) for l in logs.splitlines() if "relay: client connected" in l and PHONE in l})
        print(f"   bridge sockets landed on {serving}; client sockets on {connected}")
        check("the Hub logged the kit serving and the phone connecting", bool(serving) and bool(connected))
        check("both replicas took part (the rendezvous crossed the bus at least once)",
              len(set(serving) | set(connected)) >= 2, "same pod every time" if len(set(serving) | set(connected)) < 2 else "")
    finally:
        print("=== teardown ===")
        for bid in (KIT, PHONE):
            s, _ = call("DELETE", f"/api/bridges/{bid}", plain)
            check(f"bridge {bid} deleted through the API", s in (200, 204), str(s))
        cleanup()
        for f in os.listdir(tmp):
            os.remove(os.path.join(tmp, f))
        os.rmdir(tmp)
    failed = [n for n, ok in results if not ok]
    print(f"\n{len(results)} checks, {len(failed)} failed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
