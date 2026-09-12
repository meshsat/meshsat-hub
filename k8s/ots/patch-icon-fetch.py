#!/usr/bin/env python3
"""Teach OpenTAKServer to load its marker icons from disk (MESHSAT-1057).

THE BUG THIS FIXES. `opentakserver/app.py` main() counts rows in the `icon`
table and, when empty, does:

    r = requests.get("https://github.com/.../iconsets.sqlite", stream=True)

with no timeout. A tenant's instance has no egress: the CiliumNetworkPolicy
allows kube-dns and its own Postgres cluster and nothing else, and Cilium DROPS
rather than rejects, so connect() does not fail -- it hangs until the kernel
gives up retransmitting SYNs. Measured: 134 seconds on EVERY start, with the API
not listening on 8081 for the whole window. A healthy instance looked wedged,
and it cost two wrong diagnoses before anyone read the log.

The whole block sits inside `except BaseException`, so it is also invisible: the
stall produces one line at the end and nothing while it happens.

WHY A PATCH AND NOT A SETTING. There is no configuration flag for this in
1.7.13 -- the URL is a literal in the call. Shipping the archive alone does not
help either: the guard is the row count, so an empty `icon` table sends it to
the network regardless of what is on disk. And the path it writes to,
`OTS_DATA_FOLDER`, is `/var/lib/ots`, which the operator mounts as an EmptyDir
(render.go) -- so anything the image leaves there is masked. Hence: the archive
goes to /opt/ots/share, outside that mount, and the loader is pointed at it.

WHY IT FAILS LOUDLY. A patch that silently no-ops when upstream moves the text
would reintroduce a 134-second stall that nobody would look for twice. So the
anchor is byte-exact (CRLF included -- app.py has Windows line endings, all 535
of them), it must match EXACTLY ONCE, and anything else exits non-zero and fails
the image build. Re-running on an already-patched file is a no-op, so a rebuilt
layer is fine.

The network path is KEPT as a fallback, with a timeout, so this patch cannot
itself become the single point of failure for icons.

Usage: patch-icon-fetch.py <path to the bundled iconsets.sqlite> [app.py to patch]

The optional second argument exists so this can be PROVEN against a real app.py
outside a container: the image is about 1 GB and must not be built on the shared
runner to test a text substitution. The Dockerfile never passes it.
"""

import glob
import pathlib
import sys

MARKER = b"MESHSAT-1057"

# Byte-exact, CRLF, as it appears in opentakserver 1.7.13. Confirmed to match
# exactly once; see the module docstring for why that is enforced.
ANCHOR = (
    b'                r = requests.get(\r\n'
    b'                    "https://github.com/brian7704/OpenTAKServer-Installer/raw/master/iconsets.sqlite",\r\n'
    b'                    stream=True,\r\n'
    b'                )\r\n'
)

# Indent 16: inside main() -> with app.app_context() -> if icons == 0 -> try.
# `os`, `requests` and `logger` are all already in scope at that point -- the
# code being replaced uses all three.
REPLACEMENT = '''\
                # MESHSAT-1057: read the icon archive baked into this image.
                # Upstream fetches it from GitHub with no timeout, and this pod
                # has no egress (Cilium drops), so that hung for 134 seconds on
                # every start with the API not yet listening. The archive is
                # fetched and digest-verified at image build time by
                # k8s/ots/bundle-iconsets.py, and lives OUTSIDE the
                # /var/lib/ots EmptyDir, which masks anything the image puts
                # under OTS_DATA_FOLDER.
                class _MeshSatBlob:
                    def __init__(self, content):
                        self.content = content

                _meshsat_icons = "{bundled}"
                if os.path.exists(_meshsat_icons):
                    logger.info("Loading bundled icon sets from {{}}".format(_meshsat_icons))
                    with open(_meshsat_icons, "rb") as _meshsat_f:
                        r = _MeshSatBlob(_meshsat_f.read())
                else:
                    # Kept so this patch cannot become the single point of
                    # failure for icons -- but WITH a timeout, which upstream
                    # omits. A stall here is the defect, not the download.
                    logger.warning(
                        "Bundled icon sets missing at {{}}; falling back to the network".format(
                            _meshsat_icons
                        )
                    )
                    r = requests.get(
                        "https://github.com/brian7704/OpenTAKServer-Installer/raw/master/iconsets.sqlite",
                        stream=True,
                        timeout=(5, 30),
                    )
'''


def die(msg):
    print(f"patch-icon-fetch: FATAL {msg}", file=sys.stderr)
    return 1


VENV_GLOB = "/opt/ots/lib/python*/site-packages/opentakserver/app.py"


def main() -> int:
    if not 2 <= len(sys.argv) <= 3:
        return die("usage: patch-icon-fetch.py <bundled iconsets.sqlite> [app.py]")
    bundled = sys.argv[1]

    if len(sys.argv) == 3:
        path = pathlib.Path(sys.argv[2])
        if not path.is_file():
            return die(f"{path} is not a file")
    else:
        found = glob.glob(VENV_GLOB)
        if len(found) != 1:
            return die(
                f"expected exactly one opentakserver/app.py in the venv, found {found}.\n"
                "The venv layout changed; this patch cannot guess which file to edit."
            )
        path = pathlib.Path(found[0])
    raw = path.read_bytes()

    if MARKER in raw:
        print(f"patch-icon-fetch: {path} is already patched, nothing to do")
        return 0

    n = raw.count(ANCHOR)
    if n != 1:
        return die(
            f"the icon-download anchor matched {n} times in {path}, expected exactly 1.\n"
            "\n"
            "OpenTAKServer changed the text this patch rewrites. That is a BUILD\n"
            "FAILURE on purpose: without the patch, every tenant's instance stalls\n"
            "134 seconds on each start waiting for a GitHub fetch this network\n"
            "policy drops, and the stall is swallowed by a bare `except\n"
            "BaseException`, so nobody would find it again.\n"
            "\n"
            "Re-read app.py's main(), update ANCHOR and REPLACEMENT deliberately,\n"
            "and re-measure. Do not relax this check."
        )

    # app.py is CRLF throughout; keep it that way or the diff is the whole file.
    replacement = REPLACEMENT.format(bundled=bundled).replace("\n", "\r\n").encode()
    patched = raw.replace(ANCHOR, replacement)

    if patched.count(MARKER) != 1 or ANCHOR in patched:
        return die("the rewrite did not take; refusing to write a half-patched file")

    # Never ship a file that does not parse: the failure would surface as OTS
    # not starting at all, for every tenant, with a traceback from inside a
    # third-party package.
    try:
        compile(patched, str(path), "exec")
    except SyntaxError as e:
        return die(f"the patched app.py does not parse ({e}); refusing to write it")

    path.write_bytes(patched)
    print(
        f"patch-icon-fetch: {path} now loads icons from {bundled} "
        f"(+{len(patched) - len(raw)} bytes, network kept as a timed fallback)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
