#!/usr/bin/env python3
"""Bump one image digest pin in k8s/kustomization.yaml (MESHSAT-930; copy of omoikane daemon k8s/scripts/bump-pin.py).

Usage: bump-pin.py <kustomization.yaml> <image-name-key> <sha256:digest>

<image-name-key> is the `- name:` value of the entry in the `images:` block
(NOT the newName). The first `digest:` line after that entry is replaced,
preserving indentation and any trailing comment. Exit codes:
  0  updated (prints "updated <name> <old> -> <new>")
  3  already at that digest (prints "unchanged")
  4  entry not found — refuse rather than guess (a missing entry means the
     manifests don't consume this image; bumping nothing must be loud).

Line-based on purpose: no YAML round-tripper on the runners, and the images
block is machine-edited only here, so the shape is stable. Consumed by the
`bump_k8s_pin` CI jobs in this repo, www, and beta.
"""
import re
import sys


def main() -> int:
    path, name, digest = sys.argv[1], sys.argv[2], sys.argv[3]
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        print(f"refusing malformed digest: {digest!r}", file=sys.stderr)
        return 2
    lines = open(path).read().splitlines(keepends=True)
    in_entry = False
    for i, line in enumerate(lines):
        if re.match(rf"^\s*- name: {re.escape(name)}\s*(#.*)?$", line):
            in_entry = True
            continue
        if in_entry and re.match(r"^\s*- name: ", line):
            break  # next entry reached without a digest line
        if in_entry:
            m = re.match(r"^(\s*digest: )(sha256:[0-9a-f]{64})(.*)$", line)
            if m:
                old = m.group(2)
                if old == digest:
                    print(f"unchanged {name} {digest}")
                    return 3
                lines[i] = f"{m.group(1)}{digest}{m.group(3)}\n"
                open(path, "w").write("".join(lines))
                print(f"updated {name} {old} -> {digest}")
                return 0
    print(f"entry not found in {path}: {name}", file=sys.stderr)
    return 4


if __name__ == "__main__":
    sys.exit(main())
