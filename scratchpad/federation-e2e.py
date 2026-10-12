"""Live e2e of Hub federation, receiving side (MESHSAT-1549).

This script plays a self-hosted Hub: its own Ed25519 identity, peered with the
DEFAULT tenant on hub.meshsat.net, sending signed relays to tesseract's 9704.
It checks every refusal (not a peer, no grant, forged signature, stale date,
wrong identity, ungranted device), one real relay, a replay, and that removing
the peer closes the door again; the grant and the peer are removed at the end.

Cost: ONE real IMT MT to tesseract (Ground Control demo line). The payload is a
Reticulum DATA packet for a random destination, which the kit's node drops, so
nothing reaches the mesh, SMS or the printer.

Run: HUB_KEY=$(bao kv get -field=HUB_VERIFY_ADMIN_KEY secret/ci-no/apps/meshsat-hub/verify) python3 scratchpad/federation-e2e.py
Secrets (the API key, the relay token) are read at runtime and never printed.
First run 12 Oct 2026: 13/13, Cloudloop accepted the MT."""
import base64, datetime, hashlib, json, os, urllib.error, urllib.parse, urllib.request
from cryptography import x509
from cryptography.x509.oid import NameOID
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ed25519

HUB = "https://hub.meshsat.net"
KEY = os.environ["HUB_KEY"]
FROM_IMEI = "300434000000999"   # the probe's pretend modem
results = []

def api(method, path, body=None):
    req = urllib.request.Request(HUB + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:200]

def identity(label):
    k = ed25519.Ed25519PrivateKey.generate()
    now = datetime.datetime.now(datetime.timezone.utc)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "MeshSat Hub federation: " + label)])
    cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name).public_key(k.public_key())
            .serial_number(x509.random_serial_number()).not_valid_before(now - datetime.timedelta(hours=1))
            .not_valid_after(now + datetime.timedelta(days=30)).sign(k, None))
    spki = k.public_key().public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)
    return k, cert.public_bytes(serialization.Encoding.PEM).decode(), base64.b64encode(hashlib.sha256(spki).digest()).decode()

def post_relay(url, key, from_fp, to_fp, frame, date=None):
    body = json.dumps(frame, separators=(",", ":")).encode()
    path = urllib.parse.urlparse(url).path
    date = date or datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    msg = "\n".join(["meshsat-federation-v1", "POST", path, from_fp, to_fp, date, hashlib.sha256(body).hexdigest()]).encode()
    sig = base64.b64encode(key.sign(msg)).decode()
    req = urllib.request.Request(url, method="POST", data=body, headers={"Content-Type": "application/json",
        "X-MeshSat-Federation-From": from_fp, "X-MeshSat-Federation-To": to_fp,
        "X-MeshSat-Federation-Date": date, "X-MeshSat-Federation-Signature": sig})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read().decode()[:120]
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:120]

def check(name, got, want):
    ok = got == want
    results.append((name, ok))
    print(("PASS" if ok else "FAIL"), name, "->", got, "(want", str(want) + ")")

st, ident = api("GET", "/api/federation/identity")
assert st == 200, (st, ident)
hub_fp, relay_url = ident["fingerprint"], ident["relay_url"]
print("hub identity fingerprint", hub_fp, "relay url present:", bool(relay_url))
st, devs = api("GET", "/api/devices")
rows = devs if isinstance(devs, list) else devs.get("devices", [])
kit = next(d["imei"] for d in rows if "Tesseract" in ((d.get("name") or "") + (d.get("label") or "")))
print("target device: tesseract 9704", kit[:4] + "..." + kit[-3:])

key, cert_pem, fp = identity("e2e self-hosted probe")
other_key, _, _ = identity("impostor")
payload = bytes([0x00, 0x00]) + os.urandom(16) + bytes([0x00]) + b"meshsat federation e2e probe"
frame = {"msg_id": "e2e-" + os.urandom(6).hex(), "attempt": 1, "from_imei": FROM_IMEI, "to_imei": kit,
         "imt_topic": "IMT_TOPIC_RAW", "wire_b64": base64.b64encode(payload).decode()}

check("1 not yet a peer", post_relay(relay_url, key, fp, hub_fp, frame)[0], 404)
st, peer = api("POST", "/api/federation/peers", {"name": "e2e self-hosted probe", "relay_url": "https://example.com/api/webhook/federation/probe", "certificate_pem": cert_pem})
check("2 add peer", st, 201)
peer_id = peer["id"] if isinstance(peer, dict) else None
print("   peer fingerprint as the Hub computed it matches ours:", isinstance(peer, dict) and peer.get("fingerprint") == fp)
check("3 peer but no grant", post_relay(relay_url, key, fp, hub_fp, frame)[0], 403)
st, grant = api("POST", "/api/federation/grants", {"peer_id": peer_id, "from_imei": FROM_IMEI, "to_imei": kit})
check("4 add grant", st, 201)
grant_id = grant["id"] if isinstance(grant, dict) else None
check("5 forged signature (impostor key, our fingerprint)", post_relay(relay_url, other_key, fp, hub_fp, frame)[0], 404)
stale = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=10)).strftime("%Y-%m-%dT%H:%M:%SZ")
check("6 stale date", post_relay(relay_url, key, fp, hub_fp, frame, date=stale)[0], 404)
check("7 addressed to another identity", post_relay(relay_url, key, fp, fp, frame)[0], 404)
other = dict(frame, to_imei=next(d["imei"] for d in rows if "Parallax" in ((d.get("name") or "") + (d.get("label") or ""))))
check("8 granted sender, ungranted device", post_relay(relay_url, key, fp, hub_fp, other)[0], 403)
st, body = post_relay(relay_url, key, fp, hub_fp, frame)
print("   relay answer body:", body)
check("9 valid relay -> MT sent", st, 202)
check("10 replay of the same attempt", post_relay(relay_url, key, fp, hub_fp, frame)[0], 200)

if grant_id:
    check("11 remove grant", api("DELETE", "/api/federation/grants/" + grant_id)[0], 204)
if peer_id:
    check("12 remove peer", api("DELETE", "/api/federation/peers/" + peer_id)[0], 204)
check("13 after removal the peer is refused", post_relay(relay_url, key, fp, hub_fp, dict(frame, attempt=2))[0], 404)
print("msg_id", frame["msg_id"])
print("SUMMARY", sum(ok for _, ok in results), "/", len(results))
