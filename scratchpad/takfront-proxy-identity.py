"""MESHSAT-1046 e2e setup, run inside the throwaway OTS pod's ots-api container.

Creates the ONE OpenTAKServer identity the Hub's TAK front uses for a whole
tenant (CN=meshsatproxy), puts it in a group that relays both ways, signs it a
certificate from the tenant OTS's own CA, and prints the CA plus that identity so
the front outside the pod can present it. Every phone's stream for this tenant
goes up as this one identity; the phones keep their own certificates, issued by
a different CA the front verifies them against.

Username and password are lowercase alphanumeric on purpose: OpenTAKServer
answered 400 to "meshsat-proxy", and spike S5 only ever used names of this shape.

    kubectl exec -i deploy/ots-spike -c ots-api -- python - < setup_proxy.py
"""
import datetime
import json
import os
import urllib.error
import urllib.request

import yaml
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID, ExtendedKeyUsageOID

DATA = "/var/lib/ots"
API = "http://127.0.0.1:8081"
USER = "meshsatproxy"
GROUP = "meshsatgrp"
PASSWORD = "Proxy" + os.urandom(8).hex()

cfg = yaml.safe_load(open(os.path.join(DATA, "config.yml")))


def api(method, path, body=None, token=None):
    req = urllib.request.Request(
        API + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json",
                 **({"Authentication-Token": token} if token else {})})
    try:
        r = urllib.request.urlopen(req, timeout=20)
        return r.status, r.read().decode(errors="replace")[:400]
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")[:400]


code, body = api("POST", "/api/login?include_auth_token",
                 {"username": "administrator", "password": "password"})
print("login:", code)
token = json.loads(body)["response"]["user"]["authentication_token"] if code == 200 else None

print("add user:", api("POST", "/api/user/add",
                       {"username": USER, "password": PASSWORD,
                        "confirm_password": PASSWORD, "roles": ["user"]}, token))
print("add group:", api("POST", "/api/groups",
                        {"name": GROUP, "description": "meshsat front"}, token))
for direction in ("IN", "OUT"):
    print(f"group {direction}:", api("PUT", "/api/groups",
                                     {"users": [USER], "group_name": GROUP,
                                      "direction": direction}, token))
print("users now:", api("GET", "/api/users", None, token))

ca_pem = open(os.path.join(DATA, "ca", "ca.pem"), "rb").read()
ca_cert = x509.load_pem_x509_certificate(ca_pem)
ca_key = serialization.load_pem_private_key(
    open(os.path.join(DATA, "ca", "ca-do-not-share.key"), "rb").read(),
    password=cfg["OTS_CA_PASSWORD"].encode())

key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
now = datetime.datetime.now(datetime.timezone.utc)
cert = (x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, USER)]))
        .issuer_name(ca_cert.subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=5))
        .not_valid_after(now + datetime.timedelta(days=2))
        .add_extension(x509.ExtendedKeyUsage([ExtendedKeyUsageOID.CLIENT_AUTH]), critical=False)
        .sign(ca_key, hashes.SHA256()))

print("ca subject:", ca_cert.subject.rfc4514_string())
for name, blob in (("OTS_CA", ca_pem),
                   ("PROXY_CERT", cert.public_bytes(serialization.Encoding.PEM)),
                   ("PROXY_KEY", key.private_bytes(serialization.Encoding.PEM,
                                                   serialization.PrivateFormat.TraditionalOpenSSL,
                                                   serialization.NoEncryption()))):
    print(f"-----MESHSAT {name} BEGIN-----")
    print(blob.decode().strip())
    print(f"-----MESHSAT {name} END-----")
