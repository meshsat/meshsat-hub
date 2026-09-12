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
// CONFIRMED from the upstream source, after the phase-2 gate caught it.
// `EudServerSSL.server_bind` (opentakserver/eud_handler/EudServerSSL.py) loads
// exactly three paths under OTS_CA_FOLDER:
//
//	certs/opentakserver/opentakserver.pem          the server certificate
//	certs/opentakserver/opentakserver.nopass.key   its key, WITHOUT a passphrase
//	ca.pem                                          the client trust anchor
//
// The third name is the one that bit: the first attempt seeded
// `opentakserver.key` only, so `eud_handler --ssl` died in server_bind with a
// bare FileNotFoundError and the CoT port accepted nothing. Upstream normally
// writes both a passphrase-protected `.key` and a `.nopass.key`; the key the
// operator issues has no passphrase at all, so the same bytes serve as both.
//
// Worth knowing for any future seeding: the traceback names no filename, because
// the path is built inside the load_cert_chain call. Read EudServerSSL.py rather
// than guessing from the data folder, which is how this was got wrong once.
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
#
# opentakserver.nopass.key is the name eud_handler --ssl actually opens; see the
# note above. The operator's key carries no passphrase, so the same bytes are
# written under both names: .key for the code paths that expect upstream's
# layout, .nopass.key for the one that binds the CoT port.
for src, dst in (("/seed-tls/tls.crt", "opentakserver.pem"),
                 ("/seed-tls/tls.key", "opentakserver.key"),
                 ("/seed-tls/tls.key", "opentakserver.nopass.key")):
    if os.path.exists(src):
        target = os.path.join(CERTS, dst)
        if not os.path.exists(target):
            shutil.copyfile(src, target)
            os.chmod(target, 0o600 if dst.endswith("key") else 0o644)
            print("seeded", dst)

# Fail loudly rather than leave eud_handler to die on a bare FileNotFoundError:
# if any of the three files the SSL listener needs is missing, say which.
missing = [f for f in ("opentakserver.pem", "opentakserver.nopass.key")
           if not os.path.exists(os.path.join(CERTS, f))]
if not os.path.exists(os.path.join(CA, "ca.pem")):
    missing.append("ca.pem")
if missing:
    sys.exit("eud_handler --ssl will not start: missing " + ", ".join(missing))

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

# NOTHING is staged here for the administrator account, deliberately.
#
# An earlier version of this script wrote the generated password to
# ".admin_password" in the data folder, believing the entrypoint read it. It does
# not: nothing in OpenTAKServer 1.7.13 opens that path. The file was inert, and a
# merged commit message claimed on the strength of it that the documented default
# administrator/password "never works" on a hosted instance. Probed against the
# live gate instance: it worked, HTTP 200 with a session token.
#
# app.py creates "administrator" with the literal password "password" on first
# start (app.py:471 logs it in as many words), and the only way to change it is
# POST /api/user/password/reset, which needs the API listening and the schema
# migrated. Neither is true while this init container runs, so the change belongs
# to the operator once the Deployment is Ready: see bootstrapAdmin in
# otsadmin.go. OTS_ADMIN_PASSWORD stays in the config Secret, which is where the
# operator reads it from.
`
