"""MESHSAT-613: reach a REAL field kit's API through the Hub relay, as a phone would.

The kit (default: nllei01tesseract01) is serving through the Hub with its own
relay client (kit-relay-cert.py re-issued its certificate, the deploy job
restarted it). This provisions a throwaway "phone" bridge in the kit's tenant
through the real API (credentials + certificate), runs the Bridge repo's
TestRelayLive in RELAY_CLIENT_ONLY mode against the kit's id, which opens the
tunnel through hub.meshsat.net, does the mutual-TLS handshake against the KIT's
new certificate with ServerName = kit id, and GETs the kit's real /health; a
client without a certificate is refused by the kit. Then deletes the phone.

Usage: python3 scratchpad/kit-relay-e2e.py [bridge_id]
"""
import hashlib, json, os, secrets, subprocess, sys, tempfile, urllib.error, urllib.request

DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "-c", "postgres", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]
API = "https://hub.meshsat.net"
BRIDGE_REPO = os.path.expanduser("~/gitlab/products/cubeos/meshsat")
PHONE = "relay-probe-phone"


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


def main():
    kit = sys.argv[1] if len(sys.argv) > 1 else "nllei01tesseract01"
    tenant = sql(f"select tenant_id from bridges where bridge_id='{kit}';")
    if not tenant:
        sys.exit(f"{kit} not on the Hub")
    plain = "meshsat_" + secrets.token_hex(32)
    kid = secrets.token_hex(8)
    sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
        f"VALUES ('{kid}','{hashlib.sha256(plain.encode()).hexdigest()}','{plain[:16]}','owner','relay e2e probe (temporary)','','0001-01-01 00:00:00+00','{tenant}',now());")
    tmp = tempfile.mkdtemp(prefix="kit-relay-")
    rc = 1
    try:
        call("DELETE", f"/api/bridges/{PHONE}", plain)
        s, b = call("POST", "/api/bridges", plain, {"bridge_id": PHONE, "label": "relay e2e probe phone"})
        if s not in (200, 201):
            sys.exit(f"phone bridge: {s} {b[:120]}")
        s, b = call("POST", f"/api/bridges/{PHONE}/credentials", plain)
        pw = json.loads(b)["password"]
        s, b = call("POST", f"/api/bridges/{PHONE}/certificate", plain)
        cert = json.loads(b)
        env = dict(os.environ, RELAY_LIVE="1", RELAY_CLIENT_ONLY="1", RELAY_HUB_API=API, RELAY_BRIDGE_ID=kit,
                   RELAY_CLIENT_ID=PHONE, RELAY_CLIENT_PASSWORD=pw)
        for k, name in (("cert_pem", "RELAY_CLIENT_CERT"), ("key_pem", "RELAY_CLIENT_KEY"), ("ca_pem", "RELAY_CA")):
            p = os.path.join(tmp, name.lower() + ".pem")
            with open(p, "w") as f:
                f.write(cert[k])
            os.chmod(p, 0o600)
            env[name] = p
        print(f"phone {PHONE} provisioned in tenant {tenant}; dialling {kit} through the Hub")
        r = subprocess.run(["go", "test", "-count=1", "-run", "TestRelayLive$", "-v", "./internal/relayclient/"],
                           cwd=BRIDGE_REPO, env=env, capture_output=True, text=True, timeout=180)
        for l in r.stdout.splitlines():
            if "live_test.go" in l or "--- " in l or l.startswith("ok") or l.startswith("FAIL"):
                print("   " + l.strip()[:220])
        if r.returncode != 0:
            print(r.stderr[-400:])
        rc = r.returncode
    finally:
        s, _ = call("DELETE", f"/api/bridges/{PHONE}", plain)
        print("phone bridge deleted:", s)
        sql(f"DELETE FROM api_keys WHERE id='{kid}';")
        for f in os.listdir(tmp):
            os.remove(os.path.join(tmp, f))
        os.rmdir(tmp)
    sys.exit(rc)


if __name__ == "__main__":
    main()
