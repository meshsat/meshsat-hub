#!/usr/bin/env python3
"""Tests for bump-pin.py (MESHSAT-1047).

This script had no test, which is how the defect it now fixes survived: the
trailing comment was preserved on purpose, that was right for a hand-written
comment and wrong for the one describing the pin itself, and nothing asserted
either way.

The sharp edge these tests guard is NOT the happy path. It is that
kustomization.yaml carries seven HAND-WRITTEN comments beside other images --
which version, what the image is for, and in one case why a version is pinned at
all (the nats reload panic) -- and a rewrite that touched them would destroy
information while every deploy still looked fine.

Run: python3 k8s/scripts/bump_pin_test.py
"""

import os
import pathlib
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
SCRIPT = HERE / "bump-pin.py"

A = "sha256:" + "a" * 64
B = "sha256:" + "b" * 64

SAMPLE = """\
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
images:
  - name: ghcr.io/meshsat/meshsat-hub
    digest: {a}  # main 4790250e (2026-09-08)
  - name: docker.io/library/nats
    digest: {a}  # 2.14.6-alpine (MESHSAT-973: reload no longer panics)
  - name: docker.io/library/haproxy
    digest: {a}  # 3.1-alpine
  - name: registry.example/no-comment-here
    digest: {a}
""".format(a=A)

FAILURES = []


def run(path, name, digest, provenance=None):
    argv = [sys.executable, str(SCRIPT), str(path), name, digest]
    if provenance is not None:
        argv.append(provenance)
    p = subprocess.run(argv, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def fixture():
    fd, path = tempfile.mkstemp(suffix=".yaml")
    os.close(fd)
    pathlib.Path(path).write_text(SAMPLE)
    return pathlib.Path(path)


def check(ok, what):
    if ok:
        print(f"  PASS  {what}")
    else:
        print(f"  FAIL  {what}")
        FAILURES.append(what)


def siblings(text):
    """The comment lines that must never change, whatever is bumped."""
    return [ln for ln in text.splitlines()
            if "nats" in ln or "haproxy" in ln or "no-comment-here" in ln
            or "MESHSAT-973" in ln or "3.1-alpine" in ln]


def test_three_args_preserve_the_comment():
    """The pre-MESHSAT-1047 contract. Other repos still call it this way."""
    f = fixture()
    rc, out = run(f, "ghcr.io/meshsat/meshsat-hub", B)
    text = f.read_text()
    check(rc == 0, "three-argument form exits 0")
    check(B in text, "three-argument form writes the new digest")
    check("# main 4790250e (2026-09-08)" in text,
          "three-argument form leaves the old comment alone (unchanged contract)")
    f.unlink()


def test_provenance_rewrites_only_the_bumped_entry():
    f = fixture()
    before = siblings(f.read_text())
    rc, out = run(f, "ghcr.io/meshsat/meshsat-hub", B, "main deadbeef (2026-09-12)")
    text = f.read_text()
    check(rc == 0, "four-argument form exits 0")
    check("# main deadbeef (2026-09-12)" in text, "the new provenance is written")
    check("4790250e" not in text, "the stale commit is gone")
    check(siblings(text) == before,
          "every hand-written sibling comment is byte-identical (MESHSAT-973 note survives)")
    f.unlink()


def test_an_entry_without_a_comment_gains_one_only_when_asked():
    f = fixture()
    rc, _ = run(f, "registry.example/no-comment-here", B)
    line = [ln for ln in f.read_text().splitlines() if B in ln][0]
    check(rc == 0 and line.rstrip().endswith(B),
          "no provenance means no comment is invented")
    f.unlink()

    f = fixture()
    rc, _ = run(f, "registry.example/no-comment-here", B, "main cafe1234 (2026-09-12)")
    check("# main cafe1234 (2026-09-12)" in f.read_text(),
          "provenance adds a comment where there was none")
    f.unlink()


def test_unchanged_digest_is_still_exit_3_and_changes_nothing():
    """Exit 3 short-circuits. It must not rewrite the comment either, or a
    re-run of the same pipeline would silently restamp the date."""
    f = fixture()
    original = f.read_text()
    rc, out = run(f, "ghcr.io/meshsat/meshsat-hub", A, "main deadbeef (2026-09-12)")
    check(rc == 3, "same digest exits 3")
    check(f.read_text() == original, "same digest leaves the file byte-identical")
    f.unlink()


def test_missing_entry_is_loud():
    f = fixture()
    original = f.read_text()
    rc, out = run(f, "registry.example/not-in-this-file", B, "x")
    check(rc == 4, "an absent image exits 4 rather than bumping nothing quietly")
    check(f.read_text() == original, "an absent image changes nothing")
    f.unlink()


def test_refusals():
    f = fixture()
    original = f.read_text()
    rc, _ = run(f, "ghcr.io/meshsat/meshsat-hub", "sha256:nonsense", "x")
    check(rc == 2, "a malformed digest is refused")
    rc, _ = run(f, "ghcr.io/meshsat/meshsat-hub", B, "line one\nline two")
    check(rc == 2, "multi-line provenance is refused (it would corrupt the YAML)")
    check(f.read_text() == original, "neither refusal wrote anything")
    f.unlink()


def test_the_result_is_still_valid_yaml():
    try:
        import yaml
    except ImportError:
        print("  SKIP  pyyaml not installed, cannot assert the result parses")
        return
    f = fixture()
    run(f, "ghcr.io/meshsat/meshsat-hub", B, "main deadbeef (2026-09-12)")
    doc = yaml.safe_load(f.read_text())
    entry = [i for i in doc["images"] if i["name"] == "ghcr.io/meshsat/meshsat-hub"][0]
    check(entry["digest"] == B, "the bumped digest is what YAML actually parses")
    check(len(doc["images"]) == 4, "no entry was lost")
    f.unlink()


def main():
    print(f"bump-pin.py tests ({SCRIPT})")
    for fn in sorted(
        (v for k, v in globals().items() if k.startswith("test_")),
        key=lambda f: f.__code__.co_firstlineno,
    ):
        print(f" {fn.__name__}")
        fn()
    print()
    if FAILURES:
        print(f"{len(FAILURES)} failure(s):")
        for f in FAILURES:
            print(f"  - {f}")
        return 1
    print("all good")
    return 0


if __name__ == "__main__":
    sys.exit(main())
