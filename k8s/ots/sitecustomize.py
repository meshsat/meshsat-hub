"""Give every HTTP request OpenTAKServer makes a timeout (MESHSAT-1070).

THE BUG THIS FIXES. Nothing in opentakserver 1.7.13 passes `timeout=` to
`requests`. Counted in the wheel rather than estimated: 15 `requests.*` call
sites, of which 14 carry no timeout -- `blueprints/ots_api/mediamtx_api.py` x10
and `blueprints/scheduled_jobs.py` x4. (The 15th is the icon fetch in `app.py`,
which `patch-icon-fetch.py` gave one.) This comment said "eleven" until
2026-09-13; the miscount came from a grep that omitted `requests.patch`, and
MESHSAT-1070 repeats it as "10". A request
with no timeout waits for the kernel to give up retransmitting, which on this
network is not a fast failure -- a tenant's instance has no egress, the
CiliumNetworkPolicy allows kube-dns and its own Postgres and nothing else, and
Cilium DROPS rather than rejects. connect() does not fail; it hangs.

That already cost us one incident. `app.py` fetched a marker-icon archive from
github.com on every start with no timeout and no listening API for the whole
window -- 134 seconds, measured, every single start, swallowed by an
`except BaseException` so it produced one line at the end and nothing while it
happened (MESHSAT-1057). That specific call now reads from disk, but the same
shape survives at the other fourteen -- one of them to `data.aishub.net`.

None of those blocks startup today, because MediaMTX is disabled and the rest are
APScheduler jobs. "Does not block startup" is a weak guarantee though: an
APScheduler worker stuck forever on a socket is a thread that never comes back,
and the next scheduled run piles up behind it.

WHY THIS SHAPE AND NOT FOURTEEN PATCHES. patch-icon-fetch.py and
patch-eud-handshake.py rewrite upstream source with byte-exact anchors, which is
right when the fix is specific to one block and must fail loudly if upstream moves
it. Fourteen anchors for one property would be fourteen things to re-verify on
every upgrade, and it would still not cover the fifteenth call somebody adds next
release.

A default applied at the transport layer covers all of them, including future
ones, and touches no upstream file. `sitecustomize` is imported by `site.py` at
interpreter startup, before any application code runs, so the default is in place
for everything.

WHAT IT DOES NOT DO. It does not override an explicit timeout -- `setdefault`
leaves a caller's own value alone, so if upstream ever starts passing one, theirs
wins. The read timeout is per-read, not total, so a streamed download of any size
still works as long as bytes keep arriving.

AND IT DOES NOT COVER `httpx`, which the title's "every HTTP request" overstates.
`blueprints/ots_api/tak_gov_link_api.py` drives seven calls through
`httpx.Client(http2=True)` with no timeout of their own, and this module wraps
`requests.Session.request` only. Measured in the shipped image rather than
assumed: httpx 0.28.1 defaults to `Timeout(timeout=5.0)`, so those are bounded
already -- a different and much smaller problem than the unbounded hang above.
Filed separately; do not widen this module to chase it without re-measuring that
default, because it is a library default and can change under us.

The numbers are deliberately generous: this is a backstop against hanging for
ever, not a latency budget.
"""

_CONNECT_TIMEOUT = 10.0
_READ_TIMEOUT = 60.0


def _install() -> None:
    try:
        import requests
    except Exception:  # pragma: no cover - requests is a hard dependency of OTS
        return

    session = requests.Session
    original = session.request

    # Idempotent: a second import (or a re-exec) must not wrap the wrapper.
    if getattr(original, "_meshsat_timeout_default", False):
        return

    def request(self, method, url, **kwargs):
        # setdefault, NOT assignment: an explicit timeout from the caller wins.
        kwargs.setdefault("timeout", (_CONNECT_TIMEOUT, _READ_TIMEOUT))
        return original(self, method, url, **kwargs)

    request._meshsat_timeout_default = True
    session.request = request


_install()
