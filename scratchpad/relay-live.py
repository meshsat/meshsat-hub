"""MESHSAT-612 acceptance on production: the WebSocket relay, both ends real.

A throwaway tenant with two throwaway bridges (a kit and a phone), credentials
generated through the real API, one socket each to hub.meshsat.net through the
real edge, a frame each way with the envelope checked, the refusals for a wrong
password and for another tenant's bridge, and the whole thing run TWICE so the
two ends land on different replicas at least once (read from both pods' logs).
Everything is deleted at the end; nothing here reaches a device or a person.

Run from the runner:  python3 scratchpad/relay-live.py
"""
import asyncio, base64, hashlib, json, secrets, subprocess, sys, time, urllib.error, urllib.request

import websockets
from websockets.exceptions import InvalidStatus

DB = ["kubectl", "--context", "notrf01", "-n", "meshsat-hub-db", "exec", "meshsat-hub-main-1", "--",
      "psql", "-U", "postgres", "-d", "meshsat_hub", "-t", "-A", "-F|", "-c"]
API = "https://hub.meshsat.net"
WS = "wss://hub.meshsat.net"
T = "t-relay-probe"
KIT, PHONE = "relay-probe-kit", "relay-probe-phone"

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


def basic(user, pw):
    return {"Authorization": "Basic " + base64.b64encode(f"{user}:{pw}".encode()).decode()}


def envelope(client_id, payload):
    cid = client_id.encode()
    return b"\x01" + bytes([len(cid)]) + cid + payload


def unenvelope(frame):
    assert frame[0] == 1, "envelope version"
    n = frame[1]
    return frame[2:2 + n].decode(), frame[2 + n:]


def cleanup():
    for stmt in (f"DELETE FROM bridges WHERE tenant_id='{T}';",
                 f"DELETE FROM api_keys WHERE tenant_id='{T}';",
                 f"DELETE FROM audit_log WHERE tenant_id='{T}';",
                 f"DELETE FROM tenants WHERE id='{T}';"):
        sql(stmt)


async def round_trip(n, kit_pw, phone_pw):
    print(f"=== round trip {n} ===")
    async with websockets.connect(WS + "/api/relay/serve", additional_headers=basic(KIT, kit_pw),
                                  open_timeout=20) as bridge:
        async with websockets.connect(WS + f"/api/relay/connect/{KIT}", additional_headers=basic(PHONE, phone_pw),
                                      open_timeout=20) as phone:
            await phone.send(b"hello from the phone %d" % n)
            frame = await asyncio.wait_for(bridge.recv(), 15)
            cid, payload = unenvelope(frame)
            check(f"[{n}] client -> bridge arrives in an envelope naming the client",
                  cid == PHONE and payload == b"hello from the phone %d" % n, f"{cid} {payload!r}")
            await bridge.send(envelope(PHONE, b"hello from the kit %d" % n))
            got = await asyncio.wait_for(phone.recv(), 15)
            check(f"[{n}] bridge -> client arrives as the bare payload", got == b"hello from the kit %d" % n, repr(got))
            t0 = time.time()
            await phone.send(b"\x00" * 1024)
            frame = await asyncio.wait_for(bridge.recv(), 15)
            check(f"[{n}] a 1 KiB frame crosses intact", unenvelope(frame)[1] == b"\x00" * 1024,
                  f"{(time.time() - t0) * 1000:.0f} ms")


async def refusals(kit_pw, phone_pw):
    print("=== refusals ===")
    for name, path, hdr, want in (
        ("a wrong password is 401", "/api/relay/serve", basic(KIT, "not-the-password"), 401),
        ("no credentials is 401", f"/api/relay/connect/{KIT}", {}, 401),
        ("another tenant's bridge is 403 (the platform owns 'sim-sos-001' or nothing does)", "/api/relay/connect/sim-sos-001", basic(PHONE, phone_pw), 403),
        ("a bridge naming itself is 400", f"/api/relay/connect/{PHONE}", basic(PHONE, phone_pw), 400),
    ):
        try:
            async with websockets.connect(WS + path, additional_headers=hdr, open_timeout=20):
                check(name, False, "upgraded")
        except InvalidStatus as e:
            check(name, e.response.status_code == want, f"{e.response.status_code}")
        except Exception as e:  # noqa: BLE001
            check(name, False, repr(e)[:120])


def main():
    print("=== setup: throwaway tenant, two bridges, real credentials ===")
    cleanup()
    sql(f"INSERT INTO tenants (id,slug,name,plan,status,created_at,updated_at) "
        f"VALUES ('{T}','relay-probe','Relay probe','beta','active',now(),now());")
    plain = "meshsat_" + secrets.token_hex(32)
    h = hashlib.sha256(plain.encode()).hexdigest()
    sql(f"INSERT INTO api_keys (id,key_hash,key_prefix,role,label,device_imei,expires_at,tenant_id,created_at) "
        f"VALUES ('{secrets.token_hex(8)}','{h}','{plain[:16]}','owner','relay probe','','0001-01-01 00:00:00+00','{T}',now());")
    pws = {}
    for bid in (KIT, PHONE):
        s, b = call("POST", "/api/bridges", plain, {"bridge_id": bid, "label": "relay probe " + bid})
        check(f"bridge {bid} created", s in (200, 201), f"{s} {b[:80]}")
        s, b = call("POST", f"/api/bridges/{bid}/credentials", plain)
        pws[bid] = json.loads(b).get("password", "") if s == 200 else ""
        check(f"credentials generated for {bid}", bool(pws[bid]), f"{s}")
    if not all(pws.values()):
        cleanup()
        sys.exit(1)
    try:
        asyncio.run(refusals(pws[KIT], pws[PHONE]))
        asyncio.run(round_trip(1, pws[KIT], pws[PHONE]))
        time.sleep(2)
        asyncio.run(round_trip(2, pws[KIT], pws[PHONE]))
        time.sleep(3)
        logs = subprocess.run(["kubectl", "--context", "notrf01", "-n", "meshsat-hub", "logs", "-l", "app.kubernetes.io/name=hub",
                               "--prefix", "--since=5m", "--tail=2000"], capture_output=True, text=True).stdout
        serving = sorted({l.split("]")[0].split("/")[-1] for l in logs.splitlines() if "relay: bridge serving" in l and KIT in l})
        connected = sorted({l.split("]")[0].split("/")[-1] for l in logs.splitlines() if "relay: client connected" in l and PHONE in l})
        print(f"   bridge sockets landed on {serving}; client sockets on {connected}")
        check("both replicas took part (the rendezvous crossed the bus at least once)",
              len(set(serving) | set(connected)) >= 2 and (serving != connected or len(serving) > 1),
              "same pod every time" if serving == connected and len(serving) == 1 else "")
    finally:
        print("=== teardown ===")
        for bid in (KIT, PHONE):
            s, _ = call("DELETE", f"/api/bridges/{bid}", plain)
            check(f"bridge {bid} deleted through the API", s in (200, 204), str(s))
        cleanup()
    failed = [n for n, ok in results if not ok]
    print(f"\n{len(results)} checks, {len(failed)} failed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
