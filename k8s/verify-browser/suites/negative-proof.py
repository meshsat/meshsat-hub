#!/usr/bin/env python3
"""Falsifiability proof for the MESHSAT-1224 browser suite.

Imports the real suite and runs its real check() against the real enrollment
page, with one change: the document's Content-Security-Policy is rewritten on
the way into the browser so frame-src no longer allows blob:. That is exactly
the policy that blocked every sign-up for 25 h (MESHSAT-1223). Production is
not modified -- the rewrite happens in this browser only.
"""
import importlib.util, sys
from playwright.sync_api import sync_playwright

spec = importlib.util.spec_from_file_location("bs", "browser-suite.py")
bs = importlib.util.module_from_spec(spec); spec.loader.exec_module(bs)

URL = f"{bs.AUTH}/if/flow/{bs.FLOW}/"
BROKEN = "frame-src 'self' https://challenges.cloudflare.com"   # blob: removed

with sync_playwright() as p:
    b = p.chromium.launch(args=["--no-sandbox", "--disable-dev-shm-usage"])
    ctx = b.new_context(); ctx.add_init_script(bs.INIT)

    def rewrite(route):
        r = route.fetch(); h = dict(r.headers)
        for k in list(h):
            if k.lower() == "content-security-policy":
                del h[k]
        h["content-security-policy"] = BROKEN
        route.fulfill(response=r, headers=h)

    page = ctx.new_page(); page.route(URL, rewrite)
    bs.check(page, "auth enrollment (CSP broken on purpose)", URL, True)
    ctx.close(); b.close()

print("notes :", *bs.notes, sep="\n  ")
print("FAILURES:", *bs.failures, sep="\n  ")
print()
print("PROVEN: the suite goes red on the 1223 defect" if bs.failures
      else "USELESS: the suite stayed green on the 1223 defect -- DO NOT SHIP")
sys.exit(0 if bs.failures else 1)
