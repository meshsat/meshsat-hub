#!/usr/bin/env python3
"""Falsifiability proof for the deep-basemap check (MESHSAT-1221 AC9).

Re-creates the MESHSAT-1229 failure modes in the browser only. Production is
untouched: the request is intercepted on the way out.
  A. the deep archive hangs   (what actually happened: ingress could not reach it)
  B. the world archive answers in its place (a plausible silent regression)
"""
import importlib.util, sys
from playwright.sync_api import sync_playwright
spec = importlib.util.spec_from_file_location("bs", "browser-suite.py")
bs = importlib.util.module_from_spec(spec); spec.loader.exec_module(bs)
DEEP = bs.HUB + "/basemap/local.pmtiles"

def scenario(label, handler):
    bs.failures.clear(); bs.notes.clear()
    with sync_playwright() as p:
        b = p.chromium.launch(args=["--no-sandbox","--disable-dev-shm-usage"])
        ctx = b.new_context(); pg = ctx.new_page()
        pg.goto(bs.HUB + "/", wait_until="domcontentloaded", timeout=60000)
        pg.route("**/basemap/local.pmtiles", handler)
        bs.check_basemap(pg, "deep basemap (z15)", "/basemap/local.pmtiles", 15)
        ctx.close(); b.close()
    ok = bool(bs.failures)
    print(f"  {label}: {'CAUGHT' if ok else 'MISSED'}")
    for f in bs.failures: print(f"     -> {f[:130]}")
    return ok

# A: the request never completes, as when ingress-nginx could not reach the pod
a = scenario("A  deep archive hangs", lambda r: r.abort("timedout"))
# B: the world archive answers instead, so maxzoom is 11 not 15
def swap(route):
    route.fulfill(response=route.request.frame.page.request.get(bs.HUB + "/basemap/basemap.pmtiles",
                                                               headers={"Range": "bytes=0-16383"}))
b = scenario("B  world archive answers in its place", swap)
print()
print("PROVEN: both 1229 shapes are caught" if (a and b) else "NOT falsifiable -- do not ship")
sys.exit(0 if (a and b) else 1)
