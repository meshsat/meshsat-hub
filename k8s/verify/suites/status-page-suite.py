#!/usr/bin/env python3
"""status.meshsat.net acceptance suite (MESHSAT-1134).

Runs from the Claude runner (egress 45.138.52.48, which is on the edge's
whitelisted_ip), so the admin-gate POSITIVE checks are meaningful here and the
NEGATIVE ones are taken from a VPS (not allow-listed) over ssh. Reads only;
nothing is written to the page.

  python3 scratchpad/status-page-suite.py
"""
import json
import os
import re
import subprocess
import sys
import urllib.parse

HOST = "status.meshsat.net"
EDGES = {"NO": "185.125.171.172", "CH": "185.44.82.32", "TX": "185.121.169.27"}
COMPONENTS = ["hub-web", "hub-signin", "bridge-mqtt", "reticulum", "payments", "website", "docs",
              "installer", "edge-no", "edge-ch", "edge-tx", "monitoring"]
results = []


def check(name, ok, detail=""):
    results.append((name, ok))
    print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"  [{detail}]" if detail else ""))


def get(ip, path):
    """GET https://status.meshsat.net<path> through one specific edge (curl --resolve:
    SNI and Host stay the vhost, the socket goes to the chosen IP)."""
    r = subprocess.run(["curl", "-s", "-m", "15", "--resolve", f"{HOST}:443:{ip}", "-o", "/dev/stdout",
                        "-w", "\n__META__ %{http_code} %{content_type}", f"https://{HOST}{path}"],
                       capture_output=True, timeout=30)
    body, _, meta = r.stdout.rpartition(b"\n__META__ ")
    code, _, ctype = meta.decode(errors="replace").strip().partition(" ")
    return int(code or 0), {"Content-Type": ctype}, body


print("=== 1. every edge serves the page on the wildcard ===")
for name, ip in EDGES.items():
    try:
        st, hd, body = get(ip, "/")
        check(f"{name} edge: GET / is 200 with the page title", st == 200 and b"MeshSat" in body, f"HTTP {st}, {len(body)}B")
    except Exception as e:  # noqa: BLE001
        check(f"{name} edge: GET /", False, repr(e)[:80])

print("=== 2. public surfaces ===")
ip = EDGES["CH"]
st, _, body = get(ip, "/healthz")
try:
    hz = json.loads(body)
except json.JSONDecodeError:
    hz = {}
check("/healthz is Kener's own healthcheck (db+redis)", st == 200 and hz.get("db") is True and hz.get("redis") is True, body[:80].decode(errors="replace"))
st, hd, body = get(ip, "/rss.xml")
check("/rss.xml is an RSS feed", st == 200 and b"<rss" in body and "rss" in hd.get("Content-Type", ""), hd.get("Content-Type", ""))
st, hd, body = get(ip, "/badge/_/status")
overall = re.search(rb"<title>([^<]*)</title>", body)
check("overall badge renders", st == 200 and b"svg" in body, overall.group(1).decode() if overall else "")
for tag in COMPONENTS:
    st, _, body = get(ip, f"/badge/{tag}/status")
    m = re.search(rb"<title>([^<]*)</title>", body)
    title = m.group(1).decode() if m else ""
    check(f"component {tag}: badge present and Operational", st == 200 and title.endswith(": Operational"), title)

IN_CLUSTER = os.environ.get("VERIFY_IN_CLUSTER") == "1"
if IN_CLUSTER:
    # The cluster's egress address is NOT in whitelisted_ip, so from here the
    # admin surface must give NO response at all: this vantage is the stranger,
    # and the ssh hop to a VPS below is neither available nor needed.
    print("=== 3. admin surface: silent-dropped from the cluster, which is not allow-listed ===")
    for path in ("/manage/app/monitors", "/account/signin", "/api/v4/monitors"):
        st, _, _ = get(ip, path)
        check(f"{path} gets no response from a stranger (this cluster)", st == 0, f"HTTP {st}")
    st, _, _ = get(ip, "/")
    check("/ still answers the stranger", st == 200, f"HTTP {st}")
    print("=== 4. (skipped in-cluster: the VPS vantage needs ssh from the runner) ===")
    codes = {}
else:
    print("=== 3. admin surface: reachable from the allow-listed runner ===")
    for path, want in (("/manage/app/monitors", (200, 302)), ("/account/signin", (200,)), ("/api/v4/monitors", (401,))):
        st, _, _ = get(ip, path)
        check(f"{path} answers {want} from whitelisted_ip", st in want, f"HTTP {st}")

print("=== 4. admin surface: silent-dropped from a non-allow-listed vantage (NO VPS via CH edge) ===") if not IN_CLUSTER else None
cmd = ["ssh", "-i", "/home/claude-runner/.ssh/one_key", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "kyriakosp@185.125.171.172",
       "for p in /manage/app/monitors /account/signin /api/v4/monitors; do curl -s -m 8 --resolve status.meshsat.net:443:185.44.82.32 -o /dev/null -w \"$p %{http_code}\\n\" https://status.meshsat.net$p; done; curl -s -m 8 --resolve status.meshsat.net:443:185.44.82.32 -o /dev/null -w \"/ %{http_code}\\n\" https://status.meshsat.net/"]
if not IN_CLUSTER:
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=90).stdout
    codes = dict(l.split() for l in out.splitlines() if " " in l)
    for p in ("/manage/app/monitors", "/account/signin", "/api/v4/monitors"):
        check(f"{p} gets no response from a stranger", codes.get(p) == "000", codes.get(p, "no output"))
    check("/ still answers the stranger", codes.get("/") == "200", codes.get("/", "no output"))

print("=== 5. the page is drawn from Prometheus, and Prometheus agrees ===")
q = "min(probe_success{job=\"meshsat-edge\"})"
prom = subprocess.run(["kubectl", "--context", "kubernetes-admin@kubernetes", "-n", "monitoring", "exec",
                       "prometheus-monitoring-kube-prometheus-prometheus-0", "-c", "prometheus", "--", "wget", "-qO-",
                       "http://127.0.0.1:9090/api/v1/query?query=" + urllib.parse.quote(q)], capture_output=True, text=True, timeout=60)
if prom.returncode == 3 and os.environ.get("VERIFY_IN_CLUSTER") == "1":
    # The NL cluster is not reachable from inside notrf01 (the kubectl shim
    # refuses a foreign context with exit 3); the Prometheus agreement check
    # belongs to the runner-side run of this suite.
    print("  SKIP  Prometheus agreement (NL cluster not reachable from inside notrf01)")
else:
    try:
        val = json.loads(prom.stdout)["data"]["result"][0]["value"][1]
    except (ValueError, KeyError, IndexError):
        val = "?"
    check("all per-edge probes are 1 in Prometheus (so 'Operational' above is honest)", val == "1", f"min probe_success = {val}")

ok = sum(1 for _, r in results if r)
print(f"\n{ok}/{len(results)} PASS")
sys.exit(0 if ok == len(results) else 1)
