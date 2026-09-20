"""Re-issue a field kit's Hub certificate so it can serve the relay (MESHSAT-613).

Certificates issued before 2026-09-15 carry only ClientAuth and no SAN, so the
Bridge's relay client refuses to start on them ("re-issue it on the Hub's Fleet
page"). This does what the Fleet page does, through the same APIs and nothing
else: a temporary owner API key on the kit's tenant, POST /api/bridges/{id}/certificate
on the Hub, PUT /api/routing/hub on the kit (password untouched: the Bridge keeps
the stored one when the field is empty), then the temporary key is deleted. The
kit picks the new certificate up at its next restart (the deploy job). MQTT keeps
working on the old certificate meanwhile: NATS verifies against the CA and the
Hub keeps no revocation list.

Usage: python3 scratchpad/kit-relay-cert.py <bridge_id> [<kit host>]
"""
import hashlib, json, secrets, subprocess, sys, urllib.error, urllib.request

DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "-c", "postgres", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]
API = "https://hub.meshsat.net"


def sql(q):
    r = subprocess.run(DB + [q], capture_output=True, text=True)
    err = [l for l in r.stderr.splitlines() if "ERROR" in l]
    if err:
        print("   SQL ERR:", err[-1][:170])
    return r.stdout.strip()


def call(method, url, key=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json"}
    if key:
        h["Authorization"] = "Bearer " + key
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        r = urllib.request.urlopen(req, timeout=45)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def main():
    bid = sys.argv[1]
    host = sys.argv[2] if len(sys.argv) > 2 else bid
    tenant = sql(f"select tenant_id from bridges where bridge_id='{bid}';")
    if not tenant:
        sys.exit(f"bridge {bid} not found on the Hub")
    print(f"bridge {bid} belongs to tenant {tenant}")
    plain = "meshsat_" + secrets.token_hex(32)
    h = hashlib.sha256(plain.encode()).hexdigest()
    kid = secrets.token_hex(8)
    sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
        f"VALUES ('{kid}','{h}','{plain[:16]}','owner','relay cert re-issue (temporary)','','0001-01-01 00:00:00+00','{tenant}',now());")
    try:
        s, b = call("POST", f"{API}/api/bridges/{bid}/certificate", plain)
        cert = json.loads(b) if s in (200, 201) else {}
        if not cert.get("cert_pem") or not cert.get("key_pem") or not cert.get("ca_pem"):
            sys.exit(f"certificate issue failed: {s} {b[:160]}")
        print(f"Hub issued a new certificate (expires {cert.get('expires_at', cert.get('expiry', '?'))})")
        # tls_ca_pem stays EMPTY: it is the root store for the MQTT broker,
        # which carries a Let's Encrypt certificate; giving it the bridge CA
        # breaks the MQTT session (Hub CLAUDE.md). The relay client fetches
        # the bridge CA from the Hub itself.
        s, b = call("PUT", f"http://{host}:6050/api/routing/hub", None, {
            "tls_cert_pem": cert["cert_pem"], "tls_key_pem": cert["key_pem"], "tls_ca_pem": "",
            "tls_insecure": False,
        })
        if s != 200:
            sys.exit(f"kit refused the new certificate: {s} {b[:160]}")
        resp = json.loads(b)
        print("kit saved it:", {k: resp.get(k) for k in ("bridge_id", "has_cert", "has_password", "warning")})
    finally:
        sql(f"DELETE FROM api_keys WHERE id='{kid}';")
        print("temporary API key deleted:", sql(f"select count(*) from api_keys where id='{kid}';") == "0")


if __name__ == "__main__":
    main()
