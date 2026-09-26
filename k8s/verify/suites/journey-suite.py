#!/usr/bin/env python3
"""Phase D1b: the segment nobody has ever walked -- verify, approve, sign in.

The enrollment suite proves an account lands pending. The quota suite proves a
tenant's ceiling. Between them sits the part that has only ever been reasoned
about: clicking the verification link, a platform admin approving, and the
FIRST SIGN-IN creating the tenant. This walks it against production and cleans
up after itself.
"""
import http.cookiejar, json, os, re, subprocess, sys, time, urllib.request, urllib.error, urllib.parse, uuid

AUTH = "https://auth.meshsat.net"
HUB  = "https://hub.meshsat.net"
FLOW = "meshsat-enrollment"
CTX  = ["kubectl","--context","notrf01","-n","meshsat-hub"]
DB   = ["kubectl","--context","notrf01","-n","meshsat-hub-db","exec","meshsat-hub-main-1","--",
        "psql","-U","postgres","-d","meshsat_hub","-t","-A","-F|","-c"]
# Per run, never committed: the probe account is created and deleted inside
# this run, so nothing needs to know the value beforehand. Shape satisfies the
# enrollment password policy (upper, lower, digits, symbol).
PASSWORD = "Probe-" + uuid.uuid4().hex[:16] + "-A1"

def sh(c): return subprocess.run(c, capture_output=True, text=True).stdout.strip()
def sql(q): return sh(DB+[q])
POD = [l.split("/")[1] for l in sh(CTX+["get","pods","-o","name"]).splitlines() if l.startswith("pod/hub-")][0]
def env(v): return sh(CTX+["exec",POD,"--","printenv",v])
AK_TOKEN = env("HUB_AUTHENTIK_TOKEN")

# MESHSAT-1209: this suite's own platform-admin API key, from its Secret, in
# preference to reading the Hub pod's HUB_AUTH_TOKEN over `kubectl exec`.
#
# Two things wrong with the old way. The token is static, never expires and
# cannot be revoked, so the nightly was the reason it had to keep existing at
# all; and fetching it meant `exec` into a Hub pod, which is a capability
# equivalent to reading every Hub secret and was granted to this CronJob for
# exactly one printenv. The key is revocable, carries an expiry, and arrives as
# an ordinary env var.
#
# The fallback stays only while HUB_LEGACY_TOKEN_ENABLED is still true. Once the
# token is retired the fallback resolves to an empty string and the admin checks
# below fail loudly rather than skipping — which is the correct behaviour for a
# suite whose job is to notice things.
HUB_TOKEN = os.environ.get("HUB_VERIFY_ADMIN_KEY") or env("HUB_AUTH_TOKEN")

def ak(path, method="GET", body=None):
    data = json.dumps(body).encode() if body else None
    req = urllib.request.Request(AUTH+"/api/v3"+path, data=data, method=method,
        headers={"Authorization":"Bearer "+AK_TOKEN,"Content-Type":"application/json","Accept":"application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=45)
        return r.status, (json.loads(r.read() or b"{}") if r.status != 204 else {})
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]

def hub(path, method="GET", body=None, token=None, opener=None, tenant="default"):
    data = json.dumps(body).encode() if body else None
    h = {"Content-Type":"application/json","Accept":"application/json"}
    if token:
        h["Authorization"] = "Bearer "+token
        # The static admin token carries no tenant of its own, so
        # auth.TenantMiddleware has nothing to resolve and every admin call
        # 403s without this. It is not optional and it is easy to lose an
        # afternoon to.
        if tenant: h["X-Tenant-ID"] = tenant
    req = urllib.request.Request(HUB+path, data=data, method=method, headers=h)
    op = opener or urllib.request
    try:
        r = op.open(req, timeout=45) if opener else urllib.request.urlopen(req, timeout=45)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]

results=[]
def check(n, ok, d=""):
    results.append((n,ok)); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d) if d else ""))

probe = "journey-" + uuid.uuid4().hex[:8]
email = probe + "@meshsat.org"

class Flow:
    def __init__(self, name):
        self.name = name
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
    def get(self, query=""):
        r = self.op.open(urllib.request.Request(
            f"{AUTH}/api/v3/flows/executor/{self.name}/?query={urllib.parse.quote(query)}",
            headers={"Accept":"application/json"}), timeout=45)
        return json.loads(r.read())
    def post(self, payload, query=""):
        req = urllib.request.Request(
            f"{AUTH}/api/v3/flows/executor/{self.name}/?query={urllib.parse.quote(query)}",
            data=json.dumps(payload).encode(),
            headers={"Content-Type":"application/json","Accept":"application/json"}, method="POST")
        try:
            r = self.op.open(req, timeout=45)
            return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:    return e.code, json.loads(e.read())
            except Exception: return e.code, {}

# --- 1. the state enrolment leaves behind -------------------------------
#
# Enrolment itself is NOT walked here any more, and that is deliberate: a
# Cloudflare Turnstile CAPTCHA is bound to the flow at order 5, and a CAPTCHA
# working is a CAPTCHA that stops this script. Enrolment has its own coverage in
# captcha-e2e.py.
#
# The segment this suite exists for was never enrolment. It is the one nobody
# has ever walked on production: an approved account signing in for the FIRST
# time, and the tenant that gets created when it does. So the account is created
# through the admin API in exactly the state enrolment leaves it -- inactive, in
# meshsat-pending, carrying the attributes the flow collects -- and the walk
# starts from there.
print("=== 1. an account in the state enrolment leaves behind ===")

s, groups = ak("/core/groups/?name=meshsat-pending")
pending = (groups.get("results") or [{}])[0].get("pk") if isinstance(groups, dict) else None
check("the meshsat-pending group exists", bool(pending), str(pending))
if not pending:
    print("cannot continue"); sys.exit(1)

s, created = ak("/core/users/", "POST", {
    "username": probe,
    "name": "Journey Probe",
    "email": email,
    "is_active": False,
    "groups": [pending],
    "attributes": {
        # What the enrolment flow stamps. country is the one that decides
        # whether Dutch VAT applies to this customer's receipts at all. It is
        # a NAME, the way a person types it in the form's free-text field, not
        # a code: with "NL" here this suite passed for two weeks while every
        # real customer who typed "USA" was refused at first sign-in on the
        # two-character billing column (MESHSAT-1365).
        "country": "United States",
        "organisation": "E2E",
        "hardware": "MeshSat field kit",
        "intended_use": "walking the approval workflow end to end",
        "signup_ip": "203.0.113.7",
        "terms_accepted_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "email_verified": True,
        # The Hub approves a probe without mailing it (its address is a
        # catch-all that lands in the operator's inbox) and the pending-signup
        # notice does not list it.
        "verification_probe": True,
    },
})
check("the pending account was created", s in (200, 201), f"{s} {str(created)[:160]}")
if s not in (200, 201):
    print("cannot continue"); sys.exit(1)
pk = created["pk"]

# The password the sign-in step will use.
s, _ = ak(f"/core/users/{pk}/set_password/", "POST", {"password": PASSWORD})
check("a password was set", s in (204, 200), str(s))

s, page = ak(f"/core/users/?search={probe}")
u = (page.get("results") or [{}])[0]
check("it is inactive before approval", not u.get("is_active"))
check("it is in meshsat-pending",
      "meshsat-pending" in json.dumps(u.get("groups_obj") or u.get("groups") or []))
check("the country enrolment collects is on the account, as typed",
      (u.get("attributes") or {}).get("country") == "United States",
      "this is what decides the VAT treatment of every receipt it will ever get")

print("\n=== 3. a platform admin approves through the Hub API ===")
s, b = hub("/api/admin/signups/", token=HUB_TOKEN)
listed = probe in b
check("the request is listed for the admin", s==200 and listed, f"{s} listed={listed}")
check("the listing carries what was collected", '"hardware"' in b and '"terms_accepted_at"' in b,
      "hardware+terms present" if ('"hardware"' in b and '"terms_accepted_at"' in b) else b[:200])
s, b = hub(f"/api/admin/signups/{pk}/approve", "POST", {"role":"owner"}, token=HUB_TOKEN)
check("approve succeeds", s in (200,204), f"{s} {b[:160]}")
# The decision is history the platform can read back (MESHSAT-1366): the
# Requests page shows it, so the chain must carry it with an actor.
s, b = hub("/api/admin/signups/history?limit=20", token=HUB_TOKEN)
try:
    hist = json.loads(b) if s == 200 else []
except Exception:
    hist = []
mine = [h for h in hist if h.get("email") == email and h.get("action") == "signup_approved"]
check("the approval is in the decision history with an actor", s == 200 and mine and mine[0].get("actor"),
      f"{s} rows={len(hist)} mine={mine[:1]}")
time.sleep(2)
s, page = ak(f"/core/users/?search={probe}")
u = (page.get("results") or [{}])[0]
check("the account is now ACTIVE", u.get("is_active") is True, str(u.get("is_active")))
groups = [g.get("name") for g in (u.get("groups_obj") or [])] or u.get("groups", [])
check("it left meshsat-pending", "meshsat-pending" not in json.dumps(groups), json.dumps(groups)[:120])
# BOTH replicas: the approval landed on whichever pod the ingress picked, and
# `logs deploy/hub` reads one pod at random (it passed by luck on the first
# in-cluster run and failed on the second).
logs = sh(CTX+["logs","-l","app.kubernetes.io/name=hub","--since=3m","--all-containers","--prefix"])
check("the approval email was sent", "signup approved" in logs,
      "logged" if "signup approved" in logs else "no 'signup approved' line in the last 3 minutes")

print("\n=== 4. first sign-in creates the tenant ===")
before = sql("SELECT count(*) FROM tenants;")
# Authenticate to authentik, then walk the Hub's OIDC login.
af = Flow("meshsat-authentication")
st = af.get()
for _ in range(6):
    comp = st.get("component", "")
    if comp == "ak-stage-identification":
        # This stage carries no `fields` array. It advertises what it wants as
        # user_fields plus password_fields, and takes both in ONE post -- which
        # is why a loop looking for a field named "password" never advances.
        body = {"uid_field": probe}
        if st.get("password_fields"):
            body["password"] = PASSWORD
        s2, st = af.post(body)
    elif comp == "ak-stage-password":
        s2, st = af.post({"password": PASSWORD})
    else:
        break
    if s2 != 200:
        break
check("the approved account can authenticate",
      st.get("component") == "xak-flow-redirect" or st.get("type") == "redirect",
      f"{st.get('component')} {json.dumps(st)[:140]}")

# Now the Hub's OIDC login, following redirects on the SAME cookie jar.
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(af.jar))
try:
    r = op.open(urllib.request.Request(HUB+"/api/auth/oidc/login", headers={"Accept":"text/html"}), timeout=60)
    final, code = r.geturl(), r.status
except urllib.error.HTTPError as e:
    final, code = e.url, e.code
check("the OIDC round trip completed", code in (200,302) and "error" not in final.lower(), f"{code} {final[:110]}")

time.sleep(2)
row = sql(f"SELECT t.id, t.plan, t.status, u.email, u.role, t.billing_country FROM tenants t JOIN users u ON u.tenant_id=t.id WHERE u.email='{email}';")
check("a tenant was created for the new account", bool(row), row or "none")
if row:
    parts = row.split("|")
    check("it is on the FREE plan", parts[1]=="free", parts[1])
    check("the user is its OWNER", parts[4]=="owner", parts[4])
    check("the tenant is active", parts[2]=="active", parts[2])
    check("the typed country became the ISO code on the tenant", parts[5]=="US",
          f"billing_country={parts[5]!r} for a customer who typed 'United States' (MESHSAT-1365)")
    check("exactly ONE tenant was created", int(sql("SELECT count(*) FROM tenants;")) == int(before)+1,
          f"{before} -> {sql('SELECT count(*) FROM tenants;')}")
    tid = parts[0]
    check("its usage reports the free ceiling", '"limit":4' in (hub(f"/api/admin/tenants/{tid}/usage", token=HUB_TOKEN)[1] or ""),
          hub(f"/api/admin/tenants/{tid}/usage", token=HUB_TOKEN)[1][:120])

    print("\n=== 5. the platform directory and support access (MESHSAT-1366) ===")
    s, b = hub(f"/api/admin/tenants?q={urllib.parse.quote(email)}", token=HUB_TOKEN)
    try:
        rows = json.loads(b) if s == 200 else []
    except Exception:
        rows = []
    row = rows[0] if len(rows) == 1 else {}
    check("the directory finds the new tenant by its owner's address", s == 200 and len(rows) == 1 and row.get("id") == tid, f"{s} rows={len(rows)}")
    check("the directory row carries owner, counts and the free ceiling",
          row.get("owner_email") == email and row.get("users") == 1 and row.get("devices") == 0 and row.get("limit") == 4,
          json.dumps({k: row.get(k) for k in ("owner_email","users","devices","bridges","limit","used")}))
    s, b = hub(f"/api/admin/tenants/{tid}", token=HUB_TOKEN)
    detail = json.loads(b) if s == 200 else {}
    check("the detail says no support access is granted yet", s == 200 and not (detail.get("support_access") or {}).get("active"), f"{s} {str(detail.get('support_access'))[:120]}")
    # The operator may not open the workspace without the customer's PIN.
    s, b = hub(f"/api/admin/tenants/{tid}/view-as", "POST", {"pin": "nobody-granted-anything"}, token=HUB_TOKEN)
    check("view-as is refused before the customer grants access", s == 403 and "support_access_required" in b, f"{s} {b[:120]}")
    # The customer grants, on their own session. The OIDC round trip left the
    # meshsat_refresh cookie in the jar; the console turns it into an access
    # token with POST /api/auth/refresh, and so does this.
    s, b = hub("/api/auth/refresh", "POST", {}, opener=op, tenant=None)
    try:
        probe_token = json.loads(b).get("access_token", "") if s == 200 else ""
    except Exception:
        probe_token = ""
    check("the probe's session yields an access token", bool(probe_token), f"{s} {b[:100]}")
    PIN = uuid.uuid4().hex[:12]
    s, b = hub("/api/tenant/support-access", "POST", {"pin": PIN, "duration_minutes": 30}, token=probe_token, tenant=None)
    check("the owner grants support access for 30 minutes", s == 201 and '"active":true' in b, f"{s} {b[:160]}")
    s, b = hub(f"/api/admin/tenants/{tid}/view-as", "POST", {"pin": "wrong-" + PIN}, token=HUB_TOKEN)
    check("a wrong PIN is refused and counted", s == 401 and '"wrong_pin"' in b and '"attempts_left":4' in b, f"{s} {b[:120]}")
    s, b = hub(f"/api/admin/tenants/{tid}/view-as", "POST", {"pin": PIN}, token=HUB_TOKEN)
    check("the right PIN opens the workspace", s == 200 and f'"tenant_id":"{tid}"' in b, f"{s} {b[:120]}")
    time.sleep(1)
    started_tenant = sql(f"SELECT count(*) FROM audit_log WHERE tenant_id='{tid}' AND action='tenant_view_started';")
    started_platform = sql(f"SELECT count(*) FROM audit_log WHERE tenant_id='default' AND action='tenant_view_started' AND detail LIKE 'tenant={tid} %';")
    check("the open is on the customer's chain", started_tenant == "1", started_tenant)
    check("the open is mirrored on the platform chain", started_platform == "1", started_platform)
    s, b = hub(f"/api/admin/tenants/{tid}/view-as", "DELETE", token=HUB_TOKEN)
    check("leaving the workspace is recorded", s == 200 and sql(f"SELECT count(*) FROM audit_log WHERE tenant_id='{tid}' AND action='tenant_view_ended';") == "1", f"{s}")
    s, b = hub("/api/tenant/support-access", "DELETE", token=probe_token, tenant=None)
    check("the owner revokes the grant", s == 200 and '"active":false' in b, f"{s} {b[:120]}")
    s, b = hub(f"/api/admin/tenants/{tid}/view-as", "POST", {"pin": PIN}, token=HUB_TOKEN)
    check("after the revoke the right PIN no longer opens anything", s == 403, f"{s} {b[:120]}")

print("\n=== teardown ===")
tid = (row.split("|")[0] if row else "")
if tid:
    for tbl in ("devices","api_keys","credentials","messages","audit_log","users","oidc_identities","support_grants"):
        col = "tenant_id"
        sql(f"DELETE FROM {tbl} WHERE {col}='{tid}';")
    sql(f"DELETE FROM tenants WHERE id='{tid}';")
s, _ = ak(f"/core/users/{pk}/", "DELETE")
print("   authentik account removed:", s in (204,404), "| tenant removed:", sql(f"SELECT count(*) FROM tenants WHERE id='{tid}';")=="0" if tid else "n/a")

bad=[r for r in results if not r[1]]
print("\n%d checks, %d failed" % (len(results), len(bad)))
sys.exit(1 if bad else 0)
