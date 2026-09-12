#!/usr/bin/env python3
"""Fetch OpenTAKServer's marker icon set at IMAGE BUILD time (MESHSAT-1057).

Why this exists: `opentakserver/app.py` main() counts rows in the `icon` table and,
when empty, downloads iconsets.sqlite from github.com with NO timeout. A tenant's
instance has no egress -- the CiliumNetworkPolicy allows kube-dns and its Postgres
cluster and nothing else -- and Cilium DROPS rather than rejects, so connect() hung
until the kernel gave up: 134 seconds on every single start, with the API not
listening on 8081 for the whole window. That made a healthy instance look wedged and
cost two wrong diagnoses before anyone read the log.

So the archive comes into the image here, at build time, where there IS egress, and
patch-icon-fetch.py teaches app.py to load it from disk. No instance ever reaches the
internet, and markers work.

Verified by digest, not trusted: an unpinned asset from a third-party repository is a
supply-chain hole, and this one is loaded straight into every tenant's database.
"""

import hashlib
import os
import sys
import urllib.request

URL = "https://raw.githubusercontent.com/brian7704/OpenTAKServer-Installer/master/iconsets.sqlite"
# Measured 2026-09-12. github.com/brian7704/OpenTAKServer-Installer/raw/master/... is
# a 302 to this raw.githubusercontent.com path; both serve the same bytes.
EXPECT_SHA256 = "7733ac2d1959d2ab68b1e23d5b886b2847a5551f121110be59e6ef9a0299ce3a"
EXPECT_SIZE = 10117120
DEST = sys.argv[1] if len(sys.argv) > 1 else "/opt/ots/share/iconsets.sqlite"


def main() -> int:
    os.makedirs(os.path.dirname(DEST), exist_ok=True)
    print(f"bundle-iconsets: fetching {URL}")
    with urllib.request.urlopen(URL, timeout=60) as r:
        blob = r.read()

    if len(blob) != EXPECT_SIZE:
        print(
            f"bundle-iconsets: FATAL size {len(blob)} != expected {EXPECT_SIZE}.\n"
            "The upstream asset changed. Re-measure it deliberately and update\n"
            "EXPECT_SHA256/EXPECT_SIZE in this file; do not relax the check.",
            file=sys.stderr,
        )
        return 1

    got = hashlib.sha256(blob).hexdigest()
    if got != EXPECT_SHA256:
        print(
            f"bundle-iconsets: FATAL sha256 {got} != expected {EXPECT_SHA256}.\n"
            "This archive is loaded into every tenant's database, so a content change\n"
            "is a supply-chain event and not something to wave through.",
            file=sys.stderr,
        )
        return 1

    with open(DEST, "wb") as f:
        f.write(blob)
    print(f"bundle-iconsets: wrote {DEST} ({len(blob)} bytes, sha256 verified)")

    # Prove it is the shape app.py expects rather than assuming: the loader does
    # SELECT * FROM icons and inserts each row into the Icon model.
    import sqlite3

    con = sqlite3.connect(DEST)
    cur = con.cursor()
    cur.execute("select count(*) from icons")
    icons = cur.fetchone()[0]
    cur.execute("select * from icons limit 1")
    cols = [d[0] for d in cur.description]
    con.close()
    expected_cols = [
        "id", "iconset_uid", "filename", "groupName",
        "type2525b", "useCnt", "bitmap", "shadow",
    ]
    if cols != expected_cols:
        print(
            f"bundle-iconsets: FATAL icons columns {cols} != {expected_cols}.\n"
            "app.py does insert(Icon).values(**row), so a column that the model does\n"
            "not have would fail at run time inside a bare `except BaseException`,\n"
            "which would look exactly like the bug this fix is for.",
            file=sys.stderr,
        )
        return 1
    print(f"bundle-iconsets: {icons} icons, columns match the Icon model")
    return 0


if __name__ == "__main__":
    sys.exit(main())
