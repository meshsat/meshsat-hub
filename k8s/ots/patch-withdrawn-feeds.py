#!/usr/bin/env python3
"""Let OpenTAKServer 1.7.13 start without adsbxcot and aiscot (MESHSAT-1666).

WHAT HAPPENED. Their maintainer withdrew both packages from PyPI between 22 and
26 Oct 2026: the index and the files answer 404, and the source repositories
went private. OpenTAKServer 1.7.13 pins adsbxcot==6.0.4 and aiscot==7.0.10, so
its lock could no longer be installed from a cold build cache, and package:ots
could only build while a runner still held the installed layer. Changing any
other pin (the trivy findings of MESHSAT-1666) needs that layer rebuilt.
OpenTAKServer 1.7.14 dropped both packages the same way, by commenting out the
imports.

WHAT USES THEM. Only `opentakserver/blueprints/scheduled_jobs.py`: the ADS-B
job (`get_adsb_data`, airplanes.live) and the AIS job (`get_aishub_data`,
AISHub). Both are paused by default (`next_run_time: None` in
defaultconfig.py), and a tenant pod could not run them anyway: the
CiliumNetworkPolicy allows kube-dns and its own Postgres cluster and nothing
else. But the module imports both at the top, and app.py imports the module
while it builds the app, so without this patch OpenTAKServer does not start.

WHAT IT DOES. Replaces the two imports with stand-ins that raise a RuntimeError
naming the package when anything is looked up on them. If somebody resumes one
of the jobs, the job's own `except BaseException` logs that message; nothing
else changes.

WHY IT FAILS LOUDLY. Same contract as patch-icon-fetch.py and
patch-eud-handshake.py: the anchor is byte-exact (CRLF included -- this file
has Windows line endings), it must match EXACTLY ONCE, the result must compile,
and anything else exits non-zero and fails the image build. When it runs against
the venv it also refuses any OTHER import of either package anywhere in
opentakserver, since that would fail at run time instead. Re-running on an
already-patched file is a no-op.

Usage: patch-withdrawn-feeds.py [scheduled_jobs.py to patch]

The optional argument exists so this can be PROVEN against the real file outside
a container: the image is about 1 GB and must not be built on the shared runner
to test a text substitution. The Dockerfile never passes it.
"""

import glob
import pathlib
import re
import sys

MARKER = b"MESHSAT-1666"

WITHDRAWN = ("adsbxcot", "aiscot")

# Byte-exact, CRLF, as it appears in opentakserver 1.7.13.
ANCHOR = b"import adsbxcot\r\nimport aiscot\r\n"

REPLACEMENT = '''\
# MESHSAT-1666: adsbxcot and aiscot were withdrawn from PyPI in October 2026 and
# are not in this image (OpenTAKServer 1.7.14 dropped them too). Only the ADS-B
# and AIS jobs below use them; both are paused by default and a tenant pod has
# no egress to airplanes.live or AISHub. A resumed job fails inside its own try
# block with the message below, instead of this module failing to import, which
# would stop OpenTAKServer from starting.
class _MeshSatWithdrawn:
    def __init__(self, name):
        self._meshsat_name = name

    def __getattr__(self, attr):
        if attr.startswith("__"):
            raise AttributeError(attr)
        raise RuntimeError(
            "{} was withdrawn from PyPI and is not in this image (MESHSAT-1666)".format(
                self._meshsat_name
            )
        )


adsbxcot = _MeshSatWithdrawn("adsbxcot")
aiscot = _MeshSatWithdrawn("aiscot")
'''

VENV_GLOB = "/opt/ots/lib/python*/site-packages/opentakserver/blueprints/scheduled_jobs.py"

# Any import of either package, in any form: `import x`, `import a, x`,
# `from x import y`, `from x.sub import y`.
_IMPORT_RE = re.compile(
    rb"^[ \t]*(?:import[ \t]+(?:[\w.]+[ \t]*,[ \t]*)*(?:%s)\b|from[ \t]+(?:%s)\b)"
    % (b"|".join(w.encode() for w in WITHDRAWN), b"|".join(w.encode() for w in WITHDRAWN)),
    re.MULTILINE,
)


def die(msg):
    print(f"patch-withdrawn-feeds: FATAL {msg}", file=sys.stderr)
    return 1


def other_imports(package_root: pathlib.Path, patched: pathlib.Path):
    """Every file under opentakserver/ other than `patched` that imports either package."""
    hits = []
    for py in sorted(package_root.rglob("*.py")):
        if py == patched:
            continue
        if _IMPORT_RE.search(py.read_bytes()):
            hits.append(str(py))
    return hits


def main() -> int:
    if len(sys.argv) > 2:
        return die("usage: patch-withdrawn-feeds.py [scheduled_jobs.py]")

    in_venv = len(sys.argv) == 1
    if in_venv:
        found = glob.glob(VENV_GLOB)
        if len(found) != 1:
            return die(
                f"expected exactly one opentakserver/blueprints/scheduled_jobs.py in the venv, "
                f"found {found}.\nThe venv layout changed; this patch cannot guess which file to edit."
            )
        path = pathlib.Path(found[0])
    else:
        path = pathlib.Path(sys.argv[1])
        if not path.is_file():
            return die(f"{path} is not a file")

    if in_venv:
        # opentakserver/blueprints/scheduled_jobs.py -> opentakserver/
        others = other_imports(path.parent.parent, path)
        if others:
            return die(
                "another module in opentakserver imports adsbxcot or aiscot:\n  "
                + "\n  ".join(others)
                + "\nNeither package is installed (both were withdrawn from PyPI), so that\n"
                "module would fail at run time. Read it and extend this patch."
            )

    raw = path.read_bytes()

    if MARKER in raw:
        print(f"patch-withdrawn-feeds: {path} is already patched, nothing to do")
        return 0

    n = raw.count(ANCHOR)
    if n != 1:
        return die(
            f"the import anchor matched {n} times in {path}, expected exactly 1.\n"
            "\n"
            "OpenTAKServer changed the text this patch rewrites. That is a BUILD\n"
            "FAILURE on purpose: adsbxcot and aiscot are not installed, so if this\n"
            "module still imports them, OpenTAKServer cannot start at all.\n"
            "\n"
            "Re-read the top of scheduled_jobs.py and update ANCHOR and REPLACEMENT\n"
            "together. Do not relax the match to make the build pass."
        )

    # scheduled_jobs.py is CRLF throughout; keep it that way.
    replacement = REPLACEMENT.replace("\n", "\r\n").encode()
    patched = raw.replace(ANCHOR, replacement, 1)

    if MARKER not in patched or _IMPORT_RE.search(patched):
        return die("the rewrite did not take; refusing to write a half-patched file")

    # Never ship a file that does not parse: OpenTAKServer would not start, for
    # every tenant, with a traceback from inside a third-party package.
    try:
        compile(patched, str(path), "exec")
    except SyntaxError as e:
        return die(f"the patched scheduled_jobs.py does not parse ({e}); refusing to write it")

    path.write_bytes(patched)
    print(f"patch-withdrawn-feeds: {path} no longer imports adsbxcot or aiscot")
    return 0


if __name__ == "__main__":
    sys.exit(main())
