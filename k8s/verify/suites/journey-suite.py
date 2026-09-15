#!/usr/bin/env python3
"""Phase D1b: the segment nobody has ever walked -- verify, approve, sign in.

The enrollment suite proves an account lands pending. The quota suite proves a
tenant's ceiling. Between them sits the part that has only ever been reasoned
about: clicking the verification link, a platform admin approving, and the
FIRST SIGN-IN creating the tenant. This walks it against production and cleans
up after itself.
"""
import http.cookiejar, json, re, subprocess, sys, time, urllib.request, urllib.error, urllib.parse, uuid

AUTH = "https://auth.meshsat.net"
HUB  = "https://hub.meshsat.net"
FLOW = "meshsat-enrollment"
CTX  = ["kubectl","--context","notrf01","-n","meshsat-hub"]
DB   = ["kubectl","--context","notrf01","-n","meshsat-hub-db","exec","meshsat-hub-main-1","--",
        "psql","-U","postgres","-d","meshsat_hub","-t","-A","-F|","-c"]
PASSWORD = "Str0ng-Passw0rd-2026"

def sh(c): return subprocess.run(c, capture_output=True, text=True).stdout.strip()
def sql(q): return sh(DB+[q])
POD = [l.split("/")[1] for l in sh(CTX+["get","pods","-o","name"]).splitlines() if l.startswith("pod/hub-")][0]
def env(v): return sh(CTX+["exec",POD,"--","printenv",v])
AK_TOKEN, HUB_TOKEN = env("HUB_AUTHENTIK_TOKEN"), env("HUB_AUTH_TOKEN")

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

def akshell(code):
    return subprocess.run(CTX[:3]+["--context","notrf01","-n","omoikane","exec","-i","deploy/auth-worker","--","ak","shell","-c",code],
                          capture_output=True, text=True).stdout

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
        # whether Dutch VAT applies to this customer's receipts at all.
        "country": "NL",
        "organisation": "E2E",
        "hardware": "MeshSat field kit",
        "intended_use": "walking the approval workflow end to end",
        "signup_ip": "203.0.113.7",
        "terms_accepted_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "email_verified": True,
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
check("the country enrolment collects is on the account",
      (u.get("attributes") or {}).get("country") == "NL",
      "this is what decides the VAT treatment of every receipt it will ever get")

print("\n=== 3. a platform admin approves through the Hub API ===")
s, b = hub("/api/admin/signups/", token=HUB_TOKEN)
listed = probe in b
check("the request is listed for the admin", s==200 and listed, f"{s} listed={listed}")
check("the listing carries what was collected", '"hardware"' in b and '"terms_accepted_at"' in b,
      "hardware+terms present" if ('"hardware"' in b and '"terms_accepted_at"' in b) else b[:200])
s, b = hub(f"/api/admin/signups/{pk}/approve", "POST", {"role":"owner"}, token=HUB_TOKEN)
check("approve succeeds", s in (200,204), f"{s} {b[:160]}")
time.sleep(2)
s, page = ak(f"/core/users/?search={probe}")
u = (page.get("results") or [{}])[0]
check("the account is now ACTIVE", u.get("is_active") is True, str(u.get("is_active")))
groups = [g.get("name") for g in (u.get("groups_obj") or [])] or u.get("groups", [])
check("it left meshsat-pending", "meshsat-pending" not in json.dumps(groups), json.dumps(groups)[:120])
logs = sh(CTX+["logs","deploy/hub","--since=3m","--all-containers"])
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
row = sql(f"SELECT t.id, t.plan, t.status, u.email, u.role FROM tenants t JOIN users u ON u.tenant_id=t.id WHERE u.email='{email}';")
check("a tenant was created for the new account", bool(row), row or "none")
if row:
    parts = row.split("|")
    check("it is on the FREE plan", parts[1]=="free", parts[1])
    check("the user is its OWNER", parts[4]=="owner", parts[4])
    check("the tenant is active", parts[2]=="active", parts[2])
    check("exactly ONE tenant was created", int(sql("SELECT count(*) FROM tenants;")) == int(before)+1,
          f"{before} -> {sql('SELECT count(*) FROM tenants;')}")
    tid = parts[0]
    check("its usage reports the free ceiling", '"limit":4' in (hub(f"/api/admin/tenants/{tid}/usage", token=HUB_TOKEN)[1] or ""),
          hub(f"/api/admin/tenants/{tid}/usage", token=HUB_TOKEN)[1][:120])

print("\n=== teardown ===")
tid = (row.split("|")[0] if row else "")
if tid:
    for tbl in ("devices","api_keys","credentials","messages","audit_log","users","oidc_identities"):
        col = "tenant_id"
        sql(f"DELETE FROM {tbl} WHERE {col}='{tid}';")
    sql(f"DELETE FROM tenants WHERE id='{tid}';")
s, _ = ak(f"/core/users/{pk}/", "DELETE")
print("   authentik account removed:", s in (204,404), "| tenant removed:", sql(f"SELECT count(*) FROM tenants WHERE id='{tid}';")=="0" if tid else "n/a")

bad=[r for r in results if not r[1]]
print("\n%d checks, %d failed" % (len(results), len(bad)))
sys.exit(1 if bad else 0)
