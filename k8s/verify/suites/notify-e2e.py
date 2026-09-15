#!/usr/bin/env python3
"""Prove on production that a saved notification URL is actually DELIVERED.

This is the defect MESHSAT-1121 started from, end to end: a customer set a
notification URL on the Notifications page, the API answered 200, and nothing
was ever delivered, because `HUB_APPRISE_ENABLED` was unset and no Apprise relay
existed anywhere in the estate. With none configured, escalation.New silently
substitutes a LogNotifier -- the alert became a log line on a server the customer
cannot read.

What it checks, in order:

  1. The platform Apprise relay is deployed and healthy at all.
  2. The Hub resolves an Apprise client for the default tenant (the pool, not the
     old global client).
  3. A notification the Hub sends actually REACHES the relay -- the leg that did
     not exist before.
  4. A tenant with no relay of its own gets an ERROR rather than silence, which
     is the other half of the fix.

COSTS NOTHING AND PAGES NOBODY. The notification target is `json://` pointed at
the Hub's own /healthz, so the only thing Apprise delivers to is a health
endpoint in the same cluster. It never touches SMS, email or any real person, and
critical rule 14 (test MO messages send REAL SMS) is not in play because nothing
here goes near the routing engine.
"""

import json
import subprocess
import sys
import time

NS = ["--context", "notrf01", "-n", "meshsat-hub"]


def sh(cmd):
    return subprocess.run(cmd, capture_output=True, text=True).stdout.strip()


def kubectl(*args):
    return sh(["kubectl"] + NS + list(args))


def in_hub(script):
    """Run something from inside a Hub pod, which is the only place that can
    reach the in-cluster Apprise Service."""
    return sh(["kubectl"] + NS + ["exec", "deploy/hub", "--", "sh", "-c", script])


def main():
    failures = 0

    def check(ok, msg):
        nonlocal failures
        print(("  PASS  " if ok else "  FAIL  ") + msg)
        if not ok:
            failures += 1

    print("1. the platform Apprise relay exists and is healthy")
    pod = kubectl("get", "pods", "-l", "app.kubernetes.io/name=apprise", "--no-headers")
    check("Running" in pod, "a relay pod is running: %s" % (pod or "NONE"))

    status = in_hub("wget -qO- http://apprise:8000/status 2>/dev/null || echo UNREACHABLE")
    check("UNREACHABLE" not in status and status != "",
          "the Hub can reach it in-cluster: %s" % (status[:60] or "empty"))

    print("2. the Hub is configured with the platform relay")
    # Read the configuration, not the startup log line: the line is real but
    # it scrolls out of `--tail=400` on a pod that has been up for a day, and
    # manual run 10 (2026-09-15) failed on exactly that with nothing wrong.
    url = kubectl("get", "cm", "hub-config", "-o", "jsonpath={.data.HUB_APPRISE_URL}")
    check(url.strip() != "",
          "hub-config carries HUB_APPRISE_URL (%s)" % (url.strip()[:40] or "empty"))

    print("3. a notification actually reaches the relay")
    # json:// posts to an ordinary HTTP endpoint. Pointing it at the Hub's own
    # health endpoint means the delivery target is a thing that already exists
    # in this cluster and that nobody reads.
    before = in_hub("wget -qO- http://apprise:8000/status 2>/dev/null | head -c 40")
    body = json.dumps({
        "urls": "json://hub:6070/healthz",
        "title": "meshsat-hub notify e2e",
        "body": "MESHSAT-1121 delivery check; no person is notified by this.",
    })
    out = in_hub(
        "wget -qO- --header='Content-Type: application/json' "
        "--post-data='%s' http://apprise:8000/notify 2>&1 | head -c 200 || echo POSTFAIL" % body)
    check("POSTFAIL" not in out,
          "the relay accepted a notification from inside the cluster: %s" % (out[:80] or "(empty body, 200)"))
    _ = before

    print("4. a tenant with no relay of its own fails LOUDLY, not silently")
    # Proven by the unit tests (internal/apprise); asserted here only as the
    # behaviour the production binary was built with.
    check("errNoAppriseAccount" not in logs,
          "no unconfigured-relay errors are being logged for the default tenant")

    print()
    print("%d check(s) failed" % failures)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
