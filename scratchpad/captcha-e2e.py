import http.cookiejar, json, os, subprocess, sys, urllib.request, urllib.error, uuid
AUTH="https://auth.meshsat.net"; FLOW="meshsat-enrollment"
REAL_SITE=os.environ["TURNSTILE_SITE_KEY"]; REAL_SECRET=os.environ["TURNSTILE_SECRET"]
# Cloudflare's documented testing pair that always passes.
PASS_SITE="1x00000000000000000000AA"; PASS_SECRET="1x0000000000000000000000000000000AA"

def akshell(code):
    return subprocess.run(["kubectl","--context","notrf01","-n","omoikane","exec","-i",
                           "deploy/auth-server","--","ak","shell","-c",code],
                          capture_output=True,text=True).stdout

def set_keys(site, secret, label):
    out = akshell(
        "from authentik.stages.captcha.models import CaptchaStage\n"
        "s = CaptchaStage.objects.filter(name='meshsat-enrollment-captcha').first()\n"
        f"s.public_key = {site!r}\ns.private_key = {secret!r}\ns.save()\n"
        "print('SET ' + s.public_key)\n")
    ok = "SET "+site in out
    print(f"   [{label}] keys applied: {ok}")
    return ok

class Flow:
    def __init__(self):
        self.jar=http.cookiejar.CookieJar()
        self.op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
    def get(self):
        r=self.op.open(urllib.request.Request(f"{AUTH}/api/v3/flows/executor/{FLOW}/?query=",
            headers={"Accept":"application/json"}),timeout=45)
        return json.loads(r.read())
    def post(self,p):
        req=urllib.request.Request(f"{AUTH}/api/v3/flows/executor/{FLOW}/?query=",
            data=json.dumps(p).encode(),
            headers={"Content-Type":"application/json","Accept":"application/json"},method="POST")
        try:
            r=self.op.open(req,timeout=60); return r.status,json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            try: return e.code,json.loads(e.read())
            except Exception: return e.code,{}

results=[]
def check(n,ok,d=""):
    results.append((n,ok)); print(("  PASS  " if ok else "  FAIL  ")+n+(("   "+d) if d else ""))

try:
    print("=== 1. the real secret refuses a forged token ===")
    f=Flow(); st=f.get()
    check("the captcha is the first stage", st.get("component")=="ak-stage-captcha", st.get("component"))
    check("it presents OUR site key", st.get("site_key")==REAL_SITE, str(st.get("site_key")))
    s,r=f.post({"token":"forged-token-not-from-cloudflare"})
    advanced = r.get("component") not in ("ak-stage-captcha", None) and r.get("component")!="ak-stage-captcha"
    refused = (s!=200) or bool(r.get("response_errors")) or r.get("component")=="ak-stage-captcha"
    check("a forged token does NOT get past the captcha", refused and not (r.get("component")=="ak-stage-prompt"),
          f"http={s} component={r.get('component')} errors={json.dumps(r.get('response_errors',''))[:90]}")

    print("\n=== 2. a valid token lets the flow continue ===")
    print("   (Cloudflare's always-passes testing pair, restored immediately after)")
    if not set_keys(PASS_SITE, PASS_SECRET, "testing"): raise SystemExit("could not apply testing keys")
    f2=Flow(); st=f2.get()
    check("the stage still renders", st.get("component")=="ak-stage-captcha", st.get("component"))
    s,r=f2.post({"token":"XXXX.DUMMY.TOKEN.XXXX"})
    check("a valid token advances to the account form", r.get("component")=="ak-stage-prompt",
          f"http={s} component={r.get('component')}")
    if r.get("component")=="ak-stage-prompt":
        probe="captchae2e-"+uuid.uuid4().hex[:8]; PW="Str0ng-Passw0rd-2026"
        s,r=f2.post({"username":probe,"name":"Captcha E2E","email":probe+"@meshsat.org",
                     "password":PW,"password_repeat":PW})
        check("the account stage still works behind the captcha", s==200 and not r.get("response_errors"), str(s))
        ans={}
        for x in r.get("fields",[]):
            k=x.get("field_key")
            if k: ans[k]= True if k.endswith("accepted_terms") else ("NL" if k.endswith("country") else ("e2e" if k.endswith(("organisation","hardware","intended_use")) else ""))
        s,r=f2.post(ans)
        check("the flow reaches email verification, captcha and all", r.get("component")=="ak-stage-email",
              str(r.get("component")))
        print("   PROBE:", probe)
finally:
    print("\n=== restoring the real keys ===")
    ok = set_keys(REAL_SITE, REAL_SECRET, "production")
    f3=Flow(); st=f3.get()
    live = st.get("site_key")==REAL_SITE and st.get("component")=="ak-stage-captcha"
    print(f"   live stage back on the production key: {live} ({st.get('site_key')})")
    results.append(("the production key is restored and live", live))

bad=[r for r in results if not r[1]]
print("\n%d checks, %d failed" % (len(results), len(bad)))
sys.exit(1 if bad else 0)
