#!/usr/bin/env python3
"""Let OpenTAKServer start without adsbxcot and aiscot (MESHSAT-1710).

THE BREAK. In October 2026 both packages, by the same author, were deleted from
PyPI, from its mirrors and from GitHub; the image's hash-pinned install found
no adsbxcot==6.0.4 and every build of the image failed (pipeline 61634). They
are not optional to OpenTAKServer 1.7.13 only because
opentakserver/blueprints/scheduled_jobs.py imports them at the top, and the app
imports that module at start.

WHY DROPPING THEM IS RIGHT HERE. They serve two feed jobs, ADS-B from
airplanes.live (get_adsb_data) and AIS from aishub (get_aishub_data). Both are
paused by default (next_run_time None in defaultconfig.JOBS), and both need
internet egress a hosted instance does not have. So the packages are excluded
from the lock (overrides.txt) and this patch makes the two imports optional: an
operator who enabled a feed would see that job fail at its first record, not the
server fail to start.

WHY IT FAILS LOUDLY. Same contract as the other patches: the anchor is
byte-exact (CRLF), it must match EXACTLY ONCE, and anything else exits non-zero
and fails the image build. Re-running on a patched file is a no-op. The
Dockerfile also imports the module after the patch and asserts both names are
None, which proves it imports without the packages.

Usage: patch-optional-feeds.py [scheduled_jobs.py to patch]

The optional argument exists so the patch can be proven against the real file
outside a container; the Dockerfile never passes it.
"""

import glob
import pathlib
import sys

MARKER = b"MESHSAT-1710"

# Byte-exact, CRLF, as in opentakserver 1.7.13.
ANCHOR = b"import adsbxcot\r\nimport aiscot\r\n"

REPLACEMENT = (
    b"# MESHSAT-1710: adsbxcot and aiscot were deleted from PyPI in October 2026\r\n"
    b"# and are not in the image. They only serve the ADS-B and AIS feed jobs,\r\n"
    b"# paused by default and without the egress they need on a hosted instance.\r\n"
    b"try:\r\n"
    b"    import adsbxcot\r\n"
    b"except ImportError:\r\n"
    b"    adsbxcot = None\r\n"
    b"try:\r\n"
    b"    import aiscot\r\n"
    b"except ImportError:\r\n"
    b"    aiscot = None\r\n"
)

VENV_GLOB = "/opt/ots/lib/python*/site-packages/opentakserver/blueprints/scheduled_jobs.py"


def die(msg):
    print(f"patch-optional-feeds: FATAL {msg}", file=sys.stderr)
    return 1


def main() -> int:
    if len(sys.argv) > 2:
        return die("usage: patch-optional-feeds.py [scheduled_jobs.py]")

    if len(sys.argv) == 2:
        path = pathlib.Path(sys.argv[1])
        if not path.is_file():
            return die(f"{path} is not a file")
    else:
        found = glob.glob(VENV_GLOB)
        if len(found) != 1:
            return die(
                f"expected exactly one scheduled_jobs.py in the venv, found {found}.\n"
                "The venv layout changed; this patch cannot guess which file to edit."
            )
        path = pathlib.Path(found[0])

    raw = path.read_bytes()

    if MARKER in raw:
        print(f"patch-optional-feeds: {path} is already patched, nothing to do")
        return 0

    n = raw.count(ANCHOR)
    if n != 1:
        return die(
            f"the feed imports anchor matched {n} times in {path}, expected exactly 1.\n"
            "\n"
            "OpenTAKServer changed the imports this patch rewrites. That is a BUILD\n"
            "FAILURE on purpose: without the patch the server does not start, because\n"
            "the two packages are not in the image.\n"
            "\n"
            "Re-read the top of scheduled_jobs.py and update ANCHOR and REPLACEMENT\n"
            "together. Do not relax the match to make the build pass."
        )

    path.write_bytes(raw.replace(ANCHOR, REPLACEMENT, 1))
    print(f"patch-optional-feeds: patched {path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
