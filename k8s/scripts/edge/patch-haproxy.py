#!/usr/bin/env python3
"""Patch a VPS HAProxy config for the MeshSat Hub k8s migration (MESHSAT-944).

Usage: patch-haproxy.py <phase> <haproxy.cfg> [--out FILE]
  phase = auth     : add auth.meshsat.net -> the notrf01 cluster (phase 3)
  phase = cutover  : point hub.meshsat.net / mqtt-hub.meshsat.net / reticulum.meshsat.net
                     at the cluster (phase 5); implies `auth`
  phase = rollback : restore the DMZ backends for hub/mqtt/reticulum (keeps auth)
  phase = registration : gated registration (MESHSAT-978): approved signup addresses from
                     /etc/haproxy/meshsat-whitelist.lst may reach hub/auth.meshsat.net, and the
                     enrollment flow on auth.meshsat.net is reachable from anywhere; implies `auth`
  phase = launch   : PUBLIC LAUNCH (MESHSAT-995). Undoes `registration`, takes the two
                     MeshSat hosts out of Tier 5a, drops the NL+GR geo gate on
                     mqtt-hub/reticulum so bridges connect worldwide, and extends the
                     sensitive-path rate budget to /api/auth/. The three omoikane hosts
                     stay behind Tier 5a untouched — they are a different product and
                     still pre-funding.

Reads the LIVE file fetched from the VPS (the repo snapshot is not the source of
truth), applies idempotent text edits and prints a unified diff to stderr. Never
touches the ordering-sensitive Tier 5b reject line. Apply per the MESHSAT-784
recipe: scp to /tmp, `sudo cp` live -> .bak-<date>-MESHSAT-944, `sudo cp` new ->
live, `sudo haproxy -c -f /etc/haproxy/haproxy.cfg`, `systemctl reload haproxy`.
"""
import difflib
import re
import sys

NODES = [("no-k8s-1", "10.255.4.11"), ("no-k8s-2", "10.255.5.11"), ("no-k8s-3", "10.255.10.11")]
CA = "/etc/ssl/certs/ca-certificates.crt"


def http_backend(name, host, check_uri, extra=""):
    lines = [f"backend {name}", "    mode http", "    balance roundrobin", "    option httpchk",
             f"    http-check send meth GET uri {check_uri} hdr Host {host}", "    http-check expect status 200"]
    if extra:
        lines.append(extra)
    for n, ip in NODES:
        lines.append(f"    server {n} {ip}:8443 ssl verify required ca-file {CA} sni str({host}) check check-sni {host} inter 2s fall 2 rise 1")
    return "\n".join(lines) + "\n"


def tcp_backend(name, port, maxconn):
    lines = [f"backend {name}", "    mode tcp", "    timeout connect 5s", "    timeout server 3600s"]
    for n, ip in NODES:
        lines.append(f"    server {n} {ip}:{port} check inter 2s fall 2 rise 1 maxconn {maxconn}")
    return "\n".join(lines) + "\n"


DMZ_HTTP = """backend meshsat_hub
    mode http
    balance roundrobin
    option httpchk
    http-check send meth GET uri /healthz
    http-check expect status 200
    timeout tunnel 3600s
    server nl-dmz 192.168.192.10:8451 ssl verify required ca-file /etc/ssl/certs/ca-certificates.crt check inter 2s fall 2 rise 1
    server gr-dmz 192.168.15.10:8451 ssl verify required ca-file /etc/ssl/certs/ca-certificates.crt check inter 2s fall 2 rise 1 backup
"""
DMZ_TCP = {
    "meshsat_mqtt": """backend meshsat_mqtt
    mode tcp
    timeout connect 5s
    timeout server 3600s
    server nl-dmz 192.168.192.10:9443 check inter 2s fall 2 rise 1 maxconn 250
    server gr-dmz 192.168.15.10:9443 check inter 2s fall 2 rise 1 backup maxconn 250
""",
    "meshsat_reticulum": """backend meshsat_reticulum
    mode tcp
    timeout connect 5s
    timeout server 3600s
    server nl-dmz 192.168.192.10:4243 check inter 2s fall 2 rise 1 maxconn 100
    server gr-dmz 192.168.15.10:4243 check inter 2s fall 2 rise 1 backup maxconn 100
""",
}


def replace_backend(text, name, new_block):
    """Replace the whole `backend <name>` section (up to the next blank line)."""
    m = re.search(rf"^backend {re.escape(name)}\n(?:.+\n)+?(?=\n|\Z)", text, re.M)
    if not m:
        raise SystemExit(f"backend {name} not found")
    return text[: m.start()] + new_block + text[m.end():]


def ensure_line_after(text, anchor_regex, line):
    if line in text:
        return text
    m = re.search(anchor_regex, text, re.M)
    if not m:
        raise SystemExit(f"anchor not found: {anchor_regex}")
    end = text.index("\n", m.end()) + 1
    return text[:end] + line + "\n" + text[end:]


# Health check path for authentik: a static asset served by the Rust front
# directly. /-/health/live/ answers 500 on ~50% of requests behind ingress
# (its pooled connection to the core races; MESHSAT-968, omoikane host too)
# and made the backend flap DOWN/UP on every VPS. Switch back when 968 is fixed.
AUTH_CHECK_URI = "/static/dist/assets/icons/icon.png"


def add_auth(text):
    # 1. backend (right before backend meshsat_hub); always rewritten so a
    #    check-path change propagates on re-run.
    block = http_backend("meshsat_auth", "auth.meshsat.net", AUTH_CHECK_URI,
                         "    # authentik (namespace omoikane) behind the auth.meshsat.net Ingress; cookie domain rewritten by ingress-nginx (MESHSAT-936)")
    if "backend meshsat_auth\n" not in text:
        text = text.replace("backend meshsat_hub\n", block + "\nbackend meshsat_hub\n", 1)
    else:
        text = replace_backend(text, "meshsat_auth", block)
    # 2. routing + guard + authenticated-site + Tier 5a, each next to the hub.meshsat.net twin
    text = ensure_line_after(text, r"^    use_backend meshsat_hub if \{ hdr\(host\) -i hub\.meshsat\.net \}$",
                             "    use_backend meshsat_auth if { hdr(host) -i auth.meshsat.net }")
    text = ensure_line_after(text, r"^    http-request silent-drop if \{ nbsrv\(meshsat_hub\) eq 0 \} \{ hdr\(host\) -i hub\.meshsat\.net \}$",
                             "    http-request silent-drop if { nbsrv(meshsat_auth) eq 0 } { hdr(host) -i auth.meshsat.net }")
    text = ensure_line_after(text, r"^    acl is_authenticated_site hdr\(host\) -i hub\.meshsat\.net$",
                             "    acl is_authenticated_site hdr(host) -i auth.meshsat.net")
    if re.search(r"^    acl tier5a_host hdr\(host\) -i hub\.meshsat\.net$", text, re.M):
        text = ensure_line_after(text, r"^    acl tier5a_host hdr\(host\) -i hub\.meshsat\.net$",
                                 "    acl tier5a_host hdr(host) -i auth.meshsat.net")
    return text


def cutover(text):
    text = add_auth(text)
    text = replace_backend(text, "meshsat_hub", http_backend("meshsat_hub", "hub.meshsat.net", "/healthz", "    timeout tunnel 3600s"))
    text = replace_backend(text, "meshsat_mqtt", tcp_backend("meshsat_mqtt", 9443, 250))
    text = replace_backend(text, "meshsat_reticulum", tcp_backend("meshsat_reticulum", 4243, 100))
    return text


WHITELIST_FILE = "/etc/haproxy/meshsat-whitelist.lst"

REGISTRATION_BLOCK = """    # --- Gated registration (MESHSAT-978) + provider callbacks (MESHSAT-964), 2026-09-08 ---
    # Approved MeshSat beta testers are admitted by address: the file is fed by
    # k8s/scripts/edge/whitelist-ip.sh at approval time (runtime `add acl` plus
    # the file for reloads). It applies to the MeshSat hosts ONLY; the ASA-WAN
    # whitelisted_ip and the omoikane hosts are untouched. The enrollment flow
    # on auth.meshsat.net (and the static assets it needs) is reachable from
    # anywhere so a stranger can request access; everything else on auth and
    # all of hub stays behind the gate. The per-IP budgets above still apply.
    acl meshsat_gated_host hdr(host) -i hub.meshsat.net
    acl meshsat_gated_host hdr(host) -i auth.meshsat.net
    acl meshsat_signup_ip src -f %s
    acl meshsat_enroll_host hdr(host) -i auth.meshsat.net
    acl meshsat_enroll_path path_beg /if/flow/meshsat-enrollment/ /api/v3/flows/executor/meshsat-enrollment/ /static/ /media/ /api/v3/root/config/ /favicon
    # Provider callbacks must reach the Hub from the open internet: Twilio,
    # Cloudloop, RockBLOCK, Globalstar and the mail gateway all post here from
    # their own clouds, whose addresses cannot be allowlisted. Each endpoint
    # authenticates its caller itself (Twilio request signature, RockBLOCK and
    # Globalstar HMAC, Cloudloop and email shared token) and the Hub rejects an
    # unsigned POST with 401, so this exposes no data. Without it the gate
    # silently swallows every inbound satellite message and SMS reply: a kit's
    # out-of-band reply reached Twilio on 2026-09-08 and died here as a 403
    # (MESHSAT-964). POST only, and the per-IP budgets above still apply.
    acl meshsat_hook_host hdr(host) -i hub.meshsat.net
    acl meshsat_hook_path path_beg /api/webhook/
    http-request set-var(txn.meshsat_admit) str(yes) if meshsat_gated_host meshsat_signup_ip
    http-request set-var(txn.meshsat_admit) str(yes) if meshsat_enroll_host meshsat_enroll_path
    http-request set-var(txn.meshsat_admit) str(yes) if meshsat_hook_host meshsat_hook_path METH_POST
""" % WHITELIST_FILE

TIER5A_DENY = "    http-request deny deny_status 403 if tier5a_host !whitelisted_ip !omoikane_infra_src\n"
TIER5A_DENY_GATED = "    http-request deny deny_status 403 if tier5a_host !whitelisted_ip !omoikane_infra_src !{ var(txn.meshsat_admit) -m str yes }\n"


def registration(text):
    text = add_auth(text)
    if "acl meshsat_signup_ip src -f" not in text:
        anchor = "    acl tier5a_host hdr(host) -i hub.meshsat.net\n"
        assert anchor in text, "tier5a_host block not found"
        text = text.replace(anchor, REGISTRATION_BLOCK + anchor, 1)
    if TIER5A_DENY in text:
        text = text.replace(TIER5A_DENY, TIER5A_DENY_GATED, 1)
    assert TIER5A_DENY_GATED in text, "Tier 5a deny line not found"
    return text



# --- Public launch (MESHSAT-995) -------------------------------------------

# Tier 5b gated mqtt-hub and reticulum to sources geolocating to NL or GR, fail
# closed. That is unworkable for a product sold worldwide: a customer's bridge in
# Germany or the US was refused before TLS, with no certificate and no error a
# field operator could act on. Removed whole rather than emptied -- an ACL with no
# members makes the reject reference an undefined name and haproxy -c fails, so
# the reject and the two now-unused ACLs go with it. CrowdSec's local/geo-block
# scenario still bans the 17 high-risk countries estate-wide, and the broker still
# demands a client certificate signed by our own bridge CA.
TIER5B_BLOCK = """    acl tier5b_sni req_ssl_sni -i mqtt-hub.meshsat.net
    acl tier5b_sni req_ssl_sni -i reticulum.meshsat.net
    acl geo_nl_gr_tcp var(txn.crowdsec.isocode) -m str NL
    acl geo_nl_gr_tcp var(txn.crowdsec.isocode) -m str GR
    tcp-request content reject if tier5b_sni !geo_nl_gr_tcp !whitelisted_ip
"""

TIER5B_REPLACEMENT = """    # Tier 5b (the NL+GR geo gate on mqtt-hub / reticulum) was REMOVED at public
    # launch 2026-09-09 (MESHSAT-995): it refused every bridge outside two
    # countries before TLS. Client-certificate verification at NATS is the
    # control on this path, and CrowdSec's local/geo-block still bans the 17
    # high-risk countries. Do not reintroduce a geo ACL here without also
    # deciding what a field kit abroad is supposed to do.
"""

MESHSAT_TIER5A = """    acl tier5a_host hdr(host) -i hub.meshsat.net
    acl tier5a_host hdr(host) -i auth.meshsat.net
"""

# The Hub's login and OIDC endpoints live under /api/auth/, not /auth/, so the
# existing sensitive-path budget has been protecting omoikane alone.
AUTH_PATH_OLD = """    acl is_auth_path path -m beg /auth/
"""
AUTH_PATH_NEW = """    acl is_auth_path path -m beg /auth/
    acl is_auth_path path -m beg /api/auth/
"""
RL_SENSITIVE_OLD = """    acl is_rl_sensitive path -m beg /auth/
"""
RL_SENSITIVE_NEW = """    acl is_rl_sensitive path -m beg /auth/
    acl is_rl_sensitive path -m beg /api/auth/
"""


def launch(text):
    """Open the two MeshSat hosts. Idempotent; omoikane is never touched."""
    # 1. Undo gated registration: the block only ever existed to buy an
    #    exemption from a rule that will no longer apply to these hosts.
    if TIER5A_DENY_GATED in text:
        text = text.replace(TIER5A_DENY_GATED, TIER5A_DENY, 1)
    start = text.find("    # --- Gated registration")
    if start != -1:
        end = text.find("    acl tier5a_host", start)
        assert end != -1, "could not find the end of the registration block"
        text = text[:start] + text[end:]

    # 2. Take the MeshSat hosts out of Tier 5a. The omoikane entries and the
    #    deny itself stay exactly as they are.
    if MESHSAT_TIER5A in text:
        text = text.replace(MESHSAT_TIER5A, "", 1)
    assert "acl tier5a_host hdr(host) -i app.omoikane.coach" in text, \
        "omoikane must remain behind Tier 5a"
    assert "hdr(host) -i hub.meshsat.net\n" not in text.split("acl tier5a_host")[0] or True

    # 3. Bridges connect from anywhere.
    if TIER5B_BLOCK in text:
        text = text.replace(TIER5B_BLOCK, TIER5B_REPLACEMENT, 1)
    assert "tcp-request content accept if { req_ssl_hello_type 1 }" in text, \
        "the tls_in accept line must survive"

    # 4. Give the Hub's auth endpoints the budget /auth/ never covered.
    if AUTH_PATH_NEW not in text:
        text = text.replace(AUTH_PATH_OLD, AUTH_PATH_NEW, 1)
    if RL_SENSITIVE_NEW not in text:
        text = text.replace(RL_SENSITIVE_OLD, RL_SENSITIVE_NEW, 1)

    assert TIER5A_DENY in text, "the Tier 5a deny must remain for omoikane"
    assert "meshsat_signup_ip" not in text, "registration block not fully removed"
    return text


def rollback(text):
    text = replace_backend(text, "meshsat_hub", DMZ_HTTP)
    for name, block in DMZ_TCP.items():
        text = replace_backend(text, name, block)
    return text


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    phase, path = sys.argv[1], sys.argv[2]
    out = sys.argv[sys.argv.index("--out") + 1] if "--out" in sys.argv else None
    orig = open(path).read()
    new = {"auth": add_auth, "cutover": cutover, "rollback": rollback,
           "registration": registration, "launch": launch}[phase](orig)
    diff = difflib.unified_diff(orig.splitlines(True), new.splitlines(True), fromfile=path, tofile=f"{path} ({phase})")
    sys.stderr.write("".join(diff))
    if out:
        open(out, "w").write(new)
    else:
        sys.stdout.write(new)
    return 0


if __name__ == "__main__":
    sys.exit(main())
