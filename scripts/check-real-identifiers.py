#!/usr/bin/env python3
"""Fail when a known real device identity appears in a tracked file (MESHSAT-1283).

This repository is mirrored publicly. Until 21 Sep 2026 its tests, fixtures and
comments used real values as examples: field-kit and handheld modem IMEIs, the
kits' SIM numbers, the platform's SMS number, mesh node ids, the kits'
hostnames and their Cloudloop thing ids. They were replaced with made-up values;
this check stops them coming back.

It holds HASHES of the real values, never the values, so the check itself does
not publish what it guards. Every candidate token in every tracked text file is
hashed and looked up. It cannot recognise an identifier it has never been told
about: when new hardware joins the fleet, add its hash here (the command is in
the usage text).

Deliberately not scanned: k8s/ and .gitlab-ci.yml (live configuration that
names real infrastructure on purpose), generated output (docs/swagger, the
built web bundle), scratchpad/ operator scripts, and lock files.

stdlib only; runs in python:3-alpine in the lint stage.
"""
import hashlib
import re
import subprocess
import sys

KNOWN = {
    "3d9e31e3c25bf5c408b942b0c02d61acbe7ef7051635a399584309fa85abb239": "a field-kit modem IMEI",
    "094427ddcb629ad79fd411a12a3377f12e0bd1e0af34ed116514c0daf665b746": "a field-kit modem IMEI",
    "5564fa48d58cc6ed48fe50f06b059e5290d60e56136764544088bb22b4635fe7": "the handheld node's modem IMEI",
    "df999545cc4db83c807454115f9331129ac8de2d807d9ff3f585d0edb102a19d": "a field-kit SIM number",
    "58f41b62ff901c7f783254e7dc35b693c818bb2a940154b209c6dd0faadfc4cc": "a field-kit SIM number",
    "6fbb66bf6af2525d6bdca3622686a6d4b6ae36f67d5c6b2562b84af83c4e47a5": "the platform's SMS number",
    "af5b25153f2e9322320e7fbaf199ea00208a3c3b9a85d4407c54502528a5672c": "the platform's WhatsApp sender number (MESHSAT-1367)",
    "0c0c316deb6cd85a93325bd1658e861dde982e55c1e4d59f15a6b5048410d013": "the owner's mobile number",
    "270bf6fef2f00c56166763f4d8eb37765e7ed516747a23e18c5bfd05b42b195e": "a real mesh node id",
    "05e7eb0f2400e063257cd78628dd8d7f0bda0f86d2f1842df2fcc1e002ab85da": "a real mesh node id",
    "83e55e5b4841be5464584c249da47192bf8233c725655202c4188cd09e1ba287": "a real mesh node id",
    "c3aa084dd5e0b7c44edca0e4809fefa695c7427860df1ba53021519f71811fd5": "a real mesh node id",
    "f3491974573ec9e05edb3ee7bf1be8a773ee3fa2884dc05838c8f8cc99e4af0a": "a real mesh node id",
    "0acbcf5166a54c5582b216701247f15b97fd8026451e39bde3a099b57a18f52f": "a field-kit hostname",
    "efba505b4e1c6f0137badb1701f1b1d12921bc9be3739b0186662e59e36bba2a": "a field-kit hostname",
    "60ef29f488a8045e3f66784352db161e493ffc548fcf3a8626827a81ccb28a8b": "a Cloudloop thing or account id",
    "426d03ddef9be5849bb9a73aa59d9be7052c80298104d7607aaeef5dd797632c": "a Cloudloop thing or account id",
    "0365d2955923a0e1b3f25b531ecd2d89b1b4ba751f323f3dd2d724262f2f7562": "a Cloudloop thing or account id",
}

SKIP_PREFIXES = ("k8s/", "docs/swagger/", "cmd/meshsat-hub/web/dist/", "web/public/", "web/node_modules/", "scratchpad/")
SKIP_FILES = {".gitlab-ci.yml", "go.sum", "web/package-lock.json", "scripts/check-real-identifiers.py"}

# Candidate tokens: long digit runs (an IMEI, or a phone number with or without
# its "+" or "%2B"), 8-hex node ids, infrastructure hostnames, and the 32-char
# ids a satellite provider hands out.
CANDIDATES = re.compile(r"\d{10,15}|(?<![0-9a-fA-F])[0-9a-fA-F]{8}(?![0-9a-fA-F])|nllei01[a-z0-9]+|[A-Za-z0-9]{32}")


def digest(token: str) -> str:
    return hashlib.sha256(token.lower().encode()).hexdigest()


def main() -> int:
    if len(sys.argv) == 3 and sys.argv[1] == "--hash":
        print(f'    "{digest(sys.argv[2])}": "<what it is>",')
        return 0
    files = subprocess.run(["git", "ls-files"], capture_output=True, text=True, check=True).stdout.split("\n")
    found = []
    for path in files:
        if not path or path in SKIP_FILES or path.startswith(SKIP_PREFIXES):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                lines = fh.read().split("\n")
        except (UnicodeDecodeError, OSError):
            continue  # binary, or gone from the working tree
        for number, line in enumerate(lines, 1):
            for token in CANDIDATES.findall(line):
                what = KNOWN.get(digest(token))
                if what:
                    found.append((path, number, what))
    if not found:
        print("no known real device identity in the tracked files")
        return 0
    print("real device identities found in a public repository:\n")
    for path, number, what in found:
        print(f"  {path}:{number}: {what}")
    print("\nUse a made-up value instead: 300000000000001 for an IMEI, +31600000001 for a")
    print("number, !0a0b0c0d for a node id, bridge-kit-a for a bridge id. The value is not")
    print("printed here on purpose. To register new hardware with this check:")
    print("  python3 scripts/check-real-identifiers.py --hash <value>")
    return 1


if __name__ == "__main__":
    sys.exit(main())
