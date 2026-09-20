#!/usr/bin/env python3
"""Nightly real-browser check of the auth flows and the Hub SPA (MESHSAT-1224).

Why this exists, and why a header check is not it. On 2026-09-17 a
Content-Security-Policy was added to auth.meshsat.net (MESHSAT-1198) and
verified by reading the header back with curl. It passed. It also blocked
every sign-up for 25 hours (MESHSAT-1223): authentik frames the Cloudflare
Turnstile widget from a `blob:` URL, `frame-src` did not allow it, and the
CAPTCHA rendered blank. Nothing that reads a header can see that. Only a
browser that executes the page can.

So this suite loads the real pages in headless Chromium and fails on:
  * any securitypolicyviolation event (the 1223 class of defect),
  * an enrollment page whose CAPTCHA frame tree is broken -- authentik's
    blob: wrapper missing, or the Cloudflare challenge not nested inside it
    (the 1223 symptom itself, caught even when the CSP is not the cause),
  * an uncaught page error or a failed same-origin request.

It creates nothing and submits nothing: every step is a page load.

Falsifiability: suites/negative-proof.py re-creates the 1223 defect in the
browser -- it strips blob: from frame-src on the way in -- and asserts that
THIS file's own check() goes red. Run it after any change here. A check that
has never been seen to fail is not a check.
"""
import json, os, sys

from playwright.sync_api import sync_playwright

AUTH = os.environ.get("VERIFY_AUTH_URL", "https://auth.meshsat.net")
HUB  = os.environ.get("VERIFY_HUB_URL",  "https://hub.meshsat.net")
FLOW = os.environ.get("VERIFY_ENROLL_FLOW", "meshsat-enrollment")

# The listener has to be installed before any of the page's own script runs,
# or a violation raised during parsing is missed -- which is precisely when a
# blocked frame or script fails.
INIT = """
window.__cspViolations = [];
window.addEventListener('securitypolicyviolation', function (e) {
  window.__cspViolations.push({
    directive: e.effectiveDirective || e.violatedDirective,
    blocked: e.blockedURI,
    source: e.sourceFile || '',
    line: e.lineNumber || 0,
  });
});
"""

# The deep basemap, which is the other ~25 h regression the owner found and no
# check did (MESHSAT-1229): `basemap-confine` admitted only host/remote-node, but
# the Ingress routes /basemap/local.pmtiles from the ingress-nginx POD, so every
# deep-zoom tile request hung and the map lost its streets past zoom 11. It is
# checked here rather than with a plain curl because a hang, not an error, is how
# it failed -- and because MapLibre reads these archives by HTTP range request,
# which is exactly what the fetch below does.
#
# PMTiles v3 header: 127 bytes, magic "PMTiles" at 0, version at 7, min zoom at
# 100 and max zoom at 101. Asserting max zoom proves the DEEP archive is the one
# answering, not the world archive standing in for it (MESHSAT-1221 AC9).
ARCHIVES = [
    ("world basemap", "/basemap/basemap.pmtiles", None),
    ("deep basemap (z15)", "/basemap/local.pmtiles", 15),
]

PAGES = [
    ("auth root",        f"{AUTH}/",                      False),
    ("auth enrollment",  f"{AUTH}/if/flow/{FLOW}/",       True),
    ("hub SPA shell",    f"{HUB}/",                       False),
    # The SPA is hash-routed, so the login route is "/#/login". This said
    # "/login" until 2026-09-20, which only ever worked because the server
    # answered every unknown path with the app shell -- the soft-404 defect
    # MESHSAT-1185 fixed the same day. The moment unknown paths began 404ing,
    # this check went red, which is the nightly doing its job.
    ("hub login",        f"{HUB}/#/login",                False),
]

failures, notes = [], []


def check(page, name, url, expect_turnstile):
    errors, failed_requests = [], []
    page.on("pageerror", lambda e: errors.append(str(e)))

    def on_failed(req):
        # Third-party beacons are not this suite's business; same-origin and
        # the CAPTCHA provider are.
        if any(h in req.url for h in ("meshsat.net", "challenges.cloudflare.com")):
            failed_requests.append(f"{req.method} {req.url} ({req.failure})")

    page.on("requestfailed", on_failed)

    # Not networkidle: the Turnstile widget holds a connection open, so the
    # enrollment page never goes idle and a networkidle wait times out on a
    # perfectly healthy page.
    resp = page.goto(url, wait_until="domcontentloaded", timeout=60_000)
    status = resp.status if resp else 0
    if status >= 400:
        failures.append(f"{name}: HTTP {status} at {url}")
        return

    violations = page.evaluate("window.__cspViolations || []")
    if violations:
        for v in violations:
            failures.append(
                f"{name}: CSP violation {v['directive']} blocked {v['blocked']}"
            )
    if errors:
        failures.append(f"{name}: page error: {errors[0][:200]}")
    if failed_requests:
        failures.append(f"{name}: request failed: {failed_requests[0][:200]}")

    if expect_turnstile:
        # authentik renders its stages from JSON after load, so the widget
        # arrives a beat after DOMContentLoaded. Waiting for it -- rather than
        # sleeping -- is part of the assertion.
        try:
            page.wait_for_selector("iframe#ak-captcha", timeout=30_000)
            page.wait_for_timeout(3_000)
        except Exception:
            pass

        # The structure, measured on 2026-09-20: authentik wraps the CAPTCHA in
        # `iframe#ak-captcha` whose src is a **blob:** URL, and the Cloudflare
        # challenge loads in a frame NESTED inside that one. Two consequences,
        # both learnt by making this fail on purpose:
        #   * a size check is useless -- with `frame-src 'none'` forced on, the
        #     outer element still paints at 462x69 while the inside is empty;
        #   * the honest assertion is the frame TREE, because a blocked blob:
        #     frame never enters it, and the Cloudflare frame nested in it
        #     cannot load either.
        urls = [f.url for f in page.frames]
        blob = [u for u in urls if u.startswith("blob:")]
        cf = [u for u in urls if "challenges.cloudflare.com" in u]
        if not blob:
            failures.append(
                f"{name}: authentik's blob: CAPTCHA frame is not in the frame "
                f"tree -- this is the MESHSAT-1223 signature and sign-up is "
                f"blocked ({len(urls)} frames)"
            )
        elif not cf:
            failures.append(
                f"{name}: the blob: frame loaded but the Cloudflare Turnstile "
                f"challenge did not -- the CAPTCHA box is empty and sign-up "
                f"cannot be completed"
            )
        else:
            notes.append(f"{name}: CAPTCHA frame tree intact (blob: wrapper + Turnstile challenge)")

    notes.append(f"{name}: HTTP {status}, 0 CSP violations")


def check_basemap(page, name, path, want_maxzoom):
    """Range-request a PMTiles header the way MapLibre does, and read it."""
    res = page.evaluate(
        """async (path) => {
             const t0 = performance.now();
             try {
               const r = await fetch(path, {headers: {Range: 'bytes=0-16383'}});
               const b = new Uint8Array(await r.arrayBuffer());
               return {status: r.status, len: b.length, ms: performance.now() - t0,
                       magic: String.fromCharCode(...b.slice(0, 7)),
                       version: b[7], minzoom: b[100], maxzoom: b[101]};
             } catch (e) {
               return {error: String(e), ms: performance.now() - t0};
             }
           }""",
        HUB + path,
    )
    if res.get("error"):
        failures.append(f"{name}: {path} failed: {res['error']} after {res['ms']:.0f}ms")
        return
    if res["status"] != 206:
        failures.append(f"{name}: {path} answered {res['status']}, want 206 (range request)")
        return
    if res["magic"] != "PMTiles":
        failures.append(f"{name}: {path} is not a PMTiles archive (magic {res['magic']!r}) -- something else is answering")
        return
    if want_maxzoom is not None and res["maxzoom"] != want_maxzoom:
        failures.append(
            f"{name}: max zoom is {res['maxzoom']}, want {want_maxzoom}. The deep archive is "
            f"not the one answering, so the map has no streets past the world archive"
        )
        return
    notes.append(f"{name}: HTTP 206, PMTiles v{res['version']}, zoom {res['minzoom']}-{res['maxzoom']}, {res['ms']:.0f}ms")


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch(
            args=["--no-sandbox", "--disable-dev-shm-usage"],
        )
        ctx = browser.new_context(ignore_https_errors=False)
        ctx.add_init_script(INIT)
        # The basemap first: it needs no page, only a same-origin fetch, and it
        # is the check most likely to hang.
        probe = ctx.new_page()
        probe.goto(HUB + "/", wait_until="domcontentloaded", timeout=60_000)
        for name, path, want in ARCHIVES:
            try:
                check_basemap(probe, name, path, want)
            except Exception as e:
                failures.append(f"{name}: {type(e).__name__}: {str(e)[:160]}")
        probe.close()

        for name, url, ts in PAGES:
            page = ctx.new_page()
            try:
                check(page, name, url, ts)
            except Exception as e:  # a timeout here is a real failure
                failures.append(f"{name}: {type(e).__name__}: {str(e)[:200]}")
            finally:
                page.close()
        ctx.close()
        browser.close()

    for n in notes:
        print(f"  PASS  {n}")
    for f in failures:
        print(f"  FAIL  {f}")
    print(f"{len(notes)} checks passed, {len(failures)} failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
