package takoperator

// configScript runs in the init container before OpenTAKServer starts.
//
// It does three things and nothing else: seeds the CA material, writes
// config.yml if it is absent, and leaves everything alone on a restart. It is
// inline rather than a per-instance ConfigMap so that one tenant is one
// Deployment plus three Secrets, with no extra object to drift.
//
// Two facts from spike S5 shape it:
//
//   - OpenTAKServer skips create_ca when ca.pem already exists, which is the
//     whole reason a tenant's instance can run with NO CA KEY on disk. The
//     operator holds the key; the pod gets the certificate and a server
//     certificate already signed.
//   - The pinned values must never be regenerated. A new SECURITY_PASSWORD_SALT
//     invalidates every password hash OpenTAKServer has stored, so a "refresh"
//     would lock a tenant's TAK users out of their own server.
//
// ⚠ UNCONFIRMED, and the phase-2 gate is what settles it: the exact filenames
// OpenTAKServer expects for a server certificate it did not generate itself.
// Spike S5 seeded `srv_*` files into ca/certs/opentakserver/ and the instance
// served TLS, but that path was derived from reading the tree rather than from
// the upstream source. If `eud_handler --ssl` comes up without a certificate,
// this is the first place to look — not the PKI code, which is tested.
const configScript = `
import os, shutil, sys

try:
    import yaml
except ImportError:
    sys.exit("the OTS image no longer ships PyYAML; this script needs it")

DATA = os.environ.get("OTS_DATA_FOLDER", "/var/lib/ots")
CA = os.path.join(DATA, "ca")
CERTS = os.path.join(CA, "certs", "opentakserver")

os.makedirs(CERTS, exist_ok=True)

# --- the CA certificate, without its key ---
seed_ca = "/seed/ca.pem"
if os.path.exists(seed_ca) and not os.path.exists(os.path.join(CA, "ca.pem")):
    shutil.copyfile(seed_ca, os.path.join(CA, "ca.pem"))
    # ca-trusted.pem is what OpenTAKServer hands clients as a truststore; with a
    # single-CA chain it is the same bytes.
    shutil.copyfile(seed_ca, os.path.join(CA, "ca-trusted.pem"))
    print("seeded the tenant CA certificate; no CA key is present, by design")

# --- the server certificate this instance presents ---
for src, dst in (("/seed-tls/tls.crt", "opentakserver.pem"),
                 ("/seed-tls/tls.key", "opentakserver.key")):
    if os.path.exists(src):
        target = os.path.join(CERTS, dst)
        if not os.path.exists(target):
            shutil.copyfile(src, target)
            print("seeded", dst)

# --- config.yml, written once ---
path = os.path.join(DATA, "config.yml")
if os.path.exists(path):
    print("config.yml exists, leaving it alone")
    sys.exit(0)

user = os.environ["TAK_DB_USER"]
password = os.environ["TAK_DB_PASSWORD"]
host = os.environ["TAK_DB_HOST"]
name = os.environ["TAK_DB_NAME"]

cfg = {
    # psycopg3 rather than the default driver, matching what spike S3 migrated
    # cleanly as a plain database owner.
    "SQLALCHEMY_DATABASE_URI": f"postgresql+psycopg://{user}:{password}@{host}:5432/{name}",
    "OTS_DATA_FOLDER": DATA,
    # The API and RabbitMQ are loopback only: the Hub reaches the API through the
    # nginx in this pod, which admits one client certificate and two paths.
    "OTS_LISTENER_ADDRESS": "127.0.0.1",
    "OTS_RABBITMQ_SERVER_ADDRESS": "127.0.0.1",
    # Everything we do not run. Email off also means no self-registration.
    "OTS_ENABLE_EMAIL": False,
    "OTS_ENABLE_PLUGINS": False,
    "OTS_MEDIAMTX_ENABLE": False,
    "OTS_ENABLE_MESHTASTIC": False,
    # Pinned once by the operator, never regenerated here.
    "SECRET_KEY": os.environ["OTS_PIN_SECRET_KEY"],
    "SECURITY_PASSWORD_SALT": os.environ["OTS_PIN_PASSWORD_SALT"],
    "OTS_NODE_ID": os.environ["OTS_PIN_NODE_ID"],
    "OTS_CA_PASSWORD": os.environ["OTS_PIN_CA_PASSWORD"],
}

with open(path, "w") as f:
    yaml.safe_dump(cfg, f)
print("wrote config.yml with keys:", ", ".join(sorted(cfg)))

# The default administrator OpenTAKServer creates on first start is
# administrator/password. The operator generates a random one and the first
# start uses it, so the documented default is never valid on a hosted instance.
# Written where the entrypoint looks for it rather than passed on a command line,
# which would put it in the pod spec and in every "kubectl describe".
admin = os.environ.get("OTS_ADMIN_PASSWORD")
if admin:
    with open(os.path.join(DATA, ".admin_password"), "w") as f:
        f.write(admin)
    os.chmod(os.path.join(DATA, ".admin_password"), 0o600)
    print("staged the administrator password")
`
