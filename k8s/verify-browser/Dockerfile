# The browser the nightly real-browser check runs in (MESHSAT-1224), built and
# pushed by the package:verify-browser CI job as
# ghcr.io/meshsat/meshsat-hub-browser-verify:<PLAYWRIGHT_VERSION>; pinned in the
# root k8s/kustomization.yaml images: block.
#
# Why we build at all, when upstream publishes a "python" image: that image
# ships the BROWSERS under /ms-playwright and the OS libraries they need, but
# NOT the playwright Python package -- `python3 -m pip show playwright` in it
# reports "Package(s) not found". Measured on 2026-09-20, after the first
# in-cluster run died on `ModuleNotFoundError: No module named 'playwright'`.
# Installing it at container start was the alternative and was rejected: the
# Job runs on a read-only root filesystem as a non-root user, and a nightly
# security check that pip-installs an unpinned package from the internet every
# time it runs is not a security check.
FROM mcr.microsoft.com/playwright/python@sha256:678457c4c323b981d8b4befc57b95366bb1bb6aa30057b1269f6b171e8d9975a

# Must match the browsers the base image carries, or playwright refuses to
# launch them: the base is v1.58.0-noble.
ARG PLAYWRIGHT_VERSION=1.58.0

# Ubuntu 24.04 marks the system python as externally managed (PEP 668), so the
# package goes in its own venv rather than fighting that with a flag -- and
# python3-venv has to come first, because the base image carries a python
# without ensurepip ("The virtual environment was not created successfully
# because ensurepip is not available", build 10:16Z 2026-09-20).
# curl is for run.sh's ntfy push on failure.
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3-venv curl \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir "playwright==${PLAYWRIGHT_VERSION}" \
 && /opt/venv/bin/python -c "from playwright.sync_api import sync_playwright; print('playwright import OK')"

ENV PATH=/opt/venv/bin:$PATH \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# pwuser, the unprivileged user the base image ships with and the owner of the
# browser cache. The venv is root-owned and world-readable, which is what we
# want: the running job cannot modify its own dependencies.
USER 1000:1000
ENTRYPOINT ["/bin/bash", "/suites/run.sh"]
